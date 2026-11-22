package plugin

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marcusgrando/cpa-analytics/internal/store"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// antigravityUserAgent matches the Antigravity Hub User-Agent of the pinned
	// CPA tag (internal/misc/antigravity_version.go) so Google returns every
	// model group in retrieveUserQuotaSummary.
	antigravityUserAgent         = "antigravity/hub/2.29.0 (aidev_client; os_type=darwin; arch=arm64; cl=1013060670)"
	antigravityLoadCodeAssistURL = "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
	antigravityQuotaSummaryURL   = "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary"

	quotaPollEvery       = 5 * time.Minute
	quotaTriggerDebounce = 3 * time.Second
	quotaTriggerMinGap   = 60 * time.Second
	quotaMaxConcurrent   = 4
)

// QuotaPollerStatus reports background quota polling health for providers that
// do not return limit usage in response headers.
type QuotaPollerStatus struct {
	Polls       uint64    `json:"polls"`
	Snapshots   uint64    `json:"snapshots"`
	LastPollAt  time.Time `json:"last_poll_at,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitempty"`
}

type quotaPoller struct {
	host  Host
	store *store.Store
	logf  func(level, format string, args ...any)
	every time.Duration

	polls     atomic.Uint64
	snapshots atomic.Uint64

	mu          sync.Mutex
	nowFn       func() time.Time
	projects    map[string]string
	lastPolled  map[string]time.Time
	pending     map[string]struct{}
	lastPollAt  time.Time
	lastError   string
	lastErrorAt time.Time
	lastLogAt   time.Time

	trigger chan struct{}
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

func newQuotaPoller(host Host, s *store.Store, logf func(string, string, ...any)) *quotaPoller {
	return &quotaPoller{
		host:       host,
		store:      s,
		logf:       logf,
		nowFn:      time.Now,
		every:      quotaPollEvery,
		projects:   map[string]string{},
		lastPolled: map[string]time.Time{},
		pending:    map[string]struct{}{},
		trigger:    make(chan struct{}, 1),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
}

func (qp *quotaPoller) currentTime() time.Time {
	qp.mu.Lock()
	fn := qp.nowFn
	qp.mu.Unlock()
	if fn == nil {
		return time.Now()
	}
	return fn()
}

func (qp *quotaPoller) setClock(fn func() time.Time) {
	qp.mu.Lock()
	qp.nowFn = fn
	qp.mu.Unlock()
}

func (qp *quotaPoller) Start() {
	if qp == nil || qp.host == nil || qp.store == nil {
		if qp != nil {
			close(qp.done)
		}
		return
	}
	go qp.run()
}

func (qp *quotaPoller) Stop() {
	if qp == nil {
		return
	}
	qp.once.Do(func() { close(qp.stop) })
	<-qp.done
}

func (qp *quotaPoller) isStopped() bool {
	select {
	case <-qp.stop:
		return true
	default:
		return false
	}
}

func (qp *quotaPoller) Notify(provider, authIndex string) {
	if qp == nil || !strings.EqualFold(strings.TrimSpace(provider), "antigravity") {
		return
	}
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return
	}
	qp.mu.Lock()
	qp.pending[authIndex] = struct{}{}
	qp.mu.Unlock()
	select {
	case qp.trigger <- struct{}{}:
	default:
	}
}

func (qp *quotaPoller) Status() QuotaPollerStatus {
	if qp == nil {
		return QuotaPollerStatus{}
	}
	qp.mu.Lock()
	defer qp.mu.Unlock()
	return QuotaPollerStatus{
		Polls:       qp.polls.Load(),
		Snapshots:   qp.snapshots.Load(),
		LastPollAt:  qp.lastPollAt,
		LastError:   qp.lastError,
		LastErrorAt: qp.lastErrorAt,
	}
}

func (qp *quotaPoller) run() {
	defer close(qp.done)
	qp.pollAll(nil)

	ticker := time.NewTicker(qp.every)
	defer ticker.Stop()
	var debounce <-chan time.Time
	var timer *time.Timer
	stopTimer := func() {
		if timer != nil {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer = nil
			debounce = nil
		}
	}
	armTimer := func(d time.Duration) {
		if d < time.Millisecond {
			d = time.Millisecond
		}
		stopTimer()
		timer = time.NewTimer(d)
		debounce = timer.C
	}
	defer stopTimer()

	for {
		select {
		case <-qp.stop:
			return
		case <-ticker.C:
			stopTimer()
			qp.clearPending()
			qp.pollAll(nil)
		case <-qp.trigger:
			if timer == nil {
				armTimer(quotaTriggerDebounce)
			}
		case <-debounce:
			timer = nil
			debounce = nil
			targets, retryIn := qp.takeDuePending()
			if len(targets) > 0 {
				qp.pollAll(targets)
			}
			if retryIn > 0 {
				armTimer(retryIn)
			}
		}
	}
}

func (qp *quotaPoller) clearPending() {
	qp.mu.Lock()
	clear(qp.pending)
	qp.mu.Unlock()
}

// takeDuePending pops pending accounts past quotaTriggerMinGap and leaves
// cooldown-deferred ones queued, returning when the earliest deferred account
// becomes due.
func (qp *quotaPoller) takeDuePending() (map[string]struct{}, time.Duration) {
	now := qp.currentTime()
	qp.mu.Lock()
	defer qp.mu.Unlock()
	due := map[string]struct{}{}
	var nextRetry time.Duration
	for id := range qp.pending {
		if last, ok := qp.lastPolled[id]; ok {
			if rem := quotaTriggerMinGap - now.Sub(last); rem > 0 {
				if nextRetry == 0 || rem < nextRetry {
					nextRetry = rem
				}
				continue
			}
		}
		delete(qp.pending, id)
		due[id] = struct{}{}
	}
	return due, nextRetry
}

// pollAll polls active Antigravity credentials. When only is non-nil, only
// credentials in that set are polled.
func (qp *quotaPoller) pollAll(only map[string]struct{}) {
	if qp == nil || qp.host == nil || qp.store == nil || qp.isStopped() {
		return
	}
	creds, err := qp.listAntigravityCredentials()
	if err != nil {
		qp.recordError(fmt.Errorf("list credentials: %w", err))
		return
	}
	if only == nil {
		active := make(map[string]struct{}, len(creds))
		for _, c := range creds {
			active[c.AuthIndex] = struct{}{}
		}
		qp.pruneCaches(active)
	}
	var targets []pluginapi.HostAuthFileEntry
	for _, c := range creds {
		if only != nil {
			if _, ok := only[c.AuthIndex]; !ok {
				continue
			}
		}
		targets = append(targets, c)
	}
	if len(targets) == 0 {
		if only == nil {
			qp.mu.Lock()
			qp.lastError = ""
			qp.mu.Unlock()
		}
		return
	}

	sem := make(chan struct{}, quotaMaxConcurrent)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var allSnaps []store.QuotaSnapshot
	var firstErr error

loop:
	for _, c := range targets {
		select {
		case <-qp.stop:
			break loop
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(entry pluginapi.HostAuthFileEntry) {
			defer wg.Done()
			defer func() { <-sem }()
			snaps, err := qp.pollAccount(entry)
			mu.Lock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", entry.AuthIndex, err)
				}
			} else {
				allSnaps = append(allSnaps, snaps...)
			}
			mu.Unlock()
			polledAt := qp.currentTime()
			qp.mu.Lock()
			qp.lastPolled[entry.AuthIndex] = polledAt
			qp.mu.Unlock()
		}(c)
	}
	wg.Wait()

	if qp.isStopped() {
		return
	}

	now := qp.currentTime()
	qp.polls.Add(1)
	qp.mu.Lock()
	qp.lastPollAt = now
	qp.mu.Unlock()

	if len(allSnaps) > 0 {
		if err := qp.store.InsertQuotaSnapshots(allSnaps); err != nil {
			qp.recordError(fmt.Errorf("store snapshots: %w", err))
			return
		}
		qp.snapshots.Add(uint64(len(allSnaps)))
	}
	if firstErr != nil {
		qp.recordError(firstErr)
		return
	}
	qp.mu.Lock()
	qp.lastError = ""
	qp.mu.Unlock()
}

