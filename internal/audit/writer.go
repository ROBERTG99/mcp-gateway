package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/clock"
)

// ErrBackpressure means the audit buffer is full (or no seq could be allocated) within the
// enqueue timeout; the call must fail with -32048 before any side effect.
var ErrBackpressure = errors.New("audit backpressure")

// Sink persists a batch, extending the hash chain, and returns the stored events.
type Sink interface {
	Append(ctx context.Context, events []Event) ([]Event, error)
}

// SeqAllocator hands out unique event sequence numbers.
type SeqAllocator interface {
	Next(ctx context.Context) (int64, error)
}

// Options tune the writer (NFR-6 defaults).
type Options struct {
	Buffer          int           // events (default 10,000)
	FlushInterval   time.Duration // default 50 ms
	Batch           int           // default 500
	EnqueueTimeout  time.Duration // default 1 ms
	ShutdownTimeout time.Duration // drain budget on shutdown (default 10 s)
	Clock           clock.Clock
	Logger          *slog.Logger
	// OnFlush observes each persisted batch (metrics).
	OnFlush func(stored []Event, lag time.Duration)
}

// Writer buffers events and appends them in batches.
type Writer struct {
	sink  Sink
	seqs  SeqAllocator
	opts  Options
	slots chan struct{}
	queue chan Event
	flush chan chan error

	pending atomic.Int64
	closed  atomic.Bool // set when Run starts draining; later reservations fail
}

