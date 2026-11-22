package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcusgrando/cpa-analytics/internal/query"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type antigravityMockHost struct {
	mu           sync.Mutex
	summaryCalls int
	summaryBody  func(call int) string
	logs         []string
	authToken    string
}

func (h *antigravityMockHost) Call(method string, req []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodHostLog:
		h.mu.Lock()
		h.logs = append(h.logs, string(req))
		h.mu.Unlock()
		return []byte("{}"), nil
	case pluginabi.MethodHostAuthList:
		return json.Marshal(map[string]any{
			"files": []pluginapi.HostAuthFileEntry{
				{AuthIndex: "ag-1", Provider: "antigravity", Email: "ag@example.com"},
				{AuthIndex: "ag-off", Provider: "antigravity", Disabled: true},
				{AuthIndex: "cl-1", Provider: "claude"},
			},
		})
	case pluginabi.MethodHostAuthGet:
		var r pluginapi.HostAuthGetRequest
		_ = json.Unmarshal(req, &r)
		cred, _ := json.Marshal(map[string]string{"access_token": h.authToken})
		return json.Marshal(pluginapi.HostAuthGetResponse{AuthIndex: r.AuthIndex, JSON: cred})
	case pluginabi.MethodHostHTTPDo:
		var r pluginapi.HTTPRequest
		_ = json.Unmarshal(req, &r)
		switch r.URL {
		case antigravityLoadCodeAssistURL:
			return json.Marshal(pluginapi.HTTPResponse{
				StatusCode: http.StatusOK,
				Body:       []byte(`{"cloudaicompanionProject":"proj-ag-1"}`),
			})
		case antigravityQuotaSummaryURL:
			if r.Headers.Get("Authorization") != "Bearer "+h.authToken {
				return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusUnauthorized})
			}
			if r.Headers.Get("User-Agent") != antigravityUserAgent {
				return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusBadRequest})
			}
			h.mu.Lock()
			h.summaryCalls++
			callNum := h.summaryCalls
			h.mu.Unlock()
			return json.Marshal(pluginapi.HTTPResponse{
				StatusCode: http.StatusOK,
				Body:       []byte(h.summaryBody(callNum)),
			})
		default:
			return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusNotFound})
		}
	default:
		return []byte("{}"), nil
	}
}