func (qp *quotaPoller) pruneCaches(active map[string]struct{}) {
	qp.mu.Lock()
	defer qp.mu.Unlock()
	for id := range qp.projects {
		if _, ok := active[id]; !ok {
			delete(qp.projects, id)
		}
	}
	for id := range qp.lastPolled {
		if _, ok := active[id]; !ok {
			delete(qp.lastPolled, id)
		}
	}
}

func (qp *quotaPoller) recordError(err error) {
	now := qp.currentTime()
	qp.mu.Lock()
	qp.lastError = err.Error()
	qp.lastErrorAt = now
	shouldLog := now.Sub(qp.lastLogAt) >= 5*time.Minute
	if shouldLog {
		qp.lastLogAt = now
	}
	qp.mu.Unlock()
	if shouldLog && qp.logf != nil {
		qp.logf("warn", "cpa-analytics: antigravity quota poll: %v", err)
	}
}

func (qp *quotaPoller) listAntigravityCredentials() ([]pluginapi.HostAuthFileEntry, error) {
	raw, err := qp.host.Call(pluginabi.MethodHostAuthList, []byte("{}"))
	if err != nil {
		return nil, err
	}
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode host.auth.list: %w", err)
	}
	out := make([]pluginapi.HostAuthFileEntry, 0)
	for _, f := range resp.Files {
		if f.Disabled || f.RuntimeOnly || strings.TrimSpace(f.AuthIndex) == "" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(f.Provider), "antigravity") {
			out = append(out, f)
		}
	}
	return out, nil
}

