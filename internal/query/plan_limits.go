package query

import (
	"database/sql"
	"fmt"
)

// PlanRateLimit counts distinct upstream calls with HTTP 429. A following
// account is an observed success in the same request context. WebSocket turns
// can share that context, so this observation cannot establish a retry reason.
type PlanRateLimit struct {
	Provider         string `json:"provider"`
	AuthIndex        string `json:"auth_index"`
	Window           string `json:"window"`
	FollowingAccount string `json:"following_account,omitempty"`
	Calls            int64  `json:"calls"`
}

func planRateLimits(db *sql.DB, f Filter) ([]PlanRateLimit, error) {
	where, args := f.where(true)
	// Split billing can produce multiple rows for one call. Only snapshots
	// on the 429 itself describe the limits it encountered.
	rows, err := db.Query(`WITH failures AS (
		SELECT provider, auth_index, trace_id, session_id, model, api_key_hash, requested_at,
			requested_at + MAX(latency_ms) AS ended_at,
			MAX(limit_session_pct) AS session_pct, MAX(limit_weekly_pct) AS weekly_pct
		FROM requests
		WHERE auth_index <> '' AND failed = 1 AND status_code = 429 AND `+where+`
		GROUP BY provider, auth_index, `+callExpr+`
	), observations AS (
		SELECT f.provider, f.auth_index,
			CASE WHEN session_pct >= 100 AND weekly_pct >= 100 THEN 'both'
				WHEN session_pct >= 100 THEN 'session'
				WHEN weekly_pct >= 100 THEN 'weekly'
				WHEN session_pct IS NOT NULL AND weekly_pct IS NOT NULL THEN 'below'
				ELSE 'incomplete' END AS window,
			CASE WHEN ? = '' AND f.trace_id <> '' AND f.session_id <> '' AND f.model <> '' AND f.api_key_hash <> '' THEN (
				SELECT CASE WHEN MIN(s.auth_index) = MAX(s.auth_index) THEN MIN(s.auth_index) END
				FROM requests s
				WHERE s.trace_id = f.trace_id AND s.provider = f.provider AND s.session_id = f.session_id
					AND s.model = f.model AND s.api_key_hash = f.api_key_hash AND s.generate = 1 AND s.failed = 0
					AND s.requested_at > f.requested_at AND s.requested_at >= f.ended_at
					AND s.requested_at <= f.ended_at + 5000 AND s.requested_at < ?
				GROUP BY s.requested_at ORDER BY s.requested_at LIMIT 1
			) END AS following
		FROM failures f
	)
	SELECT provider, auth_index, window, COALESCE(NULLIF(following, auth_index), ''), COUNT(*)
	FROM observations GROUP BY 1, 2, 3, 4 ORDER BY 1, 2, 3, 4`, append(args, f.Account, f.To.UnixMilli())...)
	if err != nil {
		return nil, fmt.Errorf("plan rate limits: %w", err)
	}
	defer rows.Close()

	result := []PlanRateLimit{}
	for rows.Next() {
		var r PlanRateLimit
		if err := rows.Scan(&r.Provider, &r.AuthIndex, &r.Window, &r.FollowingAccount, &r.Calls); err != nil {
			return nil, fmt.Errorf("plan rate limits scan: %w", err)
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("plan rate limits rows: %w", err)
	}
	return result, nil
}
