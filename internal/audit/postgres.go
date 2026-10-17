package audit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// chainLockKey is the transaction advisory lock that serialises appends across replicas.
const chainLockKey int64 = 0x6d637067775f61 // "mcpgw_a"

// PostgresSink appends batches to audit_events under pg_advisory_xact_lock, so every replica
// extends the same chain (chain_pos is gap-free and totally ordered).
type PostgresSink struct {
	pool *pgxpool.Pool
}

// NewPostgresSink returns the sink.
func NewPostgresSink(pool *pgxpool.Pool) *PostgresSink { return &PostgresSink{pool: pool} }

// Append implements Sink. A batch whose COMMIT succeeded but whose acknowledgement was lost (the
// connection broke or ctx ended during COMMIT) comes back from the writer as a retry; its events
// then already exist under their seqs. Those are recognised as the same events and returned as
// stored, and only the rest is appended, so the retry neither duplicates nor wedges the writer. A
// stored row that differs from the event with its seq (e.g. a sequence reset after a restore)
// keeps the unique violation, and the writer stays failed closed.
func (p *PostgresSink) Append(ctx context.Context, events []Event) ([]Event, error) {
	stored, err := p.append(ctx, events)
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return stored, err
	}
	have, herr := p.storedSame(ctx, events)
	if herr != nil {
		return nil, fmt.Errorf("%w (checking for an earlier commit: %w)", err, herr)
	}
	if len(have) == 0 {
		return nil, err
	}
	var rest []Event
	for _, e := range events {
		if _, ok := have[e.Seq]; !ok {
			rest = append(rest, e)
		}
	}
	var added []Event
	if len(rest) > 0 {
		if added, err = p.append(ctx, rest); err != nil {
			return nil, err
		}
	}
	out := make([]Event, 0, len(events))
	for _, e := range events {
		if s, ok := have[e.Seq]; ok {
			out = append(out, s)
		} else {
			out = append(out, added[0])
			added = added[1:]
		}
	}
	return out, nil
}

// storedSame returns the stored rows whose seq is in events, keyed by seq, if every one of them is
// the event with that seq (same identity, arguments hash, decision, status and timestamp); a row
// that differs makes it return none.
func (p *PostgresSink) storedSame(ctx context.Context, events []Event) (map[int64]Event, error) {
	bySeq := make(map[int64]Event, len(events))
	seqs := make([]int64, 0, len(events))
	for _, e := range events {
		bySeq[e.Seq] = e
		seqs = append(seqs, e.Seq)
	}
	rows, err := p.pool.Query(ctx, selectEvents+" WHERE seq = ANY($1)", seqs)
	if err != nil {
		return nil, fmt.Errorf("read stored seqs: %w", err)
	}
	defer rows.Close()
	have := map[int64]Event{}
	for rows.Next() {
		s, err := ScanEvent(rows)
		if err != nil {
			return nil, err
		}
		if !sameEvent(s, bySeq[s.Seq]) {
			return nil, nil
		}
		have[s.Seq] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read stored seqs: %w", err)
	}
	return have, nil
}

func sameEvent(stored, e Event) bool {
	return stored.Tenant == e.Tenant && stored.Agent == e.Agent && stored.Server == e.Server && stored.Tool == e.Tool &&
		stored.ArgsSHA256 == e.ArgsSHA256 && stored.Decision == e.Decision && stored.Status == e.Status &&
		ptrEq(stored.ApprovalID, e.ApprovalID) && ptrEq(stored.IdempotencyKey, e.IdempotencyKey) &&
		stored.TS.Equal(e.TS.Truncate(time.Microsecond))
}

func ptrEq(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func (p *PostgresSink) append(ctx context.Context, events []Event) ([]Event, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("audit append: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", chainLockKey); err != nil {
		return nil, fmt.Errorf("audit append: lock: %w", err)
	}
	headPos, headHash := int64(0), GenesisHash
	err = tx.QueryRow(ctx, "SELECT chain_pos, hash FROM audit_events ORDER BY chain_pos DESC LIMIT 1").Scan(&headPos, &headHash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("audit append: head: %w", err)
	}
	batch := make([]Event, len(events))
	copy(batch, events)
	if err := chain(batch, headPos, headHash); err != nil {
		return nil, err
	}
	rows := make([][]any, len(batch))
	for i, e := range batch {
		rows[i] = []any{e.Seq, e.ChainPos, e.TS, e.Tenant, e.Agent, e.Server, e.Tool, string(e.ArgsRedacted), e.ArgsSHA256,
			e.Decision, e.ApprovalID, e.IdempotencyKey, e.Status, e.LatencyMS, e.PrevHash, e.Hash}
	}
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"audit_events"}, []string{"seq", "chain_pos", "ts", "tenant", "agent", "server",
		"tool", "args_redacted", "args_sha256", "decision", "approval_id", "idempotency_key", "status", "latency_ms",
		"prev_hash", "hash"}, pgx.CopyFromRows(rows))
	if err != nil {
		var pgErr *pgconn.PgError
		// Data exceptions and check or not-null violations reject the rows themselves. A unique
		// violation (23505 on seq, chain_pos or hash) is not the event's fault, e.g. a sequence
		// reset after a restore: it stays transient so the writer fails closed (-32048) and the
		// operator sees it, instead of every event being replaced by a placeholder and dropped.
		if errors.As(err, &pgErr) && pgErr.Code != "23505" && (strings.HasPrefix(pgErr.Code, "22") || strings.HasPrefix(pgErr.Code, "23")) {
			return nil, fmt.Errorf("%w: audit append: copy: %w", ErrUnstorable, err)
		}
		return nil, fmt.Errorf("audit append: copy: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("audit append: commit: %w", err)
	}
	return batch, nil
}

