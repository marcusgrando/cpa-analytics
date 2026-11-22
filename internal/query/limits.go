package query

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/marcusgrando/cpa-analytics/internal/pricing"
)

// BurnPoint is what one account spent, at API prices, per point of its weekly
// subscription limit on one day. With the same work, a rising USDPerPct means
// the limit got larger, a falling one that it got smaller.
type BurnPoint struct {
	T          int64   `json:"t"`
	AuthIndex  string  `json:"auth_index"`
	Provider   string  `json:"provider"`
	Group      string  `json:"group,omitempty"`
	GroupLabel string  `json:"group_label,omitempty"`
	WeeklyPct  float64 `json:"weekly_pct"`
	CostUSD    float64 `json:"cost_usd"`
	USDPerPct  float64 `json:"usd_per_pct"`
	Tokens
	// UnpricedTokens were spent in the measured spans but have no price, so
	// CostUSD understates the spend.
	UnpricedTokens int64 `json:"unpriced_tokens"`
	// LowSample flags days whose weekly percent moved less than lowSampleBurnPct,
	// where small deltas make USDPerPct noisy.
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
	AuthIndex  string `json:"auth_index"`
	Provider   string `json:"provider"`
	Group      string `json:"group,omitempty"`
	GroupLabel string `json:"group_label,omitempty"`
	EndedAt    int64  `json:"ended_at"`
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
	// lowSampleBurnPct marks days with small percent movement as noisy.
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
	account, provider  string
	group, groupLabel  string
	reset              int64
	lastPct            float64
	lastAt             int64
	nextAt             int64
	pairs              []weeklyPair
	afterEarlyResetUSD float64
}

func (c weeklyCycle) endedEarly() bool {
	return c.nextAt > 0 && c.nextAt < c.reset
}

func withCycleLookback(f Filter) Filter {
	f.From = f.From.Add(-time.Duration(cycleLookbackMillis) * time.Millisecond)
	return f
}

func rejectPartialAccountFilters(f Filter) error {
	for _, check := range []struct {
		param, value string
	}{
		{"model", f.Model},
		{"client", f.Client},
		{"api_key", f.APIKeyHash},
	} {
		if check.value != "" {
			return &ParamError{Param: check.param, Reason: "the weekly limit covers the whole account, so it can only be filtered by provider or account"}
		}
	}
	return nil
}

// weeklyCycles walks each account's rows in time order. Only pairs with both
// readings at or after measureFrom contribute tokens, cost and quota movement. A
// reading whose reset moved by more than resetJitterMillis starts a new cycle.
// Within a cycle, a pair whose percent went down, as when a Claude reset
// refills it, is dropped and the next pair starts from the lower reading.
func weeklyCycles(db querier, f Filter, table *pricing.Table, measureFrom int64) ([]weeklyCycle, error) {
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
		earlyUntil int64
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
			earlyUntil = 0
			spanCost, spanUnpr, spanTokens = 0, 0, Tokens{}
		}
		hasReading := pct.Valid && reset.Valid
		sameCycle := hasReading && cur != nil && abs(reset.Int64-cur.reset) <= resetJitterMillis
		if hasReading && cur != nil && !sameCycle && at < cur.reset {
			// Overlapping early resets cover one interval of realized usage.
			earlyUntil = max(earlyUntil, cur.reset)
		}
		if at < measureFrom {
			cur = &weeklyCycle{account: acct, provider: provider, reset: reset.Int64, lastPct: pct.Float64, lastAt: at}
			continue
		}
		cost, quality := p.price(model, tier, tokens.input(), tokens)
		spanCost += cost
		spanUnpr += quality.UnpricedTokens
		spanTokens.add(tokens)
		if hasReading {
			if sameCycle {
				if cur.lastAt >= measureFrom && pct.Float64 >= cur.lastPct {
					cur.pairs = append(cur.pairs, weeklyPair{at: at, pct: pct.Float64 - cur.lastPct, cost: spanCost, unpriced: spanUnpr, tokens: spanTokens})
				}
				cur.reset, cur.lastPct, cur.lastAt = reset.Int64, pct.Float64, at
			} else {
				if cur != nil {
					cur.nextAt = at
					flush()
				}
				cur = &weeklyCycle{account: acct, provider: provider, reset: reset.Int64, lastPct: pct.Float64, lastAt: at}
			}
			spanCost, spanUnpr, spanTokens = 0, 0, Tokens{}
		}
		if at < earlyUntil && cur != nil {
			cur.afterEarlyResetUSD += cost
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("weekly cycles rows: %w", err)
	}
	flush()
	return cycles, nil
}

