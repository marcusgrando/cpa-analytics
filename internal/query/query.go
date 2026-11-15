package query

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/marcusgrando/cpa-analytics/internal/pricing"
)

// Totals sums every row in the range. Retries and split-billing records are
// all counted, since each one consumed the tokens it reports.
type Totals struct {
	Tokens
	CostUSD float64 `json:"cost_usd"`
	// Records counts rows. Requests counts successful upstream calls (see callExpr).
	Records         int64            `json:"records"`
	Requests        int64            `json:"requests"`
	Sessions        int64            `json:"sessions"`
	Failed          int64            `json:"failed"`
	ProcessedTokens int64            `json:"processed_tokens"`
	CacheHitRate    float64          `json:"cache_hit_rate"`
	CostQuality     CostQuality      `json:"cost_quality"`
	CostByType      pricing.TypeCost `json:"cost_by_type"`
	// CacheSavingsUSD is what the cache reads would have cost as uncached input.
	CacheSavingsUSD float64    `json:"cache_savings_usd"`
	CostByTier      []TierCost `json:"cost_by_tier"`
}

// TierCost is the cost of one service tier, as the upstream reported it or
// CPA sent it. An empty tier was never reported. StandardCostUSD prices the
// same tokens at the standard tier, so the difference is the priority premium
// or the flex discount.
type TierCost struct {
	Tier            string  `json:"tier"`
	CostUSD         float64 `json:"cost_usd"`
	StandardCostUSD float64 `json:"standard_cost_usd"`
	Tokens          int64   `json:"tokens"`
}

// ProviderShare is one provider's part of the totals.
type ProviderShare struct {
	Provider       string  `json:"provider"`
	CostUSD        float64 `json:"cost_usd"`
	CostShare      float64 `json:"cost_share"`
	Tokens         int64   `json:"tokens"`
	UnpricedTokens int64   `json:"unpriced_tokens"`
	Sessions       int64   `json:"sessions"`
}

// SummaryResult answers the summary route.
type SummaryResult struct {
	Totals    Totals          `json:"totals"`
	Providers []ProviderShare `json:"providers"`
}

// Summary returns totals and the per-provider cost share.
func Summary(db *sql.DB, f Filter, table *pricing.Table) (SummaryResult, error) {
	rows, err := aggregate(db, f, table, "provider")
	if err != nil {
		return SummaryResult{}, err
	}
	var res SummaryResult
	byProvider := map[string]*ProviderShare{}
	byTier := map[string]*TierCost{}
	for _, r := range rows {
		res.Totals.Tokens.add(r.tokens)
		res.Totals.CostUSD += r.cost
		res.Totals.Records += r.records
		res.Totals.CostQuality.add(r.quality)
		res.Totals.CostByType.Add(r.byType)
		res.Totals.CacheSavingsUSD += r.cacheSavings
		tc := byTier[r.tier]
		if tc == nil {
			tc = &TierCost{Tier: r.tier}
			byTier[r.tier] = tc
		}
		tc.CostUSD += r.cost
		tc.StandardCostUSD += r.standardCost
		tc.Tokens += r.tokens.total()
		ps := byProvider[r.keys[0]]
		if ps == nil {
			ps = &ProviderShare{Provider: r.keys[0]}
			byProvider[r.keys[0]] = ps
		}
		ps.CostUSD += r.cost
		ps.Tokens += r.tokens.total()
		ps.UnpricedTokens += r.quality.UnpricedTokens
	}
	res.Totals.ProcessedTokens = res.Totals.Tokens.total()
	res.Totals.CacheHitRate = cacheHitRate(res.Totals.Tokens)
	res.Totals.CostByTier = make([]TierCost, 0, len(byTier))
	for _, tc := range byTier {
		res.Totals.CostByTier = append(res.Totals.CostByTier, *tc)
	}
	sort.Slice(res.Totals.CostByTier, func(i, j int) bool {
		a, b := res.Totals.CostByTier[i], res.Totals.CostByTier[j]
		if a.CostUSD != b.CostUSD {
			return a.CostUSD > b.CostUSD
		}
		if a.Tokens != b.Tokens {
			return a.Tokens > b.Tokens
		}
		return a.Tier < b.Tier
	})
	res.Providers = make([]ProviderShare, 0, len(byProvider))
	for _, ps := range byProvider {
		ps.CostShare = ratio(ps.CostUSD, res.Totals.CostUSD)
		res.Providers = append(res.Providers, *ps)
	}
	sort.Slice(res.Providers, func(i, j int) bool {
		a, b := res.Providers[i], res.Providers[j]
		if a.CostUSD != b.CostUSD {
			return a.CostUSD > b.CostUSD
		}
		if a.Tokens != b.Tokens {
			return a.Tokens > b.Tokens
		}
		return a.Provider < b.Provider
	})

	where, args := f.where(true)
	err = db.QueryRow(`SELECT COUNT(DISTINCT CASE WHEN generate = 1 AND failed = 0 THEN `+callExpr+` END),
		COUNT(DISTINCT NULLIF(session_id, '')), COALESCE(SUM(failed), 0)
		FROM requests WHERE `+where, args...).Scan(&res.Totals.Requests, &res.Totals.Sessions, &res.Totals.Failed)
	if err != nil {
		return SummaryResult{}, fmt.Errorf("summary counts: %w", err)
	}
	if err := providerSessions(db, where, args, res.Providers); err != nil {
		return SummaryResult{}, err
	}
	return res, nil
}