const selectEvents = `SELECT seq, chain_pos, ts, tenant, agent, server, tool, args_redacted::text, args_sha256, decision,
	approval_id::text, idempotency_key, status, latency_ms, prev_hash, hash FROM audit_events`

// ScanEvent reads one audit row in selectEvents column order.
func ScanEvent(row pgx.Row) (Event, error) {
	var e Event
	var args string
	err := row.Scan(&e.Seq, &e.ChainPos, &e.TS, &e.Tenant, &e.Agent, &e.Server, &e.Tool, &args, &e.ArgsSHA256, &e.Decision,
		&e.ApprovalID, &e.IdempotencyKey, &e.Status, &e.LatencyMS, &e.PrevHash, &e.Hash)
	if err != nil {
		return e, fmt.Errorf("scan audit event: %w", err)
	}
	e.TS = e.TS.UTC()
	e.ArgsRedacted = []byte(args)
	e.Normalize() // jsonb reorders keys; the hash is over the canonical form
	return e, nil
}

// SelectColumns is the column list for audit queries (used by the admin API).
func SelectColumns() string { return selectEvents }

// VerifyPostgres streams the chain in chain_pos order (from the event with seq fromSeq, or the
// genesis when fromSeq is 0) and verifies it without loading everything into memory.
func VerifyPostgres(ctx context.Context, pool *pgxpool.Pool, fromSeq int64) (VerifyResult, error) {
	startPos := int64(0)
	anchor := GenesisHash
	if fromSeq > 0 {
		err := pool.QueryRow(ctx, "SELECT chain_pos FROM audit_events WHERE seq = $1", fromSeq).Scan(&startPos)
		if errors.Is(err, pgx.ErrNoRows) {
			return VerifyResult{}, ErrEventNotFound
		}
		if err != nil {
			return VerifyResult{}, fmt.Errorf("verify: seq %d: %w", fromSeq, err)
		}
		startPos--
		anchor = ""
	}
	rows, err := pool.Query(ctx, selectEvents+" WHERE chain_pos > $1 ORDER BY chain_pos", startPos)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("verify: query: %w", err)
	}
	defer rows.Close()
	res := VerifyResult{OK: true, HeadHash: anchor}
	prev := anchor
	expectedPos := startPos + 1
	for rows.Next() {
		e, err := ScanEvent(rows)
		if err != nil {
			return VerifyResult{}, err
		}
		if e.ChainPos != expectedPos {
			return VerifyResult{Checked: res.Checked, FirstBadSeq: e.Seq, Reason: "chain_pos gap (a row was deleted)"}, nil
		}
		r := VerifyChain([]Event{e}, prev)
		if !r.OK {
			r.Checked = res.Checked
			return r, nil
		}
		prev, expectedPos = e.Hash, expectedPos+1
		res.Checked++
	}
	if err := rows.Err(); err != nil {
		return VerifyResult{}, fmt.Errorf("verify: rows: %w", err)
	}
	res.HeadHash = prev
	return res, nil
}

// PostgresSeq allocates seqs from audit_events_seq in prefetched blocks so Reserve rarely
// touches the database. Refills are single-flight; callers that find the buffer empty wait for
// the refill (bounded by their context).
type PostgresSeq struct {
	pool  *pgxpool.Pool
	block int

	mu       sync.Mutex
	buf      []int64
	fetching bool
	filled   chan struct{} // closed and replaced after every refill attempt
	warmed   atomic.Bool   // set by the first successful refill
}

// Warmed reports whether a block of seqs has ever been allocated (readiness: before that, every
// reservation waits on Postgres and fails within the enqueue timeout). While it is false it starts
// a background refill, so a gateway whose warm-up failed recovers once Postgres is back even
// though, not being ready, it receives no calls that would trigger one.
func (s *PostgresSeq) Warmed() bool {
	if s.warmed.Load() {
		return true
	}
	s.startRefill()
	return false
}

// NewPostgresSeq returns an allocator fetching block values at a time.
func NewPostgresSeq(pool *pgxpool.Pool, block int) *PostgresSeq {
	if block <= 0 {
		block = 1024
	}
	return &PostgresSeq{pool: pool, block: block, filled: make(chan struct{})}
}

// Warm fetches the first block (call at startup so the first calls do not wait).
func (s *PostgresSeq) Warm(ctx context.Context) error { return s.fetch(ctx) }

// Next implements SeqAllocator.
func (s *PostgresSeq) Next(ctx context.Context) (int64, error) {
	for {
		s.mu.Lock()
		if len(s.buf) > 0 {
			v := s.buf[0]
			s.buf = s.buf[1:]
			low := len(s.buf) < s.block/4
			s.mu.Unlock()
			if low {
				s.startRefill()
			}
			return v, nil
		}
		wait := s.filled
		s.mu.Unlock()
		s.startRefill()
		select {
		case <-wait:
		case <-ctx.Done():
			return 0, fmt.Errorf("allocate audit seq: %w", ctx.Err())
		}
	}
}

func (s *PostgresSeq) startRefill() {
	s.mu.Lock()
	if s.fetching {
		s.mu.Unlock()
		return
	}
	s.fetching = true
	s.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.fetch(ctx)
	}()
}

func (s *PostgresSeq) fetch(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, "SELECT nextval('audit_events_seq') FROM generate_series(1, $1)", s.block)
	var vals []int64
	if err == nil {
		vals, err = pgx.CollectRows(rows, pgx.RowTo[int64])
	}
	if err == nil {
		s.warmed.Store(true)
	}
	s.mu.Lock()
	s.buf = append(s.buf, vals...)
	s.fetching = false
	close(s.filled)
	s.filled = make(chan struct{})
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("allocate audit seqs: %w", err)
	}
	return nil
}
