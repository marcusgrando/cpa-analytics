package client

import "testing"

func TestParse(t *testing.T) {
	cases := []struct{ ua, name, version string }{
		{"codex_cli_rs/0.130.0 (Ubuntu 24.4.0; x86_64) xterm-256color", "codex", "0.130.0"},
		{"codex_exec/0.131.2 (Mac OS 15.1.0; arm64)", "codex", "0.131.2"},
		{"codex_vscode/0.129.0", "codex", "0.129.0"},
		{"codex_app_server/0.170.0 (Linux; x86_64)", "codex", "0.170.0"},
		{"codex_custom_client/1.2.3", "codex", "1.2.3"},
		{"claude-cli/2.3.1 (external, cli)", "claude-code", "2.3.1"},
		{"t3code/1.4.0", "t3code", "1.4.0"},
		{"t3-code/0.1.10-nightly.20261115", "t3code", "0.1.10-nightly.20261115"},
		{"opencode/0.9.12 ai-sdk/5.0.0", "opencode", "0.9.12"},
		{"antigravity/1.15.8 linux/amd64", "antigravity", "1.15.8"},
		{"Gemini-CLI/0.5.0", "gemini-cli", "0.5.0"},
		{"OpenAI/JS 4.82.0", "openai", "JS"},
		{"curl/8.5.0", "curl", "8.5.0"},
		{"!!!/1.0", "other", ""},
		{"  ", "", ""},
		{"", "", ""},
	}
	for _, tc := range cases {
		name, version := Parse(tc.ua)
		if name != tc.name || version != tc.version {
			t.Errorf("Parse(%q) = %q, %q; want %q, %q", tc.ua, name, version, tc.name, tc.version)
		}
	}
}
