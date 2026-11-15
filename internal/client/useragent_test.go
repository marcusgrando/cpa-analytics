package client

import "testing"

func TestParse(t *testing.T) {
	cases := []struct{ ua, name, version string }{
		{"codex_cli_rs/0.130.0 (Ubuntu 24.4.0; x86_64) xterm-256color", "codex", "0.130.0"},
		{"codex_exec/0.131.2 (Mac OS 15.1.0; arm64)", "codex", "0.131.2"},
		{"codex_vscode/0.129.0", "codex", "0.129.0"},
		{"claude-cli/2.3.1 (external, cli)", "claude-code", "2.3.1"},
		{"t3code/1.4.0", "t3code", "1.4.0"},
		{"opencode/0.9.12 ai-sdk/5.0.0", "opencode", "0.9.12"},
		{"curl/8.5.0", "other", ""},
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
