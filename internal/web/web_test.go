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
func TestIndexMakesNoExternalRequests(t *testing.T) {
	page := strings.ReplaceAll(string(Index()), `"http://www.w3.org/2000/svg"`, "")
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
