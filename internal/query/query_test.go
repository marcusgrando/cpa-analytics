package query

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcus-grando-azion/cpa-analytics/internal/pricing"
	"github.com/marcus-grando-azion/cpa-analytics/internal/store"
)

var (
	day0 = time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	day1 = day0.AddDate(0, 0, 1)
	day2 = day0.AddDate(0, 0, 2)
	day3 = day0.AddDate(0, 0, 3)
	day4 = day0.AddDate(0, 0, 4)
)

const (
	gpt5Input     = 1.25e-6
	gpt5Output    = 1e-5
	gpt5CacheRead = 1.25e-7
)

func testPrices(t *testing.T) *pricing.Table {
	t.Helper()
	table, err := pricing.ParseLiteLLM([]byte(`{"gpt-5": {"litellm_provider": "openai",
		"input_cost_per_token": 1.25e-6, "output_cost_per_token": 1e-5, "cache_read_input_token_cost": 1.25e-7}}`))
	if err != nil {
		t.Fatal(err)
	}
	return table
}

type seed struct {
	id, trace, provider, model, response, session, version string
	at                                                     time.Time
	generate                                               bool
	uncached, cacheRead, output, reasoning, unclassified   int64
}

func (s seed) row() store.Row {
	return store.Row{
		RequestID: s.id, TraceID: s.trace, RequestedAt: s.at.UnixMilli(), Provider: s.provider,
		Model: s.model, ResponseModel: s.response, SessionID: s.session, Generate: s.generate,
		ClientName: "codex", ClientVersion: s.version, APIKeyHash: "abc123",
		UncachedInput: s.uncached, CacheRead: s.cacheRead, OutputNonReasoning: s.output,
		OutputReasoning: s.reasoning, Unclassified: s.unclassified,
	}
}

// seedDB writes three days of traffic. Codex moves from 0.129.0 to 0.130.0 on
// day 2 and the upstream starts answering with gpt-5.1 on day 3. One row
// before the range shows 0.129.0 and gpt-5 were already known.
func seedDB(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	h := time.Hour
	seeds := []seed{
		{id: "r0", trace: "t0", provider: "codex", model: "gpt-5", response: "gpt-5", version: "0.129.0", at: day0.Add(10 * h), generate: true, uncached: 777},
		{id: "r1", trace: "t1", provider: "codex", model: "gpt-5", response: "gpt-5", session: "s1", version: "0.129.0", at: day1.Add(9 * h), generate: true, uncached: 1000, cacheRead: 3000, output: 200, reasoning: 100},
		{id: "r2a", trace: "t2", provider: "codex", model: "gpt-5", response: "gpt-5", session: "s1", version: "0.129.0", at: day1.Add(10 * h), generate: true, uncached: 500, cacheRead: 1000, output: 100},
		{id: "r2b", trace: "t2", provider: "codex", model: "gpt-5", response: "gpt-5", session: "s1", version: "0.129.0", at: day1.Add(10 * h), generate: true, uncached: 500},
		{id: "r3", trace: "t3", provider: "codex", model: "gpt-5", response: "gpt-5", version: "0.129.0", at: day1.Add(11 * h), generate: false, uncached: 99999},
		{id: "r4", trace: "t4", provider: "codex", model: "gpt-5", response: "gpt-5", session: "s2", version: "0.130.0", at: day2.Add(9 * h), generate: true, uncached: 2000, cacheRead: 6000, output: 400, reasoning: 200},
		{id: "r5", trace: "t5", provider: "codex", model: "gpt-5", response: "gpt-5", session: "s2", version: "0.130.0", at: day2.Add(10 * h), generate: true, uncached: 3000, cacheRead: 1000, output: 300},
		{id: "r6", trace: "t6", provider: "codex", model: "gpt-5", response: "gpt-5", session: "s2", version: "0.130.0", at: day2.Add(11 * h), generate: true, uncached: 4000, output: 500, unclassified: 50},
		{id: "r7", trace: "t7", provider: "openai-compat", model: "gpt-5", response: "gpt-5.1", version: "0.130.0", at: day3.Add(9 * h), generate: true, uncached: 1000, output: 100},
		{id: "r8", trace: "", provider: "openai-compat", model: "gpt-5", response: "gpt-5.1", version: "0.130.0", at: day3.Add(10 * h), generate: true, uncached: 1500, output: 100},
	}
	rows := make([]store.Row, len(seeds))
	for i, sd := range seeds {
		rows[i] = sd.row()
	}
	if err := s.InsertBatch(rows); err != nil {
		t.Fatal(err)
	}
	return s
}

