package query

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
)

// Stats holds one value per token bucket and per timing, for a median or a p90.
type Stats struct {
	UncachedInput float64 `json:"uncached_input"`
	CacheRead     float64 `json:"cache_read"`
	CacheWrite    float64 `json:"cache_write"`
	Output        float64 `json:"output"`
	Reasoning     float64 `json:"reasoning"`
	LatencyMs     float64 `json:"latency_ms"`
	// TTFTMs covers only calls that reported a first token (streamed ones).
	TTFTMs float64 `json:"ttft_ms"`
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
	Points  []DriftPoint `json:"points"`
	Markers []Marker     `json:"markers"`
}

var driftGroups = map[string]string{"response_model": modelExpr, "client_version": clientExpr, "reasoning_effort": effortExpr}

// markerKinds are always reported, whatever the grouping. Rows that missed
// the User-Agent join have no client and never produce a client marker.
var markerKinds = []struct{ kind, expr, skip string }{
	{"client_version", clientExpr, "client_name = ''"},
	{"response_model", modelExpr, "(" + modelExpr + ") = ''"},
	{"reasoning_effort", effortExpr, "reasoning_effort = ''"},
}

// Drift returns daily medians and p90 per request (see perRequest), grouped
// by the reported model, the client version or the reasoning effort, plus
// change markers.
func Drift(db *sql.DB, f Filter, group string) (DriftResult, error) {
	groupExpr, ok := driftGroups[group]
	if !ok {
		return DriftResult{}, &ParamError{Param: "group", Reason: "must be response_model, client_version or reasoning_effort"}
	}
	reqs, err := perRequest(db, f, groupExpr)
	if err != nil {
		return DriftResult{}, err
	}
	type dayKey struct {
		t   int64
		key string
	}
	buckets := map[dayKey][]inbound{}
	for _, rq := range reqs {
		k := dayKey{f.bucketStart(rq.at, dayMillis), rq.key}
		buckets[k] = append(buckets[k], rq)
	}
	res := DriftResult{Group: group, Points: make([]DriftPoint, 0, len(buckets))}
	for k, calls := range buckets {
		res.Points = append(res.Points, driftPoint(k.t, k.key, calls))
	}
	sort.Slice(res.Points, func(i, j int) bool {
		if res.Points[i].T != res.Points[j].T {
			return res.Points[i].T < res.Points[j].T
		}
		return res.Points[i].Key < res.Points[j].Key
	})
	if res.Markers, err = markers(db, f); err != nil {
		return DriftResult{}, err
	}
	return res, nil
}

func driftPoint(t int64, key string, calls []inbound) DriftPoint {
	columns := func(pick func(inbound) int64) []float64 {
		out := make([]float64, len(calls))
		for i, c := range calls {
			out[i] = float64(pick(c))
		}
		return out
	}
	uncached := columns(func(c inbound) int64 { return c.tokens.UncachedInput })
	cacheRead := columns(func(c inbound) int64 { return c.tokens.CacheRead })
	cacheWrite := columns(func(c inbound) int64 { return c.tokens.CacheWrite })
	output := columns(func(c inbound) int64 { return c.tokens.Output })
	reasoning := columns(func(c inbound) int64 { return c.tokens.Reasoning })
	latency := columns(func(c inbound) int64 { return c.latencyMs })
	var ttft []float64
	for _, c := range calls {
		if c.ttftMs > 0 {
			ttft = append(ttft, float64(c.ttftMs))
		}
	}
	stats := func(p float64) Stats {
		return Stats{
			UncachedInput: percentile(uncached, p), CacheRead: percentile(cacheRead, p),
			CacheWrite: percentile(cacheWrite, p), Output: percentile(output, p), Reasoning: percentile(reasoning, p),
			LatencyMs: percentile(latency, p), TTFTMs: percentile(ttft, p),
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

// markers returns the first local day each client version, reported model
// and reasoning effort appears in the range. Values already seen before From get no marker.
func markers(db *sql.DB, f Filter) ([]Marker, error) {
	where, args := f.where(false)
	out := []Marker{}
	for _, mk := range markerKinds {
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
			m.Kind, m.T = mk.kind, f.bucketStart(first, dayMillis)
			out = append(out, m)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("markers %s rows: %w", mk.kind, err)
		}
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
	latencyMs int64
	ttftMs    int64
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
