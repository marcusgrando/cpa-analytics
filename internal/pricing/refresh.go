package pricing

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

const kvKey = "pricing.litellm"

// KV persists the last good price table.
type KV interface {
	SaveKV(key string, value []byte) error
	LoadKV(key string) ([]byte, error)
}

// Status describes the price table currently in use.
type Status struct {
	URL         string    `json:"url"`
	FetchedAt   time.Time `json:"fetched_at"`
	Models      int       `json:"models"`
	LastAttempt time.Time `json:"last_attempt"`
	LastError   string    `json:"last_error,omitempty"`
}

// FallbackSource is an optional secondary price catalog merged only for models
// missing from the primary table.
type FallbackSource struct {
	Name  string
	URL   string
	Parse func([]byte) (*Table, error)
}

// Refresher keeps the LiteLLM table fresh, optionally merging fallback
// catalogs for models LiteLLM lacks. A failed fetch keeps the current table,
// or the copy saved in the KV store, so cost estimates degrade instead of
// disappearing.
type Refresher struct {
	fetch     func(url string) ([]byte, error)
	kv        KV
	url       string
	fallbacks []FallbackSource
	every     time.Duration
	now       func() time.Time

	mu          sync.Mutex
	table       *Table
	hasPrimary  bool
	fetchedAt   time.Time
	lastAttempt time.Time
	lastError   string
	started     bool
	stopped     bool

	stop chan struct{}
	done chan struct{}
}

// WithFallbacks attaches secondary catalogs fetched on each refresh to fill
// models missing from the primary table.
func (r *Refresher) WithFallbacks(sources ...FallbackSource) *Refresher {
	r.fallbacks = append(r.fallbacks, sources...)
	return r
}

type storedTable struct {
	FetchedAt time.Time       `json:"fetched_at"`
	Body      json.RawMessage `json:"body"`
}

// NewRefresher builds a refresher. Call Start to refresh in the background.
func NewRefresher(fetch func(string) ([]byte, error), kv KV, url string, every time.Duration) *Refresher {
	return &Refresher{
		fetch: fetch, kv: kv, url: url, every: every, now: time.Now,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
}

// Start refreshes now and then every interval until Stop.
func (r *Refresher) Start() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.started || r.stopped {
		r.mu.Unlock()
		return
	}
	r.started = true
	r.mu.Unlock()
	go func() {
		defer close(r.done)
		r.Refresh()
		ticker := time.NewTicker(r.every)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				r.Refresh()
			case <-r.stop:
				return
			}
		}
	}()
}

// Stop ends background refreshes. Safe to call more than once and without Start.
func (r *Refresher) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	wasStopped := r.stopped
	wasStarted := r.started
	r.stopped = true
	if !wasStopped {
		close(r.stop)
		if !wasStarted {
			close(r.done)
		}
	}
	r.mu.Unlock()
	<-r.done
}

// Refresh fetches the primary table and any configured fallbacks once.
func (r *Refresher) Refresh() {
	now := r.now()
	primary, err := r.fetchTable()
	fallbackTables := r.refreshFallbacks(now)
	if err == nil {
		var saveErr string
		if raw, errMarshal := json.Marshal(storedTable{FetchedAt: now, Body: primary.raw}); errMarshal == nil && r.kv != nil {
			if errSave := r.kv.SaveKV(kvKey, raw); errSave != nil {
				saveErr = errSave.Error()
			}
		}
		merged := Merge(primary.table, fallbackTables...)
		r.mu.Lock()
		r.lastAttempt = now
		r.table = merged
		r.hasPrimary = true
		r.fetchedAt = now
		r.lastError = saveErr
		r.mu.Unlock()
		return
	}

	r.mu.Lock()
	needStored := r.table == nil || !r.hasPrimary
	r.mu.Unlock()

	var loaded *Table
	var loadedAt time.Time
	var loadedPrimary bool
	if needStored {
		loaded, loadedAt, loadedPrimary = r.loadStored(fallbackTables)
	}
	r.mu.Lock()
	r.lastAttempt = now
	r.lastError = err.Error()
	if loaded != nil && (r.table == nil || !r.hasPrimary) {
		r.table = loaded
		r.fetchedAt = loadedAt
		r.hasPrimary = loadedPrimary
	}
	r.mu.Unlock()
}

func (r *Refresher) refreshFallbacks(now time.Time) []*Table {
	if len(r.fallbacks) == 0 {
		return nil
	}
	out := make([]*Table, 0, len(r.fallbacks))
	for _, fb := range r.fallbacks {
		key := "pricing.fallback." + fb.Name
		raw, err := r.fetch(fb.URL)
		if err == nil {
			if tbl, perr := fb.Parse(raw); perr == nil && tbl.Len() > 0 {
				out = append(out, tbl)
				if r.kv != nil {
					if data, merr := json.Marshal(storedTable{FetchedAt: now, Body: raw}); merr == nil {
						_ = r.kv.SaveKV(key, data)
					}
				}
				continue
			}
		}
		if r.kv == nil {
			continue
		}
		storedRaw, lerr := r.kv.LoadKV(key)
		if lerr != nil || len(storedRaw) == 0 {
			continue
		}
		var stored storedTable
		if json.Unmarshal(storedRaw, &stored) != nil {
			continue
		}
		if tbl, perr := fb.Parse(stored.Body); perr == nil && tbl.Len() > 0 {
			out = append(out, tbl)
		}
	}
	return out
}

type fetchedTable struct {
	table *Table
	raw   []byte
}

func (r *Refresher) fetchTable() (fetchedTable, error) {
	raw, err := r.fetch(r.url)
	if err != nil {
		return fetchedTable{}, fmt.Errorf("fetch price table: %w", err)
	}
	tbl, err := ParseLiteLLM(raw)
	if err != nil {
		return fetchedTable{}, err
	}
	if tbl.Len() == 0 {
		return fetchedTable{}, fmt.Errorf("price table has no priced models")
	}
	return fetchedTable{table: tbl, raw: raw}, nil
}

func (r *Refresher) loadStored(fallbacks []*Table) (*Table, time.Time, bool) {
	if r.kv != nil {
		raw, err := r.kv.LoadKV(kvKey)
		if err == nil && len(raw) > 0 {
			var stored storedTable
			if json.Unmarshal(raw, &stored) == nil {
				if tbl, err := ParseLiteLLM(stored.Body); err == nil {
					return Merge(tbl, fallbacks...), stored.FetchedAt, true
				}
			}
		}
	}
	if len(fallbacks) > 0 {
		return Merge(nil, fallbacks...), r.now(), false
	}
	return nil, time.Time{}, false
}

// Current returns the table in use and when it was fetched. With no table at
// all it returns an empty table and a zero time.
func (r *Refresher) Current() (*Table, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.table == nil {
		return &Table{rates: map[string]modelRate{}}, time.Time{}
	}
	return r.table, r.fetchedAt
}

// Status reports the table age and the last fetch error.
func (r *Refresher) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	models := 0
	if r.table != nil {
		models = r.table.Len()
	}
	return Status{URL: r.url, FetchedAt: r.fetchedAt, Models: models, LastAttempt: r.lastAttempt, LastError: r.lastError}
}
