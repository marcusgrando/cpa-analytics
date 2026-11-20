package query

import (
	"errors"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/marcusgrando/cpa-analytics/internal/store"
)

func requestsQuery(t *testing.T, q url.Values) RequestsQuery {
	t.Helper()
	rq, err := ParseRequestsQuery(q)
	if err != nil {
		t.Fatal(err)
	}
	return rq
}

func callIDs(calls []Call) []string {
	ids := make([]string, len(calls))
	for i, c := range calls {
		ids[i] = c.ID
	}
	return ids
}

func callID(trace string, at time.Time) string {
	return trace + ":" + strconv.FormatInt(at.UnixMilli(), 10)
}

// The request log lists one entry per upstream call, newest first, with the
// split rows of a call summed and priced together.
func TestRequestsListsCallsNewestFirst(t *testing.T) {
	s := seedDB(t)
	res, err := Requests(s.DB(), rangeFilter(), testPrices(t), requestsQuery(t, url.Values{}))
	if err != nil {
		t.Fatal(err)
	}
	h := time.Hour
	want := []string{
		callID("r8", day3.Add(10*h)), callID("t7", day3.Add(9*h)), callID("t6", day2.Add(11*h)),
		callID("t5", day2.Add(10*h)), callID("t4", day2.Add(9*h)), callID("t3", day1.Add(11*h)),
		callID("t2", day1.Add(10*h)), callID("t1", day1.Add(9*h)),
	}
	if got := callIDs(res.Calls); res.Total != 8 || len(got) != len(want) {
		t.Fatalf("total %d, calls %v, want %v", res.Total, got, want)
	}
	for i := range want {
		if res.Calls[i].ID != want[i] {
			t.Errorf("call %d = %s, want %s", i, res.Calls[i].ID, want[i])
		}
	}
	t2 := res.Calls[6]
	wantCost := 1000*gpt5Input + 1000*gpt5CacheRead + 100*gpt5Output
	if t2.Records != 2 || t2.UncachedInput != 1000 || t2.CacheRead != 1000 || !near(t2.CostUSD, wantCost) {
		t.Errorf("t2 = %+v, want 2 records, 1000 uncached, 1000 cache read, cost %v", t2, wantCost)
	}
	if t2.Provider != "codex" || t2.Client != "codex/0.129.0" || t2.SessionID != "s1" || t2.APIKeyHash != "abc123" ||
		len(t2.Models) != 1 || t2.Models[0] != "gpt-5" || !t2.Generate || t2.Failed {
		t.Errorf("t2 details = %+v", t2)
	}
	if r8 := res.Calls[0]; len(r8.Models) != 1 || r8.Models[0] != "gpt-5.1" || r8.CostUSD != 0 || r8.UnpricedTokens != 1600 {
		t.Errorf("unpriced call = %+v, want gpt-5.1 with 1600 unpriced tokens", r8)
	}
}

func TestRequestsSortsAndPages(t *testing.T) {
	s := seedDB(t)
	byCost, err := Requests(s.DB(), rangeFilter(), testPrices(t), requestsQuery(t, url.Values{"sort": {"cost"}, "limit": {"2"}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(byCost.Calls) != 2 || byCost.Total != 8 || byCost.Calls[0].ID != callID("t3", day1.Add(11*time.Hour)) || byCost.Calls[1].ID != callID("t6", day2.Add(11*time.Hour)) {
		t.Errorf("by cost = %v, want t3 then t6", callIDs(byCost.Calls))
	}
	page, err := Requests(s.DB(), rangeFilter(), testPrices(t), requestsQuery(t, url.Values{"offset": {"3"}, "limit": {"2"}}))
	if err != nil {
		t.Fatal(err)
	}
	if got := callIDs(page.Calls); len(got) != 2 || got[0] != callID("t5", day2.Add(10*time.Hour)) || got[1] != callID("t4", day2.Add(9*time.Hour)) {
		t.Errorf("page 2 = %v, want t5 and t4", got)
	}
	byTokens, err := Requests(s.DB(), rangeFilter(), testPrices(t), requestsQuery(t, url.Values{"sort": {"tokens"}, "limit": {"1"}}))
	if err != nil {
		t.Fatal(err)
	}
	if byTokens.Calls[0].ID != callID("t3", day1.Add(11*time.Hour)) {
		t.Errorf("by tokens = %v, want t3", callIDs(byTokens.Calls))
	}
}

func TestRequestsFiltersFailedCalls(t *testing.T) {
	s := seedDB(t)
	at := day2.Add(12 * time.Hour)
	if err := s.InsertBatch([]store.Row{{RequestID: "f1", TraceID: "tf1", RequestedAt: at.UnixMilli(), Provider: "codex", Model: "gpt-5",
		Generate: true, Failed: true, StatusCode: 429, LatencyMs: 300, ClientName: "codex", ClientVersion: "0.130.0"}}); err != nil {
		t.Fatal(err)
	}
	res, err := Requests(s.DB(), rangeFilter(), testPrices(t), requestsQuery(t, url.Values{"outcome": {"failed"}}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 || len(res.Calls) != 1 || !res.Calls[0].Failed || res.Calls[0].StatusCode != 429 || res.Calls[0].LatencyMs != 300 {
		t.Errorf("failed calls = %+v", res)
	}
	ok, err := Requests(s.DB(), rangeFilter(), testPrices(t), requestsQuery(t, url.Values{"outcome": {"ok"}}))
	if err != nil {
		t.Fatal(err)
	}
	if ok.Total != 8 {
		t.Errorf("successful calls = %d, want 8", ok.Total)
	}
}

func TestRequestsRejectsInvalidParameters(t *testing.T) {
	for _, tc := range []struct {
		q     url.Values
		param string
	}{
		{url.Values{"sort": {"price"}}, "sort"},
		{url.Values{"outcome": {"maybe"}}, "outcome"},
		{url.Values{"limit": {"0"}}, "limit"},
		{url.Values{"limit": {"201"}}, "limit"},
		{url.Values{"limit": {"ten"}}, "limit"},
		{url.Values{"offset": {"-1"}}, "offset"},
	} {
		_, err := ParseRequestsQuery(tc.q)
		var pe *ParamError
		if !errors.As(err, &pe) || pe.Param != tc.param {
			t.Errorf("%v: err = %v, want a ParamError for %s", tc.q, err, tc.param)
		}
	}
}
