package plugin

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/marcusgrando/cpa-analytics/internal/accounting"
	"github.com/marcusgrando/cpa-analytics/internal/client"
	"github.com/marcusgrando/cpa-analytics/internal/ratelimit"
	"github.com/marcusgrando/cpa-analytics/internal/store"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// RowFromRecord converts a CPA usage record into a stored row. ua is the
// inbound User-Agent joined by trace ID, empty when unknown.
func RowFromRecord(rec pluginapi.UsageRecord, ua string) store.Row {
	b := accounting.Normalize(rec.Provider, rec.ExecutorType, rec.Detail)
	requestedAt := rec.RequestedAt
	if requestedAt.IsZero() {
		requestedAt = time.Now().UTC()
	}
	limits := ratelimit.FromHeaders(rec.ResponseHeaders, requestedAt.Add(rec.Latency))
	name, version := client.Parse(ua)
	status := 0
	if rec.Failed {
		status = rec.Failure.StatusCode
	}
	row := store.Row{
		RequestID:            rec.RequestID,
		TraceID:              rec.TraceID,
		RequestedAt:          requestedAt.UnixMilli(),
		Provider:             rec.Provider,
		ExecutorType:         rec.ExecutorType,
		AuthType:             rec.AuthType,
		AuthID:               rec.AuthID,
		Model:                rec.Model,
		Alias:                rec.Alias,
		ResponseModel:        rec.ResponseModel,
		APIKeyHash:           hashAPIKey(rec.APIKey),
		AccessProvider:       rec.AccessProvider,
		Source:               rec.Source,
		SessionID:            rec.SessionID,
		ParentSessionID:      rec.ParentSessionID,
		ReasoningEffort:      rec.ReasoningEffort,
		ServiceTier:          rec.ServiceTier,
		EffectiveServiceTier: rec.EffectiveServiceTier,
		ResponseServiceTier:  rec.ResponseServiceTier,
		Stream:               rec.Stream,
		Generate:             rec.Generate,
		Failed:               rec.Failed,
		StatusCode:           status,
		LatencyMs:            rec.Latency.Milliseconds(),
		TTFTMs:               rec.TTFT.Milliseconds(),
		InputTokens:          rec.Detail.InputTokens,
		OutputTokens:         rec.Detail.OutputTokens,
		ReasoningTokens:      rec.Detail.ReasoningTokens,
		CachedTokens:         rec.Detail.CachedTokens,
		CacheReadTokens:      rec.Detail.CacheReadTokens,
		CacheCreationTokens:  rec.Detail.CacheCreationTokens,
		TotalTokens:          rec.Detail.TotalTokens,
		UncachedInput:        b.Uncached,
		CacheRead:            b.CacheRead,
		CacheWrite:           b.CacheWrite,
		OutputNonReasoning:   b.OutputNonReasoning,
		OutputReasoning:      b.Reasoning,
		Unclassified:         b.Unclassified,
		AccountingQuality:    b.Quality,
		UserAgent:            ua,
		ClientName:           name,
		ClientVersion:        version,
		AuthIndex:            rec.AuthIndex,
	}
	if w := limits.Session; w != nil {
		row.LimitSessionPct = sql.NullFloat64{Float64: w.UsedPct, Valid: true}
		row.LimitSessionReset = sql.NullInt64{Int64: w.ResetAt, Valid: w.ResetAt > 0}
	}
	if w := limits.Weekly; w != nil {
		row.LimitWeeklyPct = sql.NullFloat64{Float64: w.UsedPct, Valid: true}
		row.LimitWeeklyReset = sql.NullInt64{Int64: w.ResetAt, Valid: w.ResetAt > 0}
	}
	return row
}

func hashAPIKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}

func (p *Plugin) handleUsage(raw []byte) ([]byte, error) {
	var rec pluginapi.UsageRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("decode usage record: %w", err)
	}
	if strings.TrimSpace(rec.RequestID) == "" {
		return nil, &codedError{code: "invalid_record", msg: "usage record has no RequestID"}
	}
	ua, _ := p.traces.Take(rec.TraceID)
	w := p.currentWriter()
	if !w.Enqueue(RowFromRecord(rec, ua)) && w == nil {
		p.droppedNoStore.Add(1)
	}
	p.notifyQuotaPoller(rec.Provider, rec.AuthIndex)
	return nil, nil
}

// maxUserAgentBytes bounds what one trace entry and one row keep. Coding
// agents send well under a hundred bytes.
const maxUserAgentBytes = 1024

func (p *Plugin) interceptBefore(raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode intercept request: %w", err)
	}
	ua := req.Headers.Get("User-Agent")
	if len(ua) > maxUserAgentBytes {
		ua = ""
	}
	if ua != "" {
		p.traces.Put(req.TraceID, ua)
	}
	return nil, nil
}

func passThrough([]byte) ([]byte, error) { return nil, nil }
