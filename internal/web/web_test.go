package web

import (
	"regexp"
	"strings"
	"testing"
)

func TestIndexHasEveryView(t *testing.T) {
	page := string(Index())
	for _, id := range []string{`id="totals"`, `id="daily"`, `id="drift"`, `id="breakdown"`, `id="quality"`, `id="health"`} {
		if !strings.Contains(page, id) {
			t.Errorf("page lacks %s", id)
		}
	}
}

// The page must load nothing from the network except the plugin's own
// management routes. The SVG namespace is an identifier, not a request.
// The provider usage URLs are not fetched by the browser: the page sends them
// to CPA's api-call, which makes the request server-side with the credential.
func TestIndexMakesNoExternalRequests(t *testing.T) {
	page := strings.ReplaceAll(string(Index()), `"http://www.w3.org/2000/svg"`, "")
	for _, upstream := range []string{"https://api.anthropic.com/api/oauth/usage", "https://api.anthropic.com/api/oauth/profile", "https://chatgpt.com/backend-api/wham/usage"} {
		page = strings.ReplaceAll(page, "upstreamCall(account, '"+upstream+"'", "")
	}
	if m := regexp.MustCompile(`(?i)https?://|//[a-z0-9-]+\.[a-z]`).FindString(page); m != "" {
		t.Errorf("page references %q", m)
	}
	if !strings.Contains(page, "/v0/management/cpa-analytics/") {
		t.Error("page does not call the plugin's management routes")
	}
}

func TestIndexKeepsKeyInSessionStorage(t *testing.T) {
	page := string(Index())
	if !strings.Contains(page, "sessionStorage") || !strings.Contains(page, "Authorization") {
		t.Error("page must keep the key in sessionStorage and send it as Authorization")
	}
	if regexp.MustCompile(`localStorage\.setItem`).MatchString(page) {
		t.Error("page must never write the key to localStorage")
	}
}

// The API key filter takes a hash, so the page must offer the breakdown that lists them.
func TestIndexOffersAPIKeyBreakdown(t *testing.T) {
	if !strings.Contains(string(Index()), `data-by="api_key"`) {
		t.Error("page has no API key breakdown")
	}
}

// The management center floats its own toolbar (refresh, language, theme,
// logout) over the top-right corner of the page, so the header holds no
// controls.
func TestIndexKeepsHeaderFreeOfControls(t *testing.T) {
	page := string(Index())
	start := strings.Index(page, `<header class="top">`)
	end := strings.Index(page, `</header>`)
	if start < 0 || end < start {
		t.Fatal("page has no top header")
	}
	if header := page[start:end]; strings.Contains(header, "<button") || strings.Contains(header, "<select") {
		t.Errorf("header holds a control, it would sit under the management center toolbar:\n%s", header)
	}
	if !strings.Contains(page, `id="change-key"`) {
		t.Error("page lost the change key button")
	}
}

func TestIndexAutoRefreshesEverySixtySecondsByDefault(t *testing.T) {
	page := string(Index())
	m := regexp.MustCompile(`(?s)<select id="auto-refresh">(.*?)</select>`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("page has no auto-refresh select")
	}
	if !strings.Contains(m[1], `<option value="60" selected>`) {
		t.Errorf("60 seconds is not the default: %s", m[1])
	}
	if !strings.Contains(m[1], `<option value="0">Off</option>`) {
		t.Errorf("auto-refresh cannot be turned off: %s", m[1])
	}
}

