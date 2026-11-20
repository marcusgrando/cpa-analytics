package query

import (
	"database/sql"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/marcusgrando/cpa-analytics/internal/pricing"
)

const (
	defaultRequestsLimit = 50
	maxRequestsLimit     = 200
)

// requestSorts order the request log. Every order falls back to newest first.
var requestSorts = map[string]func(a, b *Call) bool{
	"recent":  func(a, b *Call) bool { return false },
	"cost":    func(a, b *Call) bool { return a.CostUSD > b.CostUSD },
	"latency": func(a, b *Call) bool { return a.LatencyMs > b.LatencyMs },
	"tokens":  func(a, b *Call) bool { return a.total() > b.total() },
}

// requestOutcomes select rows by whether the upstream call failed.
var requestOutcomes = map[string]string{"all": "1 = 1", "failed": "failed = 1", "ok": "failed = 0"}

// RequestsQuery picks a page of the request log.
type RequestsQuery struct {
	Sort    string
	Outcome string
	Offset  int
	Limit   int
}

// ParseRequestsQuery reads sort (recent, cost, latency or tokens), outcome
// (all, failed or ok), offset and limit (1 to 200).
func ParseRequestsQuery(q url.Values) (RequestsQuery, error) {
	rq := RequestsQuery{Sort: paramOr(q, "sort", "recent"), Outcome: paramOr(q, "outcome", "all"), Limit: defaultRequestsLimit}
	if _, ok := requestSorts[rq.Sort]; !ok {
		return RequestsQuery{}, &ParamError{Param: "sort", Reason: "must be recent, cost, latency or tokens"}
	}
	if _, ok := requestOutcomes[rq.Outcome]; !ok {
		return RequestsQuery{}, &ParamError{Param: "outcome", Reason: "must be all, failed or ok"}
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxRequestsLimit {
			return RequestsQuery{}, &ParamError{Param: "limit", Reason: "must be a whole number from 1 to 200"}
		}
		rq.Limit = n
	}
	if raw := q.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return RequestsQuery{}, &ParamError{Param: "offset", Reason: "must be a whole number from 0"}
		}
		rq.Offset = n
	}
	return rq, nil
}

func paramOr(q url.Values, name, fallback string) string {
	if v := q.Get(name); v != "" {
		return v
	}
	return fallback
}

// Call is one upstream call in the request log: the rows sharing a trace and
// start time, split billing included. It holds metadata only, never bodies.
type Call struct {
	ID string `json:"id"`
	At int64  `json:"at"`
	// Models are the reported models, or the requested ones when none was
	// reported, one per split-billing row.
	Models          []string `json:"models"`
	RequestedModel  string   `json:"requested_model"`
	Provider        string   `json:"provider"`
	Client          string   `json:"client"`
	UserAgent       string   `json:"user_agent"`
	APIKeyHash      string   `json:"api_key_hash"`
	Account         string   `json:"account"`
	SessionID       string   `json:"session_id"`
	ReasoningEffort string   `json:"reasoning_effort"`
	ServiceTier     string   `json:"service_tier"`
	Stream          bool     `json:"stream"`
	Generate        bool     `json:"generate"`
	Failed          bool     `json:"failed"`
	StatusCode      int      `json:"status_code"`
	LatencyMs       int64    `json:"latency_ms"`
	TTFTMs          int64    `json:"ttft_ms"`
	Records         int64    `json:"records"`
	Tokens
	CostUSD        float64 `json:"cost_usd"`
	UnpricedTokens int64   `json:"unpriced_tokens"`
}

func (c *Call) total() int64 { return c.Tokens.total() }

// RequestsResult is one page of the request log and the number of calls in
// the whole filter.
type RequestsResult struct {
	Total int    `json:"total"`
	Calls []Call `json:"calls"`
}

// Requests lists upstream calls in the filter. It ranks every call by its
// summed tokens, priced cost and latency, then reads the details of the page.
func Requests(db *sql.DB, f Filter, table *pricing.Table, rq RequestsQuery) (RequestsResult, error) {
	calls, err := rankCalls(db, f, table, rq)
	if err != nil {
		return RequestsResult{}, err
	}
	res := RequestsResult{Total: len(calls), Calls: []Call{}}
	if rq.Offset >= len(calls) {
		return res, nil
	}
	page := calls[rq.Offset:min(rq.Offset+rq.Limit, len(calls))]
	if err := callDetails(db, f, rq, page); err != nil {
		return RequestsResult{}, err
	}
	for _, c := range page {
		res.Calls = append(res.Calls, *c)
	}
	return res, nil
}

