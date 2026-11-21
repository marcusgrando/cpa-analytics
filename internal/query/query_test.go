package query

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcusgrando/cpa-analytics/internal/pricing"
	"github.com/marcusgrando/cpa-analytics/internal/store"
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
		"client": {"codex"}, "account": {"acc1"}, "api_key": {"abc123"}, "tz": {"-180"}}
	f, err := ParseFilter(q, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	want := Filter{From: time.UnixMilli(1000), To: time.UnixMilli(2000), Provider: "codex", Model: "gpt-5",
		Client: "codex", Account: "acc1", APIKeyHash: "abc123", TZOffsetMinutes: -180}
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

// The page compares a period with the one before it only when recording had
// started by then, so the summary reports when the data begins, whatever the
// filter.
func TestSummaryReportsWhenDataBegins(t *testing.T) {
	s := seedDB(t)
	f := rangeFilter()
	f.Provider = "openai-compat"
	sum, err := Summary(s.DB(), f, testPrices(t))
	if err != nil {
		t.Fatal(err)
	}
	if want := day0.Add(10 * time.Hour).UnixMilli(); sum.DataSince != want {
		t.Errorf("data since = %d, want %d (the first row of any provider)", sum.DataSince, want)
	}
	empty, err := store.Open(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	if sum, err = Summary(empty.DB(), rangeFilter(), testPrices(t)); err != nil || sum.DataSince != 0 {
		t.Errorf("empty database: data since = %d, err = %v, want 0", sum.DataSince, err)
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

// The totals tiles chart requests and failures per bucket, so each point
// carries them, counted the way summary counts them.
func TestTimeseriesCountsRequestsAndFailures(t *testing.T) {
	s := seedDB(t)
	failed := store.Row{RequestID: "f1", TraceID: "tf1", RequestedAt: day2.Add(12 * time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-5",
		Generate: true, Failed: true, ClientName: "codex", ClientVersion: "0.130.0"}
	if err := s.InsertBatch([]store.Row{failed}); err != nil {
		t.Fatal(err)
	}
	ts, err := Timeseries(s.DB(), rangeFilter(), testPrices(t), "day", "provider")
	if err != nil {
		t.Fatal(err)
	}
	type counts struct{ requests, failed int64 }
	want := map[string]counts{
		fmt.Sprint(day1.UnixMilli(), "codex"):         {2, 0},
		fmt.Sprint(day2.UnixMilli(), "codex"):         {3, 1},
		fmt.Sprint(day3.UnixMilli(), "openai-compat"): {2, 0},
	}
	for _, pt := range ts.Points {
		got := counts{pt.Requests, pt.Failed}
		if w := want[fmt.Sprint(pt.T, pt.Key)]; got != w {
			t.Errorf("%s on %s = %+v, want %+v", pt.Key, time.UnixMilli(pt.T).UTC().Format("01-02"), got, w)
		}
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

func TestTimeseriesByMinute(t *testing.T) {
	s := seedDB(t)
	from := day1.Add(9 * time.Hour)
	ts, err := Timeseries(s.DB(), Filter{From: from, To: from.Add(15 * time.Minute)}, testPrices(t), "minute", "provider")
	if err != nil {
		t.Fatal(err)
	}
	if ts.Bucket != "minute" || len(ts.Points) != 1 || ts.Points[0].T != from.UnixMilli() || ts.Points[0].Requests != 1 {
		t.Errorf("minute timeseries = %+v", ts)
	}
}

func TestDriftMediansPerInboundRequest(t *testing.T) {
	s := seedDB(t)
	d, err := Drift(s.DB(), rangeFilter(), testPrices(t), "client_version", "day")
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
	// Input is the whole prompt: t1 has 4000, t2 has 2000 across its two rows.
	// OutputTotal is output + reasoning: t1 has 300, t2 has 100 -> median 200.
	if p1.Median.Input != 3000 || p1.P90.Input != 3800 || p1.Median.OutputTotal != 200 {
		t.Errorf("day1 input/output_total = %+v", p1.Median)
	}
	wantT1Cost := 1000*gpt5Input + 3000*gpt5CacheRead + 300*gpt5Output
	wantT2Cost := 1000*gpt5Input + 1000*gpt5CacheRead + 100*gpt5Output
	if !near(p1.Median.CostUSD, (wantT1Cost+wantT2Cost)/2) {
		t.Errorf("day1 median cost = %v, want %v", p1.Median.CostUSD, (wantT1Cost+wantT2Cost)/2)
	}
	if !near(p1.CacheHitRate, 4000.0/6000.0) {
		t.Errorf("day1 cache hit rate = %v", p1.CacheHitRate)
	}
	p2 := points["11-11 codex/0.130.0"]
	if p2.Requests != 3 || p2.Median.UncachedInput != 3000 || p2.P90.UncachedInput != 3800 || p2.Median.Output != 400 || p2.Median.Reasoning != 0 || p2.Median.OutputTotal != 500 {
		t.Errorf("day2 = %+v", p2)
	}
	p3 := points["11-12 codex/0.130.0"]
	if p3.Requests != 2 || p3.Median.UncachedInput != 1250 {
		t.Errorf("day3 (r8 has no trace and counts by request id) = %+v", p3)
	}
}

func TestDriftMarksFirstSeenValuesInRange(t *testing.T) {
	s := seedDB(t)
	// Model view gets only model markers, never client versions.
	d, err := Drift(s.DB(), rangeFilter(), nil, "response_model", "day")
	if err != nil {
		t.Fatal(err)
	}
	wantModel := []Marker{{T: day3.UnixMilli(), Kind: "response_model", Value: "gpt-5.1"}}
	if len(d.Markers) != len(wantModel) || d.Markers[0] != wantModel[0] {
		t.Fatalf("response_model markers = %+v, want %+v", d.Markers, wantModel)
	}
	keys := map[string]bool{}
	for _, pt := range d.Points {
		keys[pt.Key] = true
	}
	if !keys["gpt-5"] || !keys["gpt-5.1"] || len(keys) != 2 {
		t.Errorf("response_model keys = %v", keys)
	}
	// Client (harness) view keeps one continuous series per client name and
	// marks new versions of clients, never models.
	dc, err := Drift(s.DB(), rangeFilter(), nil, "client", "day")
	if err != nil {
		t.Fatal(err)
	}
	wantClient := []Marker{{T: day2.UnixMilli(), Kind: "client_version", Value: "codex/0.130.0"}}
	if len(dc.Markers) != len(wantClient) || dc.Markers[0] != wantClient[0] {
		t.Fatalf("client markers = %+v, want %+v", dc.Markers, wantClient)
	}
	if len(dc.Points) != 3 {
		t.Fatalf("client points = %+v, want 3 daily points for codex", dc.Points)
	}
	for _, pt := range dc.Points {
		if pt.Key != "codex" {
			t.Errorf("client point key = %q, want codex", pt.Key)
		}
	}
	// Sub-day buckets align both points and markers to the requested bucket.
	dh, err := Drift(s.DB(), rangeFilter(), nil, "client", "hour")
	if err != nil {
		t.Fatal(err)
	}
	if dh.Bucket != "hour" || len(dh.Markers) != 1 || dh.Markers[0].T != day2.Add(9*time.Hour).UnixMilli() {
		t.Errorf("hourly drift markers = %+v, want 09:00 on day 2", dh.Markers)
	}
}

// Everything is new on the first day the plugin recorded, so that day says
// nothing about a change and gets no marker.
func TestDriftSkipsMarkersOnTheFirstDayOfData(t *testing.T) {
	s := seedDB(t)
	d, err := Drift(s.DB(), Filter{From: day0, To: day4}, nil, "response_model", "day")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range d.Markers {
		if m.T == day0.UnixMilli() {
			t.Errorf("marker on the first day of data: %+v", m)
		}
	}
	if len(d.Markers) != 1 || d.Markers[0].Value != "gpt-5.1" {
		t.Errorf("markers = %+v, want only day 3 model change", d.Markers)
	}
}

func TestDriftRejectsUnknownGroup(t *testing.T) {
	s := seedDB(t)
	_, err := Drift(s.DB(), rangeFilter(), nil, "nope", "day")
	var pe *ParamError
	if !errors.As(err, &pe) || pe.Param != "group" {
		t.Errorf("err = %v", err)
	}
	_, err = Drift(s.DB(), rangeFilter(), nil, "response_model", "week")
	if !errors.As(err, &pe) || pe.Param != "bucket" {
		t.Errorf("bucket err = %v", err)
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
	if _, err := Drift(s.DB(), rangeFilter(), nil, "client_version", "day"); err == nil || errors.As(err, &pe) {
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
	d, err := Drift(s.DB(), rangeFilter(), nil, "client_version", "day")
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
	d, err := Drift(s.DB(), rangeFilter(), nil, "client_version", "day")
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

func TestSummaryCountsSessionsPerProvider(t *testing.T) {
	s := seedDB(t)
	sum, err := Summary(s.DB(), rangeFilter(), testPrices(t))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, p := range sum.Providers {
		got[p.Provider] = p.Sessions
	}
	if got["codex"] != 2 || got["openai-compat"] != 0 {
		t.Errorf("sessions per provider = %v, want codex 2 (s1, s2) and openai-compat 0", got)
	}
}

// Latency is measured per call, so the two billing rows of one call count
// once. Calls without a first-token time (non-streaming) are left out of the
// TTFT percentiles instead of pulling them toward zero.
func TestDriftMeasuresLatencyAndTTFTPerRequest(t *testing.T) {
	s := seedDB(t)
	at := day1.Add(14 * time.Hour).UnixMilli()
	row := func(id, trace string, at, latency, ttft int64) store.Row {
		return store.Row{RequestID: id, TraceID: trace, RequestedAt: at, Provider: "codex", Model: "gpt-5",
			Generate: true, ClientName: "codex", ClientVersion: "0.149.0", LatencyMs: latency, TTFTMs: ttft, UncachedInput: 10}
	}
	rows := []store.Row{
		row("l1", "tl1", at, 1000, 200),
		row("l2a", "tl2", at+1, 3000, 0),
		row("l2b", "tl2", at+1, 3000, 0),
		row("l3", "tl3", at+2, 2000, 600),
	}
	if err := s.InsertBatch(rows); err != nil {
		t.Fatal(err)
	}
	d, err := Drift(s.DB(), rangeFilter(), nil, "client_version", "day")
	if err != nil {
		t.Fatal(err)
	}
	for _, pt := range d.Points {
		if pt.Key != "codex/0.149.0" {
			continue
		}
		if pt.Requests != 3 || pt.Median.LatencyMs != 2000 || !near(pt.P90.LatencyMs, 2800) {
			t.Errorf("latency = %+v / %+v, want 3 requests, median 2000, p90 2800", pt.Median, pt.P90)
		}
		if pt.TTFTRequests != 2 || pt.SpeedRequests != 0 || pt.InputRequests != 3 || pt.Median.TTFTMs != 400 || !near(pt.P90.TTFTMs, 560) {
			t.Errorf("ttft = %v / %v, want median 400 and p90 560 over the two streamed calls", pt.Median.TTFTMs, pt.P90.TTFTMs)
		}
		return
	}
	t.Fatalf("no point for codex/0.149.0 in %+v", d.Points)
}

// The same model reached through two providers is two rows, as in the provider
// legend, so each row's cost belongs to one provider.
func TestBreakdownByModelSplitsProviders(t *testing.T) {
	s := seedDB(t)
	other := seed{id: "x1", trace: "tx1", provider: "openai-compat", model: "gpt-5", response: "gpt-5", version: "0.130.0",
		at: day3.Add(12 * time.Hour), generate: true, uncached: 1000, output: 10}
	if err := s.InsertBatch([]store.Row{other.row()}); err != nil {
		t.Fatal(err)
	}
	b, err := Breakdown(s.DB(), rangeFilter(), testPrices(t), "model")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]BreakdownRow{}
	for _, r := range b.Rows {
		got[r.Provider+" "+r.Key] = r
	}
	if len(b.Rows) != 3 {
		t.Fatalf("rows = %+v, want gpt-5 per provider and gpt-5.1", b.Rows)
	}
	if r := got["codex gpt-5"]; r.Requests != 5 || r.Records != 7 {
		t.Errorf("codex gpt-5 = %+v", r)
	}
	if r := got["openai-compat gpt-5"]; r.Requests != 1 || r.UncachedInput != 1000 || !near(r.CostUSD, 1000*gpt5Input+10*gpt5Output) {
		t.Errorf("openai-compat gpt-5 = %+v", r)
	}
	if r := got["openai-compat gpt-5.1"]; r.Requests != 2 || r.UnpricedTokens != 2700 {
		t.Errorf("openai-compat gpt-5.1 = %+v", r)
	}
}

// A filter that matches nothing must still answer with empty lists. The page
// maps over every list, and a null markers list broke the drift chart.
func TestEmptyRangeAnswersEmptyLists(t *testing.T) {
	s := seedDB(t)
	f := Filter{From: day4.AddDate(0, 0, 10), To: day4.AddDate(0, 0, 11)}
	prices := testPrices(t)
	results := map[string]func() (any, error){
		"summary":    func() (any, error) { return Summary(s.DB(), f, prices) },
		"timeseries": func() (any, error) { return Timeseries(s.DB(), f, prices, "day", "provider") },
		"drift":      func() (any, error) { return Drift(s.DB(), f, prices, "response_model", "day") },
		"breakdown":  func() (any, error) { return Breakdown(s.DB(), f, prices, "model") },
		"models":     func() (any, error) { return Models(s.DB(), f, prices) },
		"errors":     func() (any, error) { return Errors(s.DB(), f) },
	}
	for name, run := range results {
		v, err := run()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "null") {
			t.Errorf("%s answers null: %s", name, raw)
		}
	}
}

// Each credential gets its own row, so its API-priced value can be read
// against the plan it pays for. Rows from before the plugin recorded the
// credential share the empty key.
func TestBreakdownByAccount(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows := []store.Row{
		{RequestID: "a", RequestedAt: day1.Add(time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-5", Generate: true, AuthIndex: "acc1", UncachedInput: 1000},
		{RequestID: "b", RequestedAt: day1.Add(2 * time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-5", Generate: true, AuthIndex: "acc1", UncachedInput: 3000},
		{RequestID: "c", RequestedAt: day1.Add(3 * time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-5", Generate: true, AuthIndex: "acc2", UncachedInput: 2000},
		{RequestID: "d", RequestedAt: day1.Add(4 * time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-5", Generate: true, UncachedInput: 500},
	}
	if err := s.InsertBatch(rows); err != nil {
		t.Fatal(err)
	}
	b, err := Breakdown(s.DB(), rangeFilter(), testPrices(t), "account")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"acc1": 4000 * gpt5Input, "acc2": 2000 * gpt5Input, "": 500 * gpt5Input}
	if len(b.Rows) != len(want) {
		t.Fatalf("rows = %+v", b.Rows)
	}
	for _, r := range b.Rows {
		if cost, ok := want[r.Key]; !ok || !near(r.CostUSD, cost) {
			t.Errorf("row %q cost = %v, want %v", r.Key, r.CostUSD, cost)
		}
	}
	if b.Rows[0].Key != "acc1" {
		t.Errorf("first row = %q, want the costliest account", b.Rows[0].Key)
	}
}

func TestErrorsBreakdownAndProviderAndUserAgent(t *testing.T) {
	s := seedDB(t)
	extra := []store.Row{
		{RequestID: "e1", RequestedAt: day2.Add(time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-5", AuthIndex: "acc1", Failed: true, StatusCode: 429, UserAgent: "codex_app_server/0.170.0"},
		{RequestID: "e2", RequestedAt: day2.Add(2 * time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-5", AuthIndex: "acc1", Failed: true, StatusCode: 429, UserAgent: "codex_app_server/0.170.0"},
		{RequestID: "e3", RequestedAt: day2.Add(3 * time.Hour).UnixMilli(), Provider: "claude", Model: "claude-opus", AuthIndex: "acc2", Failed: true, StatusCode: 502, UserAgent: "claude-cli/2.1.0"},
	}
	if err := s.InsertBatch(extra); err != nil {
		t.Fatal(err)
	}
	errs, err := Errors(s.DB(), rangeFilter())
	if err != nil {
		t.Fatal(err)
	}
	if errs.Total != 3 || len(errs.Rows) != 2 || errs.Rows[0].StatusCode != 429 || errs.Rows[0].Count != 2 || !near(errs.Rows[0].Share, 2.0/3.0) {
		t.Fatalf("errors = %+v", errs)
	}
	bp, err := Breakdown(s.DB(), rangeFilter(), testPrices(t), "provider")
	if err != nil {
		t.Fatal(err)
	}
	byProv := map[string]BreakdownRow{}
	for _, r := range bp.Rows {
		byProv[r.Key] = r
	}
	if byProv["codex"].Failed != 2 || byProv["claude"].Failed != 1 {
		t.Errorf("provider failed counts = %+v", byProv)
	}
	bua, err := Breakdown(s.DB(), rangeFilter(), testPrices(t), "user_agent")
	if err != nil {
		t.Fatal(err)
	}
	if len(bua.Rows) < 2 {
		t.Errorf("user_agent rows = %+v", bua.Rows)
	}
	// Session depth and per-request medians in Summary.
	sum, err := Summary(s.DB(), rangeFilter(), testPrices(t))
	if err != nil {
		t.Fatal(err)
	}
	// s1 has 2 reqs (peak 4000), s2 has 3 reqs (peak 8000) -> median req/sess = 2.5, median peak = 6000.
	if sum.Totals.MedianRequestsPerSession != 2.5 || sum.Totals.MedianPeakInputPerSession != 6000 {
		t.Errorf("session medians = req %.1f peak %.1f, want 2.5 / 6000", sum.Totals.MedianRequestsPerSession, sum.Totals.MedianPeakInputPerSession)
	}
}

func TestDriftTokensPerSec(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "tps.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows := []store.Row{
		// 200 output+reasoning over (3000 - 1000) = 2s -> 100 tok/s
		{RequestID: "a", TraceID: "ta", RequestedAt: day1.Add(time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-5", Generate: true, OutputNonReasoning: 150, OutputReasoning: 50, LatencyMs: 3000, TTFTMs: 1000},
		// 100 output over 2000ms (no ttft) -> 50 tok/s
		{RequestID: "b", TraceID: "tb", RequestedAt: day1.Add(2 * time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-5", Generate: true, OutputNonReasoning: 100, LatencyMs: 2000, TTFTMs: 0},
	}
	if err := s.InsertBatch(rows); err != nil {
		t.Fatal(err)
	}
	d, err := Drift(s.DB(), rangeFilter(), testPrices(t), "response_model", "day")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Points) != 1 || d.Points[0].SpeedRequests != 2 || d.Points[0].InputRequests != 0 || !near(d.Points[0].Median.TokensPerSec, 75) {
		t.Fatalf("tps = %+v, want median 75", d.Points)
	}
}

// LiteLLM prices some models higher once a request's input passes a
// threshold. Each request is priced by its own input, so one long request
// does not reprice the short ones it is summed with.
func TestCostChargesLongContextRequestsTheirRate(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "long.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	row := func(id string, at time.Time, uncached int64) store.Row {
		return store.Row{RequestID: id, TraceID: id, RequestedAt: at.UnixMilli(), Provider: "gemini", Model: "gemini-3-pro",
			ResponseModel: "gemini-3-pro", Generate: true, UncachedInput: uncached, OutputNonReasoning: 1000}
	}
	if err := s.InsertBatch([]store.Row{row("short", day1.Add(time.Hour), 150_000), row("long", day1.Add(2*time.Hour), 250_000)}); err != nil {
		t.Fatal(err)
	}
	table, err := pricing.ParseLiteLLM([]byte(`{"gemini/gemini-3-pro": {"input_cost_per_token": 2e-06, "output_cost_per_token": 1.2e-05,
		"input_cost_per_token_above_200k_tokens": 4e-06, "output_cost_per_token_above_200k_tokens": 1.8e-05}}`))
	if err != nil {
		t.Fatal(err)
	}
	short := 150_000*2e-6 + 1000*1.2e-5
	long := 250_000*4e-6 + 1000*1.8e-5

	sum, err := Summary(s.DB(), rangeFilter(), table)
	if err != nil {
		t.Fatal(err)
	}
	if !near(sum.Totals.CostUSD, short+long) {
		t.Errorf("summary cost = %v, want %v", sum.Totals.CostUSD, short+long)
	}
	d, err := Drift(s.DB(), rangeFilter(), table, "response_model", "day")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Points) != 1 || !near(d.Points[0].Median.CostUSD, (short+long)/2) {
		t.Errorf("drift points = %+v, want median cost %v", d.Points, (short+long)/2)
	}
}
