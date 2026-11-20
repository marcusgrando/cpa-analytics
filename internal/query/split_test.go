package query

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/marcusgrando/cpa-analytics/internal/pricing"
	"github.com/marcusgrando/cpa-analytics/internal/store"
)

// tieredPrices prices gpt-5 at OpenAI's standard, priority and flex rates.
// Cache writes have no price and fall back to input.
func tieredPrices(t *testing.T) *pricing.Table {
	t.Helper()
	table, err := pricing.ParseLiteLLM([]byte(`{"gpt-5": {"litellm_provider": "openai",
		"input_cost_per_token": 1.25e-6, "output_cost_per_token": 1e-5, "cache_read_input_token_cost": 1.25e-7,
		"input_cost_per_token_priority": 2.5e-6, "output_cost_per_token_priority": 2e-5, "cache_read_input_token_cost_priority": 2.5e-7,
		"input_cost_per_token_flex": 6.25e-7, "output_cost_per_token_flex": 5e-6, "cache_read_input_token_cost_flex": 6.25e-8}}`))
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func insertRows(t *testing.T, rows ...store.Row) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "split.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.InsertBatch(rows); err != nil {
		t.Fatal(err)
	}
	return s
}

// One priority call (Codex fast mode), one default call, one flex call and
// one call to a model with no price.
func tieredDB(t *testing.T) *store.Store {
	at := func(h int) int64 { return day1.Add(time.Duration(h) * time.Hour).UnixMilli() }
	return insertRows(t,
		store.Row{RequestID: "p", TraceID: "tp", RequestedAt: at(1), Provider: "codex", Model: "gpt-5", Generate: true,
			ResponseServiceTier: "priority", UncachedInput: 1000, CacheRead: 2000, OutputNonReasoning: 100, OutputReasoning: 50},
		store.Row{RequestID: "d", TraceID: "td", RequestedAt: at(2), Provider: "codex", Model: "gpt-5", Generate: true,
			EffectiveServiceTier: "default", UncachedInput: 1000, CacheWrite: 500, OutputNonReasoning: 200},
		store.Row{RequestID: "f", TraceID: "tf", RequestedAt: at(3), Provider: "codex", Model: "gpt-5", Generate: true,
			ResponseServiceTier: "flex", UncachedInput: 1000},
		store.Row{RequestID: "u", TraceID: "tu", RequestedAt: at(4), Provider: "codex", Model: "mystery", Generate: true,
			UncachedInput: 999},
	)
}

func TestSummarySplitsCostByType(t *testing.T) {
	sum, err := Summary(tieredDB(t).DB(), rangeFilter(), tieredPrices(t))
	if err != nil {
		t.Fatal(err)
	}
	got := sum.Totals.CostByType
	// priority 1000*2.5e-6, default 1000*1.25e-6, flex 1000*6.25e-7
	if !near(got.Input, 0.0025+0.00125+0.000625) || !near(got.CacheRead, 2000*2.5e-7) ||
		!near(got.CacheWrite, 500*1.25e-6) || !near(got.Output, 150*2e-5+200*1e-5) {
		t.Errorf("cost by type = %+v", got)
	}
	if !near(got.Total(), sum.Totals.CostUSD) {
		t.Errorf("cost by type sums to %v, total cost is %v", got.Total(), sum.Totals.CostUSD)
	}
}

// Cache savings are what the cache reads would have cost as uncached input,
// at the rates the call was billed at.
func TestSummaryReportsCacheSavings(t *testing.T) {
	sum, err := Summary(tieredDB(t).DB(), rangeFilter(), tieredPrices(t))
	if err != nil {
		t.Fatal(err)
	}
	if want := 2000 * (2.5e-6 - 2.5e-7); !near(sum.Totals.CacheSavingsUSD, want) {
		t.Errorf("cache savings = %v, want %v", sum.Totals.CacheSavingsUSD, want)
	}
}

