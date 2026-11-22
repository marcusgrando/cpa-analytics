package store

import (
	"database/sql"
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

// Plugin 0.1.3 and earlier stored Claude calls with thinking as inconsistent,
// all tokens unclassified. Their raw counters are kept, so migration 002
// reclassifies them and leaves every other row alone.
func TestMigrationReclassifiesClaudeThinkingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	inconsistent := func(id, provider, executor string, in, cacheRead, cacheWrite, out, reasoning, total int64) Row {
		return Row{RequestID: id, RequestedAt: 1, Provider: provider, ExecutorType: executor,
			InputTokens: in, CacheReadTokens: cacheRead, CachedTokens: cacheRead, CacheCreationTokens: cacheWrite,
			OutputTokens: out, ReasoningTokens: reasoning, TotalTokens: total, Unclassified: total, AccountingQuality: "inconsistent"}
	}
	rows := []Row{
		inconsistent("claude-thinking", "claude", "ClaudeExecutor", 6, 90000, 1200, 500, 300, 91706),
		inconsistent("claude-mismatch", "claude", "ClaudeExecutor", 6, 900, 0, 500, 300, 1000),
		inconsistent("codex", "codex", "CodexExecutor", 6, 90000, 1200, 500, 300, 91706),
		inconsistent("compat", "claude-proxy", "OpenAICompatExecutor", 6, 90000, 1200, 500, 300, 91706),
	}
	if err := s.InsertBatch(rows); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`DELETE FROM schema_migrations WHERE version = '002_claude_thinking'`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	type buckets struct {
		uncached, cacheRead, cacheWrite, output, reasoning, unclassified int64
		quality                                                          string
	}
	got := map[string]buckets{}
	rs, err := s.DB().Query(`SELECT request_id, uncached_input, cache_read, cache_write, output_non_reasoning, output_reasoning, unclassified, accounting_quality FROM requests`)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	for rs.Next() {
		var id string
		var b buckets
		if err := rs.Scan(&id, &b.uncached, &b.cacheRead, &b.cacheWrite, &b.output, &b.reasoning, &b.unclassified, &b.quality); err != nil {
			t.Fatal(err)
		}
		got[id] = b
	}
	if want := (buckets{6, 90000, 1200, 200, 300, 0, "complete"}); got["claude-thinking"] != want {
		t.Errorf("claude thinking row = %+v, want %+v", got["claude-thinking"], want)
	}
	for _, id := range []string{"claude-mismatch", "codex", "compat"} {
		if b := got[id]; b.quality != "inconsistent" || b.unclassified == 0 {
			t.Errorf("row %s changed: %+v", id, b)
		}
	}
}

func TestMigrationReparsesClientsAndReclassifiesMetaDevin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	rows := []Row{
		{RequestID: "ua1", RequestedAt: 1, Provider: "codex", UserAgent: "codex_app_server/0.170.0 (Linux)", ClientName: "other", ClientVersion: ""},
		{RequestID: "ua2", RequestedAt: 2, Provider: "antigravity", UserAgent: "antigravity/1.15.8 linux/amd64", ClientName: "other", ClientVersion: ""},
		{RequestID: "meta1", RequestedAt: 3, Provider: "meta", ExecutorType: "MetaExecutor", InputTokens: 500, CacheReadTokens: 300, OutputTokens: 100, ReasoningTokens: 20, TotalTokens: 600, Unclassified: 600, AccountingQuality: "unclassified"},
		{RequestID: "devin1", RequestedAt: 4, Provider: "devin", ExecutorType: "DevinExecutor", InputTokens: 400, CacheReadTokens: 100, OutputTokens: 50, ReasoningTokens: 30, TotalTokens: 480, Unclassified: 480, AccountingQuality: "unclassified"},
	}
	if err := s.InsertBatch(rows); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`DELETE FROM schema_migrations WHERE version IN ('004_reparse_clients', '005_meta_devin_accounting')`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var cn, cv string
	if err := s.DB().QueryRow(`SELECT client_name, client_version FROM requests WHERE request_id = 'ua1'`).Scan(&cn, &cv); err != nil {
		t.Fatal(err)
	}
	if cn != "codex" || cv != "0.170.0" {
		t.Errorf("ua1 = %q/%q, want codex/0.170.0", cn, cv)
	}
	if err := s.DB().QueryRow(`SELECT client_name, client_version FROM requests WHERE request_id = 'ua2'`).Scan(&cn, &cv); err != nil {
		t.Fatal(err)
	}
	if cn != "antigravity" || cv != "1.15.8" {
		t.Errorf("ua2 = %q/%q, want antigravity/1.15.8", cn, cv)
	}
	var uncached, cacheRead, outNR, outR, unclass int64
	var qual string
	if err := s.DB().QueryRow(`SELECT uncached_input, cache_read, output_non_reasoning, output_reasoning, unclassified, accounting_quality FROM requests WHERE request_id = 'meta1'`).Scan(&uncached, &cacheRead, &outNR, &outR, &unclass, &qual); err != nil {
		t.Fatal(err)
	}
	if uncached != 200 || cacheRead != 300 || outNR != 80 || outR != 20 || unclass != 0 || qual != "complete" {
		t.Errorf("meta1 = %d/%d/%d/%d/%d %q", uncached, cacheRead, outNR, outR, unclass, qual)
	}
	if err := s.DB().QueryRow(`SELECT uncached_input, cache_read, output_non_reasoning, output_reasoning, unclassified, accounting_quality FROM requests WHERE request_id = 'devin1'`).Scan(&uncached, &cacheRead, &outNR, &outR, &unclass, &qual); err != nil {
		t.Fatal(err)
	}
	if uncached != 300 || cacheRead != 100 || outNR != 50 || outR != 30 || unclass != 0 || qual != "complete" {
		t.Errorf("devin1 = %d/%d/%d/%d/%d %q", uncached, cacheRead, outNR, outR, unclass, qual)
	}
}

func TestInsertAndPruneQuotaSnapshots(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	old := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	keep := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	reset := keep.Add(7 * 24 * time.Hour).UnixMilli()
	snaps := []QuotaSnapshot{
		{RecordedAt: old.UnixMilli(), Provider: "antigravity", AuthIndex: "ag1", GroupID: "gemini", GroupLabel: "Gemini", WindowKind: "weekly", UsedPct: 12.5, ResetAt: sql.NullInt64{Int64: reset, Valid: true}},
		{RecordedAt: keep.UnixMilli(), Provider: "antigravity", AuthIndex: "ag1", GroupID: "gemini", GroupLabel: "Gemini", WindowKind: "weekly", UsedPct: 24.0, ResetAt: sql.NullInt64{Int64: reset, Valid: true}},
		{RecordedAt: keep.UnixMilli(), Provider: "antigravity", AuthIndex: "ag1", GroupID: "claude-and-gpt", GroupLabel: "Claude and GPT", WindowKind: "weekly", UsedPct: 0},
	}
	if err := s.InsertQuotaSnapshots(snaps); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prune(old.Add(24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM quota_snapshots`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("remaining quota snapshots = %d, want 2", count)
	}
}
