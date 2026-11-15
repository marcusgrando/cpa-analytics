// Package client identifies the coding agent behind a request.
package client

import "strings"

// knownProducts maps the first User-Agent product token to a client name.
var knownProducts = map[string]string{
	"codex_cli_rs": "codex",
	"codex_exec":   "codex",
	"codex_vscode": "codex",
	"claude-cli":   "claude-code",
	"t3code":       "t3code",
	"opencode":     "opencode",
}

// Parse returns the client name and version from a User-Agent. Unknown agents
// return "other" with no version. An empty User-Agent returns empty strings.
func Parse(ua string) (name, version string) {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return "", ""
	}
	product, _, _ := strings.Cut(ua, " ")
	productName, productVersion, _ := strings.Cut(product, "/")
	if known, ok := knownProducts[strings.ToLower(productName)]; ok {
		return known, productVersion
	}
	return "other", ""
}