// Each tier reports its cost and what the same tokens cost at standard
// rates, so the page can show the priority premium and the flex discount.
func TestSummarySplitsCostByTier(t *testing.T) {
	sum, err := Summary(tieredDB(t).DB(), rangeFilter(), tieredPrices(t))
	if err != nil {
		t.Fatal(err)
	}
	tiers := map[string]TierCost{}
	for _, tc := range sum.Totals.CostByTier {
		tiers[tc.Tier] = tc
	}
	want := map[string]TierCost{
		"priority": {Tier: "priority", CostUSD: 0.0025 + 0.0005 + 0.003, StandardCostUSD: 0.00125 + 0.00025 + 0.0015},
		"default":  {Tier: "default", CostUSD: 0.00125 + 0.000625 + 0.002, StandardCostUSD: 0.00125 + 0.000625 + 0.002},
		"flex":     {Tier: "flex", CostUSD: 0.000625, StandardCostUSD: 0.00125},
		"":         {Tier: "", CostUSD: 0, StandardCostUSD: 0},
	}
	if len(tiers) != len(want) {
		t.Fatalf("tiers = %+v", sum.Totals.CostByTier)
	}
	for name, w := range want {
		g := tiers[name]
		if !near(g.CostUSD, w.CostUSD) || !near(g.StandardCostUSD, w.StandardCostUSD) {
			t.Errorf("tier %q = %+v, want %+v", name, g, w)
		}
	}
	if sum.Totals.CostByTier[0].Tier != "priority" {
		t.Errorf("tiers are not sorted by cost: %+v", sum.Totals.CostByTier)
	}
}

// A client that raises its default reasoning effort spends more output
// without any model change, so effort is a drift group and a breakdown.
func TestDriftAndBreakdownByReasoningEffort(t *testing.T) {
	at := func(d time.Time, h int) int64 { return d.Add(time.Duration(h) * time.Hour).UnixMilli() }
	row := func(id string, ts int64, effort string, output int64) store.Row {
		return store.Row{RequestID: id, TraceID: "t" + id, RequestedAt: ts, Provider: "codex", Model: "gpt-5", Generate: true,
			ReasoningEffort: effort, ClientName: "codex", ClientVersion: "0.130.0", UncachedInput: 100, OutputReasoning: output}
	}
	s := insertRows(t,
		row("old", at(day0, 1), "medium", 10),
		row("m1", at(day1, 1), "medium", 100),
		row("h1", at(day2, 1), "high", 900),
		row("h2", at(day2, 2), "high", 1100),
		row("n1", at(day2, 3), "", 50),
	)
	d, err := Drift(s.DB(), rangeFilter(), nil, "reasoning_effort")
	if err != nil {
		t.Fatal(err)
	}
	medians := map[string]float64{}
	for _, pt := range d.Points {
		medians[pt.Key] = pt.Median.Reasoning
	}
	if len(medians) != 3 || medians["medium"] != 100 || medians["high"] != 1000 || medians["unset"] != 50 {
		t.Errorf("reasoning medians by effort = %v", medians)
	}
	var effortMarkers []Marker
	for _, m := range d.Markers {
		if m.Kind == "reasoning_effort" {
			effortMarkers = append(effortMarkers, m)
		}
	}
	if len(effortMarkers) != 1 || effortMarkers[0] != (Marker{T: day2.UnixMilli(), Kind: "reasoning_effort", Value: "high"}) {
		t.Errorf("effort markers = %+v, want only high on day 2", effortMarkers)
	}

	b, err := Breakdown(s.DB(), rangeFilter(), tieredPrices(t), "reasoning_effort")
	if err != nil {
		t.Fatal(err)
	}
	requests := map[string]int64{}
	for _, r := range b.Rows {
		requests[r.Key] = r.Requests
	}
	if len(requests) != 3 || requests["high"] != 2 || requests["medium"] != 1 || requests["unset"] != 1 {
		t.Errorf("breakdown by effort = %+v", b.Rows)
	}
}