// providerSessions fills Sessions on each provider share. A session that
// spans two providers counts once in each.
func providerSessions(db *sql.DB, where string, args []any, providers []ProviderShare) error {
	rows, err := db.Query(`SELECT provider, COUNT(DISTINCT NULLIF(session_id, '')) FROM requests WHERE `+where+` GROUP BY provider`, args...)
	if err != nil {
		return fmt.Errorf("provider sessions: %w", err)
	}
	defer rows.Close()
	sessions := map[string]int64{}
	for rows.Next() {
		var provider string
		var n int64
		if err := rows.Scan(&provider, &n); err != nil {
			return fmt.Errorf("provider sessions scan: %w", err)
		}
		sessions[provider] = n
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("provider sessions rows: %w", err)
	}
	for i := range providers {
		providers[i].Sessions = sessions[providers[i].Provider]
	}
	return nil
}

// Point is one bucket of one group in a time series.
type Point struct {
	// T is the bucket start in unix ms.
	T   int64  `json:"t"`
	Key string `json:"key"`
	Tokens
	Records int64   `json:"records"`
	CostUSD float64 `json:"cost_usd"`
}

// Series answers the timeseries route.
type Series struct {
	Bucket string  `json:"bucket"`
	Group  string  `json:"group"`
	Points []Point `json:"points"`
}

var bucketSizes = map[string]int64{"day": dayMillis, "hour": hourMillis}

var seriesGroups = map[string]string{"provider": "provider", "model": modelExpr, "client": clientExpr}

// Timeseries returns tokens and cost per bucket (day or hour) and group
// (provider, model or client).
func Timeseries(db *sql.DB, f Filter, table *pricing.Table, bucket, group string) (Series, error) {
	size, ok := bucketSizes[bucket]
	if !ok {
		return Series{}, &ParamError{Param: "bucket", Reason: "must be day or hour"}
	}
	groupExpr, ok := seriesGroups[group]
	if !ok {
		return Series{}, &ParamError{Param: "group", Reason: "must be provider, model or client"}
	}
	rows, err := aggregate(db, f, table, f.bucketSQL(size), groupExpr)
	if err != nil {
		return Series{}, err
	}
	type pointKey struct {
		t   int64
		key string
	}
	merged := map[pointKey]*Point{}
	for _, r := range rows {
		t, err := strconv.ParseInt(r.keys[0], 10, 64)
		if err != nil {
			return Series{}, fmt.Errorf("timeseries bucket %q: %w", r.keys[0], err)
		}
		k := pointKey{t, r.keys[1]}
		pt := merged[k]
		if pt == nil {
			pt = &Point{T: t, Key: r.keys[1]}
			merged[k] = pt
		}
		pt.Tokens.add(r.tokens)
		pt.Records += r.records
		pt.CostUSD += r.cost
	}
	s := Series{Bucket: bucket, Group: group, Points: make([]Point, 0, len(merged))}
	for _, pt := range merged {
		s.Points = append(s.Points, *pt)
	}
	sort.Slice(s.Points, func(i, j int) bool {
		if s.Points[i].T != s.Points[j].T {
			return s.Points[i].T < s.Points[j].T
		}
		return s.Points[i].Key < s.Points[j].Key
	})
	return s, nil
}

// BreakdownRow is one model, client version, day, API key hash or reasoning effort.
type BreakdownRow struct {
	Key      string `json:"key"`
	Records  int64  `json:"records"`
	Requests int64  `json:"requests"`
	Tokens
	CostUSD        float64 `json:"cost_usd"`
	CostShare      float64 `json:"cost_share"`
	TokenShare     float64 `json:"token_share"`
	CostPerMillion float64 `json:"cost_per_million_tokens"`
	CacheHitRate   float64 `json:"cache_hit_rate"`
	UnpricedTokens int64   `json:"unpriced_tokens"`
	// Medians are per request (see callExpr), input and output including reasoning.
	MedianInput  float64 `json:"median_input"`
	MedianOutput float64 `json:"median_output"`
}

// BreakdownResult answers the breakdown route.
type BreakdownResult struct {
	By   string         `json:"by"`
	Rows []BreakdownRow `json:"rows"`
}

