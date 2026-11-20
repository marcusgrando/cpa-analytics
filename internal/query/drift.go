package query

import (
	"database/sql"
	"fmt"
	"math"
	"sort"

	"github.com/marcusgrando/cpa-analytics/internal/pricing"
)

// Stats holds one value per token bucket, cost and timing, for a median or a p90.
type Stats struct {
	// Input is the whole prompt: uncached input, cache read and cache write.
	Input         float64 `json:"input"`
	UncachedInput float64 `json:"uncached_input"`
	CacheRead     float64 `json:"cache_read"`
	CacheWrite    float64 `json:"cache_write"`
	Output        float64 `json:"output"`
	Reasoning     float64 `json:"reasoning"`
	// OutputTotal includes both visible output and reasoning tokens.
	OutputTotal float64 `json:"output_total"`
	CostUSD     float64 `json:"cost_usd"`
	LatencyMs   float64 `json:"latency_ms"`
	// TTFTMs covers only calls that reported a first token (streamed ones).
	TTFTMs float64 `json:"ttft_ms"`
	// TokensPerSec is output+reasoning tokens per generation second.
	TokensPerSec float64 `json:"tokens_per_sec"`
}

// DriftPoint is one day of one group, measured per request (see perRequest).
type DriftPoint struct {
	// T is the local day start in unix ms.
	T            int64   `json:"t"`
	Key          string  `json:"key"`
	Requests     int64   `json:"requests"`
	Median       Stats   `json:"median"`
	P90          Stats   `json:"p90"`
	CacheHitRate float64 `json:"cache_hit_rate"`
}

