// Package pricing estimates what tokens would cost at API list prices, using
// the LiteLLM price table plus user overrides. Name matching follows T3 Code
// (apps/server/src/usage/usagePricing.ts).
package pricing

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/marcusgrando/cpa-analytics/internal/accounting"
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

// pricedTiers are the OpenAI service tiers LiteLLM prices apart from the
// standard one, by the suffix of their price keys.
var pricedTiers = []string{"priority", "flex"}

type modelRate struct {
	standard Rates
	// tiers holds the prices of pricedTiers the model has.
	tiers map[string]Rates
	// long holds the rates a request pays once its input passes a threshold,
	// highest threshold first.
	long []longRate
}

// longRate is LiteLLM's "_above_<N>k_tokens" pricing: a request whose input
// tokens (uncached, cache read and cache write) exceed above pays these rates
// for all its tokens.
type longRate struct {
	above    int64
	standard Rates
	tiers    map[string]Rates
}

// Table is an immutable price table.
type Table struct {
	rates     map[string]modelRate
	overrides map[string]Rates
	// thresholds are every long-context threshold in the table, highest first.
	thresholds []int64
}

var longContextKey = regexp.MustCompile(`^input_cost_per_token_above_(\d+)k_tokens$`)

// firstPartyProviders are LiteLLM prefixes for the model makers' own APIs.
// Their price wins over a cloud's or reseller's for a bare model name.
var firstPartyProviders = map[string]bool{
	"alibaba": true, "anthropic": true, "dashscope": true, "deepseek": true, "gemini": true, "google": true,
	"kimi": true, "kimi-for-coding": true, "meta": true, "mistral": true, "moonshot": true, "moonshotai": true,
	"openai": true, "qwen": true, "xai": true,
}

// unpriceable names are either unbilled or ambiguous across model generations.
var unpriceable = map[string]bool{
	"<synthetic>": true, "synthetic": true, "opus": true, "sonnet": true, "haiku": true, "fable": true,
}

var dateSuffix = regexp.MustCompile(`-(\d{8}|\d{4}-\d{2}-\d{2})$`)

// variantSuffix matches trailing effort, release-channel, and Antigravity
// internal variant suffixes (e.g. -xhigh, -high, -trial, -n) stripped only
// after exact and date-stripped lookups miss.
var variantSuffix = regexp.MustCompile(`-(xhigh|high|medium|low|minimal|none|thinking|trial|preview|latest|exp|beta|alpha|n)$`)

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
		rates[key] = modelRate{standard: standard, tiers: readTiers(entry, "", standard), long: readLong(entry, standard)}
	}
	addBareAliases(rates)
	return &Table{rates: rates, thresholds: thresholds(rates)}, nil
}

