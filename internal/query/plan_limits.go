package query

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// PlanRateLimit counts distinct upstream calls with HTTP 429. A following
// account is an observed success in the same request context. WebSocket turns
// can share that context, so this observation cannot establish a retry reason.
type PlanRateLimit struct {
	Provider         string `json:"provider"`
	AuthIndex        string `json:"auth_index"`
	Group            string `json:"group,omitempty"`
	Window           string `json:"window"`
	FollowingAccount string `json:"following_account,omitempty"`
	Calls            int64  `json:"calls"`
}

const (
	// rateLimitSnapBefore and rateLimitSnapAfter bound how far from a 429 a
	// polled quota snapshot may sit to classify its window.
	rateLimitSnapBefore = 10 * 60 * 1000
	rateLimitSnapAfter  = 2 * 60 * 1000
)

func classifyRateLimitWindow(sessionPct, weeklyPct sql.NullFloat64) string {
	switch {
	case sessionPct.Valid && sessionPct.Float64 >= 100 && weeklyPct.Valid && weeklyPct.Float64 >= 100:
		return "both"
	case sessionPct.Valid && sessionPct.Float64 >= 100:
		return "session"
	case weeklyPct.Valid && weeklyPct.Float64 >= 100:
		return "weekly"
	case sessionPct.Valid && weeklyPct.Valid:
		return "below"
	default:
		return "incomplete"
	}
}

func nearestQuotaPct(db *sql.DB, provider, authIndex, groupID, windowKind string, at int64) sql.NullFloat64 {
	var pct float64
	err := db.QueryRow(`SELECT used_pct FROM quota_snapshots
		WHERE provider = ? AND auth_index = ? AND group_id = ? AND window_kind = ?
			AND recorded_at >= ? AND recorded_at <= ?
		ORDER BY ABS(recorded_at - ?) LIMIT 1`,
		provider, authIndex, groupID, windowKind, at-rateLimitSnapBefore, at+rateLimitSnapAfter, at).Scan(&pct)
	if err != nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: pct, Valid: true}
}

func planRateLimits(db *sql.DB, f Filter) ([]PlanRateLimit, error) {
	where, args := f.where(true)
	knownGroups, err := loadKnownQuotaGroups(db, f)
	if err != nil {
		return nil, err
	}
	// Split billing can produce multiple rows for one call. Only snapshots
	// on the 429 itself (or nearest polled quota snapshot for Antigravity)
	// describe the limits it encountered.
	rows, err := db.Query(`WITH failures AS (
		SELECT provider, auth_index, trace_id, session_id, model, api_key_hash, requested_at,
			MAX(`+modelExpr+`) AS reported_model,
			requested_at + MAX(latency_ms) AS ended_at,
			MAX(limit_session_pct) AS session_pct, MAX(limit_weekly_pct) AS weekly_pct
		FROM requests
		WHERE auth_index <> '' AND failed = 1 AND status_code = 429 AND `+where+`
		GROUP BY provider, auth_index, `+callExpr+`
	)
	SELECT f.provider, f.auth_index, f.reported_model, f.requested_at, f.session_pct, f.weekly_pct,
		COALESCE(NULLIF(CASE WHEN ? = '' AND f.trace_id <> '' AND f.session_id <> '' AND f.model <> '' AND f.api_key_hash <> '' THEN (
			SELECT CASE WHEN MIN(s.auth_index) = MAX(s.auth_index) THEN MIN(s.auth_index) END
			FROM requests s
			WHERE s.trace_id = f.trace_id AND s.provider = f.provider AND s.session_id = f.session_id
				AND s.model = f.model AND s.api_key_hash = f.api_key_hash AND s.generate = 1 AND s.failed = 0
				AND s.requested_at > f.requested_at AND s.requested_at >= f.ended_at
				AND s.requested_at <= f.ended_at + 5000 AND s.requested_at < ?
			GROUP BY s.requested_at ORDER BY s.requested_at LIMIT 1
		) END, f.auth_index), '') AS following
	FROM failures f`, append(args, f.Account, f.To.UnixMilli())...)
	if err != nil {
		return nil, fmt.Errorf("plan rate limits: %w", err)
	}
	defer rows.Close()

	type rlKey struct {
		provider, authIndex, group, window, following string
	}
	counts := map[rlKey]int64{}
	for rows.Next() {
		var (
			prov, authIdx, model, following string
			at                              int64
			sessionPct, weeklyPct           sql.NullFloat64
		)
		if err := rows.Scan(&prov, &authIdx, &model, &at, &sessionPct, &weeklyPct, &following); err != nil {
			return nil, fmt.Errorf("plan rate limits scan: %w", err)
		}
		var group string
		if strings.EqualFold(prov, "antigravity") {
			group, _ = antigravityModelGroup(model, knownGroups[accountProviderKey{authIdx, prov}])
			if !sessionPct.Valid {
				sessionPct = nearestQuotaPct(db, prov, authIdx, group, "session", at)
			}
			if !weeklyPct.Valid {
				weeklyPct = nearestQuotaPct(db, prov, authIdx, group, "weekly", at)
			}
		}
		k := rlKey{prov, authIdx, group, classifyRateLimitWindow(sessionPct, weeklyPct), following}
		counts[k]++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("plan rate limits rows: %w", err)
	}
	result := make([]PlanRateLimit, 0, len(counts))
	for k, n := range counts {
		result = append(result, PlanRateLimit{
			Provider: k.provider, AuthIndex: k.authIndex, Group: k.group,
			Window: k.window, FollowingAccount: k.following, Calls: n,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.AuthIndex != b.AuthIndex {
			return a.AuthIndex < b.AuthIndex
		}
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if a.Window != b.Window {
			return a.Window < b.Window
		}
		return a.FollowingAccount < b.FollowingAccount
	})
	return result, nil
}