type antigravityCredential struct {
	AccessToken string `json:"access_token"`
	ProjectID   string `json:"project_id"`
	ProxyURL    string `json:"proxy_url"`
	Expired     string `json:"expired"`
	Timestamp   int64  `json:"timestamp"`
	ExpiresIn   int64  `json:"expires_in"`
}

func (c antigravityCredential) isExpired(now time.Time) bool {
	if s := strings.TrimSpace(c.Expired); s != "" {
		if ts, err := time.Parse(time.RFC3339Nano, s); err == nil && !ts.IsZero() {
			return !ts.After(now)
		}
	}
	if c.Timestamp > 0 && c.ExpiresIn > 0 {
		// CPA stores timestamp in Unix ms and expires_in in seconds.
		expMs := c.Timestamp + c.ExpiresIn*1000
		return expMs <= now.UnixMilli()
	}
	return false
}

type httpStatusError struct {
	code int
}

func (e httpStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

func (qp *quotaPoller) pollAccount(entry pluginapi.HostAuthFileEntry) ([]store.QuotaSnapshot, error) {
	reqRaw, err := json.Marshal(pluginapi.HostAuthGetRequest{AuthIndex: entry.AuthIndex})
	if err != nil {
		return nil, err
	}
	raw, err := qp.host.Call(pluginabi.MethodHostAuthGet, reqRaw)
	if err != nil {
		return nil, fmt.Errorf("host.auth.get: %w", err)
	}
	var authResp pluginapi.HostAuthGetResponse
	if err := json.Unmarshal(raw, &authResp); err != nil {
		return nil, fmt.Errorf("decode host.auth.get: %w", err)
	}
	var cred antigravityCredential
	if err := json.Unmarshal(authResp.JSON, &cred); err != nil {
		return nil, fmt.Errorf("decode credential json: %w", err)
	}
	if strings.TrimSpace(cred.ProxyURL) != "" {
		return nil, nil // host.http.do does not apply per-credential proxy_url
	}
	token := strings.TrimSpace(cred.AccessToken)
	if token == "" {
		return nil, errors.New("credential has no access_token")
	}
	if cred.isExpired(qp.currentTime()) {
		return nil, nil // wait for CPA's proactive background refresh
	}

	project := strings.TrimSpace(entry.ProjectID)
	if project == "" {
		project = strings.TrimSpace(cred.ProjectID)
	}
	if project == "" {
		qp.mu.Lock()
		project = qp.projects[entry.AuthIndex]
		qp.mu.Unlock()
	}
	if project == "" {
		p, err := qp.fetchAntigravityProject(token)
		if err != nil {
			return nil, fmt.Errorf("loadCodeAssist: %w", err)
		}
		if p != "" {
			project = p
			qp.mu.Lock()
			qp.projects[entry.AuthIndex] = p
			qp.mu.Unlock()
		}
	}

	payload := []byte("{}")
	if project != "" {
		payload, _ = json.Marshal(map[string]string{"project": project})
	}
	body, err := qp.postAntigravity(antigravityQuotaSummaryURL, token, payload)
	if err != nil {
		var se httpStatusError
		if errors.As(err, &se) && se.code >= 400 && se.code < 500 {
			qp.mu.Lock()
			delete(qp.projects, entry.AuthIndex)
			qp.mu.Unlock()
		}
		return nil, err
	}
	return parseAntigravityQuotaSummary(body, "antigravity", entry.AuthIndex, qp.currentTime().UnixMilli())
}

func (qp *quotaPoller) fetchAntigravityProject(token string) (string, error) {
	payload := []byte(`{"metadata":{"ideType":"ANTIGRAVITY"}}`)
	body, err := qp.postAntigravity(antigravityLoadCodeAssistURL, token, payload)
	if err != nil {
		return "", err
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", err
	}
	for _, key := range []string{"cloudaicompanionProject", "projectId", "project"} {
		switch v := doc[key].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s, nil
			}
		case map[string]any:
			if id, ok := v["id"].(string); ok {
				if s := strings.TrimSpace(id); s != "" {
					return s, nil
				}
			}
		}
	}
	return "", nil
}

