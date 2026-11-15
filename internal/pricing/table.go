// Package pricing estimates what tokens would cost at API list prices, using
// the LiteLLM price table plus user overrides. Name matching follows T3 Code
// (apps/server/src/usage/usagePricing.ts).
package pricing

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/marcus-grando-azion/cpa-analytics/internal/accounting"
)

// Rates are USD per token.
type Rates struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// Source says where a price came from.
type Source string

const (
	SourceLiteLLM  Source = "litellm"
	SourceOverride Source = "override"
)

type modelRate struct {
	standard Rates
	priority *Rates
}

// Table is an immutable price table.
type Table struct {
	rates     map[string]modelRate
	overrides map[string]Rates
}

// firstPartyProviders are LiteLLM prefixes for the model makers' own APIs.
// Their price wins over a cloud's or reseller's for a bare model name.
var firstPartyProviders = map[string]bool{
	"anthropic": true, "deepseek": true, "gemini": true, "meta": true, "mistral": true, "openai": true, "xai": true,
}

// unpriceable names are either unbilled or ambiguous across model generations.
var unpriceable = map[string]bool{
	"<synthetic>": true, "synthetic": true, "opus": true, "sonnet": true, "haiku": true, "fable": true,
}

var dateSuffix = regexp.MustCompile(`-(\d{8}|\d{4}-\d{2}-\d{2})$`)

// ParseLiteLLM reads LiteLLM's model_prices_and_context_window.json. Entries
// without both input and output prices are skipped so cost is never understated as zero.
func ParseLiteLLM(raw []byte) (*Table, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse price table: %w", err)
	}
	rates := make(map[string]modelRate, len(doc))
	for name, entryRaw := range doc {
		if name == "sample_spec" { // LiteLLM's schema example, not a model
			continue
		}
		var entry map[string]any
		if json.Unmarshal(entryRaw, &entry) != nil {
			continue
		}
		standard, ok := readRates(entry, "", nil)
		if !ok {
			continue
		}
		key := normalize(name)
		if key == "" {
			continue
		}
		mr := modelRate{standard: standard}
		if priority, ok := readRates(entry, "_priority", &standard); ok {
			mr.priority = &priority
		}
		rates[key] = mr
	}
	addBareAliases(rates)
	return &Table{rates: rates}, nil
}

func addBareAliases(rates map[string]modelRate) {
	type candidate struct {
		rate       *modelRate
		firstParty bool
	}
	candidates := map[string]candidate{}
	for key, rate := range rates {
		alias := bareName(key)
		if alias == "" || alias == key {
			continue
		}
		if _, exists := rates[alias]; exists {
			continue
		}
		prefix, _, _ := strings.Cut(key, "/")
		firstParty := firstPartyProviders[prefix]
		r := rate
		held, ok := candidates[alias]
		switch {
		case !ok || (firstParty && !held.firstParty):
			candidates[alias] = candidate{rate: &r, firstParty: firstParty}
		case held.rate != nil && firstParty == held.firstParty && !sameRate(*held.rate, r):
			// Same-rank entries disagree: leave the bare name unpriced.
			candidates[alias] = candidate{rate: nil, firstParty: firstParty}
		}
	}
	for alias, c := range candidates {
		if c.rate != nil {
			rates[alias] = *c.rate
		}
	}
}

func sameRate(a, b modelRate) bool {
	if a.standard != b.standard {
		return false
	}
	if a.priority == nil || b.priority == nil {
		return true
	}
	return *a.priority == *b.priority
}

// readRates reads prices with an optional key suffix. Missing cache prices
// keep the standard tier's ratio to input, or fall back to the input price.
func readRates(entry map[string]any, suffix string, standard *Rates) (Rates, bool) {
	input, okIn := number(entry["input_cost_per_token"+suffix])
	output, okOut := number(entry["output_cost_per_token"+suffix])
	if !okIn || !okOut {
		return Rates{}, false
	}
	cache := func(name string, standardValue float64) float64 {
		if v, ok := number(entry[name+suffix]); ok {
			return v
		}
		if standard != nil && standard.Input > 0 {
			return standardValue / standard.Input * input
		}
		return input
	}
	var stdRead, stdWrite float64
	if standard != nil {
		stdRead, stdWrite = standard.CacheRead, standard.CacheWrite
	}
	return Rates{
		Input:      input,
		Output:     output,
		CacheRead:  cache("cache_read_input_token_cost", stdRead),
		CacheWrite: cache("cache_creation_input_token_cost", stdWrite),
	}, true
}

func number(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok && !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0
}

// WithOverrides returns a table where overrides (USD per 1M tokens) win over LiteLLM.
func WithOverrides(t *Table, overrides map[string]PriceOverride) *Table {
	out := &Table{rates: t.rates, overrides: make(map[string]Rates, len(overrides))}
	for model, o := range overrides {
		r := Rates{Input: o.Input / 1e6, Output: o.Output / 1e6, CacheRead: o.CacheRead / 1e6, CacheWrite: o.CacheWrite / 1e6}
		if o.CacheRead == 0 {
			r.CacheRead = r.Input
		}
		if o.CacheWrite == 0 {
			r.CacheWrite = r.Input
		}
		out.overrides[normalize(model)] = r
	}
	return out
}

// Len returns the number of priced model names.
func (t *Table) Len() int { return len(t.rates) }

// Lookup finds the rates for a model name as CPA or the upstream reports it.
// tier "priority" uses LiteLLM's priority prices when the model has them.
func (t *Table) Lookup(model, tier string) (Rates, Source, bool) {
	key := stripVariant(normalize(model))
	bare := bareName(key)
	if bare == "" || unpriceable[bare] {
		return Rates{}, "", false
	}
	for _, candidate := range []string{key, bare, dateSuffix.ReplaceAllString(key, ""), dateSuffix.ReplaceAllString(bare, "")} {
		if r, ok := t.overrides[candidate]; ok {
			return r, SourceOverride, true
		}
	}
	for _, candidate := range []string{key, bare, dateSuffix.ReplaceAllString(key, ""), dateSuffix.ReplaceAllString(bare, "")} {
		mr, ok := t.rates[candidate]
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(tier), "priority") && mr.priority != nil {
			return *mr.priority, SourceLiteLLM, true
		}
		return mr.standard, SourceLiteLLM, true
	}
	return Rates{}, "", false
}

// Cost prices a breakdown. Reasoning is billed as output. Unclassified tokens
// cannot be priced and are left out; callers report them as unpriced.
func Cost(r Rates, b accounting.Breakdown) float64 {
	return float64(b.Uncached)*r.Input +
		float64(b.CacheRead)*r.CacheRead +
		float64(b.CacheWrite)*r.CacheWrite +
		float64(b.OutputNonReasoning+b.Reasoning)*r.Output
}

func normalize(model string) string { return strings.ToLower(strings.TrimSpace(model)) }

func bareName(key string) string {
	if i := strings.LastIndex(key, "/"); i >= 0 {
		return key[i+1:]
	}
	return key
}

// stripVariant drops Claude's bracketed context variants ("[1m]") and CPA's
// parenthesized thinking suffix ("(high)").
func stripVariant(key string) string {
	if i := strings.IndexAny(key, "[("); i >= 0 {
		return strings.TrimSpace(key[:i])
	}
	return key
}