// NewWriter returns a writer; call Run to start flushing.
func NewWriter(sink Sink, seqs SeqAllocator, opts Options) *Writer {
	if opts.Buffer == 0 {
		opts.Buffer = 10000
	}
	if opts.FlushInterval == 0 {
		opts.FlushInterval = 50 * time.Millisecond
	}
	if opts.Batch == 0 {
		opts.Batch = 500
	}
	if opts.EnqueueTimeout == 0 {
		opts.EnqueueTimeout = time.Millisecond
	}
	if opts.ShutdownTimeout == 0 {
		opts.ShutdownTimeout = 10 * time.Second
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Writer{sink: sink, seqs: seqs, opts: opts, slots: make(chan struct{}, opts.Buffer),
		queue: make(chan Event, opts.Buffer), flush: make(chan chan error)}
}

// Ticket is a reserved audit slot with its sequence number. Exactly one of Commit or Cancel
// must be called.
type Ticket struct {
	w    *Writer
	seq  int64
	done atomic.Bool
}

// Seq is the event's sequence number (reported to the agent as _meta.mcpgw.audit_seq).
func (t *Ticket) Seq() int64 { return t.seq }

// Reserve takes a buffer slot and a sequence number within the enqueue timeout.
func (w *Writer) Reserve(ctx context.Context) (*Ticket, error) {
	select {
	case w.slots <- struct{}{}:
	default:
		timer := time.NewTimer(w.opts.EnqueueTimeout)
		defer timer.Stop()
		select {
		case w.slots <- struct{}{}:
		case <-timer.C:
			return nil, ErrBackpressure
		case <-ctx.Done():
			return nil, ErrBackpressure
		}
	}
	// Checked after taking the slot: the drain sets closed before it waits for the slots to empty,
	// so a reservation either sees closed or is waited for.
	if w.closed.Load() {
		<-w.slots
		return nil, fmt.Errorf("%w: audit writer stopped", ErrBackpressure)
	}
	sctx, cancel := context.WithTimeout(ctx, w.opts.EnqueueTimeout+5*time.Millisecond)
	defer cancel()
	seq, err := w.seqs.Next(sctx)
	if err != nil {
		<-w.slots
		return nil, fmt.Errorf("%w: allocate seq: %w", ErrBackpressure, err)
	}
	return &Ticket{w: w, seq: seq}, nil
}

// Commit queues the event (never blocks: the slot was reserved).
func (t *Ticket) Commit(e Event) {
	if !t.done.CompareAndSwap(false, true) {
		return
	}
	e.Seq = t.seq
	e.Sanitize()
	t.w.pending.Add(1)
	t.w.queue <- e
}

// Cancel releases an unused reservation.
func (t *Ticket) Cancel() {
	if t.done.CompareAndSwap(false, true) {
		<-t.w.slots
	}
}

// Depth is the number of reserved or queued, not yet persisted, events.
func (w *Writer) Depth() int { return len(w.slots) }

// Capacity is the buffer size.
func (w *Writer) Capacity() int { return w.opts.Buffer }

// Flush persists everything committed so far (tests, admin verify).
func (w *Writer) Flush(ctx context.Context) error {
	reply := make(chan error, 1)
	select {
	case w.flush <- reply:
	case <-ctx.Done():
		return fmt.Errorf("audit flush: %w", ctx.Err())
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return fmt.Errorf("audit flush: %w", ctx.Err())
	}
}

// Run flushes every FlushInterval or Batch events until ctx is cancelled, then drains within
// ShutdownTimeout. Failed batches are kept and retried; nothing is dropped while running.
func (w *Writer) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.opts.FlushInterval)
	defer ticker.Stop()
	var batch []Event
	var mu sync.Mutex // guards batch for clarity; Run is the only goroutine touching it
	persist := func(pctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		for len(batch) > 0 {
			n := min(len(batch), w.opts.Batch)
			start := w.opts.Clock.Now()
			stored, err := w.sink.Append(pctx, batch[:n])
			// An unstorable batch is never retried as is: halve it until the clean prefix is
			// stored, so the offending event ends up alone at the head of the batch.
			for errors.Is(err, ErrUnstorable) && n > 1 {
				n /= 2
				stored, err = w.sink.Append(pctx, batch[:n])
			}
			if errors.Is(err, ErrUnstorable) {
				stored, err = w.storePlaceholder(pctx, batch[0], err)
			}
			if err != nil {
				return err
			}
			if w.opts.OnFlush != nil {
				w.opts.OnFlush(stored, w.opts.Clock.Now().Sub(start))
			}
			batch = batch[n:]
			w.pending.Add(-int64(n))
			for range n {
				<-w.slots
			}
		}
		return nil
	}
	drainQueue := func() {
		for {
			select {
			case e := <-w.queue:
				batch = append(batch, e)
			default:
				return
			}
		}
	}
	for {
		select {
		case e := <-w.queue:
			batch = append(batch, e)
			if len(batch) >= w.opts.Batch {
				w.logErr(persist(ctx))
			}
		case <-ticker.C:
			drainQueue()
			w.logErr(persist(ctx))
		case reply := <-w.flush:
			drainQueue()
			reply <- persist(ctx)
		case <-ctx.Done():
			w.closed.Store(true)
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.opts.ShutdownTimeout)
			defer cancel()
			for {
				// Wait for in-flight reservations to commit, then persist, retrying until the deadline.
				drainQueue()
				err := persist(sctx)
				if err == nil && w.pending.Load() == 0 && len(w.slots) == 0 {
					return nil
				}
				if sctx.Err() != nil {
					if err == nil {
						err = sctx.Err()
					}
					return fmt.Errorf("audit drain: %d events not persisted: %w", len(batch)+len(w.queue), err)
				}
				select {
				case e := <-w.queue:
					batch = append(batch, e)
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
	}
}

// storePlaceholder replaces an event the sink refused by a placeholder with the same seq, identity
// and status but no args. If the sink refuses even that, the event is dropped and logged: one
// event must never stop the audit log (and with it every call) for good.
func (w *Writer) storePlaceholder(ctx context.Context, e Event, cause error) ([]Event, error) {
	w.opts.Logger.Error("audit event unstorable; storing a placeholder", "seq", e.Seq, "error", cause)
	stored, err := w.sink.Append(ctx, []Event{e.placeholder()})
	if errors.Is(err, ErrUnstorable) {
		w.opts.Logger.Error("audit placeholder unstorable; event dropped", "seq", e.Seq, "error", err)
		return nil, nil
	}
	return stored, err
}

func (w *Writer) logErr(err error) {
	if err != nil {
		w.opts.Logger.Error("audit flush failed; will retry", "error", err, "depth", w.Depth())
	}
}
