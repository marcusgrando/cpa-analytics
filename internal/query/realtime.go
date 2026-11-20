package query

import (
	"database/sql"
	"fmt"
	"net/url"
	"time"

	"github.com/marcusgrando/cpa-analytics/internal/pricing"
)

var realtimeWindows = map[string]time.Duration{"15": 15 * time.Minute, "30": 30 * time.Minute, "60": time.Hour}

// ParseRealtimeWindow reads window, in minutes: 15 (the default), 30 or 60.
func ParseRealtimeWindow(q url.Values) (time.Duration, error) {
	w, ok := realtimeWindows[paramOr(q, "window", "15")]
	if !ok {
		return 0, &ParamError{Param: "window", Reason: "must be 15, 30 or 60"}
	}
	return w, nil
}

// RealtimeStats describes the calls in one stretch of time. The page turns
// the counts into rates over Minutes.
type RealtimeStats struct {
	From    int64   `json:"from"`
	Minutes float64 `json:"minutes"`
	// Requests counts successful upstream calls, Failed failed records.
	Requests     int64   `json:"requests"`
	Failed       int64   `json:"failed"`
	Tokens       int64   `json:"tokens"`
	CostUSD      float64 `json:"cost_usd"`
	CacheHitRate float64 `json:"cache_hit_rate"`
	// Medians are per successful call. Calls without a first token, such as
	// non-streamed ones, are left out of the TTFT median.
	MedianTTFTMs    float64 `json:"median_ttft_ms"`
	MedianLatencyMs float64 `json:"median_latency_ms"`
}

// RealtimeResult compares the last window with the viewer's day so far.
type RealtimeResult struct {
	WindowMinutes int           `json:"window_minutes"`
	Now           RealtimeStats `json:"now"`
	Today         RealtimeStats `json:"today"`
}

// Realtime reads the window before now and the local day up to now. It keeps
// the filter's provider, model, client, account and API key, and replaces its
// range.
func Realtime(db *sql.DB, f Filter, table *pricing.Table, window time.Duration, now time.Time) (RealtimeResult, error) {
	res := RealtimeResult{WindowMinutes: int(window / time.Minute)}
	midnight := time.UnixMilli(f.bucketStart(now.UnixMilli(), dayMillis))
	var err error
	if res.Now, err = realtimeStats(db, f, table, now.Add(-window), now); err != nil {
		return RealtimeResult{}, err
	}
	if res.Today, err = realtimeStats(db, f, table, midnight, now); err != nil {
		return RealtimeResult{}, err
	}
	return res, nil
}

func realtimeStats(db *sql.DB, f Filter, table *pricing.Table, from, to time.Time) (RealtimeStats, error) {
	f.From, f.To = from, to
	st := RealtimeStats{From: from.UnixMilli(), Minutes: to.Sub(from).Minutes()}
	rows, err := aggregate(db, f, table)
	if err != nil {
		return RealtimeStats{}, err
	}
	var tokens Tokens
	for _, r := range rows {
		tokens.add(r.tokens)
		st.CostUSD += r.cost
		st.Failed += r.failed
	}
	st.Tokens = tokens.total()
	st.CacheHitRate = cacheHitRate(tokens)
	calls, err := perRequest(db, f, "''")
	if err != nil {
		return RealtimeStats{}, fmt.Errorf("realtime: %w", err)
	}
	var ttfts, latencies []float64
	for _, c := range calls {
		latencies = append(latencies, float64(c.latencyMs))
		if c.ttftMs > 0 {
			ttfts = append(ttfts, float64(c.ttftMs))
		}
	}
	st.Requests = int64(len(calls))
	st.MedianTTFTMs = percentile(ttfts, 0.5)
	st.MedianLatencyMs = percentile(latencies, 0.5)
	return st, nil
}
