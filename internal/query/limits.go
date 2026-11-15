package query

import (
	"database/sql"
	"fmt"
	"sort"

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
}

// BurnResult lists burn points by day, then account.
type BurnResult struct {
	Points []BurnPoint `json:"points"`
}

const (
	// minBurnPct leaves out days whose weekly percent moved too little to
	// give a stable ratio.
	minBurnPct = 1.0
	// resetJitterMillis tolerates resets a provider recomputes per response.
	resetJitterMillis = int64(60 * 60 * 1000)
)

// LimitBurn prices each point of every account's weekly limit. Between two
// consecutive snapshots of one weekly cycle, the rows after the first and up
// to the second are what moved the percent. A pair that crosses a reset, or
// whose percent went down, is dropped. Each pair counts on the day of its
// second snapshot.
func LimitBurn(db *sql.DB, f Filter, table *pricing.Table) (BurnResult, error) {
	for param, value := range map[string]string{"model": f.Model, "client": f.Client, "api_key": f.APIKeyHash} {
		if value != "" {
			return BurnResult{}, &ParamError{Param: param, Reason: "the weekly limit covers the whole account, so it can only be filtered by provider"}
		}
	}
	where, args := f.where(true)
	rows, err := db.Query(`SELECT auth_index, provider, requested_at, (`+modelExpr+`), (`+tierExpr+`), `+
		`uncached_input, cache_read, cache_write, output_non_reasoning, output_reasoning, unclassified, limit_weekly_pct, limit_weekly_reset
		FROM requests WHERE auth_index <> '' AND `+where+` ORDER BY auth_index, requested_at, id`, args...)
	if err != nil {
		return BurnResult{}, fmt.Errorf("limit burn: %w", err)
	}
	defer rows.Close()

	type key struct {
		day     int64
		account string
	}
	byDay := map[key]*BurnPoint{}
	p := newPricer(table)
	var (
		account   string
		anchored  bool
		lastPct   float64
		lastReset int64
		spanCost  float64
		spanUnpr  int64
	)
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
			return BurnResult{}, fmt.Errorf("limit burn scan: %w", err)
		}
		if acct != account {
			account, anchored = acct, false
		}
		cost, quality := p.price(model, tier, tokens)
		spanCost += cost
		spanUnpr += quality.UnpricedTokens
		if !pct.Valid || !reset.Valid {
			continue
		}
		sameCycle := abs(reset.Int64-lastReset) <= resetJitterMillis && pct.Float64 >= lastPct
		if anchored && sameCycle {
			k := key{f.bucketStart(at, dayMillis), acct}
			bp := byDay[k]
			if bp == nil {
				bp = &BurnPoint{T: k.day, AuthIndex: acct, Provider: provider}
				byDay[k] = bp
			}
			bp.WeeklyPct += pct.Float64 - lastPct
			bp.CostUSD += spanCost
			bp.UnpricedTokens += spanUnpr
		}
		anchored, lastPct, lastReset, spanCost, spanUnpr = true, pct.Float64, reset.Int64, 0, 0
	}
	if err := rows.Err(); err != nil {
		return BurnResult{}, fmt.Errorf("limit burn rows: %w", err)
	}

	res := BurnResult{Points: []BurnPoint{}}
	for _, bp := range byDay {
		if bp.WeeklyPct < minBurnPct || bp.CostUSD <= 0 {
			continue
		}
		bp.USDPerPct = bp.CostUSD / bp.WeeklyPct
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

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
