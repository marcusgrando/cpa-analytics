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
		r, src, ok := tbl.Lookup(name, "")
		if !ok || src != SourceLiteLLM || !near(r.Input, 5e-06) || !near(r.CacheRead, 5e-07) || !near(r.CacheWrite, 6.25e-06) {
			t.Errorf("Lookup(%q) = %+v %q %v", name, r, src, ok)
		}
	}
}

func TestLookupPrefersFirstPartyForBareName(t *testing.T) {
	r, _, ok := sampleTable(t).Lookup("gpt-5.6-sol", "")
	if !ok || !near(r.Input, 2e-06) {
		t.Fatalf("got %+v %v", r, ok)
	}
}

func TestLookupConflictingResellersStayUnpriced(t *testing.T) {
	if r, _, ok := sampleTable(t).Lookup("glm-9", ""); ok {
		t.Fatalf("ambiguous bare name priced: %+v", r)
	}
}

func TestLookupPriorityTier(t *testing.T) {
	tbl := sampleTable(t)
	std, _, _ := tbl.Lookup("gpt-5.5", "default")
	pri, _, ok := tbl.Lookup("gpt-5.5", "priority")
	if !ok || !near(std.Input, 1.25e-06) || !near(pri.Input, 2.5e-06) || !near(pri.Output, 2e-05) || !near(pri.CacheRead, 2.5e-07) {
		t.Fatalf("std %+v pri %+v", std, pri)
	}
}

func TestLookupMissingCacheRatesFallBackToInput(t *testing.T) {
	r, _, ok := sampleTable(t).Lookup("vertex_ai/claude-opus-4-8", "")
	if !ok || !near(r.CacheRead, 5.5e-06) || !near(r.CacheWrite, 5.5e-06) {
		t.Fatalf("got %+v", r)
	}
}

func TestLookupUnknownAndUnpriceable(t *testing.T) {
	tbl := sampleTable(t)
	for _, name := range []string{"gpt-9-unknown", "opus", "", "image-only-model", "bad-entry", "sample_spec"} {
		if r, _, ok := tbl.Lookup(name, ""); ok {
			t.Errorf("Lookup(%q) priced: %+v", name, r)
		}
	}
}

func TestOverridesWin(t *testing.T) {
	tbl := WithOverrides(sampleTable(t), map[string]PriceOverride{
		"claude-opus-4-8": {Input: 1, Output: 2, CacheRead: ptr(0.1), CacheWrite: ptr(1.25)},
		"my-local-model":  {Input: 0.5, Output: 1},
	})
	r, src, ok := tbl.Lookup("claude-opus-4-8", "")
	if !ok || src != SourceOverride || !near(r.Input, 1e-06) || !near(r.CacheWrite, 1.25e-06) {
		t.Fatalf("got %+v %q", r, src)
	}
	r, _, ok = tbl.Lookup("my-local-model", "")
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
	r, _, ok := WithOverrides(&Table{}, overrides).Lookup("free-cache", "")
	if !ok || r.CacheRead != 0 || r.CacheWrite != 0 || !near(r.Input, 2e-06) {
		t.Fatalf("explicit zero cache prices = %+v, want 0", r)
	}
}

func ptr(v float64) *float64 { return &v }

// OpenAI's flex tier is billed at about half the standard price.
func TestLookupFlexTier(t *testing.T) {
	r, _, ok := sampleTable(t).Lookup("gpt-5.5", "flex")
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
