package pricing

import (
	"gopkg.in/yaml.v3"
	"math"
	"os"
	"testing"

	"github.com/marcusgrando/cpa-analytics/internal/accounting"
)

func sampleTable(t *testing.T) *Table {
	t.Helper()
	raw, err := os.ReadFile("testdata/litellm_sample.json")
	if err != nil {
		t.Fatal(err)
	}
	tbl, err := ParseLiteLLM(raw)
	if err != nil {
		t.Fatal(err)
	}
	return tbl
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestLookupResolvesNameVariants(t *testing.T) {
	tbl := sampleTable(t)
	for _, name := range []string{"claude-opus-4-8", "Claude-Opus-4-8", "anthropic/claude-opus-4-8", "claude-opus-4-8-20260101", "claude-opus-4-8[1m]", "claude-opus-4-8(high)"} {
		r, src, ok := tbl.Lookup(name, "", 0)
		if !ok || src != SourceLiteLLM || !near(r.Input, 5e-06) || !near(r.CacheRead, 5e-07) || !near(r.CacheWrite, 6.25e-06) {
			t.Errorf("Lookup(%q) = %+v %q %v", name, r, src, ok)
		}
	}
}

func TestLookupPrefersFirstPartyForBareName(t *testing.T) {
	r, _, ok := sampleTable(t).Lookup("gpt-5.6-sol", "", 0)
	if !ok || !near(r.Input, 2e-06) {
		t.Fatalf("got %+v %v", r, ok)
	}
}

func TestLookupConflictingResellersStayUnpriced(t *testing.T) {
	if r, _, ok := sampleTable(t).Lookup("glm-9", "", 0); ok {
		t.Fatalf("ambiguous bare name priced: %+v", r)
	}
}

func TestLookupPriorityTier(t *testing.T) {
	tbl := sampleTable(t)
	std, _, _ := tbl.Lookup("gpt-5.5", "default", 0)
	pri, _, ok := tbl.Lookup("gpt-5.5", "priority", 0)
	if !ok || !near(std.Input, 1.25e-06) || !near(pri.Input, 2.5e-06) || !near(pri.Output, 2e-05) || !near(pri.CacheRead, 2.5e-07) {
		t.Fatalf("std %+v pri %+v", std, pri)
	}
}

func TestLookupMissingCacheRatesFallBackToInput(t *testing.T) {
	r, _, ok := sampleTable(t).Lookup("vertex_ai/claude-opus-4-8", "", 0)
	if !ok || !near(r.CacheRead, 5.5e-06) || !near(r.CacheWrite, 5.5e-06) {
		t.Fatalf("got %+v", r)
	}
}

func TestLookupUnknownAndUnpriceable(t *testing.T) {
	tbl := sampleTable(t)
	for _, name := range []string{"gpt-9-unknown", "opus", "", "image-only-model", "bad-entry", "sample_spec"} {
		if r, _, ok := tbl.Lookup(name, "", 0); ok {
			t.Errorf("Lookup(%q) priced: %+v", name, r)
		}
	}
}

func TestOverridesWin(t *testing.T) {
	tbl := WithOverrides(sampleTable(t), map[string]PriceOverride{
		"claude-opus-4-8": {Input: 1, Output: 2, CacheRead: ptr(0.1), CacheWrite: ptr(1.25)},
		"my-local-model":  {Input: 0.5, Output: 1},
	})
	r, src, ok := tbl.Lookup("claude-opus-4-8", "", 0)
	if !ok || src != SourceOverride || !near(r.Input, 1e-06) || !near(r.CacheWrite, 1.25e-06) {
		t.Fatalf("got %+v %q", r, src)
	}
	r, _, ok = tbl.Lookup("my-local-model", "", 0)
	if !ok || !near(r.CacheRead, 0.5e-06) {
		t.Fatalf("override without cache rates should fall back to input: %+v", r)
	}
}

func TestCost(t *testing.T) {
	r := Rates{Input: 5e-06, Output: 2.5e-05, CacheRead: 5e-07, CacheWrite: 6.25e-06}
	b := accounting.Breakdown{Uncached: 100, CacheRead: 900, CacheWrite: 50, OutputNonReasoning: 250, Reasoning: 50, Unclassified: 7}
	want := 100*5e-06 + 900*5e-07 + 50*6.25e-06 + 300*2.5e-05
	if got := Cost(r, b); math.Abs(got-want) > 1e-12 {
		t.Fatalf("Cost = %v, want %v", got, want)
	}
}

func TestParseRejectsNonObject(t *testing.T) {
	if _, err := ParseLiteLLM([]byte(`[1,2]`)); err == nil {
		t.Fatal("expected error")
	}
}

// A cache price the operator wrote as 0 means free, not "use the input price".
func TestOverrideKeepsExplicitZeroCachePrice(t *testing.T) {
	var overrides map[string]PriceOverride
	if err := yaml.Unmarshal([]byte("free-cache:\n  input: 2\n  output: 4\n  cache_read: 0\n  cache_write: 0\n"), &overrides); err != nil {
		t.Fatal(err)
	}
	r, _, ok := WithOverrides(&Table{}, overrides).Lookup("free-cache", "", 0)
	if !ok || r.CacheRead != 0 || r.CacheWrite != 0 || !near(r.Input, 2e-06) {
		t.Fatalf("explicit zero cache prices = %+v, want 0", r)
	}
}

func ptr(v float64) *float64 { return &v }

// OpenAI's flex tier is billed at about half the standard price.
func TestLookupFlexTier(t *testing.T) {
	r, _, ok := sampleTable(t).Lookup("gpt-5.5", "flex", 0)
	if !ok || !near(r.Input, 6.25e-07) || !near(r.Output, 5e-06) || !near(r.CacheRead, 6.25e-08) {
		t.Fatalf("flex = %+v %v", r, ok)
	}
}

func TestCostByTypeAddsUpToCost(t *testing.T) {
	r := Rates{Input: 5e-06, Output: 2.5e-05, CacheRead: 5e-07, CacheWrite: 6.25e-06}
	b := accounting.Breakdown{Uncached: 100, CacheRead: 900, CacheWrite: 50, OutputNonReasoning: 250, Reasoning: 50, Unclassified: 7}
	got := CostByType(r, b)
	want := TypeCost{Input: 100 * 5e-06, CacheRead: 900 * 5e-07, CacheWrite: 50 * 6.25e-06, Output: 300 * 2.5e-05}
	if !near(got.Input, want.Input) || !near(got.CacheRead, want.CacheRead) || !near(got.CacheWrite, want.CacheWrite) || !near(got.Output, want.Output) {
		t.Fatalf("CostByType = %+v, want %+v", got, want)
	}
	if !near(got.Total(), Cost(r, b)) {
		t.Errorf("parts sum to %v, Cost is %v", got.Total(), Cost(r, b))
	}
}

// longContextTable prices a model that charges more once a request's input
// passes 200k tokens, as LiteLLM lists Gemini Pro and Claude's 1M context.
func longContextTable(t *testing.T) *Table {
	t.Helper()
	tbl, err := ParseLiteLLM([]byte(`{
		"gemini/gemini-3-pro": {"input_cost_per_token": 2e-06, "output_cost_per_token": 1.2e-05, "cache_read_input_token_cost": 2e-07,
			"input_cost_per_token_above_200k_tokens": 4e-06, "output_cost_per_token_above_200k_tokens": 1.8e-05,
			"cache_read_input_token_cost_above_200k_tokens": 4e-07},
		"openai/gpt-6": {"input_cost_per_token": 2.5e-06, "output_cost_per_token": 1.5e-05, "cache_read_input_token_cost": 2.5e-07,
			"input_cost_per_token_priority": 5e-06, "output_cost_per_token_priority": 3e-05,
			"input_cost_per_token_above_272k_tokens": 5e-06, "output_cost_per_token_above_272k_tokens": 2.25e-05,
			"input_cost_per_token_above_272k_tokens_priority": 1e-05, "output_cost_per_token_above_272k_tokens_priority": 4.5e-05}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	return tbl
}

func TestLookupChargesLongContextRatesAboveTheThreshold(t *testing.T) {
	tbl := longContextTable(t)
	at, _, _ := tbl.Lookup("gemini-3-pro", "", 200_000)
	above, _, _ := tbl.Lookup("gemini-3-pro", "", 200_001)
	if !near(at.Input, 2e-06) || !near(at.Output, 1.2e-05) || !near(at.CacheRead, 2e-07) {
		t.Errorf("at 200k = %+v, want the standard rates", at)
	}
	if !near(above.Input, 4e-06) || !near(above.Output, 1.8e-05) || !near(above.CacheRead, 4e-07) {
		t.Errorf("above 200k = %+v, want the long-context rates", above)
	}
}

func TestLookupLongContextKeepsTheServiceTier(t *testing.T) {
	tbl := longContextTable(t)
	pri, _, _ := tbl.Lookup("gpt-6", "priority", 300_000)
	std, _, _ := tbl.Lookup("gpt-6", "", 300_000)
	if !near(pri.Input, 1e-05) || !near(pri.Output, 4.5e-05) || !near(std.Input, 5e-06) {
		t.Errorf("priority above 272k = %+v, standard above 272k = %+v", pri, std)
	}
	// Without a cache read price for the long band, it keeps the standard ratio to input.
	if !near(std.CacheRead, 5e-07) {
		t.Errorf("cache read above 272k = %v, want 5e-07", std.CacheRead)
	}
}

// Band maps an input size to the smallest input that prices the same, so a
// query can group rows by band and price each group once.
func TestBandGroupsInputsThatPriceAlike(t *testing.T) {
	tbl := longContextTable(t)
	for input, want := range map[int64]int64{0: 0, 200_000: 0, 200_001: 200_001, 272_000: 200_001, 272_001: 272_001, 1_000_000: 272_001} {
		if got := tbl.Band(input); got != want {
			t.Errorf("Band(%d) = %d, want %d", input, got, want)
		}
	}
	want := "CASE WHEN (x) > 272000 THEN 272001 WHEN (x) > 200000 THEN 200001 ELSE 0 END"
	if got := tbl.BandSQL("x"); got != want {
		t.Errorf("BandSQL = %q, want %q", got, want)
	}
	if got := sampleTable(t).BandSQL("x"); got != "0" {
		t.Errorf("BandSQL without long-context prices = %q, want 0", got)
	}
}

func TestLookupStripsEffortAndVariantSuffixes(t *testing.T) {
	tbl, err := ParseLiteLLM([]byte(`{
		"gemini/gemini-3.8-flash": {"input_cost_per_token": 7.5e-07, "output_cost_per_token": 3.75e-06, "cache_read_input_token_cost": 7.5e-08},
		"gemini/gemini-4-argon": {"input_cost_per_token": 2e-06, "output_cost_per_token": 1e-05, "cache_read_input_token_cost": 1e-07},
		"gemini/gemini-4-neon": {"input_cost_per_token": 3e-06, "output_cost_per_token": 1.5e-05, "cache_read_input_token_cost": 1.5e-07},
		"gemini/gemini-4-hydrogen": {"input_cost_per_token": 1.5e-06, "output_cost_per_token": 7.5e-06, "cache_read_input_token_cost": 7.5e-08}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]float64{
		"gemini-3.8-flash-n":      7.5e-07,
		"gemini-3.8-flash-high":   7.5e-07,
		"gemini-4-argon-trial":    2e-06,
		"gemini-4-argon-high":     2e-06,
		"gemini-4-neon-xhigh":     3e-06,
		"gemini-4-hydrogen-xhigh": 1.5e-06,
		"gemini-4-argon-20260101-high": 2e-06,
	}
	for model, wantInput := range cases {
		r, src, ok := tbl.Lookup(model, "", 0)
		if !ok || src != SourceLiteLLM || !near(r.Input, wantInput) {
			t.Errorf("Lookup(%q) = %+v %q %v, want input %v", model, r, src, ok, wantInput)
		}
	}
}

func TestParseModelsDevAndOpenRouterAndMerge(t *testing.T) {
	md, err := ParseModelsDev([]byte(`{
		"google": {
			"models": {
				"gemini-4-neon": {"cost": {"input": 3, "output": 15, "cache_read": 0.15}}
			}
		},
		"reseller": {
			"models": {
				"gemini-4-neon": {"cost": {"input": 9, "output": 45}}
			}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	r, _, ok := md.Lookup("gemini-4-neon", "", 0)
	if !ok || !near(r.Input, 3e-06) || !near(r.Output, 1.5e-05) || !near(r.CacheRead, 1.5e-07) {
		t.Fatalf("models.dev lookup = %+v %v", r, ok)
	}

	or, err := ParseOpenRouter([]byte(`{"data":[
		{"id":"google/gemini-4-neon:free","pricing":{"prompt":"0","completion":"0"}},
		{"id":"google/gemini-4-neon:batch","pricing":{"prompt":"0.0000015","completion":"0.0000075"}},
		{"id":"x-ai/grok-4-fast","pricing":{"prompt":"0.0000005","completion":"0.000002","input_cache_read":"0.00000005"}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := or.Lookup("gemini-4-neon", "", 0); ok {
		t.Fatal("openrouter should skip :free and :batch entries")
	}
	r, _, ok = or.Lookup("grok-4-fast", "", 0)
	if !ok || !near(r.Input, 5e-07) || !near(r.CacheRead, 5e-08) {
		t.Fatalf("openrouter lookup = %+v %v", r, ok)
	}

	merged := Merge(sampleTable(t), md, or)
	// Primary LiteLLM wins for existing models; fallbacks fill missing ones.
	if r, _, ok := merged.Lookup("claude-opus-4-8", "", 0); !ok || !near(r.Input, 5e-06) {
		t.Fatalf("merged primary = %+v %v", r, ok)
	}
	if r, _, ok := merged.Lookup("gemini-4-neon-xhigh", "", 0); !ok || !near(r.Input, 3e-06) {
		t.Fatalf("merged models.dev = %+v %v", r, ok)
	}
	if r, _, ok := merged.Lookup("grok-4-fast", "", 0); !ok || !near(r.Input, 5e-07) {
		t.Fatalf("merged openrouter = %+v %v", r, ok)
	}
}

func TestParseModelsDevPreventsResellerSlashSpoofingAndNilWithOverrides(t *testing.T) {
	md, err := ParseModelsDev([]byte(`{
		"reseller": {
			"models": {
				"openai/gpt-5": {"cost": {"input": 99, "output": 99}}
			}
		},
		"openai": {
			"models": {
				"gpt-5": {"cost": {"input": 1.25, "output": 10}}
			}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	r, _, ok := md.Lookup("gpt-5", "", 0)
	if !ok || !near(r.Input, 1.25e-06) {
		t.Fatalf("reseller spoofed first-party price: %+v %v", r, ok)
	}

	ov := WithOverrides(nil, map[string]PriceOverride{
		"my-model-20260101-high": {Input: 2, Output: 8},
	})
	if r, src, ok := ov.Lookup("my-model-20260101-high", "", 0); !ok || src != SourceOverride || !near(r.Input, 2e-06) {
		t.Fatalf("WithOverrides(nil) lookup = %+v %q %v", r, src, ok)
	}
}
