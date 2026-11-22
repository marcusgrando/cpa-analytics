package plugin

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/marcusgrando/cpa-analytics/internal/pricing"
	"github.com/marcusgrando/cpa-analytics/internal/query"
	"github.com/marcusgrando/cpa-analytics/internal/web"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// managementBase is where CPA serves the plugin's management routes. CPA
// requires exact paths, so every route is listed.
const managementBase = "/v0/management/" + pluginName + "/"

// dashboardPath serves the page. CPA rejects a resource at the bare plugin
// prefix, so it needs a name. Resource routes are not authenticated: the page
// holds no data and fetches everything from the management routes.
const dashboardPath = "/v0/resource/plugins/" + pluginName + "/dashboard"

// dashboardCSP allows the inline page and calls to the same origin only.
const dashboardCSP = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'"

// apiError is a JSON error body. Codes tell the caller whether retrying helps:
// invalid_parameter never does, storage_unavailable may.
type apiError struct {
	status int
	code   string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

// dataRoute answers one analytics route from the database.
type dataRoute func(db *sql.DB, f query.Filter, prices *pricing.Table, q url.Values) (any, error)

type route struct {
	description string
	// data routes need the database. health does not.
	data dataRoute
}

func (p *Plugin) routes() map[string]route {
	return map[string]route{
		"options": {"Provider, model and client names for the filters", func(db *sql.DB, f query.Filter, _ *pricing.Table, _ url.Values) (any, error) {
			return query.Options(db, f)
		}},
		"summary": {"Totals, cost by provider, token type and service tier", func(db *sql.DB, f query.Filter, t *pricing.Table, _ url.Values) (any, error) {
			return query.Summary(db, f, t)
		}},
		"timeseries": {"Tokens and cost per day, hour or minute", func(db *sql.DB, f query.Filter, t *pricing.Table, q url.Values) (any, error) {
			return query.Timeseries(db, f, t, paramOr(q, "bucket", "day"), paramOr(q, "group", "provider"))
		}},
		"drift": {"Per-request token drift with change markers", func(db *sql.DB, f query.Filter, t *pricing.Table, q url.Values) (any, error) {
			return query.Drift(db, f, t, paramOr(q, "group", "response_model"), paramOr(q, "bucket", "day"))
		}},
		"breakdown": {"Usage by provider, model, client, user agent, day, API key hash, reasoning effort or account", func(db *sql.DB, f query.Filter, t *pricing.Table, q url.Values) (any, error) {
			return query.Breakdown(db, f, t, paramOr(q, "by", "model"))
		}},
		"errors": {"Failed records by provider, model, account and HTTP status", func(db *sql.DB, f query.Filter, _ *pricing.Table, _ url.Values) (any, error) {
			return query.Errors(db, f)
		}},
		"pricing": {"Price table status, unpriced and overridden models", func(db *sql.DB, f query.Filter, t *pricing.Table, _ url.Values) (any, error) {
			models, err := query.Models(db, f, t)
			if err != nil {
				return nil, err
			}
			return pricingReport{Status: p.pricingStatus(), ModelsReport: models}, nil
		}},
		"limit-burn": {"API-priced cost per point of each account's weekly subscription limit, per day", func(db *sql.DB, f query.Filter, t *pricing.Table, _ url.Values) (any, error) {
			return query.LimitBurn(db, f, t)
		}},
		"limit-history": {"Each account's weekly subscription limit readings over time", func(db *sql.DB, f query.Filter, _ *pricing.Table, _ url.Values) (any, error) {
			return query.LimitHistory(db, f)
		}},
		"limit-leftover": {"Weekly limit each account left unused when its cycle ended, priced at the cycle's cost per point", func(db *sql.DB, f query.Filter, t *pricing.Table, _ url.Values) (any, error) {
			return query.LimitLeftover(db, f, t, time.Now())
		}},
		"plan-value": {"Per-account realized usage and whole-range weekly quota burn for plan cost-benefit ranking", func(db *sql.DB, f query.Filter, t *pricing.Table, _ url.Values) (any, error) {
			val, err := query.PlanValue(db, f, t)
			if err != nil {
				return nil, err
			}
			return planValueReport{PlanValueResult: val, PlanCosts: p.planCosts()}, nil
		}},
		"affinity": {"Account switches within sessions, and the cache they wrote again", func(db *sql.DB, f query.Filter, t *pricing.Table, _ url.Values) (any, error) {
			return query.Affinity(db, f, t)
		}},
		"requests": {"One page of upstream calls with their metadata, tokens and cost", func(db *sql.DB, f query.Filter, t *pricing.Table, q url.Values) (any, error) {
			rq, err := query.ParseRequestsQuery(q)
			if err != nil {
				return nil, err
			}
			return query.Requests(db, f, t, rq)
		}},
		"latency": {"Time to first token and total latency of a sample of successful calls", func(db *sql.DB, f query.Filter, _ *pricing.Table, _ url.Values) (any, error) {
			return query.Latency(db, f, query.MaxLatencyPoints)
		}},
		"health": {"Plugin storage, writer, join and pricing health", nil},
	}
}

type pricingReport struct {
	Status pricing.Status `json:"status"`
	query.ModelsReport
}

type planValueReport struct {
	query.PlanValueResult
	PlanCosts map[string]float64 `json:"plan_costs"`
}

func paramOr(q url.Values, name, fallback string) string {
	if v := q.Get(name); v != "" {
		return v
	}
	return fallback
}

type managementRoute struct {
	Method      string
	Path        string
	Description string
}

type resourceRoute struct {
	Path        string
	Menu        string
	Description string
}

type managementRegistration struct {
	Routes    []managementRoute `json:"routes"`
	Resources []resourceRoute   `json:"resources"`
}

func (p *Plugin) managementRegister([]byte) ([]byte, error) {
	routes := p.routes()
	names := make([]string, 0, len(routes))
	for name := range routes {
		names = append(names, name)
	}
	sort.Strings(names)
	reg := managementRegistration{Resources: []resourceRoute{{
		Path: dashboardPath, Menu: "Analytics", Description: "Token usage, cost and per-request drift",
	}}}
	for _, name := range names {
		reg.Routes = append(reg.Routes, managementRoute{Method: http.MethodGet, Path: managementBase + name, Description: routes[name].description})
	}
	return json.Marshal(reg)
}

func (p *Plugin) managementHandle(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode management request: %w", err)
	}
	if req.Path == dashboardPath {
		if req.Method != http.MethodGet {
			return jsonResponse(http.StatusMethodNotAllowed, map[string]any{"error": map[string]string{"code": "method_not_allowed", "message": req.Method + " is not supported, use GET"}})
		}
		return json.Marshal(pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":            {"text/html; charset=utf-8"},
				"Content-Security-Policy": {dashboardCSP},
				"Cache-Control":           {"no-cache"},
			},
			Body: web.Index(),
		})
	}
	body, err := p.serve(req)
	if err != nil {
		var ae *apiError
		if !errors.As(err, &ae) {
			ae = &apiError{status: http.StatusInternalServerError, code: "internal_error", msg: err.Error()}
		}
		return jsonResponse(ae.status, map[string]any{"error": map[string]string{"code": ae.code, "message": ae.msg}})
	}
	return jsonResponse(http.StatusOK, body)
}

