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
	for _, upstream := range []string{
		"https://api.anthropic.com/api/oauth/usage",
		"https://api.anthropic.com/api/oauth/profile",
		"https://chatgpt.com/backend-api/wham/usage",
		"https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist",
		"https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels",
		"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
		"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota",
		"https://api.kimi.com/coding/v1/usages",
	} {
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
	for _, want := range []string{`Math.max(minStep, niceStep(maxY, 4))`, `fmtX, isRate || isCost ? 0 : 1, 8 + labelRoom)`, `fmtX, state.dailyMode === 'cost' ? 0 : 1)`} {
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
	for _, want := range []string{`<h2 id="drift-title">Per request</h2>`, `textContent = isRate ? (DRIFT_CADENCE[bucket] || 'Daily') + ' cache hit rate' : isSpeed ? 'Output tokens / s' : driftMetricLabel() + ' per request'`, `const fmtSeconds = (ms) => (ms === 0 ? '0 s'`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}

func TestIndexShowsProcessedTokensPerActiveDay(t *testing.T) {
	page := string(Index())
	for _, want := range []string{`function tile(label, value, note, change, trend)`, `' per active ' + (BUCKET_MS[data.daily.bucket] ? data.daily.bucket : 'day')`} {
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

// The weekly limit value reads the whole account, so the page sends only the
// range and provider, and names accounts from CPA's credential list.
func TestIndexChartsWeeklyLimitValue(t *testing.T) {
	page := string(Index())
	for _, want := range []string{
		`id="burn"`,
		`api('limit-burn', { ...range, provider: p.provider, account: p.account })`,
		`p.usd_per_pct`,
		`management('/v8/management/credentials')`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}

// limits.js and compare.js are tested by bun on their own and inlined into the page.
func TestIndexInlinesScripts(t *testing.T) {
	page := string(Index())
	for placeholder, function := range map[string]string{
		"/* limits.js */":    "function poolWindow(members, now)",
		"/* compare.js */":   "function relativeChange(current, previous)",
		"/* colors.js */":    "function providerPalette()",
		"/* tooltip.js */":   "function besideColumn(column, box, viewport, pointerY)",
		"/* sparkline.js */": "function sparkline(values, height, pad)",
		"/* endlabels.js */": "function endLabels(ends, limit, minGap)",
		"/* scatter.js */":   "function nearestPoint(points, x, y, radius)",
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

// The Limits view charts each weekly pool's recorded history and projects it
// from the burn rate, next to the live reading.
func TestIndexChartsWeeklyPoolHistoryAndProjection(t *testing.T) {
	page := string(Index())
	for _, want := range []string{
		`api('limit-history'`,
		`poolHistory(`,
		`burnRate(`,
		`projectPool(`,
		`First at limit`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}

func TestIndexShowsQuotaLeftAtReset(t *testing.T) {
	page := string(Index())
	for _, want := range []string{`id="leftover"`, `api('limit-leftover', { ...range, provider: p.provider, account: p.account })`, `c.left_usd`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}

func TestIndexShowsAccountAffinity(t *testing.T) {
	page := string(Index())
	for _, want := range []string{`id="affinity"`, `api('affinity', p)`, `r.switch_cache_write_usd`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}

func TestIndexOffersAccountBreakdown(t *testing.T) {
	page := string(Index())
	if !strings.Contains(page, `data-by="account"`) {
		t.Error("breakdown has no account option")
	}
}

// One login can hold a Claude and a Codex account, so a name alone is
// ambiguous wherever accounts of both providers are listed together.
func TestIndexNamesAccountsWithTheirProvider(t *testing.T) {
	page := string(Index())
	for _, want := range []string{
		`text: accountWithProvider(key)`,
		`label: accountWithProvider(a)`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}

func TestIndexCoversAllProvidersAndBehaviorDiagnostics(t *testing.T) {
	page := string(Index())
	for _, want := range []string{
		`id="f-account"`,
		`id="t-drivers"`,
		`id="account-warning"`,
		`<option value="output_total">`,
		`<option value="cost_usd">`,
		`<option value="tokens_per_sec">`,
		`data-by="provider"`,
		`data-by="user_agent"`,
		`data-by="errors"`,
		`readAntigravity`,
		`readGemini`,
		`readDevin`,
		`readKimi`,
		`renderStatusRow`,
		`markerImpact(`,
		`changeDrivers(`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}

// Brand steps validated with the dataviz palette checker: light on #ffffff,
// dark on #181a1e, all pairs among the three brands plus the spare color.
func TestIndexDefinesProviderColorsForBothThemes(t *testing.T) {
	page := string(Index())
	light := "--provider-claude: #d97757; --provider-codex: #3941ff; --provider-gemini: #4796e3; --provider-next: #008300;"
	dark := "--provider-claude: #d4704f; --provider-codex: #4549ff; --provider-gemini: #4796e3; --provider-next: #13a065;"
	if strings.Count(page, light) != 1 {
		t.Error("light theme lacks the provider colors")
	}
	if strings.Count(page, dark) != 2 {
		t.Error("dark theme lacks the provider colors in the media query and the data-theme scope")
	}
	if !strings.Contains(page, "if (space === 'provider') return providerColor(key);") {
		t.Error("provider marks do not use the provider palette")
	}
}

// Hue marks identity (provider, drift series) and status only. Token types,
// tiers, price sources and limit meters use one neutral ramp, stepped per
// theme and validated as an ordinal ramp against each surface.
func TestIndexKeepsHueForIdentityAndStatus(t *testing.T) {
	page := string(Index())
	light := "--tone1: #1e2633; --tone2: #3f495b; --tone3: #626d81; --tone4: #8590a3; --tone5: #a9b2c1;"
	dark := "--tone1: #eceef2; --tone2: #c4cad4; --tone3: #9ba3b1; --tone4: #77808f; --tone5: #5a6372;"
	if strings.Count(page, light) != 1 {
		t.Error("light theme lacks the neutral ramp")
	}
	if strings.Count(page, dark) != 2 {
		t.Error("dark theme lacks the neutral ramp in the media query and the data-theme scope")
	}
	for _, slot := range []string{"var(--s1)", "var(--s2)", "var(--s3)", "var(--s4)", "var(--s5)"} {
		if strings.Contains(page, slot) {
			t.Errorf("%s is assigned by hand; categorical slots belong to colorFor only", slot)
		}
	}
	if !strings.Contains(page, "const NEUTRAL_SPACES = new Set(['tier']);") {
		t.Error("service tiers do not use the neutral ramp")
	}
}

// Text never sits on a colored fill: the share moves to the legend, so every
// label keeps the ink contrast whatever the fill.
func TestIndexKeepsLabelsOffFills(t *testing.T) {
	page := string(Index())
	if strings.Contains(page, "dark-label") || strings.Contains(page, "block.textContent = fmtPct(share)") {
		t.Error("segmented bars still print text on the fill")
	}
	if !strings.Contains(page, "`${p.label} ${fmtValue(p.value)} (${fmtPct(p.value / total)})") {
		t.Error("legend lacks the share of each segment")
	}
}

// Secondary text clears 4.5:1 on the page and the card in both themes.
func TestIndexMutedTextClearsContrast(t *testing.T) {
	page := string(Index())
	if strings.Count(page, "--muted: #646b77;") != 1 || strings.Count(page, "--muted: #8d94a0;") != 2 {
		t.Error("muted text tokens changed without a contrast check")
	}
}

func TestIndexJoinsMetaWithoutMiddleDots(t *testing.T) {
	if strings.Contains(string(Index()), "·") {
		t.Error("page joins text with middle dots")
	}
}

// A chart column's tooltip opens beside the column, so it never hides the
// points of the day it describes.
func TestIndexOpensColumnTooltipBesideTheColumn(t *testing.T) {
	page := string(Index())
	for _, want := range []string{"showTooltip(e, info.title, info.rows, hit.getBoundingClientRect())", "besideColumn(column, box, { width: window.innerWidth, height: window.innerHeight }"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func TestIndexFoldsDriftChangesIntoDetails(t *testing.T) {
	page := string(Index())
	want := `<details id="drift-changes" hidden><summary id="drift-changes-summary"></summary><ul id="drift-markers" class="markers"></ul></details>`
	if !strings.Contains(page, want) {
		t.Errorf("drift change list is not folded into a details element")
	}
	if !strings.Contains(page, "`Changes (${matchingMarkers.length})`") {
		t.Errorf("drift changes summary does not count the changes")
	}
}

func TestIndexEmptyStatesNameNoPluginVersion(t *testing.T) {
	if strings.Contains(string(Index()), "from plugin 0.1.11") || strings.Contains(string(Index()), "from 0.1.11 on") {
		t.Errorf("empty states still name the plugin version that started recording")
	}
}

// Each totals tile that has a value per bucket charts it under the number.
// Sessions and cache savings have no per-bucket value in the series.
func TestIndexChartsTotalsPerBucket(t *testing.T) {
	page := string(Index())
	for _, want := range []string{
		`<div id="t-cost-trend" class="spark hero-spark"></div>`,
		"tile('Requests', fmtCompact(t.requests), '', change(() => relativeChange(t.requests, prev.requests)), trend((b) => b.requests || 0, fmtCompact))",
		"tile('Failed records', fmtCompact(t.failed), '', null, trend((b) => b.failed || 0, fmtCompact))",
		"tile('Cache hit rate', fmtPct(t.cache_hit_rate), '', change(() => (prev.processed_tokens > 0 ? pointChange(t.cache_hit_rate, prev.cache_hit_rate) : null)), trend(cacheHitOf, fmtPct))",
		"sumBuckets(data.daily.points, xs)",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// The drift chart names the busiest lines at their end and highlights a line
// while its legend entry is hovered or focused.
func TestIndexLabelsAndHighlightsDriftLines(t *testing.T) {
	page := string(Index())
	for _, want := range []string{
		"endLabels(ends, END_LABELS, 14)",
		".chart svg.focusing .series:not(.focus) { opacity: 0.15; }",
		"entry.addEventListener('pointerenter', focus)",
		"entry.addEventListener('focus', focus)",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// A sparkline tooltip names the metric it reads, opens beside the hovered
// point and the point is marked on the line.
func TestIndexTiesSparklineTooltipToItsTile(t *testing.T) {
	page := string(Index())
	for _, want := range []string{
		"[{ value: v === null ? 'no data' : trend.fmt(v), label }]",
		"showTooltip(e, trend.fmtX(trend.xs[i]), rows, { left: x, right: x, top: box.top, height: box.height })",
		"el('span', { class: 'spark-cursor' })",
		"drawTrend(document.getElementById('t-cost-trend'), trend((b) => b.cost_usd || 0, fmtUSD), 'Cost')",
		"recordedValues(sums.map(value), xs, size, data.summary.data_since)",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// Short periods live in the Period filter instead of a separate "Right now"
// card, and the drift chart keeps model and harness views cleanly separated.
func TestIndexOffersShortPeriodsAndCleanDriftGroups(t *testing.T) {
	page := string(Index())
	for _, want := range []string{
		`<option value="15m">Last 15 minutes</option>`,
		`<option value="1h">Last 1 hour</option>`,
		`<option value="6h">Last 6 hours</option>`,
		`<option value="24h">Last 24 hours</option>`,
		`<option value="7d" selected>Last 7 days</option>`,
		`<option value="client">Client (harness)</option>`,
		`const wantKind = group === 'client' ? 'client_version' : group;`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, `id="realtime"`) {
		t.Error("page still renders the removed Right now section")
	}
}

// The request log loads when opened, one page at a time, and a row opens its
// details in place.
func TestIndexListsRequests(t *testing.T) {
	page := string(Index())
	for _, want := range []string{
		`<details id="requests" class="card" aria-labelledby="requests-title">`,
		"api('requests', { ...p, sort: state.reqSort, outcome: state.reqOutcome, offset: state.reqOffset, limit: REQUESTS_PAGE })",
		"row.setAttribute('aria-expanded', String(open))",
		"const DETAIL_SECTIONS = ['latency', 'requests', 'leftover', 'affinity', 'quality'];",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// The latency scatter plots time to first token against total latency on log
// scales, colored by provider.
func TestIndexPlotsLatency(t *testing.T) {
	page := string(Index())
	for _, want := range []string{
		`<details id="latency" class="card" aria-labelledby="latency-title">`,
		"api('latency', p)",
		"logTicks(xDomain[0], xDomain[1])",
		"colorFor('provider', pt.provider)",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// When a window has only one account, the Limits view renders one bar instead
// of duplicating the same meter in an account row beneath the pool bar.
func TestIndexOmitsDuplicateBarForSingleAccountLimit(t *testing.T) {
	page := string(Index())
	for _, want := range []string{
		"const single = pool.members.length === 1;",
		"el('span', {}, 'At limit: '",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}
