package plugin

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeEnvelope(t *testing.T, raw []byte) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope %q: %v", raw, err)
	}
	return env
}

func lifecycle(t *testing.T, schema uint32, configYAML string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"config_yaml": []byte(configYAML), "schema_version": schema})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRegisterDeclaresCapabilities(t *testing.T) {
	p := New(nil)
	defer p.Close()
	cfg := "enabled: true\ndb_path: " + t.TempDir() + "/a.db\n"
	env := decodeEnvelope(t, p.Handle(pluginabi.MethodPluginRegister, lifecycle(t, pluginabi.SchemaVersion, cfg)))
	if !env.OK {
		t.Fatalf("register failed: %+v", env.Error)
	}
	var reg struct {
		SchemaVersion uint32 `json:"schema_version"`
		Metadata      struct {
			Name         string
			ConfigFields []struct{ Name string }
		} `json:"metadata"`
		Capabilities map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	if reg.Metadata.Name != "cpa-analytics" {
		t.Errorf("name = %q", reg.Metadata.Name)
	}
	if reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Errorf("schema_version = %d", reg.SchemaVersion)
	}
	for _, key := range []string{"usage_plugin", "request_interceptor", "request_interceptor_omit_body_before", "management_api"} {
		if reg.Capabilities[key] != true {
			t.Errorf("capability %s = %v", key, reg.Capabilities[key])
		}
	}
	got := map[string]bool{}
	for _, f := range reg.Metadata.ConfigFields {
		got[f.Name] = true
	}
	for _, name := range []string{"db_path", "retention_days", "price_overrides", "pricing_url"} {
		if !got[name] {
			t.Errorf("missing config field %s", name)
		}
	}
}

func TestRegisterRejectsOldHostSchema(t *testing.T) {
	p := New(nil)
	defer p.Close()
	env := decodeEnvelope(t, p.Handle(pluginabi.MethodPluginRegister, lifecycle(t, 6, "")))
	if env.OK || env.Error == nil || env.Error.Code != "unsupported_host" {
		t.Fatalf("expected unsupported_host error, got %+v", env)
	}
}

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := ParseConfig([]byte("enabled: true\npriority: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBPath != "/CLIProxyAPI/data/cpa-analytics.db" {
		t.Errorf("db_path = %q", cfg.DBPath)
	}
	if cfg.PricingURL != "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json" {
		t.Errorf("pricing_url = %q", cfg.PricingURL)
	}
	if cfg.RetentionDays != 0 {
		t.Errorf("retention_days = %d", cfg.RetentionDays)
	}
}

func TestParseConfigRejectsNegativeRetention(t *testing.T) {
	if _, err := ParseConfig([]byte("retention_days: -1\n")); err == nil {
		t.Fatal("expected error for negative retention_days")
	}
}

func TestParseConfigRejectsNegativePriceOverride(t *testing.T) {
	if _, err := ParseConfig([]byte("price_overrides:\n  my-model:\n    input: -1\n")); err == nil {
		t.Fatal("expected error for negative override price")
	}
}

func TestHandleUnknownMethod(t *testing.T) {
	p := New(nil)
	defer p.Close()
	env := decodeEnvelope(t, p.Handle("nope.nope", nil))
	if env.OK || env.Error == nil || env.Error.Code != "unknown_method" {
		t.Fatalf("got %+v", env)
	}
}

func TestHandleRecoversPanic(t *testing.T) {
	p := New(nil)
	defer p.Close()
	p.handlers["test.panic"] = func([]byte) ([]byte, error) { panic("boom") }
	env := decodeEnvelope(t, p.Handle("test.panic", nil))
	if env.OK || env.Error == nil || env.Error.Code != "plugin_panic" {
		t.Fatalf("got %+v", env)
	}
}
