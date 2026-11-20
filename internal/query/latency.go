package query

import (
	"database/sql"
	"fmt"
)

// MaxLatencyPoints caps the points the latency scatter draws.
const MaxLatencyPoints = 3000

// LatencyPoint is one successful call's time to first token and total latency.
type LatencyPoint struct {
	At        int64  `json:"at"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	TTFTMs    int64  `json:"ttft_ms"`
	LatencyMs int64  `json:"latency_ms"`
}

// LatencyResult holds an even sample of the calls with a first token, in
// time order, and percentiles over all of them. NoTTFT counts successful calls
// CPA reported no first token for, such as non-streamed ones.
type LatencyResult struct {
	Total        int            `json:"total"`
	NoTTFT       int            `json:"no_ttft"`
	P50TTFTMs    float64        `json:"p50_ttft_ms"`
	P95TTFTMs    float64        `json:"p95_ttft_ms"`
	P50LatencyMs float64        `json:"p50_latency_ms"`
	P95LatencyMs float64        `json:"p95_latency_ms"`
	Points       []LatencyPoint `json:"points"`
}

// Latency reads every successful generating call in the filter and keeps at
// most maxPoints of them, evenly spaced in time order.
func Latency(db *sql.DB, f Filter, maxPoints int) (LatencyResult, error) {
	where, args := f.where(true)
	// The first row of a call is the scheduled request, so it names the model.
	rows, err := db.Query(`SELECT c.at, r.provider, c.latency, c.ttft, (`+modelExpr+`)
		FROM (SELECT MIN(id) AS first, MIN(requested_at) AS at, MAX(latency_ms) AS latency, MAX(ttft_ms) AS ttft
			FROM requests WHERE generate = 1 AND failed = 0 AND `+where+` GROUP BY `+callExpr+`) AS c
		JOIN requests AS r ON r.id = c.first
		ORDER BY c.at, c.first`, args...)
	if err != nil {
		return LatencyResult{}, fmt.Errorf("latency: %w", err)
	}
	defer rows.Close()
	var all []LatencyPoint
	res := LatencyResult{Points: []LatencyPoint{}}
	for rows.Next() {
		var p LatencyPoint
		if err := rows.Scan(&p.At, &p.Provider, &p.LatencyMs, &p.TTFTMs, &p.Model); err != nil {
			return LatencyResult{}, fmt.Errorf("latency scan: %w", err)
		}
		if p.TTFTMs <= 0 || p.LatencyMs <= 0 {
			res.NoTTFT++
			continue
		}
		all = append(all, p)
	}
	if err := rows.Err(); err != nil {
		return LatencyResult{}, fmt.Errorf("latency rows: %w", err)
	}
	ttfts := make([]float64, len(all))
	latencies := make([]float64, len(all))
	for i, p := range all {
		ttfts[i], latencies[i] = float64(p.TTFTMs), float64(p.LatencyMs)
	}
	res.Total = len(all)
	res.P50TTFTMs, res.P95TTFTMs = percentile(ttfts, 0.5), percentile(ttfts, 0.95)
	res.P50LatencyMs, res.P95LatencyMs = percentile(latencies, 0.5), percentile(latencies, 0.95)
	if len(all) <= maxPoints {
		res.Points = append(res.Points, all...)
		return res, nil
	}
	for i := 0; i < maxPoints; i++ {
		res.Points = append(res.Points, all[i*len(all)/maxPoints])
	}
	return res, nil
}
