// Package plugin implements the cpa-analytics plugin behind the CLIProxyAPI C ABI.
// The cgo shim in the module root forwards every call to Plugin.Handle.
package plugin

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marcusgrando/cpa-analytics/internal/client"
	"github.com/marcusgrando/cpa-analytics/internal/pricing"
	"github.com/marcusgrando/cpa-analytics/internal/store"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	pluginName    = "cpa-analytics"
	pluginVersion = "0.1.10"
	// minHostSchema is the first host schema that honors
	// request_interceptor_omit_body_before, so request bodies never reach the plugin.
	minHostSchema uint32 = 7

	traceTTL      = 10 * time.Minute
	traceCapacity = 50_000
	queueCapacity = 10_000
	flushEvery    = time.Second
	batchSize     = 500
	pruneEvery    = 24 * time.Hour
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

	traces *client.Traces

	mu         sync.Mutex
	cfg        Config
	store      *store.Store
	writer     *store.Writer
	storageErr string
	refresher  *pricing.Refresher
	pruneStop  chan struct{}

	// droppedNoStore counts records received while the database could not be opened.
	droppedNoStore atomic.Uint64
}

// Health is what the plugin reports about itself.
type Health struct {
	DBPath        string            `json:"db_path"`
	StorageError  string            `json:"storage_error,omitempty"`
	RetentionDays int               `json:"retention_days"`
	Writer        store.WriterStats `json:"writer"`
	TraceHits     uint64            `json:"trace_hits"`
	TraceMisses   uint64            `json:"trace_misses"`
	Pricing       pricing.Status    `json:"pricing"`
}

// New builds a plugin. host may be nil in tests that do not need callbacks.
func New(host Host) *Plugin {
	p := &Plugin{host: host, traces: client.NewTraces(traceTTL, traceCapacity, time.Now)}
	p.handlers = map[string]handlerFunc{
		pluginabi.MethodPluginRegister:         p.register,
		pluginabi.MethodPluginReconfigure:      p.register,
		pluginabi.MethodPluginShutdown:         p.shutdown,
		pluginabi.MethodUsageHandle:            p.handleUsage,
		pluginabi.MethodRequestInterceptBefore: p.interceptBefore,
		pluginabi.MethodRequestInterceptAfter:  passThrough,
		pluginabi.MethodManagementRegister:     p.managementRegister,
		pluginabi.MethodManagementHandle:       p.managementHandle,
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

// Close flushes queued rows and closes the database. Safe to call more than once.
func (p *Plugin) Close() {
	p.mu.Lock()
	w, s, r, prune := p.writer, p.store, p.refresher, p.pruneStop
	p.writer, p.store, p.refresher, p.pruneStop = nil, nil, nil, nil
	p.mu.Unlock()
	if prune != nil {
		close(prune)
	}
	r.Stop()
	w.Close()
	if s != nil {
		s.Close()
	}
}

// Health returns storage, writer, join and pricing statistics.
func (p *Plugin) Health() Health {
	p.mu.Lock()
	h := Health{DBPath: p.cfg.DBPath, StorageError: p.storageErr, RetentionDays: p.cfg.RetentionDays, Writer: p.writer.Stats()}
	p.mu.Unlock()
	h.Pricing = p.pricingStatus()
	h.Writer.Dropped += p.droppedNoStore.Load()
	h.TraceHits, h.TraceMisses = p.traces.Stats()
	return h
}

func (p *Plugin) currentWriter() *store.Writer {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.writer
}

// openStorage (re)opens the database when the path changed or a previous
// open failed. A failure is recorded for health and never fails registration.
func (p *Plugin) openStorage(path string) {
	p.mu.Lock()
	if p.store != nil && p.cfg.DBPath == path {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	p.Close()
	s, err := store.Open(path)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.storageErr = err.Error()
		p.logf("error", "cpa-analytics: storage unavailable: %v", err)
		return
	}
	p.storageErr = ""
	p.store = s
	p.writer = store.NewWriter(s, queueCapacity, flushEvery, batchSize, func(format string, args ...any) {
		p.logf("warn", format, args...)
	})
}

// logf sends a log line through host.log. It never fails the caller.
func (p *Plugin) logf(level, format string, args ...any) {
	if p.host == nil {
		return
	}
	raw, err := json.Marshal(map[string]any{"level": level, "message": fmt.Sprintf(format, args...)})
	if err != nil {
		return
	}
	go func() {
		defer func() { _ = recover() }()
		_, _ = p.host.Call(pluginabi.MethodHostLog, raw)
	}()
}

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
	p.openStorage(cfg.DBPath)
	p.mu.Lock()
	restartPricing := p.refresher == nil || p.cfg.PricingURL != cfg.PricingURL
	p.cfg = cfg
	var kv pricing.KV
	if p.store != nil {
		kv = p.store
	}
	p.mu.Unlock()
	if restartPricing {
		p.startPricing(cfg.PricingURL, kv)
	}
	p.startPrune()
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
		Author:           "Marcus Grando",
		GitHubRepository: "https://github.com/marcusgrando/cpa-analytics",
		ConfigFields: []pluginapi.ConfigField{
			{Name: "db_path", Type: pluginapi.ConfigFieldTypeString, Default: defaultDBPath, Description: "SQLite file for usage rows. Put it on a mounted volume."},
			{Name: "retention_days", Type: pluginapi.ConfigFieldTypeInteger, Default: 0, Description: "Delete rows older than this many days. 0 keeps everything."},
			{Name: "price_overrides", Type: pluginapi.ConfigFieldTypeObject, Description: "Model name to USD per 1M tokens: input, output, cache_read, cache_write."},
			{Name: "pricing_url", Type: pluginapi.ConfigFieldTypeString, Default: defaultPricingURL, Description: "LiteLLM price table URL, refreshed every 24 hours."},
		},
	}
}