// Marker flags the day a client version, reported model or reasoning effort
// first appears.
type Marker struct {
	T     int64  `json:"t"`
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// DriftResult answers the drift route.
type DriftResult struct {
	Group   string       `json:"group"`
	Bucket  string       `json:"bucket"`
	Points  []DriftPoint `json:"points"`
	Markers []Marker     `json:"markers"`
}

var driftGroups = map[string]string{
	"response_model":   modelExpr,
	"client":           clientNameExpr,
	"client_version":   clientExpr,
	"reasoning_effort": effortExpr,
}

type markerSpec struct{ kind, expr, skip string }

// groupMarker maps each drift grouping to the single marker kind that belongs
// to it, so model views never mix in harness versions and vice versa.
var groupMarker = map[string]markerSpec{
	"response_model":   {"response_model", modelExpr, "(" + modelExpr + ") = ''"},
	"client":           {"client_version", clientExpr, "client_name = ''"},
	"client_version":   {"client_version", clientExpr, "client_name = ''"},
	"reasoning_effort": {"reasoning_effort", effortExpr, "reasoning_effort = ''"},
}

// Drift returns per-bucket medians and p90 per request (see perRequest),
// grouped by reported model, client (harness), client version or reasoning
// effort, plus change markers matching the active grouping.
func Drift(db *sql.DB, f Filter, table *pricing.Table, group, bucket string) (DriftResult, error) {
	groupExpr, ok := driftGroups[group]
	if !ok {
		return DriftResult{}, &ParamError{Param: "group", Reason: "must be response_model, client, client_version or reasoning_effort"}
	}
	size, ok := bucketSizes[bucket]
	if !ok {
		return DriftResult{}, &ParamError{Param: "bucket", Reason: "must be day, hour or minute"}
	}
	reqs, err := perRequestPriced(db, f, table, groupExpr)
	if err != nil {
		return DriftResult{}, err
	}
	type bucketKey struct {
		t   int64
		key string
	}
	buckets := map[bucketKey][]inbound{}
	for _, rq := range reqs {
		k := bucketKey{f.bucketStart(rq.at, size), rq.key}
		buckets[k] = append(buckets[k], rq)
	}
	res := DriftResult{Group: group, Bucket: bucket, Points: make([]DriftPoint, 0, len(buckets))}
	for k, calls := range buckets {
		res.Points = append(res.Points, driftPoint(k.t, k.key, calls))
	}
	sort.Slice(res.Points, func(i, j int) bool {
		if res.Points[i].T != res.Points[j].T {
			return res.Points[i].T < res.Points[j].T
		}
		return res.Points[i].Key < res.Points[j].Key
	})
	if res.Markers, err = markers(db, f, groupMarker[group], size); err != nil {
		return DriftResult{}, err
	}
	return res, nil
}

func driftPoint(t int64, key string, calls []inbound) DriftPoint {
	columns := func(pick func(inbound) float64) []float64 {
		out := make([]float64, len(calls))
		for i, c := range calls {
			out[i] = pick(c)
		}
		return out
	}
	input := columns(func(c inbound) float64 { return float64(c.tokens.input()) })
	uncached := columns(func(c inbound) float64 { return float64(c.tokens.UncachedInput) })
	cacheRead := columns(func(c inbound) float64 { return float64(c.tokens.CacheRead) })
	cacheWrite := columns(func(c inbound) float64 { return float64(c.tokens.CacheWrite) })
	output := columns(func(c inbound) float64 { return float64(c.tokens.Output) })
	reasoning := columns(func(c inbound) float64 { return float64(c.tokens.Reasoning) })
	outputTotal := columns(func(c inbound) float64 { return float64(c.tokens.Output + c.tokens.Reasoning) })
	costUSD := columns(func(c inbound) float64 { return c.costUSD })
	latency := columns(func(c inbound) float64 { return float64(c.latencyMs) })
	var ttft, tps []float64
	for _, c := range calls {
		if c.ttftMs > 0 {
			ttft = append(ttft, float64(c.ttftMs))
		}
		outTok := c.tokens.Output + c.tokens.Reasoning
		genMs := c.latencyMs - c.ttftMs
		if c.ttftMs == 0 || genMs <= 0 {
			genMs = c.latencyMs
		}
		if outTok > 0 && genMs > 0 {
			tps = append(tps, float64(outTok)*1000.0/float64(genMs))
		}
	}
	stats := func(p float64) Stats {
		return Stats{
			Input: percentile(input, p), UncachedInput: percentile(uncached, p), CacheRead: percentile(cacheRead, p),
			CacheWrite: percentile(cacheWrite, p), Output: percentile(output, p), Reasoning: percentile(reasoning, p),
			OutputTotal: percentile(outputTotal, p), CostUSD: percentile(costUSD, p),
			LatencyMs: percentile(latency, p), TTFTMs: percentile(ttft, p), TokensPerSec: percentile(tps, p),
		}
	}
	var sum Tokens
	for _, c := range calls {
		sum.add(c.tokens)
	}
	return DriftPoint{
		T: t, Key: key, Requests: int64(len(calls)),
		Median: stats(0.5), P90: stats(0.9), CacheHitRate: cacheHitRate(sum),
	}
}

// markers returns the first local bucket each value of mk appears in the range.
// Values already seen before From get no marker, and neither does the first day
// the plugin recorded anything, since every value is new on that day.
func markers(db *sql.DB, f Filter, mk markerSpec, size int64) ([]Marker, error) {
	first, err := dataSince(db)
	if err != nil {
		return nil, fmt.Errorf("markers: %w", err)
	}
	firstDay := f.bucketStart(first, dayMillis)
	where, args := f.where(false)
	out := []Marker{}
	q := "SELECT " + mk.expr + ", MIN(requested_at) FROM requests WHERE " + where +
		" AND generate = 1 AND NOT (" + mk.skip + ") GROUP BY 1 HAVING MIN(requested_at) >= ?"
	rows, err := db.Query(q, append(args, f.From.UnixMilli())...)
	if err != nil {
		return nil, fmt.Errorf("markers %s: %w", mk.kind, err)
	}
	for rows.Next() {
		var m Marker
		var first int64
		if err := rows.Scan(&m.Value, &first); err != nil {
			rows.Close()
			return nil, fmt.Errorf("markers %s scan: %w", mk.kind, err)
		}
		if f.bucketStart(first, dayMillis) == firstDay {
			continue
		}
		m.Kind, m.T = mk.kind, f.bucketStart(first, size)
		out = append(out, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, fmt.Errorf("markers %s rows: %w", mk.kind, err)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.T != b.T {
			return a.T < b.T
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Value < b.Value
	})
	return out, nil
}

// inbound is one request (one upstream call) within one group.
type inbound struct {
	key       string
	at        int64
	tokens    Tokens
	costUSD   float64
	latencyMs int64
	ttftMs    int64
}

// perRequestPriced sums successful generating rows per upstream call and group,
// pricing each model/tier slice with table so split-billing rows of one call
// combine into one per-request cost.
func perRequestPriced(db *sql.DB, f Filter, table *pricing.Table, keyExpr string) ([]inbound, error) {
	if table == nil {
		return perRequest(db, f, keyExpr)
	}
	where, args := f.where(true)
	q := "SELECT " + callExpr + ", CAST(" + keyExpr + " AS TEXT), (" + modelExpr + "), (" + tierExpr + "), MIN(requested_at), MAX(latency_ms), MAX(ttft_ms), " + tokenSums +
		" FROM requests WHERE " + where + " AND generate = 1 AND failed = 0 GROUP BY 1, 2, 3, 4"
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("per request priced: %w", err)
	}
	defer rows.Close()
	type callKey struct{ call, key string }
	p := newPricer(table)
	byCall := map[callKey]*inbound{}
	var order []callKey
	for rows.Next() {
		var (
			call, key, model, tier string
			at, latency, ttft      int64
			tok                    Tokens
		)
		targets := append([]any{&call, &key, &model, &tier, &at, &latency, &ttft}, tok.scanTargets()...)
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("per request priced scan: %w", err)
		}
		ck := callKey{call, key}
		in := byCall[ck]
		if in == nil {
			in = &inbound{key: key, at: at, latencyMs: latency, ttftMs: ttft}
			byCall[ck] = in
			order = append(order, ck)
		} else {
			if at < in.at {
				in.at = at
			}
			if latency > in.latencyMs {
				in.latencyMs = latency
			}
			if ttft > in.ttftMs {
				in.ttftMs = ttft
			}
		}
		in.tokens.add(tok)
		cost, _ := p.price(model, tier, tok.input(), tok)
		in.costUSD += cost
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("per request priced rows: %w", err)
	}
	out := make([]inbound, 0, len(order))
	for _, ck := range order {
		out = append(out, *byCall[ck])
	}
	return out, nil
}

