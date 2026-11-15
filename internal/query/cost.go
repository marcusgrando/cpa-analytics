package query

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/marcusgrando/cpa-analytics/internal/accounting"
	"github.com/marcusgrando/cpa-analytics/internal/pricing"
)

// Tokens holds the normalized, non-overlapping token buckets.
type Tokens struct {
	UncachedInput int64 `json:"uncached_input"`
	CacheRead     int64 `json:"cache_read"`
	CacheWrite    int64 `json:"cache_write"`
	// Output excludes reasoning.
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
	// Unclassified tokens were reported only as a total and cannot be priced.
	Unclassified int64 `json:"unclassified"`
}

func (t Tokens) input() int64 { return t.UncachedInput + t.CacheRead + t.CacheWrite }

func (t Tokens) total() int64 { return t.input() + t.Output + t.Reasoning + t.Unclassified }

func (t *Tokens) add(o Tokens) {
	t.UncachedInput += o.UncachedInput
	t.CacheRead += o.CacheRead
	t.CacheWrite += o.CacheWrite
	t.Output += o.Output
	t.Reasoning += o.Reasoning
	t.Unclassified += o.Unclassified
}

func (t Tokens) breakdown() accounting.Breakdown {
	return accounting.Breakdown{
		Uncached: t.UncachedInput, CacheRead: t.CacheRead, CacheWrite: t.CacheWrite,
		OutputNonReasoning: t.Output, Reasoning: t.Reasoning,
	}
}

// CostQuality splits tokens by where their price came from.
type CostQuality struct {
	LiteLLMTokens  int64 `json:"litellm_tokens"`
	OverrideTokens int64 `json:"override_tokens"`
	UnpricedTokens int64 `json:"unpriced_tokens"`
}

func (q *CostQuality) add(o CostQuality) {
	q.LiteLLMTokens += o.LiteLLMTokens
	q.OverrideTokens += o.OverrideTokens
	q.UnpricedTokens += o.UnpricedTokens
}

func cacheHitRate(t Tokens) float64 { return ratio(float64(t.CacheRead), float64(t.input())) }

func ratio(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

type lookupKey struct{ model, tier string }

type lookup struct {
	rates  pricing.Rates
	source pricing.Source
	ok     bool
}

// pricer caches price lookups for one query.
type pricer struct {
	table *pricing.Table
	cache map[lookupKey]lookup
}

func newPricer(table *pricing.Table) *pricer {
	return &pricer{table: table, cache: map[lookupKey]lookup{}}
}

func (p *pricer) lookup(model, tier string) lookup {
	key := lookupKey{model, tier}
	if l, ok := p.cache[key]; ok {
		return l
	}
	var l lookup
	if p.table != nil {
		l.rates, l.source, l.ok = p.table.Lookup(model, tier)
	}
	p.cache[key] = l
	return l
}

// price returns the cost of t and where each token's price came from.
// Unknown models and unclassified tokens are unpriced, never free.
func (p *pricer) price(model, tier string, t Tokens) (float64, CostQuality) {
	l := p.lookup(model, tier)
	if !l.ok {
		return 0, CostQuality{UnpricedTokens: t.total()}
	}
	q := CostQuality{UnpricedTokens: t.Unclassified}
	if l.source == pricing.SourceOverride {
		q.OverrideTokens = t.total() - t.Unclassified
	} else {
		q.LiteLLMTokens = t.total() - t.Unclassified
	}
	return pricing.Cost(l.rates, t.breakdown()), q
}

// costRow is one aggregate per key tuple, model and tier.
type costRow struct {
	keys    []string
	model   string
	records int64
	tokens  Tokens
	cost    float64
	quality CostQuality
	price   lookup
}

const tokenSums = "SUM(uncached_input), SUM(cache_read), SUM(cache_write), SUM(output_non_reasoning), SUM(output_reasoning), SUM(unclassified)"

func (t *Tokens) scanTargets() []any {
	return []any{&t.UncachedInput, &t.CacheRead, &t.CacheWrite, &t.Output, &t.Reasoning, &t.Unclassified}
}

// aggregate sums every row in the filter, grouped by keyExprs plus model and
// tier, and prices each group. Keys come back as text.
func aggregate(db *sql.DB, f Filter, table *pricing.Table, keyExprs ...string) ([]costRow, error) {
	cols := make([]string, 0, len(keyExprs)+2)
	for _, expr := range keyExprs {
		cols = append(cols, "CAST("+expr+" AS TEXT)")
	}
	cols = append(cols, "("+modelExpr+")", "("+tierExpr+")")
	groups := make([]string, len(cols))
	for i := range cols {
		groups[i] = fmt.Sprint(i + 1)
	}
	where, args := f.where(true)
	q := "SELECT " + strings.Join(cols, ", ") + ", COUNT(*), " + tokenSums +
		" FROM requests WHERE " + where + " GROUP BY " + strings.Join(groups, ", ")
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("aggregate: %w", err)
	}
	defer rows.Close()
	p := newPricer(table)
	var out []costRow
	for rows.Next() {
		r := costRow{keys: make([]string, len(keyExprs))}
		var tier string
		targets := make([]any, 0, len(keyExprs)+9)
		for i := range r.keys {
			targets = append(targets, &r.keys[i])
		}
		targets = append(targets, &r.model, &tier, &r.records)
		targets = append(targets, r.tokens.scanTargets()...)
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("aggregate scan: %w", err)
		}
		r.price = p.lookup(r.model, tier)
		r.cost, r.quality = p.price(r.model, tier, r.tokens)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("aggregate rows: %w", err)
	}
	return out, nil
}