func TestAntigravityQuotaPollerFeedsHistoryBurnAndPlans(t *testing.T) {
	t0 := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Hour)
	reset := t0.Add(7 * 24 * time.Hour).Format(time.RFC3339)

	host := &antigravityMockHost{
		authToken: "secret-antigravity-token-xyz",
		summaryBody: func(call int) string {
			gemRem := 0.90
			claudeRem := 0.80
			if call >= 2 {
				gemRem = 0.80    // +10% weekly used for Gemini
				claudeRem = 0.75 // +5% weekly used for Claude and GPT
			}
			b, _ := json.Marshal(map[string]any{
				"groups": []map[string]any{
					{
						"displayName": "Gemini Models",
						"buckets": []map[string]any{
							{"window": "5h", "remainingFraction": 0.5, "resetTime": t0.Add(5 * time.Hour).Format(time.RFC3339)},
							{"window": "weekly", "remainingFraction": gemRem, "resetTime": reset},
						},
					},
					{
						"displayName": "Claude and GPT models",
						"buckets": []map[string]any{
							{"window": "weekly", "remainingFraction": claudeRem, "resetTime": reset},
						},
					},
				},
			})
			return string(b)
		},
	}

	p := New(host)
	defer p.Close()

	dbPath := filepath.Join(t.TempDir(), "ag.db")
	cfgYAML := "db_path: " + dbPath + "\nprice_overrides:\n  gemini-3-pro:\n    input: 1.0\n    output: 2.0\n  claude-sonnet-4-6:\n    input: 3.0\n    output: 15.0\n"
	regReq, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte(cfgYAML), SchemaVersion: pluginabi.SchemaVersion})
	if out := p.Handle(pluginabi.MethodPluginRegister, regReq); !strings.Contains(string(out), `"ok":true`) {
		t.Fatalf("register failed: %s", out)
	}

	// Wait for initial poll from register().
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p.Health().QuotaPoller.Polls >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := p.Health().QuotaPoller; st.Polls < 1 || st.Snapshots < 3 || st.LastError != "" {
		t.Fatalf("unexpected poller health after startup: %+v", st)
	}

	// Record one Gemini request and one Claude request on the same Antigravity account.
	now := time.Now().UTC()
	for _, rec := range []pluginapi.UsageRecord{
		{
			RequestID: "ag-req-1", TraceID: "t1", RequestedAt: now,
			Provider: "antigravity", ExecutorType: "AntigravityExecutor",
			Model: "gemini-3-pro", AuthIndex: "ag-1", Generate: true,
			Detail: pluginapi.UsageDetail{InputTokens: 1_000_000, OutputTokens: 500_000, TotalTokens: 1_500_000},
		},
		{
			RequestID: "ag-req-2", TraceID: "t2", RequestedAt: now.Add(time.Second),
			Provider: "antigravity", ExecutorType: "AntigravityExecutor",
			Model: "claude-sonnet-4-6", AuthIndex: "ag-1", Generate: true,
			Detail: pluginapi.UsageDetail{InputTokens: 1_000_000, OutputTokens: 200_000, TotalTokens: 1_200_000},
		},
	} {
		raw, _ := json.Marshal(rec)
		if out := p.Handle(pluginabi.MethodUsageHandle, raw); !strings.Contains(string(out), `"ok":true`) {
			t.Fatalf("usage.handle failed: %s", out)
		}
	}
	// Flush writer so rows are committed before the second poll.
	p.mu.Lock()
	w := p.writer
	qp := p.poller
	p.mu.Unlock()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && w.Stats().Written < 2 {
		time.Sleep(10 * time.Millisecond)
	}

	// Run a second poll after the requests.
	qp.now = func() time.Time { return now.Add(2 * time.Second) }
	qp.pollAll(nil)

	from := now.Add(-1 * time.Hour).UnixMilli()
	to := now.Add(1 * time.Hour).UnixMilli()
	q := url.Values{"from": {itoa64(from)}, "to": {itoa64(to)}}

	// 1. limit-history returns separate weekly series per Antigravity group.
	var hist query.HistoryResult
	callManagementJSON(t, p, "limit-history", q, &hist)
	if len(hist.Accounts) != 2 {
		t.Fatalf("limit-history accounts = %d, want 2: %+v", len(hist.Accounts), hist.Accounts)
	}

	// 2. limit-burn returns separate points per Antigravity group.
	var burn query.BurnResult
	callManagementJSON(t, p, "limit-burn", q, &burn)
	if len(burn.Points) != 2 {
		t.Fatalf("limit-burn points = %d, want 2: %+v", len(burn.Points), burn.Points)
	}

	// 3. plan-value keeps Antigravity groups separate with independent measured quota.
	var pv planValueReport
	callManagementJSON(t, p, "plan-value", q, &pv)
	if len(pv.Accounts) != 2 {
		t.Fatalf("plan-value accounts = %d, want 2: %+v", len(pv.Accounts), pv.Accounts)
	}
	byGroup := map[string]query.AccountValue{}
	for _, a := range pv.Accounts {
		byGroup[a.Group] = a
	}
	if g := byGroup["gemini"]; !g.QuotaMeasured || g.WeeklyPct < 9.9 || g.WeeklyPct > 10.1 || g.MeasuredCostUSD < 1.99 || g.MeasuredCostUSD > 2.01 {
		t.Errorf("gemini plan-value = %+v, want ~10%% and ~$2", g)
	}
	if c := byGroup["claude-and-gpt"]; !c.QuotaMeasured || c.WeeklyPct < 4.9 || c.WeeklyPct > 5.1 || c.MeasuredCostUSD < 5.99 || c.MeasuredCostUSD > 6.01 {
		t.Errorf("claude-and-gpt plan-value = %+v, want ~5%% and ~$6", c)
	}

	// Verify the OAuth token never appeared in any host log.
	host.mu.Lock()
	for _, line := range host.logs {
		if strings.Contains(line, host.authToken) {
			t.Errorf("access_token leaked into host.log: %s", line)
		}
	}
	host.mu.Unlock()
}

func itoa64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func callManagementJSON(t *testing.T, p *Plugin, route string, q url.Values, dst any) {
	t.Helper()
	req, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   managementBase + route,
		Query:  q,
	})
	raw := p.Handle(pluginabi.MethodManagementHandle, req)
	var env struct {
		OK     bool                       `json:"ok"`
		Result pluginapi.ManagementResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK || env.Result.StatusCode != http.StatusOK {
		t.Fatalf("%s failed: %s", route, raw)
	}
	if err := json.Unmarshal(env.Result.Body, dst); err != nil {
		t.Fatalf("decode %s body: %v (%s)", route, err, env.Result.Body)
	}
}