// perRequest sums the successful generating rows per request and group. A
// request is one upstream call (see callExpr): one per HTTP request once
// failed attempts are left out, and one per WebSocket turn. Split billing rows
// of a call that land in two model groups count once in each. Failed attempts
// are left out so an outage does not read as a drop in tokens per request.
// MAX keeps one timing per call when its billing rows repeat it.
func perRequest(db *sql.DB, f Filter, keyExpr string) ([]inbound, error) {
	where, args := f.where(true)
	q := "SELECT CAST(" + keyExpr + " AS TEXT), MIN(requested_at), MAX(latency_ms), MAX(ttft_ms), " + tokenSums +
		" FROM requests WHERE " + where + " AND generate = 1 AND failed = 0 GROUP BY " + callExpr + ", 1"
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("per request: %w", err)
	}
	defer rows.Close()
	var out []inbound
	for rows.Next() {
		var in inbound
		if err := rows.Scan(append([]any{&in.key, &in.at, &in.latencyMs, &in.ttftMs}, in.tokens.scanTargets()...)...); err != nil {
			return nil, fmt.Errorf("per request scan: %w", err)
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("per request rows: %w", err)
	}
	return out, nil
}

// percentile interpolates linearly between the closest ranks.
func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	rank := p * float64(len(sorted)-1)
	lo, hi := int(math.Floor(rank)), int(math.Ceil(rank))
	return sorted[lo] + (sorted[hi]-sorted[lo])*(rank-float64(lo))
}
