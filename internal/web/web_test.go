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
