package audit

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
)

// MemorySink keeps the chain in memory (tests and benchmarks).
type MemorySink struct {
	mu     sync.Mutex
	events []Event
}

// NewMemorySink returns an empty sink.
func NewMemorySink() *MemorySink { return &MemorySink{} }

// Append implements Sink.
func (m *MemorySink) Append(_ context.Context, events []Event) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	head, pos := GenesisHash, int64(0)
	if n := len(m.events); n > 0 {
		head, pos = m.events[n-1].Hash, m.events[n-1].ChainPos
	}
	batch := slices.Clone(events)
	if err := chain(batch, pos, head); err != nil {
		return nil, err
	}
	m.events = append(m.events, batch...)
	return batch, nil
}

// Events returns a copy of the chain in order.
func (m *MemorySink) Events() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.events)
}

// MemorySeq is an in-process sequence.
type MemorySeq struct{ n atomic.Int64 }

// NewMemorySeq starts at 1.
func NewMemorySeq() *MemorySeq { return &MemorySeq{} }

// Next implements SeqAllocator.
func (s *MemorySeq) Next(context.Context) (int64, error) { return s.n.Add(1), nil }
