package plugin

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marcusgrando/cpa-analytics/internal/store"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const apiBase = "/v0/management/cpa-analytics/"

func manage(t *testing.T, p *Plugin, method, route string, q url.Values) pluginapi.ManagementResponse {
	t.Helper()
	env := call(t, p, pluginabi.MethodManagementHandle, map[string]any{
		"Method": method, "Path": apiBase + route, "Query": q, "host_callback_id": "cb-1",
	})
	if !env.OK {
		t.Fatalf("management.handle %s: %+v", route, env.Error)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func errorCode(t *testing.T, resp pluginapi.ManagementResponse) (string, string) {
	t.Helper()
	var body struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("error body %q: %v", resp.Body, err)
	}
	return body.Error.Code, body.Error.Message
}

func insertRows(t *testing.T, p *Plugin, rows ...store.Row) {
	t.Helper()
	p.mu.Lock()
	s := p.store
	p.mu.Unlock()
	if err := s.InsertBatch(rows); err != nil {
		t.Fatal(err)
	}
}

func usageRow(id string, at time.Time) store.Row {
	return store.Row{RequestID: id, TraceID: id, RequestedAt: at.UnixMilli(), Provider: "codex", Model: "gpt-5",
		Generate: true, ClientName: "codex", ClientVersion: "0.130.0", UncachedInput: 100, OutputNonReasoning: 10}
}

func TestManagementRegisterListsRoutes(t *testing.T) {
	p, _ := startPlugin(t)
	env := call(t, p, pluginabi.MethodManagementRegister, map[string]any{"BasePath": "/v0/management"})
	if !env.OK {
		t.Fatalf("register: %+v", env.Error)
	}
	var reg struct {
		Routes []struct{ Method, Path string } `json:"routes"`
	}
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range reg.Routes {
		if r.Method != "GET" {
			t.Errorf("route %s method %s", r.Path, r.Method)
		}
		got[r.Path] = true
	}
	for _, name := range []string{"summary", "timeseries", "drift", "breakdown", "health", "pricing", "limit-burn", "limit-history", "limit-leftover", "affinity", "options"} {
		if !got[apiBase+name] {
			t.Errorf("missing route %s in %+v", name, reg.Routes)
		}
	}
}

func TestManagementOptions(t *testing.T) {
	p, _ := startPlugin(t)
	from := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	a := usageRow("a", from)
	a.ResponseModel, a.APIKeyHash = "gpt-5-reported", "key-a"
	b := a
	b.RequestID, b.ClientVersion = "b", "0.131.0"
	c := usageRow("c", from.Add(time.Hour))
	c.Provider, c.Model, c.ClientName, c.APIKeyHash = "claude", "claude-sonnet", "claude-code", "key-b"
	c.Failed, c.Generate = true, false
	unknown := usageRow("unknown", from.Add(time.Hour))
	unknown.Provider, unknown.Model, unknown.ClientName = "", "", ""
	old := usageRow("old", from.Add(-time.Millisecond))
	old.Provider, old.Model, old.ClientName = "old", "old", "old"
	future := old
	future.RequestID, future.RequestedAt = "future", to.UnixMilli()
	insertRows(t, p, a, b, c, unknown, old, future)
	type options struct {
		Providers []string `json:"providers"`
		Models    []string `json:"models"`
		Clients   []string `json:"clients"`
	}
	for _, tc := range []struct {
		name, param, value string
		want               options
	}{
		{"all rows including failed and non-generating", "", "", options{[]string{"claude", "codex"}, []string{"claude-sonnet", "gpt-5-reported"}, []string{"claude-code", "codex"}}},
		{"API key isolation", "api_key", "key-a", options{[]string{"codex"}, []string{"gpt-5-reported"}, []string{"codex"}}},
		{"reported model", "model", "gpt-5-reported", options{[]string{"codex"}, []string{"gpt-5-reported"}, []string{"codex"}}},
		{"no matches", "provider", "absent", options{[]string{}, []string{}, []string{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := url.Values{"from": {strconv.FormatInt(from.UnixMilli(), 10)}, "to": {strconv.FormatInt(to.UnixMilli(), 10)}}
			if tc.param != "" {
				q.Set(tc.param, tc.value)
			}
			resp := manage(t, p, "GET", "options", q)
			var got options
			if resp.StatusCode != 200 || json.Unmarshal(resp.Body, &got) != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("options: status %d body %s, want %+v", resp.StatusCode, resp.Body, tc.want)
			}
		})
	}
	resp := manage(t, p, "GET", "options", url.Values{"from": {"invalid"}})
	if code, _ := errorCode(t, resp); resp.StatusCode != 400 || code != "invalid_parameter" {
		t.Fatalf("invalid options filter: %d %s", resp.StatusCode, resp.Body)
	}
	if resp := manage(t, p, "POST", "options", nil); resp.StatusCode != 405 {
		t.Fatalf("options accepts writes: %d", resp.StatusCode)
	}
	p.store.Close()
	resp = manage(t, p, "GET", "options", nil)
	if code, _ := errorCode(t, resp); resp.StatusCode != 503 || code != "storage_unavailable" {
		t.Fatalf("closed options storage: %d %s", resp.StatusCode, resp.Body)
	}
}

func TestManagementSummaryReturnsJSON(t *testing.T) {
	p, _ := startPlugin(t)
	now := time.Now()
	insertRows(t, p, usageRow("a", now.Add(-time.Hour)), usageRow("b", now.Add(-2*time.Hour)))
	resp := manage(t, p, "GET", "summary", nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Headers.Get("Content-Type"), "application/json") {
		t.Fatalf("status %d headers %v body %s", resp.StatusCode, resp.Headers, resp.Body)
	}
	var sum struct {
		Totals struct {
			Records       int64 `json:"records"`
			UncachedInput int64 `json:"uncached_input"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(resp.Body, &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Totals.Records != 2 || sum.Totals.UncachedInput != 200 {
		t.Errorf("summary = %s", resp.Body)
	}
}

func TestManagementRoutesAnswer(t *testing.T) {
	p, _ := startPlugin(t)
	insertRows(t, p, usageRow("a", time.Now().Add(-time.Hour)))
	for _, route := range []string{"timeseries", "drift", "breakdown", "health", "pricing", "limit-burn", "limit-history", "limit-leftover", "affinity"} {
		resp := manage(t, p, "GET", route, nil)
		if resp.StatusCode != 200 || !json.Valid(resp.Body) {
			t.Errorf("%s: status %d body %s", route, resp.StatusCode, resp.Body)
		}
	}
}

func TestManagementRejectsBadParameters(t *testing.T) {
	p, _ := startPlugin(t)
	cases := []struct {
		route string
		q     url.Values
		param string
	}{
		{"summary", url.Values{"from": {"abc"}}, "from"},
		{"summary", url.Values{"from": {"2000"}, "to": {"1000"}}, "from"},
		{"timeseries", url.Values{"group": {"nope"}}, "group"},
		{"timeseries", url.Values{"bucket": {"week"}}, "bucket"},
		{"drift", url.Values{"group": {"nope"}}, "group"},
		{"breakdown", url.Values{"by": {"nope"}}, "by"},
		{"limit-burn", url.Values{"model": {"gpt-5"}}, "model"},
		{"limit-history", url.Values{"client": {"codex"}}, "client"},
		{"limit-leftover", url.Values{"api_key": {"abc123"}}, "api_key"},
	}
	for _, c := range cases {
		resp := manage(t, p, "GET", c.route, c.q)
		code, msg := errorCode(t, resp)
		if resp.StatusCode != 400 || code != "invalid_parameter" || !strings.Contains(msg, strconv.Quote(c.param)) {
			t.Errorf("%s %v: %d %s %q", c.route, c.q, resp.StatusCode, code, msg)
		}
	}
}

func TestManagementReportsStorageFaults(t *testing.T) {
	p, _ := startPlugin(t)
	p.mu.Lock()
	p.store.Close()
	p.mu.Unlock()
	resp := manage(t, p, "GET", "summary", nil)
	if code, _ := errorCode(t, resp); resp.StatusCode != 503 || code != "storage_unavailable" {
		t.Errorf("closed db: %d %s", resp.StatusCode, resp.Body)
	}
}

func TestManagementWithoutStorage(t *testing.T) {
	p := New(nil)
	defer p.Close()
	// A regular file where the database directory should be: MkdirAll fails, even as root.
	parent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parent, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := "db_path: " + parent + "/a.db\n"
	if env := decodeEnvelope(t, p.Handle(pluginabi.MethodPluginRegister, lifecycle(t, pluginabi.SchemaVersion, cfg))); !env.OK {
		t.Fatalf("register: %+v", env.Error)
	}
	resp := manage(t, p, "GET", "drift", nil)
	if code, msg := errorCode(t, resp); resp.StatusCode != 503 || code != "storage_unavailable" || msg == "" {
		t.Errorf("no storage: %d %s", resp.StatusCode, resp.Body)
	}
	// Health still answers so the dashboard can show why.
	resp = manage(t, p, "GET", "health", nil)
	var h struct {
		StorageError string `json:"storage_error"`
	}
	if err := json.Unmarshal(resp.Body, &h); err != nil || resp.StatusCode != 200 || h.StorageError == "" {
		t.Errorf("health: %d %s", resp.StatusCode, resp.Body)
	}
}

func TestManagementUnknownRouteAndMethod(t *testing.T) {
	p, _ := startPlugin(t)
	if resp := manage(t, p, "GET", "nope", nil); resp.StatusCode != 404 {
		t.Errorf("unknown route: %d", resp.StatusCode)
	}
	if resp := manage(t, p, "POST", "summary", nil); resp.StatusCode != 405 {
		t.Errorf("POST: %d", resp.StatusCode)
	}
}

func TestHealthIncludesPricingAndRetention(t *testing.T) {
	p, _ := startPluginWith(t, "retention_days: 30\n")
	resp := manage(t, p, "GET", "health", nil)
	var h struct {
		RetentionDays int `json:"retention_days"`
		Pricing       struct {
			URL       string `json:"url"`
			LastError string `json:"last_error"`
		} `json:"pricing"`
	}
	if err := json.Unmarshal(resp.Body, &h); err != nil {
		t.Fatal(err)
	}
	if h.RetentionDays != 30 || h.Pricing.URL == "" {
		t.Errorf("health = %s", resp.Body)
	}
}

func TestRetentionPrunesOldRows(t *testing.T) {
	p, _ := startPluginWith(t, "retention_days: 1\n")
	now := time.Now()
	insertRows(t, p, usageRow("old", now.Add(-49*time.Hour)), usageRow("new", now.Add(-time.Hour)))
	p.pruneOnce()
	var n int
	p.mu.Lock()
	err := p.store.DB().QueryRow(`SELECT COUNT(*) FROM requests WHERE request_id = 'old'`).Scan(&n)
	p.mu.Unlock()
	if err != nil || n != 0 {
		t.Errorf("old rows left = %d, err %v", n, err)
	}
	p.mu.Lock()
	err = p.store.DB().QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n)
	p.mu.Unlock()
	if err != nil || n != 1 {
		t.Errorf("rows = %d, want the recent one kept", n)
	}
}

func TestRetentionZeroKeepsEverything(t *testing.T) {
	p, _ := startPlugin(t)
	insertRows(t, p, usageRow("ancient", time.Now().AddDate(-3, 0, 0)))
	p.pruneOnce()
	var n int
	p.mu.Lock()
	err := p.store.DB().QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n)
	p.mu.Unlock()
	if err != nil || n != 1 {
		t.Errorf("rows = %d, err %v", n, err)
	}
}

func TestDashboardResourceIsServed(t *testing.T) {
	p, _ := startPlugin(t)
	env := call(t, p, pluginabi.MethodManagementRegister, map[string]any{})
	var reg struct {
		Resources []struct{ Path, Menu string } `json:"resources"`
	}
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	if len(reg.Resources) != 1 || reg.Resources[0].Path != dashboardPath || reg.Resources[0].Menu != "Analytics" {
		t.Fatalf("resources = %+v", reg.Resources)
	}
	env = call(t, p, pluginabi.MethodManagementHandle, map[string]any{"Method": "GET", "Path": dashboardPath})
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Headers.Get("Content-Type") != "text/html; charset=utf-8" || !strings.Contains(string(resp.Body), `id="drift"`) {
		t.Errorf("dashboard: %d %v", resp.StatusCode, resp.Headers)
	}
	if !strings.Contains(resp.Headers.Get("Content-Security-Policy"), "connect-src 'self'") {
		t.Errorf("CSP = %q", resp.Headers.Get("Content-Security-Policy"))
	}
}

// Lowering retention_days on a running plugin prunes now, not at the next
// daily tick.
func TestReconfigureAppliesRetentionNow(t *testing.T) {
	p, path := startPlugin(t)
	insertRows(t, p, usageRow("old", time.Now().Add(-49*time.Hour)))
	env := decodeEnvelope(t, p.Handle(pluginabi.MethodPluginReconfigure, lifecycle(t, pluginabi.SchemaVersion, "db_path: "+path+"\nretention_days: 1\n")))
	if !env.OK {
		t.Fatalf("reconfigure: %+v", env.Error)
	}
	var n int
	for i := 0; i < 100; i++ {
		p.mu.Lock()
		err := p.store.DB().QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n)
		p.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("rows = %d two seconds after lowering retention", n)
}

// Close waits for the retention job. A prune still running after Close would
// reopen the database files once the host believes the plugin is gone; here
// that shows up as files appearing while TempDir removes the directory.
func TestCloseWaitsForRetentionJob(t *testing.T) {
	for i := 0; i < 100; i++ {
		dir := t.TempDir()
		p := New(nil)
		env := decodeEnvelope(t, p.Handle(pluginabi.MethodPluginRegister, lifecycle(t, pluginabi.SchemaVersion, "db_path: "+filepath.Join(dir, "a.db")+"\nretention_days: 1\n")))
		if !env.OK {
			t.Fatalf("register: %+v", env.Error)
		}
		p.Close()
		if err := os.RemoveAll(dir); err != nil {
			t.Fatalf("iteration %d: database files changed after Close: %v", i, err)
		}
	}
}
