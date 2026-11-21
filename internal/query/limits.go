package query

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/marcusgrando/cpa-analytics/internal/pricing"
)

// BurnPoint is what one account spent, at API prices, per point of its weekly
// subscription limit on one day. With the same work, a rising USDPerPct means
// the limit got larger, a falling one that it got smaller.
type BurnPoint struct {
	T         int64   `json:"t"`
	AuthIndex string  `json:"auth_index"`
	Provider  string  `json:"provider"`
	WeeklyPct float64 `json:"weekly_pct"`
	CostUSD   float64 `json:"cost_usd"`
	USDPerPct float64 `json:"usd_per_pct"`
	Tokens
	// UnpricedTokens were spent in the measured spans but have no price, so
	// CostUSD understates the spend.
	UnpricedTokens int64 `json:"unpriced_tokens"`
	// LowSample flags days whose weekly percent moved less than lowSampleBurnPct,
	// where integer header rounding makes USDPerPct noisy.
	LowSample bool `json:"low_sample,omitempty"`
}

// BurnResult lists burn points by day, then account.
type BurnResult struct {
	Points []BurnPoint `json:"points"`
}

// LeftoverCycle is one weekly cycle that ended, with the part of the limit it
// left unused.
// LeftUSD prices the unused points at what the cycle's own traffic paid per
// point, and is 0 when the cycle moved too little to price a point.
type LeftoverCycle struct {
	AuthIndex string `json:"auth_index"`
	Provider  string `json:"provider"`
	EndedAt   int64  `json:"ended_at"`
	// Early is set when a new cycle began before the reset, as when a banked
	// Codex reset is redeemed. EndedAt is then the first reading of the new
	// cycle, so the real end lies between LastReadingAt and EndedAt.
	Early         bool    `json:"early"`
	LastReadingAt int64   `json:"last_reading_at"`
	UsedPct       float64 `json:"used_pct"`
	LeftPct       float64 `json:"left_pct"`
	USDPerPct     float64 `json:"usd_per_pct"`
	LeftUSD       float64 `json:"left_usd"`
}

// LeftoverResult lists the cycles that ended in the range, oldest first.
type LeftoverResult struct {
	Cycles []LeftoverCycle `json:"cycles"`
}

const (
	// minBurnPct leaves out days whose weekly percent moved too little to
	// give a stable ratio.
	minBurnPct = 1.0
	// lowSampleBurnPct marks days with small integer-percent movement as noisy.
	lowSampleBurnPct = 3.0
	// resetJitterMillis tolerates resets a provider recomputes per response.
	resetJitterMillis = int64(60 * 60 * 1000)
	// cycleLookbackMillis reaches the start of a weekly cycle that ends at the
	// start of the range.
	cycleLookbackMillis = 8 * dayMillis
)

// weeklyPair is the move between two consecutive readings of one cycle: the
// points gained and what the rows after the first reading, up to the second,
// cost. at is the second reading.
type weeklyPair struct {
	at       int64
	pct      float64
	cost     float64
	unpriced int64
	tokens   Tokens
}

// weeklyCycle is one account's weekly cycle as its readings saw it. nextAt is
// the first reading of the following cycle, 0 when none was seen.
type weeklyCycle struct {
	account, provider string
	reset             int64
	lastPct           float64
	lastAt            int64
	nextAt            int64
	pairs             []weeklyPair
}

func (c weeklyCycle) endedEarly() bool {
	return c.nextAt > 0 && c.nextAt < c.reset
}

func withCycleLookback(f Filter) Filter {
	f.From = f.From.Add(-time.Duration(cycleLookbackMillis) * time.Millisecond)
	return f
}

func rejectPartialAccountFilters(f Filter) error {
	for param, value := range map[string]string{"model": f.Model, "client": f.Client, "api_key": f.APIKeyHash} {
		if value != "" {
			return &ParamError{Param: param, Reason: "the weekly limit covers the whole account, so it can only be filtered by provider"}
		}
	}
	return nil
}

