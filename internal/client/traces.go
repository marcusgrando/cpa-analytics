package client

import (
	"container/list"
	"sync"
	"sync/atomic"
	"time"
)

// Traces remembers the User-Agent of each inbound request, keyed by CPA's
// trace ID, until the usage record for that request arrives.
type Traces struct {
	ttl      time.Duration
	capacity int
	now      func() time.Time

	mu      sync.Mutex
	order   *list.List // front = oldest
	entries map[string]*list.Element

	hits, misses atomic.Uint64
}

type traceEntry struct {
	traceID string
	ua      string
	at      time.Time
}

// NewTraces builds a map that forgets entries after ttl and holds at most capacity entries.
func NewTraces(ttl time.Duration, capacity int, now func() time.Time) *Traces {
	return &Traces{ttl: ttl, capacity: capacity, now: now, order: list.New(), entries: make(map[string]*list.Element)}
}

// Put records the User-Agent seen for a trace.
func (t *Traces) Put(traceID, ua string) {
	if traceID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if el, ok := t.entries[traceID]; ok {
		t.order.Remove(el)
	}
	t.entries[traceID] = t.order.PushBack(&traceEntry{traceID: traceID, ua: ua, at: t.now()})
	for t.order.Len() > t.capacity {
		t.removeLocked(t.order.Front())
	}
}

// Take returns the User-Agent for a trace. The entry stays, because one
// inbound request can produce several usage records (retries, split billing).
func (t *Traces) Take(traceID string) (string, bool) {
	if traceID == "" {
		t.misses.Add(1)
		return "", false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expireLocked()
	el, ok := t.entries[traceID]
	if !ok {
		t.misses.Add(1)
		return "", false
	}
	t.hits.Add(1)
	return el.Value.(*traceEntry).ua, true
}

// Stats returns how many lookups found or missed a trace.
func (t *Traces) Stats() (hits, misses uint64) { return t.hits.Load(), t.misses.Load() }

// Len returns the number of live entries.
func (t *Traces) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.order.Len()
}

func (t *Traces) expireLocked() {
	cutoff := t.now().Add(-t.ttl)
	for el := t.order.Front(); el != nil; el = t.order.Front() {
		if !el.Value.(*traceEntry).at.Before(cutoff) {
			return
		}
		t.removeLocked(el)
	}
}

func (t *Traces) removeLocked(el *list.Element) {
	delete(t.entries, el.Value.(*traceEntry).traceID)
	t.order.Remove(el)
}