// ParseModelsDev reads models.dev's api.json catalog (USD per 1M tokens per
// provider/model). First-party providers win when resolving bare model names.
func ParseModelsDev(raw []byte) (*Table, error) {
	var doc map[string]struct {
		Models map[string]struct {
			Cost *struct {
				Input      *float64 `json:"input"`
				Output     *float64 `json:"output"`
				CacheRead  *float64 `json:"cache_read"`
				CacheWrite *float64 `json:"cache_write"`
			} `json:"cost"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse models.dev table: %w", err)
	}
	rates := make(map[string]modelRate)
	for prov, p := range doc {
		provNorm := normalize(prov)
		if provNorm == "" {
			continue
		}
		for id, m := range p.Models {
			if m.Cost == nil || m.Cost.Input == nil || m.Cost.Output == nil {
				continue
			}
			in, okIn := validNonNegative(*m.Cost.Input)
			out, okOut := validNonNegative(*m.Cost.Output)
			if !okIn || !okOut {
				continue
			}
			cr := in
			if m.Cost.CacheRead != nil {
				if v, ok := validNonNegative(*m.Cost.CacheRead); ok {
					cr = v
				}
			}
			cw := in
			if m.Cost.CacheWrite != nil {
				if v, ok := validNonNegative(*m.Cost.CacheWrite); ok {
					cw = v
				}
			}
			idNorm := normalize(id)
			if idNorm == "" {
				continue
			}
			key := idNorm
			if !strings.HasPrefix(key, provNorm+"/") {
				key = provNorm + "/" + idNorm
			}
			rates[key] = modelRate{standard: Rates{
				Input: in / 1e6, Output: out / 1e6, CacheRead: cr / 1e6, CacheWrite: cw / 1e6,
			}}
		}
	}
	addBareAliases(rates)
	return &Table{rates: rates}, nil
}

// ParseOpenRouter reads OpenRouter's /api/v1/models JSON (USD per token as
// strings). Free and batch suffixed variants are skipped so paid traffic is
// never priced at $0 or batch rates.
func ParseOpenRouter(raw []byte) (*Table, error) {
	var doc struct {
		Data []struct {
			ID      string `json:"id"`
			Pricing struct {
				Prompt          string `json:"prompt"`
				Completion      string `json:"completion"`
				InputCacheRead  string `json:"input_cache_read"`
				InputCacheWrite string `json:"input_cache_write"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse openrouter table: %w", err)
	}
	rates := make(map[string]modelRate, len(doc.Data))
	for _, m := range doc.Data {
		id := normalize(m.ID)
		if id == "" || strings.HasSuffix(id, ":free") || strings.HasSuffix(id, ":batch") {
			continue
		}
		in, okIn := parseNonNegativeString(m.Pricing.Prompt)
		out, okOut := parseNonNegativeString(m.Pricing.Completion)
		if !okIn || !okOut {
			continue
		}
		cr, okCR := parseNonNegativeString(m.Pricing.InputCacheRead)
		if !okCR {
			cr = in
		}
		cw, okCW := parseNonNegativeString(m.Pricing.InputCacheWrite)
		if !okCW {
			cw = in
		}
		rates[id] = modelRate{standard: Rates{Input: in, Output: out, CacheRead: cr, CacheWrite: cw}}
	}
	addBareAliases(rates)
	return &Table{rates: rates}, nil
}

func validNonNegative(v float64) (float64, bool) {
	return v, !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0
}

func parseNonNegativeString(raw string) (float64, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return validNonNegative(v)
}

// Merge combines primary with one or more fallback tables. Primary entries
// always win; fallback entries fill only model keys missing from earlier tables.
func Merge(primary *Table, fallbacks ...*Table) *Table {
	merged := make(map[string]modelRate)
	if primary != nil {
		for k, v := range primary.rates {
			merged[k] = v
		}
	}
	for _, fb := range fallbacks {
		if fb == nil {
			continue
		}
		for k, v := range fb.rates {
			if _, exists := merged[k]; !exists {
				merged[k] = v
			}
		}
	}
	return &Table{rates: merged, thresholds: thresholds(merged)}
}

// readTiers reads the pricedTiers rates for the price keys ending in suffix.
func readTiers(entry map[string]any, suffix string, standard Rates) map[string]Rates {
	var tiers map[string]Rates
	for _, tier := range pricedTiers {
		if r, ok := readRates(entry, suffix+"_"+tier, &standard); ok {
			if tiers == nil {
				tiers = map[string]Rates{}
			}
			tiers[tier] = r
		}
	}
	return tiers
}

func readLong(entry map[string]any, standard Rates) []longRate {
	var long []longRate
	for name := range entry {
		m := longContextKey.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		k, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			continue
		}
		suffix := "_above_" + m[1] + "k_tokens"
		r, ok := readRates(entry, suffix, &standard)
		if !ok {
			continue
		}
		long = append(long, longRate{above: k * 1000, standard: r, tiers: readTiers(entry, suffix, r)})
	}
	sort.Slice(long, func(i, j int) bool { return long[i].above > long[j].above })
	return long
}

func thresholds(rates map[string]modelRate) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, mr := range rates {
		for _, l := range mr.long {
			if !seen[l.above] {
				seen[l.above] = true
				out = append(out, l.above)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] > out[j] })
	return out
}

// Band returns the smallest input size that prices the same as input under
// every model's long-context thresholds, so rows can be grouped by band and
// priced once per group. It is 0 below every threshold.
func (t *Table) Band(input int64) int64 {
	if t == nil {
		return 0
	}
	for _, above := range t.thresholds {
		if input > above {
			return above + 1
		}
	}
	return 0
}

// BandSQL is Band as a SQL expression over inputExpr. The thresholds are
// integers parsed from the price table, never caller input.
func (t *Table) BandSQL(inputExpr string) string {
	if t == nil || len(t.thresholds) == 0 {
		return "0"
	}
	var b strings.Builder
	b.WriteString("CASE")
	for _, above := range t.thresholds {
		fmt.Fprintf(&b, " WHEN (%s) > %d THEN %d", inputExpr, above, above+1)
	}
	b.WriteString(" ELSE 0 END")
	return b.String()
}

