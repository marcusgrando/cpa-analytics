package accounting

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestNormalize(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		executor string
		detail   pluginapi.UsageDetail
		want     Breakdown
	}{
		{
			name: "codex subset", provider: "codex", executor: "CodexExecutor",
			detail: pluginapi.UsageDetail{InputTokens: 1000, CacheReadTokens: 800, CachedTokens: 800, OutputTokens: 200, ReasoningTokens: 50, TotalTokens: 1200},
			want:   Breakdown{Uncached: 200, CacheRead: 800, OutputNonReasoning: 150, Reasoning: 50, Total: 1200, Quality: QualityComplete},
		},
		{
			name: "claude independent", provider: "claude", executor: "ClaudeExecutor",
			detail: pluginapi.UsageDetail{InputTokens: 100, CacheReadTokens: 900, CacheCreationTokens: 50, OutputTokens: 300},
			want:   Breakdown{Uncached: 100, CacheRead: 900, CacheWrite: 50, OutputNonReasoning: 300, Total: 1350, Quality: QualityComplete},
		},
		{
			name: "claude thinking inside output", provider: "claude", executor: "ClaudeExecutor",
			detail: pluginapi.UsageDetail{InputTokens: 6, CacheReadTokens: 90000, CachedTokens: 90000, CacheCreationTokens: 1200, OutputTokens: 500, ReasoningTokens: 300, TotalTokens: 91706},
			want:   Breakdown{Uncached: 6, CacheRead: 90000, CacheWrite: 1200, OutputNonReasoning: 200, Reasoning: 300, Total: 91706, Quality: QualityComplete},
		},
		{
			name: "claude total matching neither layout is inconsistent", provider: "claude", executor: "ClaudeExecutor",
			detail: pluginapi.UsageDetail{InputTokens: 6, CacheReadTokens: 900, OutputTokens: 500, ReasoningTokens: 300, TotalTokens: 1000},
			want:   Breakdown{Unclassified: 1000, Total: 1000, Quality: QualityInconsistent},
		},
		{
			name: "gemini separate reasoning", provider: "gemini", executor: "GeminiExecutor",
			detail: pluginapi.UsageDetail{InputTokens: 500, CacheReadTokens: 100, OutputTokens: 40, ReasoningTokens: 60},
			want:   Breakdown{Uncached: 400, CacheRead: 100, OutputNonReasoning: 40, Reasoning: 60, Total: 600, Quality: QualityComplete},
		},
		{
			name: "openai compatible executor wins over claude in name", provider: "claude-proxy", executor: "OpenAICompatExecutor",
			detail: pluginapi.UsageDetail{InputTokens: 100, CacheReadTokens: 40, OutputTokens: 10},
			want:   Breakdown{Uncached: 60, CacheRead: 40, OutputNonReasoning: 10, Total: 110, Quality: QualityComplete},
		},
		{
			name: "unknown provider stays unclassified", provider: "mystery", executor: "MysteryExecutor",
			detail: pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 20, TotalTokens: 120},
			want:   Breakdown{Unclassified: 120, Total: 120, Quality: QualityUnclassified},
		},
		{
			name: "cache read above input is inconsistent", provider: "codex", executor: "CodexExecutor",
			detail: pluginapi.UsageDetail{InputTokens: 100, CacheReadTokens: 500, OutputTokens: 10},
			want:   Breakdown{Unclassified: 110, Total: 110, Quality: QualityInconsistent},
		},
		{
			name: "total mismatch is inconsistent", provider: "codex", executor: "CodexExecutor",
			detail: pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 10, TotalTokens: 999},
			want:   Breakdown{Unclassified: 999, Total: 999, Quality: QualityInconsistent},
		},
		{
			name: "cached only on subset becomes cache read", provider: "codex", executor: "CodexExecutor",
			detail: pluginapi.UsageDetail{CachedTokens: 70},
			want:   Breakdown{Unclassified: 70, Total: 70, Quality: QualityUnclassified},
		},
		{
			name: "empty usage", provider: "codex", executor: "CodexExecutor",
			detail: pluginapi.UsageDetail{},
			want:   Breakdown{Quality: QualityComplete},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Normalize(tc.provider, tc.executor, tc.detail)
			if got != tc.want {
				t.Fatalf("Normalize() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestInputTotal(t *testing.T) {
	b := Breakdown{Uncached: 1, CacheRead: 2, CacheWrite: 3, OutputNonReasoning: 4, Reasoning: 5}
	if b.Input() != 6 || b.Output() != 9 {
		t.Fatalf("Input()=%d Output()=%d", b.Input(), b.Output())
	}
}
