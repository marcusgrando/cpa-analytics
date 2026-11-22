package query

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcusgrando/cpa-analytics/internal/pricing"
	"github.com/marcusgrando/cpa-analytics/internal/store"
)

type sample struct {
	id, account, model string
	at                 time.Time
	uncached           int64
	weekly             float64 // -1 when the response reported no limits
	reset              time.Time
}

func (s sample) row() store.Row {
	r := store.Row{
		RequestID: s.id, RequestedAt: s.at.UnixMilli(), Provider: "codex", Model: s.model, Generate: true,
		AuthIndex: s.account, UncachedInput: s.uncached,
	}
	if s.weekly >= 0 {
		r.LimitWeeklyPct = sql.NullFloat64{Float64: s.weekly, Valid: true}
		r.LimitWeeklyReset = sql.NullInt64{Int64: s.reset.UnixMilli(), Valid: true}
	}
	return r
}

// The cost between two snapshots of the same weekly cycle is what moved the
// weekly percent between them. Day 1 spends 4000 input tokens for 2 points.
// Day 2 starts a new cycle, so its first pair is dropped, then spends 8400
// tokens for 3.5 points. Day 3 moves less than one point and is left out, as
// is the unpriced account and the row with no credential.
func TestLimitBurnPricesEachPointOfTheWeeklyLimit(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := time.Hour
	week1, week2 := day1.Add(5*24*h), day1.Add(12*24*h)
	samples := []sample{
		{id: "a1", account: "acc1", model: "gpt-5", at: day1.Add(9 * h), uncached: 1000, weekly: 10, reset: week1},
		{id: "a2", account: "acc1", model: "gpt-5", at: day1.Add(10 * h), uncached: 2000, weekly: -1},
		{id: "a3", account: "acc1", model: "gpt-5", at: day1.Add(11 * h), uncached: 2000, weekly: 12, reset: week1},
		// A drop within the cycle cannot be priced. The next pair starts here.
		{id: "a4", account: "acc1", model: "gpt-5", at: day1.Add(12 * h), uncached: 5000, weekly: 11.9, reset: week1},
		{id: "a5", account: "acc1", model: "gpt-5", at: day2.Add(9 * h), uncached: 7000, weekly: 1, reset: week2},
		// Resets a few seconds apart are the same cycle.
		{id: "a6", account: "acc1", model: "gpt-5", at: day2.Add(10 * h), uncached: 8000, weekly: 4, reset: week2.Add(30 * time.Second)},
		{id: "a7", account: "acc1", model: "gpt-5", at: day2.Add(11 * h), uncached: 400, weekly: 4.5, reset: week2},
		{id: "a8", account: "acc1", model: "gpt-5", at: day3.Add(9 * h), uncached: 1000, weekly: 4.8, reset: week2},
		{id: "b1", account: "acc2", model: "unknown-model", at: day1.Add(9 * h), uncached: 1000, weekly: 10, reset: week1},
		{id: "b2", account: "acc2", model: "unknown-model", at: day1.Add(11 * h), uncached: 9000, weekly: 20, reset: week1},
		{id: "n1", account: "", model: "gpt-5", at: day1.Add(9 * h), uncached: 1000, weekly: 10, reset: week1},
		{id: "n2", account: "", model: "gpt-5", at: day1.Add(11 * h), uncached: 9000, weekly: 20, reset: week1},
	}
	rows := make([]store.Row, len(samples))
	for i, sm := range samples {
		rows[i] = sm.row()
	}
	if err := s.InsertBatch(rows); err != nil {
		t.Fatal(err)
	}

	got, err := LimitBurn(s.DB(), rangeFilter(), testPrices(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []BurnPoint{
		{T: day1.UnixMilli(), AuthIndex: "acc1", Provider: "codex", WeeklyPct: 2, CostUSD: 4000 * gpt5Input, Tokens: Tokens{UncachedInput: 4000}},
		{T: day2.UnixMilli(), AuthIndex: "acc1", Provider: "codex", WeeklyPct: 3.5, CostUSD: 8400 * gpt5Input, Tokens: Tokens{UncachedInput: 8400}},
	}
	if len(got.Points) != len(want) {
		t.Fatalf("points = %+v, want %+v", got.Points, want)
	}
	for i, w := range want {
		g := got.Points[i]
		if g.T != w.T || g.AuthIndex != w.AuthIndex || g.Provider != w.Provider || !near(g.WeeklyPct, w.WeeklyPct) || !near(g.CostUSD, w.CostUSD) ||
			!near(g.USDPerPct, w.CostUSD/w.WeeklyPct) || g.Tokens != w.Tokens {
			t.Errorf("point %d = %+v, want %+v and %v per point", i, g, w, w.CostUSD/w.WeeklyPct)
		}
	}
}

// The weekly percent belongs to the whole account, so a filter that keeps
// only part of its traffic would price the points with part of the cost.
func TestLimitBurnRejectsPartialAccountFilters(t *testing.T) {
	s := seedDB(t)
	for _, f := range []Filter{{Model: "gpt-5"}, {Client: "codex"}, {APIKeyHash: "abc123"}} {
		f.From, f.To = day1, day4
		var pe *ParamError
		if _, err := LimitBurn(s.DB(), f, testPrices(t)); !errors.As(err, &pe) {
			t.Errorf("filter %+v: err = %v, want a ParamError", f, err)
		}
	}
	f := rangeFilter()
	f.Provider = "codex"
	if _, err := LimitBurn(s.DB(), f, testPrices(t)); err != nil {
		t.Errorf("provider filter: %v", err)
	}
}

func insertSamples(t *testing.T, samples []sample) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	rows := make([]store.Row, len(samples))
	for i, sm := range samples {
		rows[i] = sm.row()
	}
	if err := s.InsertBatch(rows); err != nil {
		t.Fatal(err)
	}
	return s
}

// What an account had left of its weekly limit when the cycle ended is quota
// it paid for and never used. The value of each point comes from the cycle's
// own traffic.
func TestLimitLeftoverPricesWhatEachCycleLeftUnused(t *testing.T) {
	h := time.Hour
	now := day4.Add(-time.Minute)
	samples := []sample{
		// acc1 used 30% of a cycle that reset at day1 20:00: 4000 tokens moved it 20 points.
		{id: "a1", account: "acc1", model: "gpt-5", at: day1.Add(9 * h), uncached: 1000, weekly: 10, reset: day1.Add(20 * h)},
		{id: "a2", account: "acc1", model: "gpt-5", at: day1.Add(10 * h), uncached: 4000, weekly: -1},
		{id: "a3", account: "acc1", model: "gpt-5", at: day1.Add(11 * h), weekly: 30, reset: day1.Add(20 * h)},
		// Its next cycle is still running at now.
		{id: "a4", account: "acc1", model: "gpt-5", at: day2.Add(9 * h), weekly: 5, reset: day1.Add(20*h + 7*24*h)},
		// acc2 redeemed a reset early: a new cycle showed up before day3 12:00.
		{id: "b1", account: "acc2", model: "gpt-5", at: day1.Add(10 * h), weekly: 50, reset: day3.Add(12 * h)},
		{id: "b2", account: "acc2", model: "gpt-5", at: day2.Add(10 * h), weekly: 0, reset: day2.Add(10*h + 7*24*h)},
		// acc3's cycle ended before the range.
		{id: "c1", account: "acc3", model: "gpt-5", at: day0.Add(1 * h), weekly: 40, reset: day0.Add(12 * h)},
		// acc4's cycle began before the range and ended inside it.
		{id: "d1", account: "acc4", model: "gpt-5", at: day1.Add(-3 * 24 * h), weekly: 80, reset: day1.Add(2 * h)},
		// A Claude reset refills the percent within the same cycle.
		{id: "e1", account: "acc5", model: "gpt-5", at: day1.Add(9 * h), weekly: 90, reset: day3},
		{id: "e2", account: "acc5", model: "gpt-5", at: day1.Add(10 * h), weekly: 20, reset: day3},
		{id: "e3", account: "acc5", model: "gpt-5", at: day1.Add(11 * h), uncached: 2000, weekly: 25, reset: day3},
	}
	s := insertSamples(t, samples)

	got, err := LimitLeftover(s.DB(), rangeFilter(), testPrices(t), now)
	if err != nil {
		t.Fatal(err)
	}
	perPoint := 4000 * gpt5Input / 20
	want := []LeftoverCycle{
		{AuthIndex: "acc4", Provider: "codex", EndedAt: day1.Add(2 * h).UnixMilli(), LastReadingAt: day1.Add(-3 * 24 * h).UnixMilli(), UsedPct: 80, LeftPct: 20},
		{AuthIndex: "acc1", Provider: "codex", EndedAt: day1.Add(20 * h).UnixMilli(), LastReadingAt: day1.Add(11 * h).UnixMilli(), UsedPct: 30, LeftPct: 70, USDPerPct: perPoint, LeftUSD: 70 * perPoint},
		{AuthIndex: "acc2", Provider: "codex", EndedAt: day2.Add(10 * h).UnixMilli(), LastReadingAt: day1.Add(10 * h).UnixMilli(), Early: true, UsedPct: 50, LeftPct: 50},
		{AuthIndex: "acc5", Provider: "codex", EndedAt: day3.UnixMilli(), LastReadingAt: day1.Add(11 * h).UnixMilli(), UsedPct: 25, LeftPct: 75, USDPerPct: 2000 * gpt5Input / 5, LeftUSD: 75 * 2000 * gpt5Input / 5},
	}
	if len(got.Cycles) != len(want) {
		t.Fatalf("cycles = %+v, want %+v", got.Cycles, want)
	}
	for i, w := range want {
		g := got.Cycles[i]
		if g.AuthIndex != w.AuthIndex || g.Provider != w.Provider || g.EndedAt != w.EndedAt || g.LastReadingAt != w.LastReadingAt || g.Early != w.Early ||
			!near(g.UsedPct, w.UsedPct) || !near(g.LeftPct, w.LeftPct) || !near(g.USDPerPct, w.USDPerPct) || !near(g.LeftUSD, w.LeftUSD) {
			t.Errorf("cycle %d = %+v, want %+v", i, g, w)
		}
	}
}

func TestLimitLeftoverRejectsPartialAccountFilters(t *testing.T) {
	s := seedDB(t)
	f := rangeFilter()
	f.Model = "gpt-5"
	var pe *ParamError
	if _, err := LimitLeftover(s.DB(), f, testPrices(t), day4); !errors.As(err, &pe) {
		t.Errorf("err = %v, want a ParamError", err)
	}
}

// The history keeps the last reading of each account per bucket, and the last
// reading before the range so the page knows where each account started.
func TestLimitHistoryKeepsTheLastReadingPerBucket(t *testing.T) {
	h := time.Hour
	reset := day3.Add(5 * h)
	s := insertSamples(t, []sample{
		{id: "old", account: "acc1", model: "gpt-5", at: day0.Add(-8 * 24 * h), weekly: 99, reset: day0},
		{id: "before", account: "acc1", model: "gpt-5", at: day0.Add(20 * h), weekly: 40, reset: reset},
		{id: "a1", account: "acc1", model: "gpt-5", at: day1.Add(9 * h), weekly: 41, reset: reset},
		{id: "a2", account: "acc1", model: "gpt-5", at: day1.Add(9*h + 5*time.Minute), weekly: 42, reset: reset},
		{id: "a3", account: "acc1", model: "gpt-5", at: day1.Add(9*h + 20*time.Minute), weekly: 44, reset: reset},
		{id: "a4", account: "acc1", model: "gpt-5", at: day1.Add(10 * h), weekly: -1},
		{id: "b1", account: "acc2", model: "gpt-5", at: day2.Add(1 * h), weekly: 3, reset: reset.Add(24 * h)},
		{id: "n1", account: "", model: "gpt-5", at: day2.Add(1 * h), weekly: 3, reset: reset},
	})
	got, err := LimitHistory(s.DB(), rangeFilter())
	if err != nil {
		t.Fatal(err)
	}
	if got.BucketMs != 15*60*1000 {
		t.Errorf("bucket = %d, want 15 minutes", got.BucketMs)
	}
	want := []HistoryAccount{
		{AuthIndex: "acc1", Provider: "codex", Points: []HistoryPoint{
			{T: day0.Add(20 * h).UnixMilli(), WeeklyPct: 40, WeeklyReset: reset.UnixMilli()},
			{T: day1.Add(9*h + 5*time.Minute).UnixMilli(), WeeklyPct: 42, WeeklyReset: reset.UnixMilli()},
			{T: day1.Add(9*h + 20*time.Minute).UnixMilli(), WeeklyPct: 44, WeeklyReset: reset.UnixMilli()},
		}},
		{AuthIndex: "acc2", Provider: "codex", Points: []HistoryPoint{
			{T: day2.Add(1 * h).UnixMilli(), WeeklyPct: 3, WeeklyReset: reset.Add(24 * h).UnixMilli()},
		}},
	}
	if fmt.Sprint(got.Accounts) != fmt.Sprint(want) {
		t.Errorf("accounts = %+v\nwant %+v", got.Accounts, want)
	}
	f := rangeFilter()
	f.Client = "codex"
	var pe *ParamError
	if _, err := LimitHistory(s.DB(), f); !errors.As(err, &pe) {
		t.Errorf("client filter: err = %v, want a ParamError", err)
	}
}

func TestPlanValueAggregatesRealizedAndMeasuredQuotaAcrossProviders(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "pv.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := time.Hour
	reset := day1.Add(6 * 24 * h)
	rows := []store.Row{
		// Codex account with weekly headers across two pairs (total +5% for 6000 uncached + 2000 cache read + 500 output).
		{
			RequestID: "c1", TraceID: "c1", RequestedAt: day1.Add(1 * h).UnixMilli(),
			Provider: "codex", Model: "gpt-5", AuthIndex: "acc-codex", Generate: true,
			UncachedInput: 1000, LimitWeeklyPct: sql.NullFloat64{Float64: 10, Valid: true},
			LimitWeeklyReset: sql.NullInt64{Int64: reset.UnixMilli(), Valid: true},
		},
		{
			RequestID: "c2", TraceID: "c2", RequestedAt: day1.Add(2 * h).UnixMilli(),
			Provider: "codex", Model: "gpt-5", AuthIndex: "acc-codex", Generate: true,
			UncachedInput: 4000, CacheRead: 2000, OutputNonReasoning: 500,
			LimitWeeklyPct:   sql.NullFloat64{Float64: 14, Valid: true},
			LimitWeeklyReset: sql.NullInt64{Int64: reset.UnixMilli(), Valid: true},
		},
		{
			RequestID: "c3", TraceID: "c3", RequestedAt: day2.Add(1 * h).UnixMilli(),
			Provider: "codex", Model: "gpt-5", AuthIndex: "acc-codex", Generate: true,
			UncachedInput:    2000,
			LimitWeeklyPct:   sql.NullFloat64{Float64: 15, Valid: true},
			LimitWeeklyReset: sql.NullInt64{Int64: reset.UnixMilli(), Valid: true},
		},
		// Antigravity account with no weekly headers: realized totals only.
		{
			RequestID: "a1", TraceID: "a1", RequestedAt: day1.Add(3 * h).UnixMilli(),
			Provider: "antigravity", Model: "claude-sonnet-4-6", AuthIndex: "acc-ag", Generate: true,
			UncachedInput: 3000, CacheRead: 1000, OutputNonReasoning: 200,
		},
	}
	if err := s.InsertBatch(rows); err != nil {
		t.Fatal(err)
	}
	got, err := PlanValue(s.DB(), rangeFilter(), testPrices(t))
	if err != nil {
		t.Fatal(err)
	}
	if got.DataSince != day1.Add(1*h).UnixMilli() {
		t.Errorf("data_since = %d, want %d", got.DataSince, day1.Add(1*h).UnixMilli())
	}
	byID := map[string]AccountValue{}
	for _, a := range got.Accounts {
		byID[a.AuthIndex] = a
	}
	cx := byID["acc-codex"]
	if cx.Requests != 3 || cx.UncachedInput != 7000 || cx.CacheRead != 2000 || cx.Output != 500 {
		t.Errorf("codex realized = %+v", cx)
	}
	if !near(cx.WeeklyPct, 5.0) || cx.MeasuredTokens.UncachedInput != 6000 || cx.MeasuredTokens.CacheRead != 2000 || cx.MeasuredTokens.Output != 500 || cx.LowSample {
		t.Errorf("codex measured = %+v", cx)
	}
	if cx.TopModel != "gpt-5" || !near(cx.TopModelShare, 1.0) {
		t.Errorf("codex top model = %q (%v)", cx.TopModel, cx.TopModelShare)
	}
	ag := byID["acc-ag"]
	if ag.Provider != "antigravity" || ag.Group != "claude-and-gpt" || ag.Requests != 1 || ag.WeeklyPct != 0 || ag.MeasuredCostUSD != 0 || ag.TopModel != "claude-sonnet-4-6" {
		t.Errorf("antigravity = %+v", ag)
	}
	f := rangeFilter()
	f.Model = "gpt-5"
	var pe *ParamError
	if _, err := PlanValue(s.DB(), f, testPrices(t)); !errors.As(err, &pe) {
		t.Errorf("model filter: err = %v, want ParamError", err)
	}
}

func TestPlanValueMeasuresQuotaAcrossEarlyReset(t *testing.T) {
	oldReset := day2
	newReset := day1.Add(7 * 24 * time.Hour)
	s := insertSamples(t, []sample{
		{id: "start", account: "early", model: "gpt-5", at: day1, weekly: 0, reset: oldReset, uncached: 10},
		{id: "before-reset", account: "early", model: "gpt-5", at: day1.Add(time.Hour), weekly: 80, reset: oldReset, uncached: 800},
		{id: "new-cycle", account: "early", model: "gpt-5", at: day1.Add(2 * time.Hour), weekly: 5, reset: newReset, uncached: 20},
		{id: "after-reset", account: "early", model: "gpt-5", at: day1.Add(3 * time.Hour), weekly: 45, reset: newReset, uncached: 400},
		{id: "old", account: "scheduled", model: "gpt-5", at: day1, weekly: 80, reset: oldReset},
		{id: "renewal", account: "scheduled", model: "gpt-5", at: oldReset, weekly: 1, reset: newReset},
		{id: "flat", account: "scheduled", model: "gpt-5", at: oldReset.Add(time.Hour), weekly: 1, reset: newReset},
		{id: "headerless", account: "headerless", model: "gpt-5", at: day1, weekly: -1},
	})

	check := func(account string, from, to time.Time, resets int, measured bool, points float64, priced, realized int64) {
		t.Helper()
		got, err := PlanValue(s.DB(), Filter{From: from, To: to, Account: account}, testPrices(t))
		if err != nil || len(got.Accounts) != 1 {
			t.Fatalf("%s: %+v, %v", account, got, err)
		}
		a := got.Accounts[0]
		if a.EarlyResets != resets || a.QuotaMeasured != measured || a.WeeklyPct != points ||
			a.MeasuredTokens.UncachedInput != priced || a.UncachedInput != realized ||
			!near(a.MeasuredCostUSD, float64(priced)*gpt5Input) || !near(a.CostUSD, float64(realized)*gpt5Input) {
			t.Errorf("%s from %s to %s: %+v", account, from, to, a)
		}
	}
	check("early", day1, day4, 1, true, 120, 1200, 1230)
	check("early", day1, day1.Add(2*time.Hour), 0, true, 80, 800, 810)
	check("early", day1.Add(2*time.Hour), day4, 1, true, 40, 400, 420)
	check("early", day1.Add(3*time.Hour), day4, 0, false, 0, 0, 400)
	check("scheduled", day1, day4, 0, true, 0, 0, 0)
	check("headerless", day1, day4, 0, false, 0, 0, 0)
}

func TestPlanValuePricesUsageBetweenAnEarlyResetAndPreviousDeadline(t *testing.T) {
	newReset := day2.Add(7 * 24 * time.Hour)
	s := insertSamples(t, []sample{
		{id: "before", account: "reset", model: "gpt-5", at: day1, weekly: 98, reset: day2, uncached: 10},
		{id: "new", account: "reset", model: "gpt-5", at: day1.Add(time.Hour), weekly: 1, reset: newReset, uncached: 20},
		{id: "no-headers", account: "reset", model: "gpt-5", at: day1.Add(2 * time.Hour), weekly: -1, uncached: 30},
		{id: "deadline", account: "reset", model: "gpt-5", at: day2, weekly: 2, reset: newReset, uncached: 40},
		{id: "other", account: "other", model: "gpt-5", at: day1.Add(2 * time.Hour), weekly: -1, uncached: 50},
	})
	for _, tc := range []struct {
		from   time.Time
		want   int64
		resets int
	}{
		{day1, 50, 1}, {day1.Add(2 * time.Hour), 30, 0}, {day2, 0, 0},
	} {
		got, err := PlanValue(s.DB(), Filter{From: tc.from, To: day4}, testPrices(t))
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range got.Accounts {
			want, resets := tc.want, tc.resets
			if a.AuthIndex == "other" {
				want, resets = 0, 0
			}
			if !near(a.AfterEarlyResetUSD, float64(want)*gpt5Input) || a.EarlyResets != resets {
				t.Fatalf("from %s: %+v", tc.from, a)
			}
		}
	}
}

func TestAntigravityQuotaSnapshotsFeedHistoryBurnLeftoverPlanValueAnd429(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "ag-query.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	reset1 := day2.Add(6 * time.Hour)
	reset2 := reset1.Add(7 * 24 * time.Hour)

	snaps := []store.QuotaSnapshot{
		// Gemini group: 10% -> 30% on day1, then resets at reset1 (leaving 70% unused).
		{RecordedAt: day1.Add(1 * time.Hour).UnixMilli(), Provider: "antigravity", AuthIndex: "ag1", GroupID: "gemini", GroupLabel: "Gemini", WindowKind: "weekly", UsedPct: 10, ResetAt: sql.NullInt64{Int64: reset1.UnixMilli(), Valid: true}},
		{RecordedAt: day1.Add(3 * time.Hour).UnixMilli(), Provider: "antigravity", AuthIndex: "ag1", GroupID: "gemini", GroupLabel: "Gemini", WindowKind: "weekly", UsedPct: 30, ResetAt: sql.NullInt64{Int64: reset1.UnixMilli(), Valid: true}},
		{RecordedAt: day1.Add(3 * time.Hour).UnixMilli(), Provider: "antigravity", AuthIndex: "ag1", GroupID: "gemini", GroupLabel: "Gemini", WindowKind: "session", UsedPct: 100, ResetAt: sql.NullInt64{Int64: day1.Add(8 * time.Hour).UnixMilli(), Valid: true}},
		{RecordedAt: day2.Add(7 * time.Hour).UnixMilli(), Provider: "antigravity", AuthIndex: "ag1", GroupID: "gemini", GroupLabel: "Gemini", WindowKind: "weekly", UsedPct: 5, ResetAt: sql.NullInt64{Int64: reset2.UnixMilli(), Valid: true}},
		// Claude and GPT group: 20% -> 25% on day1.
		{RecordedAt: day1.Add(1 * time.Hour).UnixMilli(), Provider: "antigravity", AuthIndex: "ag1", GroupID: "claude-and-gpt", GroupLabel: "Claude and GPT", WindowKind: "weekly", UsedPct: 20, ResetAt: sql.NullInt64{Int64: reset2.UnixMilli(), Valid: true}},
		{RecordedAt: day1.Add(3 * time.Hour).UnixMilli(), Provider: "antigravity", AuthIndex: "ag1", GroupID: "claude-and-gpt", GroupLabel: "Claude and GPT", WindowKind: "weekly", UsedPct: 25, ResetAt: sql.NullInt64{Int64: reset2.UnixMilli(), Valid: true}},
	}
	if err := s.InsertQuotaSnapshots(snaps); err != nil {
		t.Fatal(err)
	}

	rows := []store.Row{
		{
			RequestID: "g1", TraceID: "g1", RequestedAt: day1.Add(2 * time.Hour).UnixMilli(),
			Provider: "antigravity", Model: "gemini-3-pro", AuthIndex: "ag1", Generate: true,
			UncachedInput: 1_000_000, // $2.00 in testPrices? Use gpt-5 / claude-sonnet-4-6 or price override.
		},
		{
			RequestID: "g-429", TraceID: "g-429", RequestedAt: day1.Add(3*time.Hour + 30*time.Second).UnixMilli(),
			Provider: "antigravity", Model: "gemini-3-pro", AuthIndex: "ag1", Generate: true, Failed: true, StatusCode: 429,
		},
		{
			RequestID: "c1", TraceID: "c1", RequestedAt: day1.Add(2 * time.Hour).UnixMilli(),
			Provider: "antigravity", Model: "claude-sonnet-4-6", AuthIndex: "ag1", Generate: true,
			UncachedInput: 1_000_000,
		},
	}
	if err := s.InsertBatch(rows); err != nil {
		t.Fatal(err)
	}

	tbl := pricing.WithOverrides(testPrices(t), map[string]pricing.PriceOverride{
		"gemini-3-pro":      {Input: 2.0, Output: 4.0},
		"claude-sonnet-4-6": {Input: 3.0, Output: 15.0},
	})

	hist, err := LimitHistory(s.DB(), rangeFilter())
	if err != nil {
		t.Fatal(err)
	}
	if len(hist.Accounts) != 2 {
		t.Fatalf("LimitHistory accounts = %d, want 2: %+v", len(hist.Accounts), hist.Accounts)
	}

	burn, err := LimitBurn(s.DB(), rangeFilter(), tbl)
	if err != nil {
		t.Fatal(err)
	}
	if len(burn.Points) != 2 {
		t.Fatalf("LimitBurn points = %d, want 2: %+v", len(burn.Points), burn.Points)
	}
	burnByGroup := map[string]BurnPoint{}
	for _, bp := range burn.Points {
		burnByGroup[bp.Group] = bp
	}
	if bp := burnByGroup["gemini"]; !near(bp.WeeklyPct, 20) || !near(bp.CostUSD, 2.0) || !near(bp.USDPerPct, 0.10) {
		t.Errorf("gemini burn = %+v", bp)
	}
	if bp := burnByGroup["claude-and-gpt"]; !near(bp.WeeklyPct, 5) || !near(bp.CostUSD, 3.0) || !near(bp.USDPerPct, 0.60) {
		t.Errorf("claude-and-gpt burn = %+v", bp)
	}

	left, err := LimitLeftover(s.DB(), rangeFilter(), tbl, day4)
	if err != nil {
		t.Fatal(err)
	}
	if len(left.Cycles) != 1 || left.Cycles[0].Group != "gemini" || !near(left.Cycles[0].LeftPct, 70) || !near(left.Cycles[0].LeftUSD, 7.0) {
		t.Fatalf("LimitLeftover = %+v", left.Cycles)
	}

	pv, err := PlanValue(s.DB(), rangeFilter(), tbl)
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.Accounts) != 2 {
		t.Fatalf("PlanValue accounts = %d, want 2: %+v", len(pv.Accounts), pv.Accounts)
	}
	if len(pv.RateLimits) != 1 || pv.RateLimits[0].Group != "gemini" || pv.RateLimits[0].Window != "session" {
		t.Fatalf("PlanValue rate_limits = %+v, want 1 gemini session 429", pv.RateLimits)
	}
}