// Breakdown returns one row per model, client version, day, API key hash or
// reasoning effort.
// Days are sorted by date, the others by cost.
func Breakdown(db *sql.DB, f Filter, table *pricing.Table, by string) (BreakdownResult, error) {
	var keyExpr string
	switch by {
	case "model":
		keyExpr = modelExpr
	case "client":
		keyExpr = clientExpr
	case "day":
		keyExpr = f.bucketSQL(dayMillis)
	case "api_key":
		keyExpr = "api_key_hash"
	case "reasoning_effort":
		keyExpr = effortExpr
	default:
		return BreakdownResult{}, &ParamError{Param: "by", Reason: "must be model, client, day, api_key or reasoning_effort"}
	}
	rows, err := aggregate(db, f, table, keyExpr)
	if err != nil {
		return BreakdownResult{}, err
	}
	reqs, err := perRequest(db, f, keyExpr)
	if err != nil {
		return BreakdownResult{}, err
	}

	byKey := map[string]*BreakdownRow{}
	var totalCost float64
	var totalTokens int64
	for _, r := range rows {
		br := byKey[r.keys[0]]
		if br == nil {
			br = &BreakdownRow{Key: r.keys[0]}
			byKey[r.keys[0]] = br
		}
		br.Records += r.records
		br.Tokens.add(r.tokens)
		br.CostUSD += r.cost
		br.UnpricedTokens += r.quality.UnpricedTokens
		totalCost += r.cost
		totalTokens += r.tokens.total()
	}
	inputs, outputs := map[string][]float64{}, map[string][]float64{}
	for _, rq := range reqs {
		inputs[rq.key] = append(inputs[rq.key], float64(rq.tokens.input()))
		outputs[rq.key] = append(outputs[rq.key], float64(rq.tokens.Output+rq.tokens.Reasoning))
	}

	res := BreakdownResult{By: by, Rows: make([]BreakdownRow, 0, len(byKey))}
	for key, br := range byKey {
		br.Requests = int64(len(inputs[key]))
		br.CostShare = ratio(br.CostUSD, totalCost)
		br.TokenShare = ratio(float64(br.Tokens.total()), float64(totalTokens))
		br.CostPerMillion = ratio(br.CostUSD*1e6, float64(br.Tokens.total()-br.UnpricedTokens))
		br.CacheHitRate = cacheHitRate(br.Tokens)
		br.MedianInput = percentile(inputs[key], 0.5)
		br.MedianOutput = percentile(outputs[key], 0.5)
		if by == "day" {
			ms, err := strconv.ParseInt(key, 10, 64)
			if err != nil {
				return BreakdownResult{}, fmt.Errorf("breakdown day %q: %w", key, err)
			}
			br.Key = time.UnixMilli(ms + f.offsetMillis()).UTC().Format(time.DateOnly)
		}
		res.Rows = append(res.Rows, *br)
	}
	sort.Slice(res.Rows, func(i, j int) bool {
		a, b := res.Rows[i], res.Rows[j]
		if by != "day" {
			if a.CostUSD != b.CostUSD {
				return a.CostUSD > b.CostUSD
			}
			if a.Tokens.total() != b.Tokens.total() {
				return a.Tokens.total() > b.Tokens.total()
			}
		}
		return a.Key < b.Key
	})
	return res, nil
}

// ModelTokens is a model and the tokens it processed in the range.
type ModelTokens struct {
	Model  string `json:"model"`
	Tokens int64  `json:"tokens"`
}

// ModelsReport lists models without a price and models priced by override.
type ModelsReport struct {
	Unpriced   []ModelTokens `json:"unpriced"`
	Overridden []ModelTokens `json:"overridden"`
}

// Models reports which models in the range lack a price or use an override.
func Models(db *sql.DB, f Filter, table *pricing.Table) (ModelsReport, error) {
	rows, err := aggregate(db, f, table)
	if err != nil {
		return ModelsReport{}, err
	}
	unpriced, overridden := map[string]int64{}, map[string]int64{}
	for _, r := range rows {
		switch {
		case !r.price.ok:
			unpriced[r.model] += r.tokens.total()
		case r.price.source == pricing.SourceOverride:
			overridden[r.model] += r.tokens.total()
		}
	}
	return ModelsReport{Unpriced: sortedModels(unpriced), Overridden: sortedModels(overridden)}, nil
}

func sortedModels(m map[string]int64) []ModelTokens {
	out := make([]ModelTokens, 0, len(m))
	for model, tokens := range m {
		out = append(out, ModelTokens{Model: model, Tokens: tokens})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tokens != out[j].Tokens {
			return out[i].Tokens > out[j].Tokens
		}
		return out[i].Model < out[j].Model
	})
	return out
}