func rankCalls(db *sql.DB, f Filter, table *pricing.Table, rq RequestsQuery) ([]*Call, error) {
	where, args := f.where(true)
	rows, err := db.Query(`SELECT `+callExpr+`, (`+modelExpr+`), (`+tierExpr+`), (`+table.BandSQL(inputExpr)+`),
		MIN(requested_at), MAX(latency_ms), MAX(failed), COUNT(*), `+tokenSums+`
		FROM requests WHERE `+where+` AND `+requestOutcomes[rq.Outcome]+` GROUP BY 1, 2, 3, 4`, args...)
	if err != nil {
		return nil, fmt.Errorf("requests: %w", err)
	}
	defer rows.Close()
	p := newPricer(table)
	byID := map[string]*Call{}
	var calls []*Call
	for rows.Next() {
		var (
			id, model, tier   string
			band, at, latency int64
			failed, records   int64
			tokens            Tokens
		)
		targets := append([]any{&id, &model, &tier, &band, &at, &latency, &failed, &records}, tokens.scanTargets()...)
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("requests scan: %w", err)
		}
		c := byID[id]
		if c == nil {
			c = &Call{ID: id, At: at}
			byID[id] = c
			calls = append(calls, c)
		}
		c.At = min(c.At, at)
		c.LatencyMs = max(c.LatencyMs, latency)
		c.Failed = c.Failed || failed > 0
		c.Records += records
		c.Tokens.add(tokens)
		cost, quality := p.price(model, tier, band, tokens)
		c.CostUSD += cost
		c.UnpricedTokens += quality.UnpricedTokens
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("requests rows: %w", err)
	}
	first := requestSorts[rq.Sort]
	sort.SliceStable(calls, func(i, j int) bool {
		a, b := calls[i], calls[j]
		if first(a, b) || first(b, a) {
			return first(a, b)
		}
		if a.At != b.At {
			return a.At > b.At
		}
		return a.ID > b.ID
	})
	return calls, nil
}

// callDetails fills the metadata of the page's calls from their rows.
func callDetails(db *sql.DB, f Filter, rq RequestsQuery, page []*Call) error {
	where, args := f.where(true)
	byID := make(map[string]*Call, len(page))
	marks := make([]string, len(page))
	for i, c := range page {
		byID[c.ID] = c
		marks[i] = "?"
		args = append(args, c.ID)
	}
	rows, err := db.Query(`SELECT `+callExpr+`, (`+modelExpr+`), model, provider, (`+clientExpr+`), user_agent, api_key_hash,
		auth_index, session_id, reasoning_effort, (`+tierExpr+`), stream, generate, status_code, ttft_ms
		FROM requests WHERE `+where+` AND `+requestOutcomes[rq.Outcome]+` AND `+callExpr+` IN (`+strings.Join(marks, ", ")+`)
		ORDER BY requested_at, id`, args...)
	if err != nil {
		return fmt.Errorf("request details: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, model, requested, provider, client, agent, key string
			account, session, effort, tier                     string
			stream, generate                                   bool
			status                                             int
			ttft                                               int64
		)
		if err := rows.Scan(&id, &model, &requested, &provider, &client, &agent, &key, &account, &session, &effort, &tier,
			&stream, &generate, &status, &ttft); err != nil {
			return fmt.Errorf("request details scan: %w", err)
		}
		c := byID[id]
		if c == nil {
			continue
		}
		if !slices.Contains(c.Models, model) {
			c.Models = append(c.Models, model)
		}
		// The first row of a call is the scheduled request. Split-billing rows
		// after it only add a model.
		if c.Provider == "" {
			c.RequestedModel, c.Provider, c.Client, c.UserAgent, c.APIKeyHash = requested, provider, client, agent, key
			c.Account, c.SessionID, c.ReasoningEffort, c.ServiceTier = account, session, effort, tier
		}
		c.Stream = c.Stream || stream
		c.Generate = c.Generate || generate
		c.StatusCode = max(c.StatusCode, status)
		c.TTFTMs = max(c.TTFTMs, ttft)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("request details rows: %w", err)
	}
	return nil
}