func rangeFilter() Filter { return Filter{From: day1, To: day4} }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestParseFilterDefaultsToLastSevenDays(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	f, err := ParseFilter(url.Values{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !f.To.Equal(now) || !f.From.Equal(now.Add(-7*24*time.Hour)) {
		t.Errorf("range = %v..%v", f.From, f.To)
	}
}

func TestParseFilterReadsParams(t *testing.T) {
	q := url.Values{"from": {"1000"}, "to": {"2000"}, "provider": {"codex"}, "model": {"gpt-5"},
		"client": {"codex"}, "api_key": {"abc123"}, "tz": {"-180"}}
	f, err := ParseFilter(q, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	want := Filter{From: time.UnixMilli(1000), To: time.UnixMilli(2000), Provider: "codex", Model: "gpt-5",
		Client: "codex", APIKeyHash: "abc123", TZOffsetMinutes: -180}
	if f != want {
		t.Errorf("filter = %+v, want %+v", f, want)
	}
}

func TestParseFilterRejectsBadParams(t *testing.T) {
	cases := []struct {
		q     url.Values
		param string
	}{
		{url.Values{"from": {"abc"}}, "from"},
		{url.Values{"to": {"1e3"}}, "to"},
		{url.Values{"from": {"2000"}, "to": {"1000"}}, "from"},
		{url.Values{"from": {"1000"}, "to": {"1000"}}, "from"},
		{url.Values{"from": {"0"}, "to": {"40000000000"}}, "from"},
		{url.Values{"tz": {"900"}}, "tz"},
		{url.Values{"tz": {"x"}}, "tz"},
	}
	for _, c := range cases {
		_, err := ParseFilter(c.q, time.UnixMilli(1_800_000_000_000))
		var pe *ParamError
		if !errors.As(err, &pe) || pe.Param != c.param {
			t.Errorf("%v: err = %v, want ParamError on %q", c.q, err, c.param)
		}
	}
}

func TestSummaryTotalsCountEveryRow(t *testing.T) {
	s := seedDB(t)
	sum, err := Summary(s.DB(), rangeFilter(), testPrices(t))
	if err != nil {
		t.Fatal(err)
	}
	tot := sum.Totals
	if tot.Records != 9 || tot.Requests != 7 || tot.Sessions != 2 {
		t.Errorf("records/requests/sessions = %d/%d/%d, want 9/7/2", tot.Records, tot.Requests, tot.Sessions)
	}
	if tot.UncachedInput != 113499 || tot.CacheRead != 11000 || tot.Output != 1700 || tot.Reasoning != 300 || tot.Unclassified != 50 {
		t.Errorf("tokens = %+v", tot)
	}
	if tot.ProcessedTokens != 126549 {
		t.Errorf("processed = %d, want 126549", tot.ProcessedTokens)
	}
	wantCost := 110999*gpt5Input + 11000*gpt5CacheRead + 1800*gpt5Output
	if !near(tot.CostUSD, wantCost) {
		t.Errorf("cost = %v, want %v", tot.CostUSD, wantCost)
	}
	if !near(tot.CacheHitRate, 11000.0/124499.0) {
		t.Errorf("cache hit rate = %v", tot.CacheHitRate)
	}
	q := tot.CostQuality
	if q.LiteLLMTokens != 123799 || q.OverrideTokens != 0 || q.UnpricedTokens != 2750 {
		t.Errorf("cost quality = %+v, want 123799/0/2750 (unclassified and gpt-5.1 unpriced)", q)
	}
	if len(sum.Providers) != 2 || sum.Providers[0].Provider != "codex" || !near(sum.Providers[0].CostShare, 1) {
		t.Errorf("providers = %+v", sum.Providers)
	}
	if p := sum.Providers[1]; p.Provider != "openai-compat" || p.CostUSD != 0 || p.Tokens != 2700 || p.UnpricedTokens != 2700 {
		t.Errorf("unpriced provider = %+v, want 2700 unpriced tokens and no cost", p)
	}
}

func TestSummaryAppliesFilters(t *testing.T) {
	s := seedDB(t)
	f := rangeFilter()
	f.Provider = "openai-compat"
	sum, err := Summary(s.DB(), f, testPrices(t))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Totals.Records != 2 || sum.Totals.UncachedInput != 2500 {
		t.Errorf("filtered totals = %+v", sum.Totals)
	}
	f = rangeFilter()
	f.Model = "gpt-5.1"
	if sum, _ = Summary(s.DB(), f, testPrices(t)); sum.Totals.Records != 2 {
		t.Errorf("model filter matches the reported model: records = %d, want 2", sum.Totals.Records)
	}
	f = rangeFilter()
	f.APIKeyHash = "nope"
	if sum, _ = Summary(s.DB(), f, testPrices(t)); sum.Totals.Records != 0 {
		t.Errorf("api key filter: records = %d", sum.Totals.Records)
	}
}

func TestTimeseriesByDayAndProvider(t *testing.T) {
	s := seedDB(t)
	ts, err := Timeseries(s.DB(), rangeFilter(), testPrices(t), "day", "provider")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, pt := range ts.Points {
		if pt.T == day2.UnixMilli() && pt.Key == "codex" {
			found = true
			want := 9000*gpt5Input + 7000*gpt5CacheRead + 1400*gpt5Output
			if !near(pt.CostUSD, want) || pt.UncachedInput != 9000 {
				t.Errorf("day2 codex = %+v, want cost %v", pt, want)
			}
		}
	}
	if !found || len(ts.Points) != 3 {
		t.Errorf("points = %+v", ts.Points)
	}
}

func TestTimeseriesBucketsInLocalDays(t *testing.T) {
	s := seedDB(t)
	f := rangeFilter()
	f.TZOffsetMinutes = -600 // UTC-10: 09:00 UTC on day 2 is still day 1 locally.
	ts, err := Timeseries(s.DB(), f, testPrices(t), "day", "provider")
	if err != nil {
		t.Fatal(err)
	}
	localDay1 := day1.Add(10 * time.Hour).UnixMilli()
	var uncached int64
	for _, pt := range ts.Points {
		if pt.T == localDay1 {
			uncached += pt.UncachedInput
		}
	}
	// r1 (09:00 UTC on day 1) falls on the previous local day.
	if uncached != 500+500+99999+2000 {
		t.Errorf("local day 1 uncached = %d, points %+v", uncached, ts.Points)
	}
}

func TestTimeseriesRejectsUnknownBucketAndGroup(t *testing.T) {
	s := seedDB(t)
	for _, c := range []struct{ bucket, group, param string }{{"week", "provider", "bucket"}, {"day", "nope", "group"}} {
		_, err := Timeseries(s.DB(), rangeFilter(), testPrices(t), c.bucket, c.group)
		var pe *ParamError
		if !errors.As(err, &pe) || pe.Param != c.param {
			t.Errorf("%+v: err = %v", c, err)
		}
	}
}

func TestDriftMediansPerInboundRequest(t *testing.T) {
	s := seedDB(t)
	d, err := Drift(s.DB(), rangeFilter(), "client_version")
	if err != nil {
		t.Fatal(err)
	}
	points := map[string]DriftPoint{}
	for _, pt := range d.Points {
		points[time.UnixMilli(pt.T).UTC().Format("01-02")+" "+pt.Key] = pt
	}
	if len(points) != 3 {
		t.Fatalf("points = %+v", d.Points)
	}
	p1 := points["11-10 codex/0.129.0"]
	// t1 and t2 (two split-billing rows of one call, summed); r3 has generate=false and is left out.
	if p1.Requests != 2 || p1.Median.UncachedInput != 1000 || p1.Median.CacheRead != 2000 || p1.P90.CacheRead != 2800 {
		t.Errorf("day1 = %+v", p1)
	}
	if !near(p1.CacheHitRate, 4000.0/6000.0) {
		t.Errorf("day1 cache hit rate = %v", p1.CacheHitRate)
	}
	p2 := points["11-11 codex/0.130.0"]
	if p2.Requests != 3 || p2.Median.UncachedInput != 3000 || p2.P90.UncachedInput != 3800 || p2.Median.Output != 400 || p2.Median.Reasoning != 0 {
		t.Errorf("day2 = %+v", p2)
	}
	p3 := points["11-12 codex/0.130.0"]
	if p3.Requests != 2 || p3.Median.UncachedInput != 1250 {
		t.Errorf("day3 (r8 has no trace and counts by request id) = %+v", p3)
	}
}

func TestDriftMarksFirstSeenValuesInRange(t *testing.T) {
	s := seedDB(t)
	d, err := Drift(s.DB(), rangeFilter(), "response_model")
	if err != nil {
		t.Fatal(err)
	}
	want := []Marker{
		{T: day2.UnixMilli(), Kind: "client_version", Value: "codex/0.130.0"},
		{T: day3.UnixMilli(), Kind: "response_model", Value: "gpt-5.1"},
	}
	if len(d.Markers) != len(want) {
		t.Fatalf("markers = %+v, want %+v", d.Markers, want)
	}
	for i := range want {
		if d.Markers[i] != want[i] {
			t.Errorf("marker %d = %+v, want %+v", i, d.Markers[i], want[i])
		}
	}
	keys := map[string]bool{}
	for _, pt := range d.Points {
		keys[pt.Key] = true
	}
	if !keys["gpt-5"] || !keys["gpt-5.1"] || len(keys) != 2 {
		t.Errorf("response_model keys = %v", keys)
	}
}

func TestDriftRejectsUnknownGroup(t *testing.T) {
	s := seedDB(t)
	_, err := Drift(s.DB(), rangeFilter(), "nope")
	var pe *ParamError
	if !errors.As(err, &pe) || pe.Param != "group" {
		t.Errorf("err = %v", err)
	}
}

func TestBreakdownByModel(t *testing.T) {
	s := seedDB(t)
	b, err := Breakdown(s.DB(), rangeFilter(), testPrices(t), "model")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Rows) != 2 || b.Rows[0].Key != "gpt-5" {
		t.Fatalf("rows = %+v", b.Rows)
	}
	g := b.Rows[0]
	if g.Requests != 5 || g.Records != 7 || g.MedianInput != 4000 || g.MedianOutput != 300 {
		t.Errorf("gpt-5 = %+v", g)
	}
	if !near(g.CostShare, 1) || !near(g.CostUSD, 110999*gpt5Input+11000*gpt5CacheRead+1800*gpt5Output) {
		t.Errorf("gpt-5 cost = %+v", g)
	}
	if u := b.Rows[1]; u.Key != "gpt-5.1" || u.CostUSD != 0 || u.UnpricedTokens != 2700 {
		t.Errorf("gpt-5.1 = %+v", u)
	}
}

func TestBreakdownByClientAndDay(t *testing.T) {
	s := seedDB(t)
	b, err := Breakdown(s.DB(), rangeFilter(), testPrices(t), "client")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Rows) != 2 || b.Rows[0].Key != "codex/0.130.0" && b.Rows[0].Key != "codex/0.129.0" {
		t.Errorf("client rows = %+v", b.Rows)
	}
	b, err = Breakdown(s.DB(), rangeFilter(), testPrices(t), "day")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Rows) != 3 || b.Rows[0].Key != "2026-11-10" {
		t.Errorf("day rows = %+v", b.Rows)
	}
	if _, err := Breakdown(s.DB(), rangeFilter(), testPrices(t), "nope"); err == nil {
		t.Error("unknown by accepted")
	}
}

