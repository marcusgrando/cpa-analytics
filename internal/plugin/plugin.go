// Package plugin implements the cpa-analytics plugin behind the CLIProxyAPI C ABI.
// The cgo shim in the module root forwards every call to Plugin.Handle.
package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
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
	pluginVersion = "0.4.1"
	// minHostSchema is the first host schema that honors
	// request_interceptor_omit_body_before, so request bodies never reach the plugin.
	minHostSchema uint32 = 7

	traceTTL      = 10 * time.Minute
	traceCapacity = 50_000
	queueCapacity = 10_000
	flushEvery    = time.Second
	batchSize     = 500
	pruneEvery    = 24 * time.Hour
	httpTimeout   = 30 * time.Second
	logQueueSize  = 64
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

	lifecycleMu sync.Mutex
	closed      bool

	logMu   sync.Mutex
	logCh   chan []byte
	logDone chan struct{}
	logOff  bool

	mu          sync.Mutex
	cfg         Config
	store       *store.Store
	writer      *store.Writer
	storageErr  string
	refresher   *pricing.Refresher
	pricingStop chan struct{}
	poller      *quotaPoller
	pruneStop   chan struct{}
	// pruning counts running prune jobs. Close waits for them before it
	// closes the store, so no query outlives the plugin.
	pruning sync.WaitGroup

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
	QuotaPoller   QuotaPollerStatus `json:"quota_poller"`
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
	if host != nil {
		p.logCh = make(chan []byte, logQueueSize)
		p.logDone = make(chan struct{})
		go p.runLogger()
	}
	return p
}

func (p *Plugin) runLogger() {
	defer close(p.logDone)
	for raw := range p.logCh {
		func() {
			defer func() { _ = recover() }()
			_, _ = p.host.Call(pluginabi.MethodHostLog, raw)
		}()
	}
}

func (p *Plugin) stopLogger() {
	p.logMu.Lock()
	if p.logCh == nil || p.logOff {
		done := p.logDone
		p.logMu.Unlock()
		if done != nil {
			<-done
		}
		return
	}
	p.logOff = true
	close(p.logCh)
	done := p.logDone
	p.logMu.Unlock()
	<-done
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
	p.lifecycleMu.Lock()
	p.closed = true
	p.closeResourcesLocked()
	p.lifecycleMu.Unlock()
	p.stopLogger()
}

// closeResourcesLocked stops background jobs and closes the store. Callers must
// hold p.lifecycleMu.
func (p *Plugin) closeResourcesLocked() {
	p.mu.Lock()
	w, s, r, pStop, qp, prune := p.writer, p.store, p.refresher, p.pricingStop, p.poller, p.pruneStop
	p.writer, p.store, p.refresher, p.pricingStop, p.poller, p.pruneStop = nil, nil, nil, nil, nil, nil
	p.mu.Unlock()
	if prune != nil {
		close(prune)
	}
	p.pruning.Wait()
	if pStop != nil {
		close(pStop)
	}
	qp.Stop()
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
	qp := p.poller
	p.mu.Unlock()
	h.Pricing = p.pricingStatus()
	h.QuotaPoller = qp.Status()
	h.Writer.Dropped += p.droppedNoStore.Load()
	h.TraceHits, h.TraceMisses = p.traces.Stats()
	return h
}

func (p *Plugin) currentWriter() *store.Writer {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.writer
}

func (p *Plugin) startQuotaPoller() {
	p.mu.Lock()
	if p.poller != nil || p.store == nil || p.host == nil {
		p.mu.Unlock()
		return
	}
	qp := newQuotaPoller(p.host, p.store, p.logf)
	p.poller = qp
	p.mu.Unlock()
	qp.Start()
}

func (p *Plugin) notifyQuotaPoller(provider, authIndex string) {
	p.mu.Lock()
	qp := p.poller
	p.mu.Unlock()
	qp.Notify(provider, authIndex)
}

func (p *Plugin) planCosts() map[string]float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.cfg.PlanCosts) == 0 {
		return map[string]float64{}
	}
	out := make(map[string]float64, len(p.cfg.PlanCosts))
	for k, v := range p.cfg.PlanCosts {
		out[k] = v
	}
	return out
}

// openStorageLocked (re)opens the database when the path changed or a previous
// open failed. A failure is recorded for health and never fails registration.
// Callers must hold p.lifecycleMu.
func (p *Plugin) openStorageLocked(path string) {
	p.mu.Lock()
	if p.store != nil && p.cfg.DBPath == path {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	p.closeResourcesLocked()
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

// logf sends a log line through host.log via a bounded queue. It never blocks
// or fails the caller.
func (p *Plugin) logf(level, format string, args ...any) {
	if p.host == nil {
		return
	}
	raw, err := json.Marshal(map[string]any{"level": level, "message": fmt.Sprintf(format, args...)})
	if err != nil {
		return
	}
	p.logMu.Lock()
	if p.logCh != nil && !p.logOff {
		select {
		case p.logCh <- raw:
		default:
		}
	}
	p.logMu.Unlock()
}

type hostHTTPRequestWithOp struct {
	OperationID string      `json:"operation_id,omitempty"`
	Method      string      `json:"method,omitempty"`
	URL         string      `json:"url,omitempty"`
	Headers     http.Header `json:"headers,omitempty"`
	Body        []byte      `json:"body,omitempty"`
}

// doHostHTTP performs an outbound HTTP call via host.http.do with a per-request
// timeout and cancellation via host.http.operation_open / host.http.cancel when
// supported by the host.
func doHostHTTP(host Host, req pluginapi.HTTPRequest, timeout time.Duration, stop <-chan struct{}) (pluginapi.HTTPResponse, error) {
	if host == nil {
		return pluginapi.HTTPResponse{}, errors.New("host callbacks unavailable")
	}
	var opID string
	if openRaw, err := host.Call(pluginabi.MethodHostHTTPOperationOpen, []byte("{}")); err == nil && len(openRaw) > 0 {
		var openResp struct {
			OperationID string `json:"operation_id"`
		}
		if json.Unmarshal(openRaw, &openResp) == nil {
			opID = strings.TrimSpace(openResp.OperationID)
		}
	}
	payload, err := json.Marshal(hostHTTPRequestWithOp{
		OperationID: opID,
		Method:      req.Method,
		URL:         req.URL,
		Headers:     req.Headers,
		Body:        req.Body,
	})
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	done := make(chan struct{})
	if opID != "" && (timeout > 0 || stop != nil) {
		go func() {
			var timer *time.Timer
			var timerCh <-chan time.Time
			if timeout > 0 {
				timer = time.NewTimer(timeout)
				timerCh = timer.C
				defer timer.Stop()
			}
			select {
			case <-done:
				return
			case <-timerCh:
			case <-stop:
			}
			cancelReq, _ := json.Marshal(map[string]string{"operation_id": opID})
			_, _ = host.Call(pluginabi.MethodHostHTTPCancel, cancelReq)
		}()
	}
	raw, err := host.Call(pluginabi.MethodHostHTTPDo, payload)
	close(done)
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	var resp pluginapi.HTTPResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("decode host.http.do response: %w", err)
	}
	return resp, nil
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
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.closed {
		return nil, &codedError{code: "plugin_closed", msg: "plugin is closed"}
	}
	p.openStorageLocked(cfg.DBPath)
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
	p.startQuotaPoller()
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
			{Name: "plan_costs", Type: pluginapi.ConfigFieldTypeObject, Description: "Monthly subscription USD by plan key (for example 'claude:Max 20x', 'codex:Pro') or account auth_index."},
		},
	}
}
