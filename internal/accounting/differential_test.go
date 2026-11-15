package accounting

import (
	"math/rand"
	"testing"

	cpausage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TestMatchesCPA compares the port with CPA's own implementation, so a CPA
// upgrade that changes the rules fails here.
func TestMatchesCPA(t *testing.T) {
	providers := [][2]string{
		{"codex", "CodexExecutor"}, {"claude", "ClaudeExecutor"}, {"gemini", "GeminiExecutor"},
		{"antigravity", "AntigravityExecutor"}, {"openai-compatibility", "OpenAICompatExecutor"},
		{"xai", "XAIExecutor"}, {"mystery", "MysteryExecutor"}, {"", ""},
	}
	rng := rand.New(rand.NewSource(1))
	pick := func() int64 {
		switch rng.Intn(4) {
		case 0:
			return 0
		case 1:
			return int64(rng.Intn(50))
		default:
			return int64(rng.Intn(5000))
		}
	}
	for i := 0; i < 20000; i++ {
		pv := providers[rng.Intn(len(providers))]
		d := pluginapi.UsageDetail{
			InputTokens: pick(), OutputTokens: pick(), ReasoningTokens: pick(),
			CachedTokens: pick(), CacheReadTokens: pick(), CacheCreationTokens: pick(),
		}
		if rng.Intn(3) == 0 {
			d.TotalTokens = d.InputTokens + d.OutputTokens
		}
		got := Normalize(pv[0], pv[1], d)
		ref := cpausage.EnsureTokenBreakdownForProvider(cpausage.Detail{
			InputTokens: d.InputTokens, OutputTokens: d.OutputTokens, ReasoningTokens: d.ReasoningTokens,
			CachedTokens: d.CachedTokens, CacheReadTokens: d.CacheReadTokens, CacheCreationTokens: d.CacheCreationTokens,
			TotalTokens: d.TotalTokens,
		}, pv[0], pv[1]).TokenBreakdown
		want := Breakdown{
			Uncached: ref.Input.UncachedTokens, CacheRead: ref.Input.CacheReadTokens, CacheWrite: ref.Input.CacheWriteTokens,
			OutputNonReasoning: ref.Output.NonReasoningTokens, Reasoning: ref.Output.ReasoningTokens,
			Unclassified: ref.UnclassifiedTokens, Total: ref.TotalTokens, Quality: string(ref.Quality),
		}
		if got != want {
			t.Fatalf("provider %v detail %+v: port %+v, CPA %+v", pv, d, got, want)
		}
	}
}