func addBareAliases(rates map[string]modelRate) {
	type candidate struct {
		rate       *modelRate
		firstParty bool
	}
	candidates := map[string]candidate{}
	keys := make([]string, 0, len(rates))
	for key := range rates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		rate := rates[key]
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

func sameTiers(a, b map[string]Rates) bool {
	if len(a) != len(b) {
		return false
	}
	for tier, ra := range a {
		if rb, ok := b[tier]; !ok || ra != rb {
			return false
		}
	}
	return true
}

func sameRate(a, b modelRate) bool {
	if a.standard != b.standard || len(a.long) != len(b.long) || !sameTiers(a.tiers, b.tiers) {
		return false
	}
	for i := range a.long {
		if a.long[i].above != b.long[i].above || a.long[i].standard != b.long[i].standard || !sameTiers(a.long[i].tiers, b.long[i].tiers) {
			return false
		}
	}
	return true
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
	out := &Table{overrides: make(map[string]Rates, len(overrides))}
	if t != nil {
		out.rates = t.rates
		out.thresholds = t.thresholds
	}
	for model, o := range overrides {
		out.overrides[stripVariant(normalize(model))] = Rates{
			Input:      o.Input / 1e6,
			Output:     o.Output / 1e6,
			CacheRead:  orInput(o.CacheRead, o.Input) / 1e6,
			CacheWrite: orInput(o.CacheWrite, o.Input) / 1e6,
		}
	}
	return out
}

func orInput(cache *float64, input float64) float64 {
	if cache == nil {
		return input
	}
	return *cache
}

// Len returns the number of priced model names.
func (t *Table) Len() int { return len(t.rates) }

// Lookup finds the rates for a model name as CPA or the upstream reports it.
// The priority and flex tiers use LiteLLM's prices for them when the model
// has them, and the standard prices otherwise. input is the request's input
// tokens, which select the long-context rates past their threshold.
// Overrides have no long-context rates.
func (t *Table) Lookup(model, tier string, input int64) (Rates, Source, bool) {
	key := stripVariant(normalize(model))
	bare := bareName(key)
	if bare == "" || unpriceable[bare] {
		return Rates{}, "", false
	}
	candidates := lookupCandidates(key, bare)
	for _, candidate := range candidates {
		if r, ok := t.overrides[candidate]; ok {
			return r, SourceOverride, true
		}
	}
	for _, candidate := range candidates {
		mr, ok := t.rates[candidate]
		if !ok {
			continue
		}
		tier = strings.ToLower(strings.TrimSpace(tier))
		for _, l := range mr.long {
			if input > l.above {
				if r, ok := l.tiers[tier]; ok {
					return r, SourceLiteLLM, true
				}
				return l.standard, SourceLiteLLM, true
			}
		}
		if r, ok := mr.tiers[tier]; ok {
			return r, SourceLiteLLM, true
		}
		return mr.standard, SourceLiteLLM, true
	}
	return Rates{}, "", false
}

// lookupCandidates orders candidate model keys from most specific to least:
// exact key/bare, date-stripped, then stripping trailing effort/channel/variant
// suffixes (up to two levels, e.g. -20260101-high or -trial-high).
func lookupCandidates(key, bare string) []string {
	out := make([]string, 0, 10)
	seen := make(map[string]bool, 10)
	add := func(s string) {
		if s != "" && !unpriceable[bareName(s)] && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	add(key)
	add(bare)
	dk, db := dateSuffix.ReplaceAllString(key, ""), dateSuffix.ReplaceAllString(bare, "")
	add(dk)
	add(db)
	for _, base := range []string{dk, db} {
		cur := base
		for range 2 {
			next := variantSuffix.ReplaceAllString(cur, "")
			next = dateSuffix.ReplaceAllString(next, "")
			if next == cur || next == "" {
				break
			}
			add(next)
			cur = next
		}
	}
	return out
}

// TypeCost is a cost split by what the tokens were billed as.
type TypeCost struct {
	Input      float64 `json:"input"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	// Output includes reasoning, which is billed as output.
	Output float64 `json:"output"`
}

// Total is the whole cost.
func (c TypeCost) Total() float64 { return c.Input + c.CacheRead + c.CacheWrite + c.Output }

// Add sums o into c.
func (c *TypeCost) Add(o TypeCost) {
	c.Input += o.Input
	c.CacheRead += o.CacheRead
	c.CacheWrite += o.CacheWrite
	c.Output += o.Output
}

// CostByType prices a breakdown per token type. Reasoning is billed as
// output. Unclassified tokens cannot be priced and are left out; callers
// report them as unpriced.
func CostByType(r Rates, b accounting.Breakdown) TypeCost {
	return TypeCost{
		Input:      float64(b.Uncached) * r.Input,
		CacheRead:  float64(b.CacheRead) * r.CacheRead,
		CacheWrite: float64(b.CacheWrite) * r.CacheWrite,
		Output:     float64(b.OutputNonReasoning+b.Reasoning) * r.Output,
	}
}

// Cost prices a breakdown. See CostByType.
func Cost(r Rates, b accounting.Breakdown) float64 { return CostByType(r, b).Total() }

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
