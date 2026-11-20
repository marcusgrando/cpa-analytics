package query

import (
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/marcusgrando/cpa-analytics/internal/store"
)

// Realtime compares the last minutes with the day so far, so a slowdown or a
// cache collapse shows within minutes instead of in the next daily point.
func TestRealtimeComparesTheLastWindowWithToday(t *testing.T) {
	s, err := store.Open(t.TempDir() + "/rt.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := day2.Add(15 * time.Hour)
	row := func(id string, at time.Time, ttft, latency, uncached, cacheRead, output int64) store.Row {
		return store.Row{RequestID: id, TraceID: id, RequestedAt: at.UnixMilli(), Provider: "codex", Model: "gpt-5", Generate: true,
			Stream: ttft > 0, TTFTMs: ttft, LatencyMs: latency, UncachedInput: uncached, CacheRead: cacheRead, OutputNonReasoning: output}
	}
	failed := row("d", now.Add(-2*time.Minute), 0, 100, 0, 0, 0)
	failed.Failed = true
	if err := s.InsertBatch([]store.Row{
		row("a", now.Add(-5*time.Minute), 400, 2000, 1000, 0, 100),
		row("b", now.Add(-10*time.Minute), 600, 4000, 1000, 3000, 0),
		row("c", now.Add(-3*time.Hour), 200, 1000, 2000, 0, 0),
		failed,
		row("e", day1.Add(20*time.Hour), 900, 9000, 5000, 0, 0),
	}); err != nil {
		t.Fatal(err)
	}
	res, err := Realtime(s.DB(), Filter{}, testPrices(t), 15*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	n, d := res.Now, res.Today
	if res.WindowMinutes != 15 || n.Minutes != 15 || n.Requests != 2 || n.Failed != 1 || n.Tokens != 5100 ||
		n.MedianTTFTMs != 500 || n.MedianLatencyMs != 3000 || !near(n.CacheHitRate, 0.6) {
		t.Errorf("now = %+v", n)
	}
	wantCost := 2000*gpt5Input + 3000*gpt5CacheRead + 100*gpt5Output
	if !near(n.CostUSD, wantCost) {
		t.Errorf("now cost = %v, want %v", n.CostUSD, wantCost)
	}
	if d.From != day2.UnixMilli() || d.Minutes != 900 || d.Requests != 3 || d.Tokens != 7100 || d.MedianTTFTMs != 400 || d.MedianLatencyMs != 2000 {
		t.Errorf("today = %+v", d)
	}
}

// The day starts at the viewer's local midnight.
func TestRealtimeTodayFollowsTheViewerTimezone(t *testing.T) {
	s := seedDB(t)
	now := day2.Add(15 * time.Hour)
	res, err := Realtime(s.DB(), Filter{TZOffsetMinutes: -180}, testPrices(t), 15*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := day2.Add(3 * time.Hour).UnixMilli(); res.Today.From != want || res.Today.Minutes != 720 {
		t.Errorf("today from %d over %v minutes, want %d over 720", res.Today.From, res.Today.Minutes, want)
	}
}

func TestRealtimeRejectsOtherWindows(t *testing.T) {
	for _, raw := range []string{"20", "0", "x"} {
		_, err := ParseRealtimeWindow(url.Values{"window": {raw}})
		var pe *ParamError
		if !errors.As(err, &pe) || pe.Param != "window" {
			t.Errorf("window=%s: err = %v, want a ParamError for window", raw, err)
		}
	}
	if w, err := ParseRealtimeWindow(url.Values{}); err != nil || w != 15*time.Minute {
		t.Errorf("default window = %v, %v", w, err)
	}
}