func (qp *quotaPoller) postAntigravity(url, token string, body []byte) ([]byte, error) {
	resp, err := doHostHTTP(qp.host, pluginapi.HTTPRequest{
		Method: http.MethodPost,
		URL:    url,
		Headers: http.Header{
			"Authorization": {"Bearer " + token},
			"Content-Type":  {"application/json"},
			"User-Agent":    {antigravityUserAgent},
		},
		Body: body,
	}, httpTimeout, qp.stop)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, httpStatusError{code: resp.StatusCode}
	}
	return resp.Body, nil
}

var (
	trailingModelsRe = regexp.MustCompile(`(?i)\s+models$`)
	nonAlnumSlugRe   = regexp.MustCompile(`[^a-z0-9]+`)
)

// parseAntigravityQuotaSummary decodes retrieveUserQuotaSummary using the same
// group slugging and window rules as internal/web/limits.js.
func parseAntigravityQuotaSummary(raw []byte, provider, authIndex string, recordedAt int64) ([]store.QuotaSnapshot, error) {
	var doc struct {
		Groups []struct {
			DisplayName string `json:"displayName"`
			Buckets     []struct {
				Window            string   `json:"window"`
				RemainingFraction *float64 `json:"remainingFraction"`
				ResetTime         string   `json:"resetTime"`
			} `json:"buckets"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode quota summary: %w", err)
	}
	type dedupKey struct {
		groupID, windowKind string
	}
	seen := map[dedupKey]bool{}
	var snaps []store.QuotaSnapshot
	for _, g := range doc.Groups {
		label := strings.TrimSpace(trailingModelsRe.ReplaceAllString(strings.TrimSpace(g.DisplayName), ""))
		if label == "" {
			continue
		}
		id := strings.Trim(nonAlnumSlugRe.ReplaceAllString(strings.ToLower(label), "-"), "-")
		if id == "" {
			continue
		}
		for _, kind := range []struct {
			api, stored string
		}{{"5h", "session"}, {"weekly", "weekly"}} {
			dk := dedupKey{id, kind.stored}
			if seen[dk] {
				continue
			}
			for _, b := range g.Buckets {
				if b.Window != kind.api || b.RemainingFraction == nil {
					continue
				}
				rem := *b.RemainingFraction
				if math.IsNaN(rem) || math.IsInf(rem, 0) {
					continue
				}
				used := math.Min(100, math.Max(0, (1-rem)*100))
				var reset sql.NullInt64
				if rem < 1 {
					if ts, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(b.ResetTime)); err == nil && ts.UnixMilli() > 0 {
						reset = sql.NullInt64{Int64: ts.UnixMilli(), Valid: true}
					}
				}
				seen[dk] = true
				snaps = append(snaps, store.QuotaSnapshot{
					RecordedAt: recordedAt,
					Provider:   provider,
					AuthIndex:  authIndex,
					GroupID:    id,
					GroupLabel: label,
					WindowKind: kind.stored,
					UsedPct:    used,
					ResetAt:    reset,
				})
				break
			}
		}
	}
	if len(snaps) == 0 {
		return nil, errors.New("quota summary has no usage windows")
	}
	return snaps, nil
}