func TestAntigravityGroupsAllGeminiTogetherAndSeparatesClaudeGPT(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "ag-groups.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	reset := day3.Add(6 * time.Hour)
	// Include a 0% baseline snapshot (reset_at NULL), an Argon bucket snapshot, and a post-429 snapshot.
	snaps := []store.QuotaSnapshot{
		{RecordedAt: day1.Add(1 * time.Hour).UnixMilli(), Provider: "antigravity", AuthIndex: "ag1", GroupID: "gemini", GroupLabel: "Gemini", WindowKind: "weekly", UsedPct: 0},
		{RecordedAt: day1.Add(3 * time.Hour).UnixMilli(), Provider: "antigravity", AuthIndex: "ag1", GroupID: "gemini", GroupLabel: "Gemini", WindowKind: "weekly", UsedPct: 10, ResetAt: sql.NullInt64{Int64: reset.UnixMilli(), Valid: true}},
		// Real upstream Argon bucket stays visible in LimitHistory, not as a 3rd group in Plans/Burn.
		{RecordedAt: day1.Add(1 * time.Hour).UnixMilli(), Provider: "antigravity", AuthIndex: "ag1", GroupID: "gemini-4-argon", GroupLabel: "Gemini 4 Argon", WindowKind: "weekly", UsedPct: 0},
		{RecordedAt: day1.Add(3 * time.Hour).UnixMilli(), Provider: "antigravity", AuthIndex: "ag1", GroupID: "gemini-4-argon", GroupLabel: "Gemini 4 Argon", WindowKind: "weekly", UsedPct: 5, ResetAt: sql.NullInt64{Int64: reset.UnixMilli(), Valid: true}},
		// Pre-429 expired snapshot vs post-429 fresh snapshot.
		{RecordedAt: day1.Add(3*time.Hour + 1*time.Minute).UnixMilli(), Provider: "antigravity", AuthIndex: "ag1", GroupID: "gemini", GroupLabel: "Gemini", WindowKind: "session", UsedPct: 20, ResetAt: sql.NullInt64{Int64: day1.Add(3*time.Hour + 2*time.Minute).UnixMilli(), Valid: true}},
		{RecordedAt: day1.Add(3*time.Hour + 4*time.Minute).UnixMilli(), Provider: "antigravity", AuthIndex: "ag1", GroupID: "gemini", GroupLabel: "Gemini", WindowKind: "session", UsedPct: 100, ResetAt: sql.NullInt64{Int64: day1.Add(8 * time.Hour).UnixMilli(), Valid: true}},
	}
	if err := s.InsertQuotaSnapshots(snaps); err != nil {
		t.Fatal(err)
	}
	rows := []store.Row{
		{RequestID: "ar1", TraceID: "ar1", RequestedAt: day1.Add(2 * time.Hour).UnixMilli(), Provider: "antigravity", Model: "gemini-4-argon", AuthIndex: "ag1", Generate: true, UncachedInput: 1_000_000},
		{RequestID: "ar-429", TraceID: "ar-429", RequestedAt: day1.Add(3*time.Hour + 3*time.Minute).UnixMilli(), Provider: "antigravity", Model: "gemini-4-argon", AuthIndex: "ag1", Generate: true, Failed: true, StatusCode: 429},
	}
	if err := s.InsertBatch(rows); err != nil {
		t.Fatal(err)
	}
	tbl := pricing.WithOverrides(testPrices(t), map[string]pricing.PriceOverride{
		"gemini-4-argon": {Input: 2.0, Output: 10.0},
	})

	// LimitHistory keeps both Gemini and Gemini 4 Argon buckets for Limits trends.
	hist, err := LimitHistory(s.DB(), rangeFilter())
	if err != nil {
		t.Fatal(err)
	}
	if len(hist.Accounts) != 2 {
		t.Fatalf("LimitHistory accounts = %d, want 2 (gemini + gemini-4-argon): %+v", len(hist.Accounts), hist.Accounts)
	}

	// Burn and PlanValue unify all Gemini models (including gemini-4-argon) under "gemini"
	// and capture the 0% (NULL reset_at) -> 10% opening burn.
	burn, err := LimitBurn(s.DB(), rangeFilter(), tbl)
	if err != nil {
		t.Fatal(err)
	}
	if len(burn.Points) != 1 || burn.Points[0].Group != "gemini" || !near(burn.Points[0].WeeklyPct, 10) || !near(burn.Points[0].CostUSD, 2.0) {
		t.Fatalf("LimitBurn = %+v, want 1 gemini point (+10%%, $2)", burn.Points)
	}

	pv, err := PlanValue(s.DB(), rangeFilter(), tbl)
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.Accounts) != 1 || pv.Accounts[0].Group != "gemini" || !near(pv.Accounts[0].WeeklyPct, 10) || !near(pv.Accounts[0].MeasuredCostUSD, 2.0) {
		t.Fatalf("PlanValue accounts = %+v, want 1 unified gemini row", pv.Accounts)
	}
	// The 429 ignores the expired pre-429 snapshot and classifies via the post-429 snapshot (session 100%).
	if len(pv.RateLimits) != 1 || pv.RateLimits[0].Group != "gemini" || pv.RateLimits[0].Window != "session" {
		t.Fatalf("PlanValue rate_limits = %+v, want gemini session 429", pv.RateLimits)
	}
}

