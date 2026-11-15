package query

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

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
		{T: day1.UnixMilli(), AuthIndex: "acc1", Provider: "codex", WeeklyPct: 2, CostUSD: 4000 * gpt5Input},
		{T: day2.UnixMilli(), AuthIndex: "acc1", Provider: "codex", WeeklyPct: 3.5, CostUSD: 8400 * gpt5Input},
	}
	if len(got.Points) != len(want) {
		t.Fatalf("points = %+v, want %+v", got.Points, want)
	}
	for i, w := range want {
		g := got.Points[i]
		if g.T != w.T || g.AuthIndex != w.AuthIndex || g.Provider != w.Provider || !near(g.WeeklyPct, w.WeeklyPct) || !near(g.CostUSD, w.CostUSD) ||
			!near(g.USDPerPct, w.CostUSD/w.WeeklyPct) {
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
