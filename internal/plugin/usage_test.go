package plugin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcus-grando-azion/cpa-analytics/internal/store"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// startPlugin registers a plugin with a temp database and returns it with the db path.
func startPlugin(t *testing.T) (*Plugin, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "a.db")
	p := New(nil)
	env := decodeEnvelope(t, p.Handle(pluginabi.MethodPluginRegister, lifecycle(t, pluginabi.SchemaVersion, "db_path: "+path+"\n")))
	if !env.OK {
		t.Fatalf("register: %+v", env.Error)
	}
	t.Cleanup(p.Close)
	return p, path
}

func call(t *testing.T, p *Plugin, method string, v any) envelope {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return decodeEnvelope(t, p.Handle(method, raw))
}

// rowsAfterClose flushes the writer by closing the plugin and reads rows back.
func rowsAfterClose(t *testing.T, p *Plugin, path string) map[string]store.Row {
	t.Helper()
	p.Close()
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rs, err := s.DB().Query(`SELECT request_id, trace_id, client_name, client_version, user_agent, api_key_hash, status_code, failed, uncached_input, cache_read, output_reasoning, accounting_quality, response_model FROM requests`)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	out := map[string]store.Row{}
	for rs.Next() {
		var r store.Row
		if err := rs.Scan(&r.RequestID, &r.TraceID, &r.ClientName, &r.ClientVersion, &r.UserAgent, &r.APIKeyHash, &r.StatusCode, &r.Failed, &r.UncachedInput, &r.CacheRead, &r.OutputReasoning, &r.AccountingQuality, &r.ResponseModel); err != nil {
			t.Fatal(err)
		}
		out[r.RequestID] = r
	}
	return out
}

func codexRecord(requestID, traceID string) pluginapi.UsageRecord {
	return pluginapi.UsageRecord{
		RequestID: requestID, TraceID: traceID, Provider: "codex", ExecutorType: "CodexExecutor",
		Model: "gpt-5.5", ResponseModel: "gpt-5.5-2026-10-01", Generate: true,
		RequestedAt: time.Unix(1_800_000_000, 0), Latency: 2 * time.Second,
		Detail: pluginapi.UsageDetail{InputTokens: 1000, CacheReadTokens: 800, CachedTokens: 800, OutputTokens: 200, ReasoningTokens: 50},
	}
}

func TestUsageJoinsUserAgentByTrace(t *testing.T) {
	p, path := startPlugin(t)
	env := call(t, p, pluginabi.MethodRequestInterceptBefore, pluginapi.RequestInterceptRequest{
		RequestID: "x", TraceID: "abcd1234", Headers: http.Header{"User-Agent": {"codex_cli_rs/0.130.0 (Linux)"}},
	})
	if !env.OK || string(env.Result) != "{}" {
		t.Fatalf("intercept = %+v %s", env.Error, env.Result)
	}
	if env := call(t, p, pluginabi.MethodUsageHandle, codexRecord("r1", "abcd1234")); !env.OK {
		t.Fatalf("usage: %+v", env.Error)
	}
	r := rowsAfterClose(t, p, path)["r1"]
	if r.ClientName != "codex" || r.ClientVersion != "0.130.0" || r.UserAgent != "codex_cli_rs/0.130.0 (Linux)" {
		t.Fatalf("row = %+v", r)
	}
	if r.UncachedInput != 200 || r.CacheRead != 800 || r.OutputReasoning != 50 || r.AccountingQuality != "complete" {
		t.Fatalf("buckets = %+v", r)
	}
	if r.ResponseModel != "gpt-5.5-2026-10-01" {
		t.Fatalf("response model = %q", r.ResponseModel)
	}
}

func TestUsageWithUnknownTraceIsStoredWithoutClient(t *testing.T) {
	p, path := startPlugin(t)
	call(t, p, pluginabi.MethodUsageHandle, codexRecord("r1", "never-seen"))
	r, ok := rowsAfterClose(t, p, path)["r1"]
	if !ok || r.ClientName != "" || r.UserAgent != "" {
		t.Fatalf("row = %+v, found %v", r, ok)
	}
}

func TestUsageReplayKeepsOneRow(t *testing.T) {
	p, path := startPlugin(t)
	call(t, p, pluginabi.MethodUsageHandle, codexRecord("r1", "t"))
	call(t, p, pluginabi.MethodUsageHandle, codexRecord("r1", "t"))
	if n := len(rowsAfterClose(t, p, path)); n != 1 {
		t.Fatalf("rows = %d", n)
	}
}

func TestAPIKeyStoredOnlyAsHash(t *testing.T) {
	p, path := startPlugin(t)
	rec := codexRecord("r1", "t")
	rec.APIKey = "sk-very-secret-key"
	call(t, p, pluginabi.MethodUsageHandle, rec)
	r := rowsAfterClose(t, p, path)["r1"]
	if len(r.APIKeyHash) != 12 {
		t.Fatalf("hash = %q", r.APIKeyHash)
	}
	for _, f := range []string{path, path + "-wal"} {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if bytes.Contains(data, []byte("sk-very-secret-key")) {
			t.Fatalf("raw key found in %s", f)
		}
	}
}

func TestFailedRecordKeepsStatusWithoutBody(t *testing.T) {
	p, path := startPlugin(t)
	rec := codexRecord("r1", "t")
	rec.Failed = true
	rec.Failure = pluginapi.UsageFailure{StatusCode: 429, Body: "secret upstream body"}
	call(t, p, pluginabi.MethodUsageHandle, rec)
	r := rowsAfterClose(t, p, path)["r1"]
	if !r.Failed || r.StatusCode != 429 {
		t.Fatalf("row = %+v", r)
	}
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte("secret upstream body")) {
		t.Fatal("failure body stored")
	}
}

func TestUsageWithUnwritableDBIsAcceptedAndCounted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	p := New(nil)
	defer p.Close()
	env := decodeEnvelope(t, p.Handle(pluginabi.MethodPluginRegister, lifecycle(t, pluginabi.SchemaVersion, "db_path: "+dir+"/sub/a.db\n")))
	if !env.OK {
		t.Fatalf("register must succeed even when storage fails: %+v", env.Error)
	}
	if env := call(t, p, pluginabi.MethodUsageHandle, codexRecord("r1", "t")); !env.OK {
		t.Fatalf("usage: %+v", env.Error)
	}
	h := p.Health()
	if h.StorageError == "" || h.Writer.Dropped != 1 {
		t.Fatalf("health = %+v", h)
	}
}

func TestInterceptAfterIsPassThrough(t *testing.T) {
	p, _ := startPlugin(t)
	env := call(t, p, pluginabi.MethodRequestInterceptAfter, pluginapi.RequestInterceptRequest{RequestID: "x"})
	if !env.OK || string(env.Result) != "{}" {
		t.Fatalf("got %+v %s", env.Error, env.Result)
	}
}
