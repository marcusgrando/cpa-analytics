// Package client identifies the coding agent behind a request.
package client

import "strings"

// knownProducts maps the first User-Agent product token to a canonical client name.
var knownProducts = map[string]string{
	"codex":            "codex",
	"codex_cli_rs":     "codex",
	"codex_exec":       "codex",
	"codex_vscode":     "codex",
	"codex_app_server": "codex",
	"codex-cli":        "codex",
	"codex-tui":        "codex",
	"codex-desktop":    "codex",
	"claude-cli":       "claude-code",
	"claude-code":      "claude-code",
	"claude_code":      "claude-code",
	"claude-agent-sdk": "claude-code",
	"t3code":           "t3code",
	"t3-code":          "t3code",
	"t3_code":          "t3code",
	"opencode":         "opencode",
	"open-code":        "opencode",
	"antigravity":      "antigravity",
	"antigravity-acp":  "antigravity",
	"agy":              "antigravity",
	"gemini-cli":       "gemini-cli",
	"gemini_cli":       "gemini-cli",
	"geminicli":        "gemini-cli",
	"cursor":           "cursor",
	"cursor-agent":     "cursor",
	"aider":            "aider",
	"cline":            "cline",
	"roo-code":         "roo-code",
	"roocode":          "roo-code",
	"kilo-code":        "kilo-code",
	"kilocode":         "kilo-code",
}

const maxTokenLen = 64

// Parse returns the client name and version from a User-Agent. Known coding
// agents map to canonical names; any other valid product[/version] token keeps
// its normalized name and version so version drift still shows up. Unparseable
// agents return "other" with no version; an empty User-Agent returns empty strings.
func Parse(ua string) (name, version string) {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return "", ""
	}
	product, _, _ := strings.Cut(ua, " ")
	productName, productVersion, _ := strings.Cut(product, "/")
	lower := strings.ToLower(strings.TrimSpace(productName))
	ver := cleanVersion(productVersion)
	if known, ok := knownProducts[lower]; ok {
		return known, ver
	}
	if strings.HasPrefix(lower, "codex_") || strings.HasPrefix(lower, "codex-") {
		return "codex", ver
	}
	if validProductName(lower) {
		return lower, ver
	}
	return "other", ""
}

func validProductName(s string) bool {
	if s == "" || len(s) > maxTokenLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if i == 0 && !alnum {
			return false
		}
		if !alnum && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}

func cleanVersion(raw string) string {
	v := strings.TrimSpace(raw)
	v = strings.TrimRight(v, ";,)")
	if v == "" || len(v) > maxTokenLen {
		return ""
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] >= 0x7f {
			return ""
		}
	}
	return v
}
