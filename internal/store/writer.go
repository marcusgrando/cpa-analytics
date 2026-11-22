package store

import (
	"sync"
	"sync/atomic"
	"time"
)

// WriterStats reports what the writer has done since it started.
type WriterStats struct {
	Written     uint64    `json:"written"`
	Dropped     uint64    `json:"dropped"`
	Queued      int       `json:"queued"`
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitempty"`
}

// Writer batches rows into the store on its own goroutine so usage handling
// never waits on SQLite. When the queue is full or a batch fails, rows are
// dropped and counted.
type Writer struct {
	store      *Store
	rows       chan Row
	flushEvery time.Duration
	batch      int
	logf       func(string, ...any)

	written, dropped atomic.Uint64
	pendingCount     atomic.Int64

	enqueueMu sync.RWMutex
	closed    bool

	mu          sync.Mutex
	lastError   string
	lastErrorAt time.Time
	lastLogAt   time.Time

	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// NewWriter starts a writer with a queue of capacity rows, flushing every
// flushEvery or when batch rows are waiting.
func NewWriter(s *Store, capacity int, flushEvery time.Duration, batch int, logf func(string, ...any)) *Writer {
	return newWriter(s, capacity, flushEvery, batch, logf, true)
}

func newWriter(s *Store, capacity int, flushEvery time.Duration, batch int, logf func(string, ...any), start bool) *Writer {
	w := &Writer{
		store: s, rows: make(chan Row, capacity), flushEvery: flushEvery, batch: batch, logf: logf,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	if start {
		go w.run()
	} else {
		close(w.done)
	}
	return w
}

// Enqueue hands a row to the writer. It returns false and counts a drop when
// the queue is full, the writer is closed, or the writer is nil.
func (w *Writer) Enqueue(r Row) bool {
	if w == nil {
		return false
	}
	w.enqueueMu.RLock()
	if w.closed {
		w.enqueueMu.RUnlock()
		w.dropped.Add(1)
		return false
	}
	select {
	case w.rows <- r:
		w.enqueueMu.RUnlock()
		return true
	default:
		w.enqueueMu.RUnlock()
		w.dropped.Add(1)
		return false
	}
}

// Stats returns a snapshot of the writer counters.
func (w *Writer) Stats() WriterStats {
	if w == nil {
		return WriterStats{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return WriterStats{
		Written: w.written.Load(), Dropped: w.dropped.Load(), Queued: len(w.rows) + int(w.pendingCount.Load()),
		LastError: w.lastError, LastErrorAt: w.lastErrorAt,
	}
}

// Close flushes queued rows and stops the writer. Safe to call more than once.
func (w *Writer) Close() {
	if w == nil {
		return
	}
	w.once.Do(func() {
		w.enqueueMu.Lock()
		w.closed = true
		w.enqueueMu.Unlock()
		close(w.stop)
	})
	<-w.done
}

func (w *Writer) run() {
	defer close(w.done)
	ticker := time.NewTicker(w.flushEvery)
	defer ticker.Stop()
	pending := make([]Row, 0, w.batch)
	for {
		select {
		case r := <-w.rows:
			pending = append(pending, r)
			w.pendingCount.Store(int64(len(pending)))
			if len(pending) >= w.batch {
				pending = w.flush(pending)
			}
		case <-ticker.C:
			pending = w.flush(pending)
		case <-w.stop:
			for {
				select {
				case r := <-w.rows:
					pending = append(pending, r)
					w.pendingCount.Store(int64(len(pending)))
				default:
					w.flush(pending)
					return
				}
			}
		}
	}
}

func (w *Writer) flush(pending []Row) []Row {
	if len(pending) == 0 {
		w.pendingCount.Store(0)
		return pending
	}
	if err := w.store.InsertBatch(pending); err != nil {
		w.dropped.Add(uint64(len(pending)))
		now := time.Now()
		w.mu.Lock()
		w.lastError = err.Error()
		w.lastErrorAt = now
		shouldLog := now.Sub(w.lastLogAt) >= time.Minute
		if shouldLog {
			w.lastLogAt = now
		}
		w.mu.Unlock()
		if shouldLog && w.logf != nil {
			w.logf("cpa-analytics: dropped %d usage rows: %v", len(pending), err)
		}
	} else {
		w.written.Add(uint64(len(pending)))
		w.mu.Lock()
		w.lastError = ""
		w.mu.Unlock()
	}
	w.pendingCount.Store(0)
	return pending[:0]
}