func (p *Plugin) serve(req pluginapi.ManagementRequest) (any, error) {
	name, ok := strings.CutPrefix(req.Path, managementBase)
	r, known := p.routes()[name]
	if !ok || !known {
		return nil, &apiError{status: http.StatusNotFound, code: "not_found", msg: "no route " + req.Path}
	}
	if req.Method != http.MethodGet {
		return nil, &apiError{status: http.StatusMethodNotAllowed, code: "method_not_allowed", msg: req.Method + " is not supported, use GET"}
	}
	if r.data == nil {
		return p.Health(), nil
	}
	f, err := query.ParseFilter(req.Query, time.Now())
	if err != nil {
		return nil, invalidParameter(err)
	}
	p.mu.Lock()
	s, storageErr := p.store, p.storageErr
	p.mu.Unlock()
	if s == nil {
		return nil, &apiError{status: http.StatusServiceUnavailable, code: "storage_unavailable", msg: "database not open: " + storageErr}
	}
	out, err := r.data(s.DB(), f, p.prices(), req.Query)
	if err != nil {
		var pe *query.ParamError
		if errors.As(err, &pe) {
			return nil, invalidParameter(err)
		}
		p.logf("warn", "cpa-analytics: query %s failed: %v", name, err)
		return nil, &apiError{status: http.StatusServiceUnavailable, code: "storage_unavailable", msg: err.Error()}
	}
	return out, nil
}

func invalidParameter(err error) error {
	return &apiError{status: http.StatusBadRequest, code: "invalid_parameter", msg: err.Error()}
}

func jsonResponse(status int, v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode response: %w", err)
	}
	return json.Marshal(pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}},
		Body:       body,
	})
}
