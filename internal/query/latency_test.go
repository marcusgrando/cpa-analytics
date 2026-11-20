package query

import (
	"testing"
	"time"

	"github.com/marcusgrando/cpa-analytics/internal/store"
)

// The scatter plots each successful call's time to first token against its
// total latency. Percentiles cover every call, the points an even sample.
func TestLatencyPointsSampleCallsWithAFirstToken(t *testing.T) {
	s, err := store.Open(t.TempDir() + "/lat.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var rows []store.Row
	for i, ms := range []int64{100, 200, 300, 400, 500} {
		rows = append(rows, store.Row{RequestID: "s" + string(rune('a'+i)), TraceID: "s" + string(rune('a'+i)),
			RequestedAt: day1.Add(time.Duration(i) * time.Hour).UnixMilli(), Provider: "claude", Model: "claude-opus-5-5",
			Generate: true, Stream: true, TTFTMs: ms, LatencyMs: ms * 10})
	}
	rows = append(rows,
		store.Row{RequestID: "plain", TraceID: "plain", RequestedAt: day1.Add(6 * time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-5", Generate: true, LatencyMs: 800},
		store.Row{RequestID: "bad", TraceID: "bad", RequestedAt: day1.Add(7 * time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-5", Generate: true, Failed: true, TTFTMs: 9, LatencyMs: 9},
	)
	if err := s.InsertBatch(rows); err != nil {
		t.Fatal(err)
	}
	res, err := Latency(s.DB(), rangeFilter(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 5 || res.NoTTFT != 1 || res.P50TTFTMs != 300 || res.P50LatencyMs != 3000 || !near(res.P95TTFTMs, 480) || !near(res.P95LatencyMs, 4800) {
		t.Errorf("latency = %+v", res)
	}
	if len(res.Points) != 2 || res.Points[0].TTFTMs != 100 || res.Points[1].TTFTMs != 300 ||
		res.Points[0].Provider != "claude" || res.Points[0].Model != "claude-opus-5-5" || res.Points[1].LatencyMs != 3000 {
		t.Errorf("points = %+v, want the 1st and 3rd calls", res.Points)
	}
}