// Reasoning effort explains output growth with no model change, so the drift
// chart and the breakdown both offer it.
func TestIndexOffersReasoningEffort(t *testing.T) {
	page := string(Index())
	for _, want := range []string{`<option value="reasoning_effort">`, `data-by="reasoning_effort"`, `reasoning_effort: 'reasoning effort'`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}

func TestIndexSplitsCostByTypeAndTier(t *testing.T) {
	page := string(Index())
	for _, want := range []string{`id="t-types"`, `id="t-tiers"`, `cost_by_type`, `cost_by_tier`, `cache_savings_usd`, `standard_cost_usd`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}

// Inside the management center the theme comes from the center's own toggle,
// which sets data-theme on its <html>, not from the system preference.
func TestIndexFollowsManagementCenterTheme(t *testing.T) {
	page := string(Index())
	for _, want := range []string{`window.parent.document.documentElement`, `attributeFilter: ['data-theme']`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}

// Token counts are whole numbers. With a median of 1.5 tokens a fractional
// step drew the labels 0, 1, 1, 2, 2, so count axes step by at least one.
func TestIndexCountAxesStepByWholeTokens(t *testing.T) {
	page := string(Index())
	for _, want := range []string{`Math.max(minStep, niceStep(maxY, 4))`, `fmtDay, isRate ? 0 : 1)`, `fmtX, state.dailyMode === 'cost' ? 0 : 1)`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}

func TestIndexSplitsTokensByType(t *testing.T) {
	page := string(Index())
	for _, want := range []string{`id="t-token-types"`, `Tokens by token type`, `label: 'Unclassified', value: t.unclassified`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}

func TestIndexShowsTokensAndSessionsPerProvider(t *testing.T) {
	page := string(Index())
	for _, want := range []string{`fmtCompact(p.tokens) + ' tokens'`, `fmtCompact(p.sessions) + ' sessions'`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}

func TestIndexShowsCostPerRequestAndTimings(t *testing.T) {
	page := string(Index())
	for _, want := range []string{`tile('Cost per request'`, `'Per request'`, `<option value="latency_ms">`, `<option value="ttft_ms">`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}

// The drift card measures timings too, so its title names the chosen measure.
func TestIndexDriftTitleFollowsMeasure(t *testing.T) {
	page := string(Index())
	for _, want := range []string{`<h2 id="drift-title">Per request</h2>`, `textContent = isRate ? 'Daily cache hit rate' : driftMetricLabel() + ' per request'`, `const fmtSeconds = (ms) => (ms === 0 ? '0 s'`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}

func TestIndexShowsProcessedTokensPerActiveDay(t *testing.T) {
	page := string(Index())
	for _, want := range []string{`function tile(label, value, note, change)`, `' per active ' + (data.daily.bucket === 'hour' ? 'hour' : 'day')`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}

// The model breakdown has one row per provider and model. A row shows its
// provider and opening it filters by both.
func TestIndexModelRowsCarryProvider(t *testing.T) {
	page := string(Index())
	for _, want := range []string{`colorFor('provider', r.provider)`, `setFilter('provider', r.provider, { refresh: false })`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}

// The totals compare each figure with the period before it, when recording
// covered that period, so a jump shows without opening the charts.
func TestIndexComparesWithPreviousPeriod(t *testing.T) {
	page := string(Index())
	for _, want := range []string{
		`recordedThrough(prevRange, prev.data_since)`,
		`relativeChange(t.cost_usd, prev.cost_usd)`,
		`relativeChange(t.requests, prev.requests)`,
		`relativeChange(t.processed_tokens, prev.processed_tokens)`,
		`pointChange(t.cache_hit_rate, prev.cache_hit_rate)`,
		`id="t-cost-change"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}

// Total input per request is the default drift measure: it moves when the
// context grows, whether or not the cache absorbs it.
func TestIndexDriftDefaultsToTotalInput(t *testing.T) {
	page := string(Index())
	for _, want := range []string{`driftMetric: 'input'`, `<option value="input" selected>Input incl. cache</option>`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}

// limits.js and compare.js are tested by bun on their own and inlined into the page.
func TestIndexInlinesScripts(t *testing.T) {
	page := string(Index())
	for placeholder, function := range map[string]string{
		"/* limits.js */":  "function poolWindow(members, now)",
		"/* compare.js */": "function relativeChange(current, previous)",
	} {
		if strings.Contains(page, placeholder) {
			t.Errorf("%s placeholder was not replaced", placeholder)
		}
		if !strings.Contains(page, function) {
			t.Errorf("page lacks %q", function)
		}
	}
}

// Limits reads quota only through CPA's management routes and never calls
// the routes that spend credits.
func TestIndexReadsLimitsThroughCPA(t *testing.T) {
	page := string(Index())
	for _, want := range []string{`id="limits"`, `data-view="limits"`, `/v8/management/credentials`, `/v8/management/requests/api-call`, `'Bearer $TOKEN$'`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	for _, banned := range []string{"reset-credits", "/consume", "chat/completions"} {
		if strings.Contains(page, banned) {
			t.Errorf("page references %s", banned)
		}
	}
}

func TestIndexKeepsViewSwitchOutOfHeader(t *testing.T) {
	page := string(Index())
	header := page[strings.Index(page, `<header class="top">`):strings.Index(page, `</header>`)]
	if strings.Contains(header, "data-view") {
		t.Error("the view switch belongs in the filter row")
	}
}
