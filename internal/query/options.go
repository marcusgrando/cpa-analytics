package query

import (
	"database/sql"
	"fmt"
	"sort"
)

// FilterOptions contains only dimension names. Filling a select must not
// compute costs or per-request percentiles.
type FilterOptions struct {
	Providers []string `json:"providers"`
	Models    []string `json:"models"`
	Clients   []string `json:"clients"`
}

func Options(db *sql.DB, f Filter) (FilterOptions, error) {
	where, args := f.where(true)
	rows, err := db.Query(`SELECT DISTINCT provider, (`+modelExpr+`), client_name FROM requests WHERE `+where, args...)
	if err != nil {
		return FilterOptions{}, fmt.Errorf("filter options: %w", err)
	}
	defer rows.Close()
	sets := [3]map[string]bool{{}, {}, {}}
	for rows.Next() {
		var provider, model, client string
		if err := rows.Scan(&provider, &model, &client); err != nil {
			return FilterOptions{}, fmt.Errorf("filter options scan: %w", err)
		}
		for i, value := range []string{provider, model, client} {
			if value != "" {
				sets[i][value] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return FilterOptions{}, fmt.Errorf("filter options rows: %w", err)
	}
	values := func(set map[string]bool) []string {
		out := make([]string, 0, len(set))
		for value := range set {
			out = append(out, value)
		}
		sort.Strings(out)
		return out
	}
	return FilterOptions{Providers: values(sets[0]), Models: values(sets[1]), Clients: values(sets[2])}, nil
}