type groupKey struct {
	authIndex string
	provider  string // lowercased for joins
	group     string
}

func snapshotFilterWhere(f Filter, fromMs, toMs int64) (string, []any) {
	where := "recorded_at >= ? AND recorded_at < ?"
	args := []any{fromMs, toMs}
	if f.Provider != "" {
		where += " AND lower(provider) = lower(?)"
		args = append(args, f.Provider)
	}
	if f.Account != "" {
		where += " AND auth_index = ?"
		args = append(args, f.Account)
	}
	return where, args
}

// antigravityModelGroup maps every Antigravity model into two quota families
// for Plans, Burn, Leftover, and 429 attribution: Gemini (all Gemini models,
// including Argon/Neon/Pro/Flash) and Claude and GPT (all 3P Claude/GPT models).
// Individual upstream buckets (such as Gemini 4 Argon) are still preserved in
// LimitHistory so the Limits tab reflects every bucket Google reports.
func antigravityModelGroup(model string) (string, string) {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(m, "claude") || strings.Contains(m, "gpt"):
		return "claude-and-gpt", "Claude and GPT"
	case strings.Contains(m, "gemini"):
		return "gemini", "Gemini"
	default:
		return "other", "Other"
	}
}

type polledReading struct {
	at    int64
	pct   float64
	reset sql.NullInt64
}

type polledReq struct {
	at       int64
	cost     float64
	unpriced int64
	tokens   Tokens
}