func TestModelsReportsUnpricedAndOverridden(t *testing.T) {
	s := seedDB(t)
	prices := pricing.WithOverrides(testPrices(t), map[string]pricing.PriceOverride{"gpt-5": {Input: 1, Output: 2}})
	m, err := Models(s.DB(), rangeFilter(), prices)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Unpriced) != 1 || m.Unpriced[0].Model != "gpt-5.1" || m.Unpriced[0].Tokens != 2700 {
		t.Errorf("unpriced = %+v", m.Unpriced)
	}
	if len(m.Overridden) != 1 || m.Overridden[0].Model != "gpt-5" {
		t.Errorf("overridden = %+v", m.Overridden)
	}
}

func TestQueriesFailOnClosedDB(t *testing.T) {
	s := seedDB(t)
	s.Close()
	if _, err := Summary(s.DB(), rangeFilter(), testPrices(t)); err == nil {
		t.Error("summary on closed db succeeded")
	}
	var pe *ParamError
	if _, err := Drift(s.DB(), rangeFilter(), "client_version"); err == nil || errors.As(err, &pe) {
		t.Errorf("drift err = %v, want a storage error", err)
	}
}

// A request whose attempts all failed carries no tokens. Counting it would
// drag the medians toward zero during an outage that is neither a client nor
// a model change.
func TestDriftSkipsFailedRequests(t *testing.T) {
	s := seedDB(t)
	failed := []store.Row{
		{RequestID: "f1", TraceID: "tf1", RequestedAt: day1.Add(12 * time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-5",
			Generate: true, Failed: true, ClientName: "codex", ClientVersion: "0.129.0"},
		{RequestID: "f2", TraceID: "tf2", RequestedAt: day1.Add(13 * time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-5",
			Generate: true, Failed: true, ClientName: "codex", ClientVersion: "0.129.0"},
	}
	if err := s.InsertBatch(failed); err != nil {
		t.Fatal(err)
	}
	d, err := Drift(s.DB(), rangeFilter(), "client_version")
	if err != nil {
		t.Fatal(err)
	}
	for _, pt := range d.Points {
		if time.UnixMilli(pt.T).UTC().Format("01-02") == "11-10" && (pt.Requests != 2 || pt.Median.UncachedInput != 1000) {
			t.Errorf("day1 with failed requests = %+v, want the two successful requests only", pt)
		}
	}
	b, err := Breakdown(s.DB(), rangeFilter(), testPrices(t), "client")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range b.Rows {
		if r.Key == "codex/0.129.0" && (r.Records != 6 || r.Requests != 2) {
			t.Errorf("breakdown row = %+v, want 6 records and 2 successful requests", r)
		}
	}
}

// The API key filter takes a hash, so the breakdown must list the hashes.
func TestBreakdownByAPIKey(t *testing.T) {
	s := seedDB(t)
	extra := store.Row{RequestID: "k1", TraceID: "tk1", RequestedAt: day2.Add(time.Hour).UnixMilli(), Provider: "codex",
		Model: "gpt-5", Generate: true, APIKeyHash: "def456def456", UncachedInput: 10}
	if err := s.InsertBatch([]store.Row{extra}); err != nil {
		t.Fatal(err)
	}
	b, err := Breakdown(s.DB(), rangeFilter(), testPrices(t), "api_key")
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]int64{}
	for _, r := range b.Rows {
		keys[r.Key] = r.Records
	}
	if len(keys) != 2 || keys["abc123"] != 9 || keys["def456def456"] != 1 {
		t.Errorf("api key rows = %+v", b.Rows)
	}
}

// CPA gives every turn of a WebSocket connection the trace of the upgrade
// request. Each turn is its own upstream call with its own start time, so it
// counts as its own request.
func TestDriftCountsEachWebSocketTurn(t *testing.T) {
	s := seedDB(t)
	var turns []store.Row
	for i, input := range []int64{100, 300, 500} {
		turns = append(turns, store.Row{RequestID: fmt.Sprintf("ws%d", i), TraceID: "ws-conn", RequestedAt: day3.Add(time.Duration(12+i) * time.Hour).UnixMilli(),
			Provider: "codex", Model: "gpt-5", Generate: true, ClientName: "codex", ClientVersion: "0.131.0", UncachedInput: input})
	}
	if err := s.InsertBatch(turns); err != nil {
		t.Fatal(err)
	}
	d, err := Drift(s.DB(), rangeFilter(), "client_version")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, pt := range d.Points {
		if pt.Key == "codex/0.131.0" {
			found = true
			if pt.Requests != 3 || pt.Median.UncachedInput != 300 {
				t.Errorf("websocket turns = %+v, want 3 requests with median 300", pt)
			}
		}
	}
	if !found {
		t.Fatalf("no point for codex/0.131.0: %+v", d.Points)
	}
	sum, err := Summary(s.DB(), rangeFilter(), testPrices(t))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Totals.Requests != 10 {
		t.Errorf("summary requests = %d, want 7 seeded calls plus 3 turns", sum.Totals.Requests)
	}
}