// weeklyCycles walks each account's rows in time order. Only pairs with both
// readings at or after measureFrom contribute tokens, cost and quota movement. A
// reading whose reset moved by more than resetJitterMillis starts a new cycle.
// Within a cycle, a pair whose percent went down, as when a Claude reset
// refills it, is dropped and the next pair starts from the lower reading.
func weeklyCycles(db *sql.DB, f Filter, table *pricing.Table, measureFrom int64) ([]weeklyCycle, error) {
	where, args := f.where(true)
	if f.From.UnixMilli() < measureFrom {
		// Only the last complete reading before the period can establish a reset at its start.
		where += " AND (requested_at >= ? OR (limit_weekly_pct IS NOT NULL AND limit_weekly_reset IS NOT NULL))"
		args = append(args, measureFrom)
	}
	rows, err := db.Query(`SELECT auth_index, provider, requested_at, (`+modelExpr+`), (`+tierExpr+`), `+
		`uncached_input, cache_read, cache_write, output_non_reasoning, output_reasoning, unclassified, limit_weekly_pct, limit_weekly_reset
		FROM requests WHERE auth_index <> '' AND `+where+` ORDER BY auth_index, provider, requested_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("weekly cycles: %w", err)
	}
	defer rows.Close()

	p := newPricer(table)
	var (
		cycles     []weeklyCycle
		cur        *weeklyCycle
		spanCost   float64
		spanUnpr   int64
		spanTokens Tokens
	)
	flush := func() {
		if cur != nil {
			cycles = append(cycles, *cur)
			cur = nil
		}
	}
	for rows.Next() {
		var (
			acct, provider, model, tier string
			at                          int64
			tokens                      Tokens
			pct                         sql.NullFloat64
			reset                       sql.NullInt64
		)
		targets := append([]any{&acct, &provider, &at, &model, &tier}, tokens.scanTargets()...)
		if err := rows.Scan(append(targets, &pct, &reset)...); err != nil {
			return nil, fmt.Errorf("weekly cycles scan: %w", err)
		}
		if cur != nil && (acct != cur.account || provider != cur.provider) {
			flush()
			spanCost, spanUnpr, spanTokens = 0, 0, Tokens{}
		}
		if at < measureFrom {
			cur = &weeklyCycle{account: acct, provider: provider, reset: reset.Int64, lastPct: pct.Float64, lastAt: at}
			continue
		}
		cost, quality := p.price(model, tier, tokens.input(), tokens)
		spanCost += cost
		spanUnpr += quality.UnpricedTokens
		spanTokens.add(tokens)
		if !pct.Valid || !reset.Valid {
			continue
		}
		switch {
		case cur != nil && abs(reset.Int64-cur.reset) <= resetJitterMillis:
			if cur.lastAt >= measureFrom && pct.Float64 >= cur.lastPct {
				cur.pairs = append(cur.pairs, weeklyPair{at: at, pct: pct.Float64 - cur.lastPct, cost: spanCost, unpriced: spanUnpr, tokens: spanTokens})
			}
			cur.reset, cur.lastPct, cur.lastAt = reset.Int64, pct.Float64, at
		default:
			if cur != nil {
				cur.nextAt = at
				flush()
			}
			cur = &weeklyCycle{account: acct, provider: provider, reset: reset.Int64, lastPct: pct.Float64, lastAt: at}
		}
		spanCost, spanUnpr, spanTokens = 0, 0, Tokens{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("weekly cycles rows: %w", err)
	}
	flush()
	return cycles, nil
}

// LimitBurn prices each point of every account's weekly limit. Each pair of
// readings within one cycle counts on the day of its second reading.
func LimitBurn(db *sql.DB, f Filter, table *pricing.Table) (BurnResult, error) {
	if err := rejectPartialAccountFilters(f); err != nil {
		return BurnResult{}, err
	}
	cycles, err := weeklyCycles(db, f, table, f.From.UnixMilli())
	if err != nil {
		return BurnResult{}, err
	}
	type key struct {
		day     int64
		account string
	}
	byDay := map[key]*BurnPoint{}
	for _, c := range cycles {
		for _, pair := range c.pairs {
			k := key{f.bucketStart(pair.at, dayMillis), c.account}
			bp := byDay[k]
			if bp == nil {
				bp = &BurnPoint{T: k.day, AuthIndex: c.account, Provider: c.provider}
				byDay[k] = bp
			}
			bp.WeeklyPct += pair.pct
			bp.CostUSD += pair.cost
			bp.Tokens.add(pair.tokens)
			bp.UnpricedTokens += pair.unpriced
		}
	}

	res := BurnResult{Points: []BurnPoint{}}
	for _, bp := range byDay {
		if bp.WeeklyPct < minBurnPct || bp.CostUSD <= 0 {
			continue
		}
		bp.USDPerPct = bp.CostUSD / bp.WeeklyPct
		bp.LowSample = bp.WeeklyPct < lowSampleBurnPct
		res.Points = append(res.Points, *bp)
	}
	sort.Slice(res.Points, func(i, j int) bool {
		a, b := res.Points[i], res.Points[j]
		if a.T != b.T {
			return a.T < b.T
		}
		return a.AuthIndex < b.AuthIndex
	})
	return res, nil
}

// LimitLeftover lists the weekly cycles that ended in the range with what
// each left unused. A cycle ends at its reset, or earlier when a reading of a
// new cycle arrives first. A cycle whose reset is still ahead of now has not
// ended.
func LimitLeftover(db *sql.DB, f Filter, table *pricing.Table, now time.Time) (LeftoverResult, error) {
	if err := rejectPartialAccountFilters(f); err != nil {
		return LeftoverResult{}, err
	}
	history := withCycleLookback(f)
	cycles, err := weeklyCycles(db, history, table, history.From.UnixMilli())
	if err != nil {
		return LeftoverResult{}, err
	}
	res := LeftoverResult{Cycles: []LeftoverCycle{}}
	for _, c := range cycles {
		end, early := c.reset, false
		if c.endedEarly() {
			end, early = c.nextAt, true
		}
		if end > now.UnixMilli() || end < f.From.UnixMilli() || end >= f.To.UnixMilli() {
			continue
		}
		lc := LeftoverCycle{
			AuthIndex: c.account, Provider: c.provider, EndedAt: end, Early: early, LastReadingAt: c.lastAt,
			UsedPct: math.Min(c.lastPct, 100), LeftPct: math.Max(0, 100-c.lastPct),
		}
		var moved, cost float64
		for _, pair := range c.pairs {
			moved += pair.pct
			cost += pair.cost
		}
		if moved >= minBurnPct && cost > 0 {
			lc.USDPerPct = cost / moved
			lc.LeftUSD = lc.LeftPct * lc.USDPerPct
		}
		res.Cycles = append(res.Cycles, lc)
	}
	sort.Slice(res.Cycles, func(i, j int) bool {
		a, b := res.Cycles[i], res.Cycles[j]
		if a.EndedAt != b.EndedAt {
			return a.EndedAt < b.EndedAt
		}
		return a.AuthIndex < b.AuthIndex
	})
	return res, nil
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// HistoryPoint is one reading of an account's weekly limit. WeeklyReset is 0
// when the response did not give a readable reset.
type HistoryPoint struct {
	T           int64   `json:"t"`
	WeeklyPct   float64 `json:"weekly_pct"`
	WeeklyReset int64   `json:"weekly_reset"`
}

// HistoryAccount is one account's readings, oldest first. The first point is
// the last reading before the range when there is one.
type HistoryAccount struct {
	AuthIndex string         `json:"auth_index"`
	Provider  string         `json:"provider"`
	Points    []HistoryPoint `json:"points"`
}

// HistoryResult lists every account that reported its weekly limit.
type HistoryResult struct {
	BucketMs int64            `json:"bucket_ms"`
	Accounts []HistoryAccount `json:"accounts"`
}

const (
	minHistoryBucketMillis = 15 * 60 * 1000
	// maxHistoryBuckets bounds the points per account on long ranges.
	maxHistoryBuckets = 700
)

// LimitHistory returns each account's weekly limit readings, the last one per
// bucket, plus the last reading before the range within a cycle's reach. The
// page pools them with the plan weights it reads live.
func LimitHistory(db *sql.DB, f Filter) (HistoryResult, error) {
	if err := rejectPartialAccountFilters(f); err != nil {
		return HistoryResult{}, err
	}
	bucket := int64(minHistoryBucketMillis)
	if span := f.To.Sub(f.From).Milliseconds(); span/maxHistoryBuckets > bucket {
		bucket = (span/maxHistoryBuckets/60000 + 1) * 60000
	}
	where, args := f.where(false)
	rows, err := db.Query(`SELECT auth_index, provider, requested_at, limit_weekly_pct, COALESCE(limit_weekly_reset, 0)
		FROM requests WHERE auth_index <> '' AND limit_weekly_pct IS NOT NULL AND requested_at >= ? AND `+where+`
		ORDER BY auth_index, requested_at, id`, append([]any{f.From.UnixMilli() - cycleLookbackMillis}, args...)...)
	if err != nil {
		return HistoryResult{}, fmt.Errorf("limit history: %w", err)
	}
	defer rows.Close()

	res := HistoryResult{BucketMs: bucket, Accounts: []HistoryAccount{}}
	from := f.From.UnixMilli()
	var cur *HistoryAccount
	// beforeRange is the bucket of every reading before the range, so only the
	// last of them stays. bucketStart never returns it.
	const beforeRange = math.MinInt64 + 1
	lastBucket := int64(math.MinInt64)
	for rows.Next() {
		var (
			acct, provider string
			pt             HistoryPoint
		)
		if err := rows.Scan(&acct, &provider, &pt.T, &pt.WeeklyPct, &pt.WeeklyReset); err != nil {
			return HistoryResult{}, fmt.Errorf("limit history scan: %w", err)
		}
		if cur == nil || cur.AuthIndex != acct {
			res.Accounts = append(res.Accounts, HistoryAccount{AuthIndex: acct, Provider: provider})
			cur = &res.Accounts[len(res.Accounts)-1]
			lastBucket = math.MinInt64
		}
		b := int64(beforeRange)
		if pt.T >= from {
			b = f.bucketStart(pt.T, bucket)
		}
		if b == lastBucket {
			cur.Points[len(cur.Points)-1] = pt
			continue
		}
		cur.Points = append(cur.Points, pt)
		lastBucket = b
	}
	if err := rows.Err(); err != nil {
		return HistoryResult{}, fmt.Errorf("limit history rows: %w", err)
	}
	return res, nil
}

// AccountValue summarizes one account's realized usage over the selected range
// and, when the provider reports weekly quota headers, its measured quota burn
// across the whole range.
type AccountValue struct {
	AuthIndex string `json:"auth_index"`
	Provider  string `json:"provider"`
	Requests  int64  `json:"requests"`
	Failed    int64  `json:"failed"`
	Tokens
	CostUSD                float64 `json:"cost_usd"`
	UnpricedTokens         int64   `json:"unpriced_tokens"`
	CacheHitRate           float64 `json:"cache_hit_rate"`
	TopModel               string  `json:"top_model,omitempty"`
	TopModelShare          float64 `json:"top_model_share,omitempty"`
	EarlyResets            int     `json:"early_resets"`
	QuotaMeasured          bool    `json:"quota_measured"`
	WeeklyPct              float64 `json:"weekly_pct"`
	MeasuredCostUSD        float64 `json:"measured_cost_usd"`
	MeasuredUnpricedTokens int64   `json:"measured_unpriced_tokens"`
	MeasuredTokens         Tokens  `json:"measured_tokens"`
	LowSample              bool    `json:"low_sample,omitempty"`
}

// PlanValueResult answers the plan-value route.
type PlanValueResult struct {
	DataSince int64          `json:"data_since"`
	Accounts  []AccountValue `json:"accounts"`
}

// PlanValue returns per-account realized usage and whole-range weekly quota
// burn so the dashboard can rank plans by potential and realized cost-benefit.
func PlanValue(db *sql.DB, f Filter, table *pricing.Table) (PlanValueResult, error) {
	if err := rejectPartialAccountFilters(f); err != nil {
		return PlanValueResult{}, err
	}
	since, err := dataSince(db)
	if err != nil {
		return PlanValueResult{}, err
	}
	rows, err := aggregate(db, f, table, "auth_index", "provider")
	if err != nil {
		return PlanValueResult{}, err
	}
	type acctKey struct {
		authIndex string
		provider  string
	}
	type modelMix struct {
		cost   float64
		tokens int64
	}
	byAcct := map[acctKey]*AccountValue{}
	modelsByAcct := map[acctKey]map[string]*modelMix{}
	for _, r := range rows {
		if r.keys[0] == "" {
			continue
		}
		k := acctKey{r.keys[0], r.keys[1]}
		av := byAcct[k]
		if av == nil {
			av = &AccountValue{AuthIndex: k.authIndex, Provider: k.provider}
			byAcct[k] = av
			modelsByAcct[k] = map[string]*modelMix{}
		}
		av.Failed += r.failed
		av.Tokens.add(r.tokens)
		av.CostUSD += r.cost
		av.UnpricedTokens += r.quality.UnpricedTokens
		mm := modelsByAcct[k][r.model]
		if mm == nil {
			mm = &modelMix{}
			modelsByAcct[k][r.model] = mm
		}
		mm.cost += r.cost
		mm.tokens += r.tokens.total()
	}

	where, args := f.where(true)
	reqRows, err := db.Query(`SELECT auth_index, provider, COUNT(DISTINCT CASE WHEN generate = 1 AND failed = 0 THEN `+callExpr+` END)
		FROM requests WHERE auth_index <> '' AND `+where+` GROUP BY auth_index, provider`, args...)
	if err != nil {
		return PlanValueResult{}, fmt.Errorf("plan value requests: %w", err)
	}
	defer reqRows.Close()
	for reqRows.Next() {
		var authIdx, prov string
		var reqs int64
		if err := reqRows.Scan(&authIdx, &prov, &reqs); err != nil {
			return PlanValueResult{}, fmt.Errorf("plan value requests scan: %w", err)
		}
		if av := byAcct[acctKey{authIdx, prov}]; av != nil {
			av.Requests = reqs
		}
	}
	if err := reqRows.Err(); err != nil {
		return PlanValueResult{}, fmt.Errorf("plan value requests rows: %w", err)
	}

	cycles, err := weeklyCycles(db, withCycleLookback(f), table, f.From.UnixMilli())
	if err != nil {
		return PlanValueResult{}, err
	}
	for _, c := range cycles {
		k := acctKey{c.account, c.provider}
		av := byAcct[k]
		if av == nil {
			continue // Only seen before the selected period.
		}
		if c.endedEarly() {
			av.EarlyResets++
		}
		for _, pair := range c.pairs {
			av.QuotaMeasured = true
			av.WeeklyPct += pair.pct
			av.MeasuredCostUSD += pair.cost
			av.MeasuredUnpricedTokens += pair.unpriced
			av.MeasuredTokens.add(pair.tokens)
		}
	}

	res := PlanValueResult{DataSince: since, Accounts: make([]AccountValue, 0, len(byAcct))}
	for k, av := range byAcct {
		av.CacheHitRate = cacheHitRate(av.Tokens)
		if av.WeeklyPct > 0 && av.WeeklyPct < lowSampleBurnPct {
			av.LowSample = true
		}
		if mix := modelsByAcct[k]; len(mix) > 0 {
			useCost := av.CostUSD > 0
			var bestModel string
			var bestScore float64
			for model, mm := range mix {
				score := float64(mm.tokens)
				if useCost {
					score = mm.cost
				}
				if bestModel == "" || score > bestScore || (score == bestScore && model < bestModel) {
					bestModel = model
					bestScore = score
				}
			}
			av.TopModel = bestModel
			if useCost {
				av.TopModelShare = ratio(bestScore, av.CostUSD)
			} else {
				av.TopModelShare = ratio(bestScore, float64(av.Tokens.total()))
			}
		}
		res.Accounts = append(res.Accounts, *av)
	}
	sort.Slice(res.Accounts, func(i, j int) bool {
		a, b := res.Accounts[i], res.Accounts[j]
		if a.CostUSD != b.CostUSD {
			return a.CostUSD > b.CostUSD
		}
		if a.AuthIndex != b.AuthIndex {
			return a.AuthIndex < b.AuthIndex
		}
		return a.Provider < b.Provider
	})
	return res, nil
}