// polledWeeklyCycles builds weekly cycles from polled quota_snapshots paired
// with each group's requests, using the same cycle, refill and early-reset
// rules as weeklyCycles. For Antigravity, only the two primary groups (gemini
// and claude-and-gpt) are walked so Gemini sub-buckets (like gemini-4-argon)
// are not split out from Gemini in Plans, Burn, or Leftover.
func polledWeeklyCycles(db querier, f Filter, table *pricing.Table, measureFrom int64) ([]weeklyCycle, error) {
	where, args := snapshotFilterWhere(f, f.From.UnixMilli(), f.To.UnixMilli())
	snapRows, err := db.Query(`SELECT auth_index, provider, group_id, group_label, recorded_at, used_pct, reset_at
		FROM quota_snapshots
		WHERE window_kind = 'weekly' AND (lower(provider) <> 'antigravity' OR group_id IN ('gemini', 'claude-and-gpt')) AND `+where+`
		ORDER BY auth_index, lower(provider), group_id, recorded_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("polled weekly cycles: %w", err)
	}
	defer snapRows.Close()

	readings := map[groupKey][]polledReading{}
	labels := map[groupKey]string{}
	origProvider := map[groupKey]string{}
	var orderedKeys []groupKey
	for snapRows.Next() {
		var acct, prov, gid, label string
		var r polledReading
		if err := snapRows.Scan(&acct, &prov, &gid, &label, &r.at, &r.pct, &r.reset); err != nil {
			return nil, fmt.Errorf("polled weekly cycles scan: %w", err)
		}
		gk := groupKey{acct, strings.ToLower(prov), gid}
		if _, seen := readings[gk]; !seen {
			orderedKeys = append(orderedKeys, gk)
			origProvider[gk] = prov
		}
		readings[gk] = append(readings[gk], r)
		if label != "" {
			labels[gk] = label
		}
	}
	if err := snapRows.Err(); err != nil {
		return nil, fmt.Errorf("polled weekly cycles rows: %w", err)
	}
	if len(orderedKeys) == 0 {
		return nil, nil
	}

	reqFilter := f
	if reqFilter.From.UnixMilli() < measureFrom {
		reqFilter.From = time.UnixMilli(measureFrom)
	}
	reqWhere, reqArgs := reqFilter.where(true)
	reqRows, err := db.Query(`SELECT auth_index, provider, requested_at, (`+modelExpr+`), (`+tierExpr+`), `+
		`uncached_input, cache_read, cache_write, output_non_reasoning, output_reasoning, unclassified
		FROM requests WHERE auth_index <> '' AND `+reqWhere+` ORDER BY auth_index, lower(provider), requested_at, id`, reqArgs...)
	if err != nil {
		return nil, fmt.Errorf("polled weekly requests: %w", err)
	}
	defer reqRows.Close()

	p := newPricer(table)
	reqsByGroup := map[groupKey][]polledReq{}
	for reqRows.Next() {
		var acct, prov, model, tier string
		var at int64
		var tokens Tokens
		if err := reqRows.Scan(append([]any{&acct, &prov, &at, &model, &tier}, tokens.scanTargets()...)...); err != nil {
			return nil, fmt.Errorf("polled weekly requests scan: %w", err)
		}
		gid, _ := antigravityModelGroup(model)
		gk := groupKey{acct, strings.ToLower(prov), gid}
		if _, has := readings[gk]; !has {
			continue
		}
		cost, quality := p.price(model, tier, tokens.input(), tokens)
		reqsByGroup[gk] = append(reqsByGroup[gk], polledReq{at: at, cost: cost, unpriced: quality.UnpricedTokens, tokens: tokens})
	}
	if err := reqRows.Err(); err != nil {
		return nil, fmt.Errorf("polled weekly requests rows: %w", err)
	}

	var cycles []weeklyCycle
	for _, gk := range orderedKeys {
		snaps := readings[gk]
		reqs := reqsByGroup[gk]
		label := labels[gk]
		prov := origProvider[gk]
		var (
			cur        *weeklyCycle
			highPct    float64
			highAt     int64
			zeroAt     int64
			hasZero    bool
			earlyUntil int64
			spanCost   float64
			spanUnpr   int64
			spanTokens Tokens
			ri         int
		)
		flush := func() {
			if cur != nil {
				cycles = append(cycles, *cur)
				cur = nil
				highPct, highAt = 0, 0
			}
		}
		consumeReq := func(r polledReq) {
			spanCost += r.cost
			spanUnpr += r.unpriced
			spanTokens.add(r.tokens)
			if r.at < earlyUntil && cur != nil {
				cur.afterEarlyResetUSD += r.cost
			}
		}
		for _, sn := range snaps {
			// Consume requests strictly before sn.at into the preceding span.
			for ri < len(reqs) && reqs[ri].at < sn.at {
				consumeReq(reqs[ri])
				ri++
			}
			// Untouched 0% snapshots carry reset_at = NULL because Google reports
			// a rolling now+7d reset for unused buckets. Treat them as a 0%
			// cycle baseline / reset anchor so opening 0%->X% burn is captured
			// and early resets to 0% close the previous cycle promptly.
			if !sn.reset.Valid {
				if sn.pct == 0 {
					if cur != nil {
						if sn.at < cur.reset {
							earlyUntil = max(earlyUntil, cur.reset)
						}
						cur.nextAt = sn.at
						flush()
					}
					hasZero = true
					zeroAt = sn.at
					spanCost, spanUnpr, spanTokens = 0, 0, Tokens{}
				}
				for ri < len(reqs) && reqs[ri].at == sn.at {
					consumeReq(reqs[ri])
					ri++
				}
				continue
			}

			sameCycle := cur != nil && abs(sn.reset.Int64-cur.reset) <= resetJitterMillis
			isEarlyReset := cur != nil && !sameCycle && sn.at < cur.reset
			if isEarlyReset {
				earlyUntil = max(earlyUntil, cur.reset)
			}
			// Requests at sn.at belong to the old span unless this snapshot
			// starts a new cycle via early reset (in which case they count
			// toward the new cycle's afterEarlyResetUSD, matching weeklyCycles).
			if !isEarlyReset {
				for ri < len(reqs) && reqs[ri].at == sn.at {
					consumeReq(reqs[ri])
					ri++
				}
			}
			if sn.at < measureFrom {
				if sameCycle {
					cur.reset, cur.lastPct, cur.lastAt = sn.reset.Int64, sn.pct, sn.at
					if sn.pct > highPct {
						highPct, highAt = sn.pct, sn.at
					}
				} else {
					cur = &weeklyCycle{account: gk.authIndex, provider: prov, group: gk.group, groupLabel: label, reset: sn.reset.Int64, lastPct: sn.pct, lastAt: sn.at}
					highPct, highAt = sn.pct, sn.at
				}
				hasZero = false
				spanCost, spanUnpr, spanTokens = 0, 0, Tokens{}
				continue
			}
			// Within one polled cycle, Google's remainingFraction can jitter
			// slightly downward between polls (e.g. in-flight reservation
			// adjustments). Track a high-water mark so downward jitter never
			// drops accumulated cost or double-counts the rebound.
			if sameCycle {
				cur.reset, cur.lastPct, cur.lastAt = sn.reset.Int64, sn.pct, sn.at
				if highAt < measureFrom {
					if sn.pct > highPct {
						highPct = sn.pct
					}
					highAt = sn.at
					spanCost, spanUnpr, spanTokens = 0, 0, Tokens{}
				} else if sn.pct > highPct {
					cur.pairs = append(cur.pairs, weeklyPair{at: sn.at, pct: sn.pct - highPct, cost: spanCost, unpriced: spanUnpr, tokens: spanTokens})
					highPct, highAt = sn.pct, sn.at
					spanCost, spanUnpr, spanTokens = 0, 0, Tokens{}
				} else if sn.pct == highPct && spanCost == 0 && spanUnpr == 0 && len(cur.pairs) == 0 {
					cur.pairs = append(cur.pairs, weeklyPair{at: sn.at})
				}
			} else {
				if cur != nil {
					cur.nextAt = sn.at
					flush()
				}
				cur = &weeklyCycle{account: gk.authIndex, provider: prov, group: gk.group, groupLabel: label, reset: sn.reset.Int64, lastPct: sn.pct, lastAt: sn.at}
				highPct, highAt = sn.pct, sn.at
				if hasZero && zeroAt >= measureFrom && sn.pct > 0 {
					cur.pairs = append(cur.pairs, weeklyPair{at: sn.at, pct: sn.pct, cost: spanCost, unpriced: spanUnpr, tokens: spanTokens})
				}
				spanCost, spanUnpr, spanTokens = 0, 0, Tokens{}
			}
			hasZero = false
			if isEarlyReset {
				for ri < len(reqs) && reqs[ri].at == sn.at {
					consumeReq(reqs[ri])
					ri++
				}
			}
		}
		for ri < len(reqs) {
			consumeReq(reqs[ri])
			ri++
		}
		flush()
	}
	return cycles, nil
}

// LimitBurn prices each point of every account's weekly limit. Each pair of
// readings within one cycle counts on the day of its second reading.
func LimitBurn(db *sql.DB, f Filter, table *pricing.Table) (BurnResult, error) {
	if err := rejectPartialAccountFilters(f); err != nil {
		return BurnResult{}, err
	}
	var cycles []weeklyCycle
	if err := withReadTx(db, func(q querier) error {
		headerCycles, err := weeklyCycles(q, f, table, f.From.UnixMilli())
		if err != nil {
			return err
		}
		polled, err := polledWeeklyCycles(q, f, table, f.From.UnixMilli())
		if err != nil {
			return err
		}
		cycles = append(headerCycles, polled...)
		return nil
	}); err != nil {
		return BurnResult{}, err
	}
	type key struct {
		day      int64
		account  string
		provider string
		group    string
	}
	byDay := map[key]*BurnPoint{}
	for _, c := range cycles {
		for _, pair := range c.pairs {
			k := key{f.bucketStart(pair.at, dayMillis), c.account, strings.ToLower(c.provider), c.group}
			bp := byDay[k]
			if bp == nil {
				bp = &BurnPoint{T: k.day, AuthIndex: c.account, Provider: c.provider, Group: c.group, GroupLabel: c.groupLabel}
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
		if a.AuthIndex != b.AuthIndex {
			return a.AuthIndex < b.AuthIndex
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Group < b.Group
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
	var cycles []weeklyCycle
	if err := withReadTx(db, func(q querier) error {
		headerCycles, err := weeklyCycles(q, history, table, history.From.UnixMilli())
		if err != nil {
			return err
		}
		polled, err := polledWeeklyCycles(q, history, table, history.From.UnixMilli())
		if err != nil {
			return err
		}
		cycles = append(headerCycles, polled...)
		return nil
	}); err != nil {
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
			AuthIndex: c.account, Provider: c.provider, Group: c.group, GroupLabel: c.groupLabel, EndedAt: end, Early: early, LastReadingAt: c.lastAt,
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
		if a.AuthIndex != b.AuthIndex {
			return a.AuthIndex < b.AuthIndex
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Group < b.Group
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
// the last reading before the range when there is one. Group is set for
// providers with per-model-group weekly quotas (such as Antigravity).
type HistoryAccount struct {
	AuthIndex  string         `json:"auth_index"`
	Provider   string         `json:"provider"`
	Group      string         `json:"group,omitempty"`
	GroupLabel string         `json:"group_label,omitempty"`
	Points     []HistoryPoint `json:"points"`
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
	res := HistoryResult{BucketMs: bucket, Accounts: []HistoryAccount{}}
	err := withReadTx(db, func(q querier) error {
		where, args := f.where(false)
		rows, err := q.Query(`SELECT auth_index, provider, requested_at, limit_weekly_pct, COALESCE(limit_weekly_reset, 0)
			FROM requests WHERE auth_index <> '' AND limit_weekly_pct IS NOT NULL AND requested_at >= ? AND `+where+`
			ORDER BY auth_index, provider, requested_at, id`, append([]any{f.From.UnixMilli() - cycleLookbackMillis}, args...)...)
		if err != nil {
			return fmt.Errorf("limit history: %w", err)
		}
		defer rows.Close()

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
				return fmt.Errorf("limit history scan: %w", err)
			}
			if cur == nil || cur.AuthIndex != acct || cur.Provider != provider {
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
			return fmt.Errorf("limit history rows: %w", err)
		}

		snapWhere, snapArgs := snapshotFilterWhere(f, f.From.UnixMilli()-cycleLookbackMillis, f.To.UnixMilli())
		snapRows, err := q.Query(`SELECT auth_index, provider, group_id, group_label, recorded_at, used_pct, COALESCE(reset_at, 0)
			FROM quota_snapshots WHERE window_kind = 'weekly' AND `+snapWhere+`
			ORDER BY auth_index, lower(provider), group_id, recorded_at, id`, snapArgs...)
		if err != nil {
			return fmt.Errorf("limit history snapshots: %w", err)
		}
		defer snapRows.Close()

		cur = nil
		lastBucket = int64(math.MinInt64)
		for snapRows.Next() {
			var (
				acct, provider, gid, label string
				pt                         HistoryPoint
			)
			if err := snapRows.Scan(&acct, &provider, &gid, &label, &pt.T, &pt.WeeklyPct, &pt.WeeklyReset); err != nil {
				return fmt.Errorf("limit history snapshots scan: %w", err)
			}
			if cur == nil || cur.AuthIndex != acct || !strings.EqualFold(cur.Provider, provider) || cur.Group != gid {
				res.Accounts = append(res.Accounts, HistoryAccount{AuthIndex: acct, Provider: provider, Group: gid, GroupLabel: label})
				cur = &res.Accounts[len(res.Accounts)-1]
				lastBucket = math.MinInt64
			} else if label != "" {
				cur.GroupLabel = label
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
		if err := snapRows.Err(); err != nil {
			return fmt.Errorf("limit history snapshots rows: %w", err)
		}
		return nil
	})
	return res, err
}

// AccountValue summarizes one account's realized usage over the selected range
// and, when the provider reports weekly quota headers or polled snapshots, its
// measured quota burn across the whole range.
type AccountValue struct {
	AuthIndex  string `json:"auth_index"`
	Provider   string `json:"provider"`
	Group      string `json:"group,omitempty"`
	GroupLabel string `json:"group_label,omitempty"`
	Requests   int64  `json:"requests"`
	Failed     int64  `json:"failed"`
	Tokens
	CostUSD                float64 `json:"cost_usd"`
	UnpricedTokens         int64   `json:"unpriced_tokens"`
	CacheHitRate           float64 `json:"cache_hit_rate"`
	TopModel               string  `json:"top_model,omitempty"`
	TopModelShare          float64 `json:"top_model_share,omitempty"`
	EarlyResets            int     `json:"early_resets"`
	AfterEarlyResetUSD     float64 `json:"after_early_reset_usd"`
	QuotaMeasured          bool    `json:"quota_measured"`
	WeeklyPct              float64 `json:"weekly_pct"`
	MeasuredCostUSD        float64 `json:"measured_cost_usd"`
	MeasuredUnpricedTokens int64   `json:"measured_unpriced_tokens"`
	MeasuredTokens         Tokens  `json:"measured_tokens"`
	LowSample              bool    `json:"low_sample,omitempty"`
}

// PlanValueResult answers the plan-value route.
type PlanValueResult struct {
	DataSince  int64           `json:"data_since"`
	Accounts   []AccountValue  `json:"accounts"`
	RateLimits []PlanRateLimit `json:"rate_limits"`
}

// PlanValue returns per-account realized usage and whole-range weekly quota
// burn so the dashboard can rank plans by potential and realized cost-benefit.
func PlanValue(db *sql.DB, f Filter, table *pricing.Table) (PlanValueResult, error) {
	if err := rejectPartialAccountFilters(f); err != nil {
		return PlanValueResult{}, err
	}
	var res PlanValueResult
	err := withReadTx(db, func(q querier) error {
		since, err := dataSince(q)
		if err != nil {
			return err
		}
		rows, err := aggregate(q, f, table, "auth_index", "provider")
		if err != nil {
			return err
		}
		type acctKey struct {
			authIndex string
			provider  string // lowercased for joins
			group     string
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
			var gid, glabel string
			if strings.EqualFold(r.keys[1], "antigravity") {
				gid, glabel = antigravityModelGroup(r.model)
			}
			k := acctKey{r.keys[0], strings.ToLower(r.keys[1]), gid}
			av := byAcct[k]
			if av == nil {
				av = &AccountValue{AuthIndex: k.authIndex, Provider: r.keys[1], Group: gid, GroupLabel: glabel}
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

		// Count distinct generating successful calls per account (and per
		// Antigravity quota group, deduplicating multi-model split-billing
		// rows of one call in Go).
		where, args := f.where(true)
		reqRows, err := q.Query(`SELECT auth_index, provider,
			CASE WHEN lower(provider) = 'antigravity' THEN (`+modelExpr+`) ELSE '' END AS model_key,
			CASE WHEN lower(provider) = 'antigravity' THEN (`+callExpr+`) ELSE '' END AS call_key,
			COUNT(DISTINCT `+callExpr+`)
			FROM requests WHERE auth_index <> '' AND generate = 1 AND failed = 0 AND `+where+`
			GROUP BY auth_index, provider, model_key, call_key`, args...)
		if err != nil {
			return fmt.Errorf("plan value requests: %w", err)
		}
		defer reqRows.Close()
		seenAGCalls := map[acctKey]map[string]struct{}{}
		for reqRows.Next() {
			var authIdx, prov, modelKey, callKey string
			var reqs int64
			if err := reqRows.Scan(&authIdx, &prov, &modelKey, &callKey, &reqs); err != nil {
				return fmt.Errorf("plan value requests scan: %w", err)
			}
			lp := strings.ToLower(prov)
			if lp == "antigravity" {
				gid, _ := antigravityModelGroup(modelKey)
				k := acctKey{authIdx, lp, gid}
				if av := byAcct[k]; av != nil {
					if seenAGCalls[k] == nil {
						seenAGCalls[k] = map[string]struct{}{}
					}
					if _, dup := seenAGCalls[k][callKey]; !dup {
						seenAGCalls[k][callKey] = struct{}{}
						av.Requests++
					}
				}
				continue
			}
			if av := byAcct[acctKey{authIdx, lp, ""}]; av != nil {
				av.Requests += reqs
			}
		}
		if err := reqRows.Err(); err != nil {
			return fmt.Errorf("plan value requests rows: %w", err)
		}

		cycles, err := weeklyCycles(q, withCycleLookback(f), table, f.From.UnixMilli())
		if err != nil {
			return err
		}
		polled, err := polledWeeklyCycles(q, withCycleLookback(f), table, f.From.UnixMilli())
		if err != nil {
			return err
		}
		cycles = append(cycles, polled...)
		for _, c := range cycles {
			k := acctKey{c.account, strings.ToLower(c.provider), c.group}
			av := byAcct[k]
			if av == nil {
				continue // Only seen before the selected period.
			}
			av.AfterEarlyResetUSD += c.afterEarlyResetUSD
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

		limits, err := planRateLimits(q, f)
		if err != nil {
			return err
		}
		res = PlanValueResult{DataSince: since, RateLimits: limits, Accounts: make([]AccountValue, 0, len(byAcct))}
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
			if a.Provider != b.Provider {
				return a.Provider < b.Provider
			}
			return a.Group < b.Group
		})
		return nil
	})
	return res, err
}
