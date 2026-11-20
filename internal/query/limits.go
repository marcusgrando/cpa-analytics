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

func rejectPartialAccountFilters(f Filter) error {
	for param, value := range map[string]string{"model": f.Model, "client": f.Client, "api_key": f.APIKeyHash} {
		if value != "" {
			return &ParamError{Param: param, Reason: "the weekly limit covers the whole account, so it can only be filtered by provider"}
		}
	}
	return nil
}

// weeklyCycles walks every account's rows from since to f.To in time order. A
// reading whose reset moved by more than resetJitterMillis starts a new cycle.
// Within a cycle, a pair whose percent went down, as when a Claude reset
// refills it, is dropped and the next pair starts from the lower reading.
func weeklyCycles(db *sql.DB, f Filter, table *pricing.Table, since int64) ([]weeklyCycle, error) {
	where, args := f.where(false)
	rows, err := db.Query(`SELECT auth_index, provider, requested_at, (`+modelExpr+`), (`+tierExpr+`), `+
		`uncached_input, cache_read, cache_write, output_non_reasoning, output_reasoning, unclassified, limit_weekly_pct, limit_weekly_reset
		FROM requests WHERE auth_index <> '' AND requested_at >= ? AND `+where+` ORDER BY auth_index, requested_at, id`, append([]any{since}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("weekly cycles: %w", err)
	}
	defer rows.Close()

	p := newPricer(table)
	var (
		cycles   []weeklyCycle
		cur      *weeklyCycle
		spanCost float64
		spanUnpr int64
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
		if cur != nil && acct != cur.account {
			flush()
		}
		cost, quality := p.price(model, tier, tokens.input(), tokens)
		spanCost += cost
		spanUnpr += quality.UnpricedTokens
		if !pct.Valid || !reset.Valid {
			continue
		}
		switch {
		case cur != nil && abs(reset.Int64-cur.reset) <= resetJitterMillis:
			if pct.Float64 >= cur.lastPct {
				cur.pairs = append(cur.pairs, weeklyPair{at: at, pct: pct.Float64 - cur.lastPct, cost: spanCost, unpriced: spanUnpr})
			}
			cur.reset, cur.lastPct, cur.lastAt = reset.Int64, pct.Float64, at
		default:
			if cur != nil {
				cur.nextAt = at
				flush()
			}
			cur = &weeklyCycle{account: acct, provider: provider, reset: reset.Int64, lastPct: pct.Float64, lastAt: at}
		}
		spanCost, spanUnpr = 0, 0
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
	cycles, err := weeklyCycles(db, f, table, f.From.UnixMilli()-cycleLookbackMillis)
	if err != nil {
		return LeftoverResult{}, err
	}
	res := LeftoverResult{Cycles: []LeftoverCycle{}}
	for _, c := range cycles {
		end, early := c.reset, false
		if c.nextAt > 0 && c.nextAt < c.reset {
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
