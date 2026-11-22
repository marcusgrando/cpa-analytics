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
	// When filtering by account, inspect all accounts across sessions touched
	// by that account so cross-account switches are still visible.
	targetAccount := f.Account
	baseFilter := f
	baseFilter.Account = ""
	where, args := baseFilter.where(true)
	rows, err := db.Query(`SELECT provider, session_id, (`+modelExpr+`) AS m, (`+tierExpr+`) AS t, (`+table.BandSQL(inputExpr)+`) AS b, auth_index, `+callExpr+`, MIN(requested_at) AS at, MIN(id) AS first_id, SUM(cache_write)
		FROM requests WHERE generate = 1 AND failed = 0 AND session_id <> '' AND auth_index <> '' AND `+where+`
		GROUP BY provider, session_id, m, t, b, auth_index, `+callExpr+`
		ORDER BY provider, session_id, m, at, first_id`, args...)
	if err != nil {
		return AffinityResult{}, fmt.Errorf("affinity: %w", err)
	}
	defer rows.Close()

	type callEntry struct {
		account    string
		cacheWrite int64
		cost       float64
	}
	type pairEntry struct {
		provider string
		calls    []callEntry
		hasAcct  bool
	}
	p := newPricer(table)
	var pairs []*pairEntry
	var cur *pairEntry
	var curPairKey, curCallID string
	for rows.Next() {
		var (
			provider, session, model, tier, account, callID string
			band, at, firstID, cacheWrite                   int64
		)
		if err := rows.Scan(&provider, &session, &model, &tier, &band, &account, &callID, &at, &firstID, &cacheWrite); err != nil {
			return AffinityResult{}, fmt.Errorf("affinity scan: %w", err)
		}
		_ = at
		_ = firstID
		cost, _ := p.price(model, tier, band, Tokens{CacheWrite: cacheWrite})
		key := provider + "\x1f" + session + "\x1f" + model
		if cur == nil || key != curPairKey {
			cur = &pairEntry{provider: provider}
			pairs = append(pairs, cur)
			curPairKey = key
			curCallID = ""
		}
		if targetAccount == "" || account == targetAccount {
			cur.hasAcct = true
		}
		if len(cur.calls) > 0 && curCallID == callID && cur.calls[len(cur.calls)-1].account == account {
			cur.calls[len(cur.calls)-1].cacheWrite += cacheWrite
			cur.calls[len(cur.calls)-1].cost += cost
		} else {
			cur.calls = append(cur.calls, callEntry{account: account, cacheWrite: cacheWrite, cost: cost})
			curCallID = callID
		}
	}
	if err := rows.Err(); err != nil {
		return AffinityResult{}, fmt.Errorf("affinity rows: %w", err)
	}

	byProvider := map[string]*AffinityRow{}
	for _, pe := range pairs {
		if !pe.hasAcct {
			continue
		}
		row := byProvider[pe.provider]
		if row == nil {
			row = &AffinityRow{Provider: pe.provider}
			byProvider[pe.provider] = row
		}
		var lastAccount string
		pairSwitched := false
		for i, c := range pe.calls {
			switched := i > 0 && c.account != lastAccount
			row.Calls++
			row.CacheWrite += c.cacheWrite
			row.CacheWriteUSD += c.cost
			if switched {
				row.Switches++
				row.SwitchCacheWrite += c.cacheWrite
				row.SwitchCacheWriteUSD += c.cost
				pairSwitched = true
			}
			lastAccount = c.account
		}
		if len(pe.calls) >= 2 {
			row.Sessions++
			if pairSwitched {
				row.SwitchedSessions++
			}
		}
	}

	res := AffinityResult{Providers: make([]AffinityRow, 0, len(byProvider))}
	for _, r := range byProvider {
		res.Providers = append(res.Providers, *r)
	}
	sort.Slice(res.Providers, func(i, j int) bool { return res.Providers[i].Provider < res.Providers[j].Provider })
	return res, nil
}
