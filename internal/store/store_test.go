package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "nested", "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func countRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestInsertBatchIsIdempotentOnRequestID(t *testing.T) {
	s := openTemp(t)
	rows := []Row{{RequestID: "a", TraceID: "t1", RequestedAt: 1, Model: "m", InputTokens: 10}, {RequestID: "b", RequestedAt: 2}}
	if err := s.InsertBatch(rows); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertBatch(rows[:1]); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s); n != 2 {
		t.Fatalf("rows = %d", n)
	}
	var model string
	var input int64
	if err := s.DB().QueryRow(`SELECT model, input_tokens FROM requests WHERE request_id='a'`).Scan(&model, &input); err != nil {
		t.Fatal(err)
	}
	if model != "m" || input != 10 {
		t.Fatalf("model=%q input=%d", model, input)
	}
}

func TestReopenKeepsDataAndSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertBatch([]Row{{RequestID: "a", RequestedAt: 1}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n := countRows(t, s); n != 1 {
		t.Fatalf("rows = %d", n)
	}
}

func TestPruneRemovesOldRows(t *testing.T) {
	s := openTemp(t)
	old := time.Unix(100, 0)
	if err := s.InsertBatch([]Row{{RequestID: "old", RequestedAt: old.UnixMilli()}, {RequestID: "new", RequestedAt: time.Unix(1000, 0).UnixMilli()}}); err != nil {
		t.Fatal(err)
	}
	n, err := s.Prune(time.Unix(500, 0))
	if err != nil || n != 1 {
		t.Fatalf("Prune = %d, %v", n, err)
	}
	if c := countRows(t, s); c != 1 {
		t.Fatalf("rows = %d", c)
	}
}

func TestKVRoundTrip(t *testing.T) {
	s := openTemp(t)
	if v, err := s.LoadKV("missing"); err != nil || v != nil {
		t.Fatalf("missing = %q, %v", v, err)
	}
	if err := s.SaveKV("k", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveKV("k", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if v, err := s.LoadKV("k"); err != nil || string(v) != "v2" {
		t.Fatalf("k = %q, %v", v, err)
	}
}

func TestOpenFailsOnReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := Open(filepath.Join(dir, "sub", "a.db")); err == nil {
		t.Fatal("expected error")
	}
}