func TestAntigravityPolledJitterDoesNotDoubleCountWeeklyBurn(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "ag-jitter.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	reset := day3.Add(6 * time.Hour)
	// Mimics live Google Antigravity downward jitter (2.85 -> 2.59 -> 2.77 -> 3.09).
	// Net movement from 1.63 to 3.09 is 1.46%, not 1.22 + 0.18 + 0.32 = 1.72%.
	snaps := []store.QuotaSnapshot{
		{RecordedAt: day1.Add(1 * time.Hour).UnixMilli(), Provider: "antigravity", AuthIndex: "ag1", GroupID: "gemini", GroupLabel: "Gemini", WindowKind: "weekly", UsedPct: 1.63, ResetAt: sql.NullInt64{Int64: reset.UnixMilli(), Valid: true}},
		{RecordedAt: day1.Add(2 * time.Hour).UnixMilli(), Provider: "antigravity", AuthIndex: "ag1", GroupID: "gemini", GroupLabel: "Gemini", WindowKind: "weekly", UsedPct: 2.85, ResetAt: sql.NullInt64{Int64: reset.UnixMilli(), Valid: true}},
		{RecordedAt: day1.Add(3 * time.Hour).UnixMilli(), Provider: "antigravity", AuthIndex: "ag1", GroupID: "gemini", GroupLabel: "Gemini", WindowKind: "weekly", UsedPct: 2.59, ResetAt: sql.NullInt64{Int64: reset.UnixMilli(), Valid: true}},
		{RecordedAt: day1.Add(4 * time.Hour).UnixMilli(), Provider: "antigravity", AuthIndex: "ag1", GroupID: "gemini", GroupLabel: "Gemini", WindowKind: "weekly", UsedPct: 2.77, ResetAt: sql.NullInt64{Int64: reset.UnixMilli(), Valid: true}},
		{RecordedAt: day1.Add(5 * time.Hour).UnixMilli(), Provider: "antigravity", AuthIndex: "ag1", GroupID: "gemini", GroupLabel: "Gemini", WindowKind: "weekly", UsedPct: 3.09, ResetAt: sql.NullInt64{Int64: reset.UnixMilli(), Valid: true}},
	}
	if err := s.InsertQuotaSnapshots(snaps); err != nil {
		t.Fatal(err)
	}
	rows := []store.Row{
		{RequestID: "r1", TraceID: "r1", RequestedAt: day1.Add(90 * time.Minute).UnixMilli(), Provider: "antigravity", Model: "gemini-4-neon", AuthIndex: "ag1", Generate: true, UncachedInput: 1_000_000},
		{RequestID: "r2", TraceID: "r2", RequestedAt: day1.Add(150 * time.Minute).UnixMilli(), Provider: "antigravity", Model: "gemini-4-neon", AuthIndex: "ag1", Generate: true, UncachedInput: 1_000_000},
		{RequestID: "r3", TraceID: "r3", RequestedAt: day1.Add(210 * time.Minute).UnixMilli(), Provider: "antigravity", Model: "gemini-4-neon", AuthIndex: "ag1", Generate: true, UncachedInput: 1_000_000},
		{RequestID: "r4", TraceID: "r4", RequestedAt: day1.Add(270 * time.Minute).UnixMilli(), Provider: "antigravity", Model: "gemini-4-neon", AuthIndex: "ag1", Generate: true, UncachedInput: 1_000_000},
	}
	if err := s.InsertBatch(rows); err != nil {
		t.Fatal(err)
	}
	tbl := pricing.WithOverrides(testPrices(t), map[string]pricing.PriceOverride{
		"gemini-4-neon": {Input: 3.0, Output: 15.0},
	})

	burn, err := LimitBurn(s.DB(), rangeFilter(), tbl)
	if err != nil {
		t.Fatal(err)
	}
	if len(burn.Points) != 1 || !near(burn.Points[0].WeeklyPct, 1.46) || !near(burn.Points[0].CostUSD, 12.0) {
		t.Fatalf("LimitBurn = %+v, want 1.46%% and $12.00", burn.Points)
	}
	pv, err := PlanValue(s.DB(), rangeFilter(), tbl)
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.Accounts) != 1 || !near(pv.Accounts[0].WeeklyPct, 1.46) || !near(pv.Accounts[0].MeasuredCostUSD, 12.0) {
		t.Fatalf("PlanValue = %+v, want 1.46%% and $12.00", pv.Accounts)
	}
}
