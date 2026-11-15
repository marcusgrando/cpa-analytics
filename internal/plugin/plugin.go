// Package plugin implements the cpa-analytics plugin behind the CLIProxyAPI C ABI.
// The cgo shim in the module root forwards every call to Plugin.Handle.
package plugin

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	pluginName    = "cpa-analytics"
	pluginVersion = "0.1.0"
	// minHostSchema is the first host schema that honors
	// request_interceptor_omit_body_before, so request bodies never reach the plugin.
	minHostSchema uint32 = 7
)

// Host calls back into CLIProxyAPI (host.http.do, host.log).
type Host interface {
	Call(method string, req []byte) ([]byte, error)
}

type handlerFunc func(req []byte) ([]byte, error)

// Plugin holds the runtime state shared by all ABI calls.
type Plugin struct {
	host     Host
	handlers map[string]handlerFunc

	mu  sync.Mutex
	cfg Config
}

// New builds a plugin. host may be nil in tests that do not need callbacks.
func New(host Host) *Plugin {
	p := &Plugin{host: host}
	p.handlers = map[string]handlerFunc{
		pluginabi.MethodPluginRegister:    p.register,
		pluginabi.MethodPluginReconfigure: p.register,
		pluginabi.MethodPluginShutdown:    p.shutdown,
	}
	return p
}

// Handle runs one ABI method and always returns an envelope, including after a panic.
func (p *Plugin) Handle(method string, req []byte) (out []byte) {
	defer func() {
		if r := recover(); r != nil {
			out = errorEnvelope("plugin_panic", fmt.Sprintf("%s panicked: %v", method, r))
		}
	}()
	handler, ok := p.handlers[method]
	if !ok {
		return errorEnvelope("unknown_method", "unknown method: "+method)
	}
	result, err := handler(req)
	if err != nil {
		var coded *codedError
		if asCoded(err, &coded) {
			return errorEnvelope(coded.code, coded.msg)
		}
		return errorEnvelope("plugin_error", err.Error())
	}
	return okEnvelope(result)
}

// Close releases resources. Safe to call more than once.
func (p *Plugin) Close() {}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

type capabilities struct {
	UsagePlugin                      bool `json:"usage_plugin"`
	RequestInterceptor               bool `json:"request_interceptor"`
	RequestInterceptorOmitBodyBefore bool `json:"request_interceptor_omit_body_before"`
	RequestInterceptorOmitBodyAfter  bool `json:"request_interceptor_omit_body_after"`
	ManagementAPI                    bool `json:"management_api"`
}

func (p *Plugin) register(raw []byte) ([]byte, error) {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("decode lifecycle request: %w", err)
		}
	}
	if req.SchemaVersion < minHostSchema {
		return nil, &codedError{code: "unsupported_host", msg: fmt.Sprintf("cpa-analytics needs host plugin schema %d or newer, host sent %d", minHostSchema, req.SchemaVersion)}
	}
	cfg, err := ParseConfig(req.ConfigYAML)
	if err != nil {
		return nil, &codedError{code: "invalid_config", msg: err.Error()}
	}
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
	return json.Marshal(registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata:      metadata(),
		Capabilities: capabilities{
			UsagePlugin:                      true,
			RequestInterceptor:               true,
			RequestInterceptorOmitBodyBefore: true,
			RequestInterceptorOmitBodyAfter:  true,
			ManagementAPI:                    true,
		},
	})
}

func (p *Plugin) shutdown([]byte) ([]byte, error) {
	p.Close()
	return []byte("{}"), nil
}

func metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:             pluginName,
		Version:          pluginVersion,
		Author:           "Azion",
		GitHubRepository: "https://github.com/marcus-grando-azion/cpa-analytics",
		ConfigFields: []pluginapi.ConfigField{
			{Name: "db_path", Type: pluginapi.ConfigFieldTypeString, Default: defaultDBPath, Description: "SQLite file for usage rows. Put it on a mounted volume."},
			{Name: "retention_days", Type: pluginapi.ConfigFieldTypeInteger, Default: 0, Description: "Delete rows older than this many days. 0 keeps everything."},
			{Name: "price_overrides", Type: pluginapi.ConfigFieldTypeObject, Description: "Model name to USD per 1M tokens: input, output, cache_read, cache_write."},
			{Name: "pricing_url", Type: pluginapi.ConfigFieldTypeString, Default: defaultPricingURL, Description: "LiteLLM price table URL, refreshed every 24 hours."},
		},
	}
}
