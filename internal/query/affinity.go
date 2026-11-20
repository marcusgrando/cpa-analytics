package query

import (
	"database/sql"
	"fmt"
	"sort"

	"github.com/marcusgrando/cpa-analytics/internal/pricing"
)

// AffinityRow tells whether CPA kept each session on one account. CPA binds a
// session and model pair to an account (sdk/cliproxy/auth/selector.go), since
// the prompt cache belongs to the account. A switch moves the pair to another
// account, and the call that switched writes its cache again.
type AffinityRow struct {
	Provider string `json:"provider"`
	// Calls are the successful calls that named a session and a credential.
	Calls int64 `json:"calls"`
	// Sessions are the session and model pairs with at least two calls, the
	// ones that could switch. SwitchedSessions switched at least once.
	Sessions         int64 `json:"sessions"`
	SwitchedSessions int64 `json:"switched_sessions"`
	Switches         int64 `json:"switches"`
	// CacheWrite covers every counted call, SwitchCacheWrite the calls that
	// switched. Both are tokens, priced in the USD fields.
	CacheWrite          int64   `json:"cache_write"`
	SwitchCacheWrite    int64   `json:"switch_cache_write"`
	CacheWriteUSD       float64 `json:"cache_write_usd"`
	SwitchCacheWriteUSD float64 `json:"switch_cache_write_usd"`
}

// AffinityResult has one row per provider.
type AffinityResult struct {
	Providers []AffinityRow `json:"providers"`
}

// Affinity counts account switches within each session and model pair. Only
// successful generating calls count, one per upstream call, and only rows
// that name both a session and a credential. Rows recorded before the plugin
// kept the credential have none.
func Affinity(db *sql.DB, f Filter, table *pricing.Table) (AffinityResult, error) {
	where, args := f.where(true)
	rows, err := db.Query(`SELECT provider, session_id, (`+modelExpr+`) AS m, (`+tierExpr+`), auth_index, MIN(requested_at) AS at, SUM(cache_write), SUM(`+inputExpr+`)
		FROM requests WHERE generate = 1 AND failed = 0 AND session_id <> '' AND auth_index <> '' AND `+where+`
		GROUP BY provider, session_id, m, `+callExpr+`, auth_index
		ORDER BY provider, session_id, m, at`, args...)
	if err != nil {
		return AffinityResult{}, fmt.Errorf("affinity: %w", err)
	}
	defer rows.Close()

	p := newPricer(table)
	byProvider := map[string]*AffinityRow{}
	var (
		pairKey, lastAccount string
		pairCalls            int
		pairSwitched         bool
		row                  *AffinityRow
	)
	closePair := func() {
		if row == nil || pairCalls < 2 {
			return
		}
		row.Sessions++
		if pairSwitched {
			row.SwitchedSessions++
		}
	}
	for rows.Next() {
		var (
			provider, session, model, tier, account string
			at, cacheWrite, input                   int64
		)
		if err := rows.Scan(&provider, &session, &model, &tier, &account, &at, &cacheWrite, &input); err != nil {
			return AffinityResult{}, fmt.Errorf("affinity scan: %w", err)
		}
		key := provider + "\x1f" + session + "\x1f" + model
		switched := key == pairKey && account != lastAccount
		if key != pairKey {
			closePair()
			pairKey, pairCalls, pairSwitched = key, 0, false
		}
		row = byProvider[provider]
		if row == nil {
			row = &AffinityRow{Provider: provider}
			byProvider[provider] = row
		}
		cost, _ := p.price(model, tier, input, Tokens{CacheWrite: cacheWrite})
		row.Calls++
		row.CacheWrite += cacheWrite
		row.CacheWriteUSD += cost
		if switched {
			row.Switches++
			row.SwitchCacheWrite += cacheWrite
			row.SwitchCacheWriteUSD += cost
			pairSwitched = true
		}
		pairCalls++
		lastAccount = account
	}
	if err := rows.Err(); err != nil {
		return AffinityResult{}, fmt.Errorf("affinity rows: %w", err)
	}
	closePair()

	res := AffinityResult{Providers: make([]AffinityRow, 0, len(byProvider))}
	for _, r := range byProvider {
		res.Providers = append(res.Providers, *r)
	}
	sort.Slice(res.Providers, func(i, j int) bool { return res.Providers[i].Provider < res.Providers[j].Provider })
	return res, nil
}
