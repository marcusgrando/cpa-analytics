package store

import (
	"fmt"
	"testing"
	"time"
)

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWriterFlushesBatches(t *testing.T) {
	s := openTemp(t)
	w := NewWriter(s, 100, 10*time.Millisecond, 3, func(string, ...any) {})
	defer w.Close()
	for i := 0; i < 7; i++ {
		if !w.Enqueue(Row{RequestID: fmt.Sprint(i), RequestedAt: int64(i)}) {
			t.Fatal("enqueue refused")
		}
	}
	waitFor(t, func() bool { return w.Stats().Written == 7 })
	if n := countRows(t, s); n != 7 {
		t.Fatalf("rows = %d", n)
	}
}

func TestWriterDropsWhenFull(t *testing.T) {
	s := openTemp(t)
	w := newWriter(s, 1, time.Hour, 500, func(string, ...any) {}, false)
	defer w.Close()
	w.Enqueue(Row{RequestID: "a"})
	if w.Enqueue(Row{RequestID: "b"}) {
		t.Fatal("enqueue accepted on full channel")
	}
	if got := w.Stats().Dropped; got != 1 {
		t.Fatalf("dropped = %d", got)
	}
}

func TestWriterKeepsRunningAfterStoreError(t *testing.T) {
	s := openTemp(t)
	var logged []string
	w := NewWriter(s, 10, 5*time.Millisecond, 10, func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
	defer w.Close()
	s.Close()
	w.Enqueue(Row{RequestID: "a"})
	waitFor(t, func() bool { return w.Stats().LastError != "" })
	st := w.Stats()
	if st.Dropped != 1 || st.Written != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if !w.Enqueue(Row{RequestID: "b"}) {
		t.Fatal("writer stopped accepting rows")
	}
}

func TestNilWriterCountsDrops(t *testing.T) {
	var w *Writer
	if w.Enqueue(Row{RequestID: "a"}) {
		t.Fatal("nil writer accepted row")
	}
	w.Close()
}
