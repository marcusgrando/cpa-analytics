package query

import (
	"database/sql"
	"fmt"
)

// ErrorRow aggregates failed requests by provider, model, account and HTTP status.
type ErrorRow struct {
	Provider   string  `json:"provider"`
	Model      string  `json:"model"`
	AuthIndex  string  `json:"auth_index"`
	StatusCode int     `json:"status_code"`
	Count      int64   `json:"count"`
	Share      float64 `json:"share"`
	LastAt     int64   `json:"last_at"`
}

// ErrorsResult answers the errors route.
type ErrorsResult struct {
	Total int64      `json:"total"`
	Rows  []ErrorRow `json:"rows"`
}

const maxErrorRows = 200

// Errors returns failed records grouped by provider, model, account and status code.
func Errors(db *sql.DB, f Filter) (ErrorsResult, error) {
	where, args := f.where(true)
	rows, err := db.Query(`SELECT provider, (`+modelExpr+`), auth_index, status_code, COUNT(*), MAX(requested_at)
		FROM requests WHERE failed = 1 AND `+where+`
		GROUP BY 1, 2, 3, 4
		ORDER BY 5 DESC, 6 DESC, 1, 2, 3, 4
		LIMIT ?`, append(args, maxErrorRows)...)
	if err != nil {
		return ErrorsResult{}, fmt.Errorf("errors: %w", err)
	}
	defer rows.Close()
	res := ErrorsResult{Rows: []ErrorRow{}}
	for rows.Next() {
		var r ErrorRow
		if err := rows.Scan(&r.Provider, &r.Model, &r.AuthIndex, &r.StatusCode, &r.Count, &r.LastAt); err != nil {
			return ErrorsResult{}, fmt.Errorf("errors scan: %w", err)
		}
		res.Total += r.Count
		res.Rows = append(res.Rows, r)
	}
	if err := rows.Err(); err != nil {
		return ErrorsResult{}, fmt.Errorf("errors rows: %w", err)
	}
	// If the result hit the limit, compute exact total so shares stay accurate.
	if len(res.Rows) == maxErrorRows {
		if err := db.QueryRow(`SELECT COUNT(*) FROM requests WHERE failed = 1 AND `+where, args...).Scan(&res.Total); err != nil {
			return ErrorsResult{}, fmt.Errorf("errors total: %w", err)
		}
	}
	for i := range res.Rows {
		res.Rows[i].Share = ratio(float64(res.Rows[i].Count), float64(res.Total))
	}
	return res, nil
}
