package query

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/marcusgrando/cpa-analytics/internal/store"
)

type call struct {
	id, trace, session, model, account string
	at                                 time.Duration
	cacheWrite                         int64
	failed                             bool
}

func (c call) row() store.Row {
	return store.Row{
		RequestID: c.id, TraceID: c.trace, RequestedAt: day1.Add(c.at).UnixMilli(), Provider: "codex", Model: c.model,
		SessionID: c.session, AuthIndex: c.account, Generate: true, Failed: c.failed, CacheWrite: c.cacheWrite,
	}
}

// CPA binds a session and model to one account, because the prompt cache
// belongs to the account. A call that lands on another account than the
// previous call of its session and model is a switch, and the cache it
// writes is paid again.
func TestAffinityCountsAccountSwitchesWithinASessionAndModel(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m := time.Minute
	calls := []call{
		{id: "1", trace: "t1", session: "s1", model: "gpt-5", account: "acc1", at: 1 * m, cacheWrite: 100},
		{id: "2", trace: "t2", session: "s1", model: "gpt-5", account: "acc1", at: 2 * m},
		{id: "3", trace: "t3", session: "s1", model: "gpt-5", account: "acc2", at: 3 * m, cacheWrite: 5000},
		// A failed attempt is not a call.
		{id: "4", trace: "t4", session: "s1", model: "gpt-5", account: "acc3", at: 4 * m, failed: true},
		{id: "5", trace: "t5", session: "s1", model: "gpt-5", account: "acc2", at: 5 * m},
		{id: "6", trace: "t6", session: "s1", model: "gpt-5", account: "acc1", at: 6 * m, cacheWrite: 3000},
		{id: "7", trace: "t7", session: "s2", model: "gpt-5", account: "acc1", at: 1 * m, cacheWrite: 200},
		// Split billing: two rows of one call.
		{id: "8a", trace: "t8", session: "s2", model: "gpt-5", account: "acc1", at: 2 * m, cacheWrite: 10},
		{id: "8b", trace: "t8", session: "s2", model: "gpt-5", account: "acc1", at: 2 * m, cacheWrite: 20},
		// Another model is bound on its own.
		{id: "9", trace: "t9", session: "s2", model: "gpt-5.1", account: "acc2", at: 3 * m},
		{id: "10", trace: "t10", session: "s2", model: "gpt-5.1", account: "acc2", at: 4 * m},
		// A single call cannot switch. Rows without a session or credential are not counted.
		{id: "11", trace: "t11", session: "s3", model: "gpt-5", account: "acc1", at: 1 * m, cacheWrite: 7},
		{id: "12", trace: "t12", session: "", model: "gpt-5", account: "acc1", at: 1 * m},
		{id: "13", trace: "t13", session: "s4", model: "gpt-5", account: "", at: 1 * m},
		{id: "14", trace: "t14", session: "s4", model: "gpt-5", account: "", at: 2 * m},
	}
	rows := make([]store.Row, len(calls))
	for i, c := range calls {
		rows[i] = c.row()
	}
	if err := s.InsertBatch(rows); err != nil {
		t.Fatal(err)
	}

	got, err := Affinity(s.DB(), rangeFilter(), testPrices(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Providers) != 1 {
		t.Fatalf("providers = %+v", got.Providers)
	}
	g := got.Providers[0]
	want := AffinityRow{
		Provider: "codex", Calls: 10, Sessions: 3, SwitchedSessions: 1, Switches: 2,
		CacheWrite: 8337, SwitchCacheWrite: 8000,
		CacheWriteUSD: 8337 * gpt5Input, SwitchCacheWriteUSD: 8000 * gpt5Input,
	}
	if g.Provider != want.Provider || g.Calls != want.Calls || g.Sessions != want.Sessions || g.SwitchedSessions != want.SwitchedSessions ||
		g.Switches != want.Switches || g.CacheWrite != want.CacheWrite || g.SwitchCacheWrite != want.SwitchCacheWrite ||
		!near(g.CacheWriteUSD, want.CacheWriteUSD) || !near(g.SwitchCacheWriteUSD, want.SwitchCacheWriteUSD) {
		t.Errorf("codex = %+v\nwant %+v", g, want)
	}
}
