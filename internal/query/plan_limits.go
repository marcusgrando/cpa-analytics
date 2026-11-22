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

type quotaSnapPoint struct {
	at    int64
	pct   float64
	reset sql.NullInt64
}

type quotaSnapKey struct {
	provider, authIndex, group, windowKind string
}

// loadRateLimitSnapshots prefetches Antigravity quota snapshots covering the
// filter range so 429 classification runs in memory without 2N queries.
func loadRateLimitSnapshots(db querier, f Filter) (map[quotaSnapKey][]quotaSnapPoint, error) {
	where, args := snapshotFilterWhere(f, f.From.UnixMilli()-rateLimitSnapBefore, f.To.UnixMilli()+rateLimitSnapAfter)
	rows, err := db.Query(`SELECT lower(provider), auth_index, group_id, window_kind, recorded_at, used_pct, reset_at
		FROM quota_snapshots WHERE `+where+` ORDER BY recorded_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("plan rate limit snapshots: %w", err)
	}
	defer rows.Close()
	out := map[quotaSnapKey][]quotaSnapPoint{}
	for rows.Next() {
		var k quotaSnapKey
		var pt quotaSnapPoint
		if err := rows.Scan(&k.provider, &k.authIndex, &k.group, &k.windowKind, &pt.at, &pt.pct, &pt.reset); err != nil {
			return nil, fmt.Errorf("plan rate limit snapshots scan: %w", err)
		}
		out[k] = append(out[k], pt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("plan rate limit snapshots rows: %w", err)
	}
	return out, nil
}

// lookupRateLimitSnap picks the best polled snapshot for a 429 at timestamp at:
// prefers the earliest post-429 snapshot within rateLimitSnapAfter (from the
// debounced post-traffic poll), otherwise falls back to the latest non-expired
// pre-429 snapshot within rateLimitSnapBefore.
func lookupRateLimitSnap(snaps []quotaSnapPoint, at int64) sql.NullFloat64 {
	var (
		hasPost bool
		postPct float64
		hasPre  bool
		prePct  float64
	)
	for _, s := range snaps {
		if s.at >= at && s.at <= at+rateLimitSnapAfter {
			if !hasPost {
				hasPost = true
				postPct = s.pct
			}
			break
		}
		if s.at < at && s.at >= at-rateLimitSnapBefore {
			if s.reset.Valid && s.reset.Int64 <= at {
				continue // window already reset before this 429
			}
			hasPre = true
			prePct = s.pct
		}
	}
	if hasPost {
		return sql.NullFloat64{Float64: postPct, Valid: true}
	}
	if hasPre {
		return sql.NullFloat64{Float64: prePct, Valid: true}
	}
	return sql.NullFloat64{}
}

func planRateLimits(db querier, f Filter) ([]PlanRateLimit, error) {
	snaps, err := loadRateLimitSnapshots(db, f)
	if err != nil {
		return nil, err
	}
	where, args := f.where(true)
	// Split billing can produce multiple rows for one call. Only snapshots
	// on the 429 itself (or nearest polled quota snapshot for Antigravity)
	// describe the limits it encountered.
	rows, err := db.Query(`WITH failures AS (
		SELECT provider, auth_index,
			MIN(trace_id) AS trace_id, MIN(session_id) AS session_id,
			MIN(model) AS model, MIN(api_key_hash) AS api_key_hash,
			MIN(requested_at) AS requested_at,
			MAX(`+modelExpr+`) AS reported_model,
			MIN(requested_at) + MAX(latency_ms) AS ended_at,
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
			group, _ = antigravityModelGroup(model)
			lp := strings.ToLower(prov)
			if !sessionPct.Valid {
				sessionPct = lookupRateLimitSnap(snaps[quotaSnapKey{lp, authIdx, group, "session"}], at)
			}
			if !weeklyPct.Valid {
				weeklyPct = lookupRateLimitSnap(snaps[quotaSnapKey{lp, authIdx, group, "weekly"}], at)
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
