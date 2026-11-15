// Package accounting splits raw upstream token counters into non-overlapping
// buckets.
//
// It is a port of CLIProxyAPI sdk/cliproxy/usage/accounting.go at tag v8.0.71
// (EnsureTokenBreakdownForProvider and its helpers). CPA computes the same
// breakdown internally but does not pass it to plugins. When upgrading CPA,
// diff that file against the tag above and port any rule change here.
package accounting

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Quality values match CPA's TokenAccountingQuality.
const (
	QualityComplete     = "complete"
	QualityInconsistent = "inconsistent"
	QualityUnclassified = "unclassified"
)

// Breakdown holds mutually exclusive token buckets.
// Total = Uncached + CacheRead + CacheWrite + OutputNonReasoning + Reasoning + Unclassified.
type Breakdown struct {
	Uncached           int64
	CacheRead          int64
	CacheWrite         int64
	OutputNonReasoning int64
	Reasoning          int64
	Unclassified       int64
	Total              int64
	Quality            string
}

// Input is every input token, cached or not.
func (b Breakdown) Input() int64 { return b.Uncached + b.CacheRead + b.CacheWrite }

// Output is every output token, reasoning included.
func (b Breakdown) Output() int64 { return b.OutputNonReasoning + b.Reasoning }

type semantics uint8

const (
	semanticsUnknown semantics = iota
	// semanticsSubset: cache tokens are inside input, reasoning is inside output (OpenAI, Codex).
	semanticsSubset
	// semanticsIndependent: every counter is separate (Anthropic).
	semanticsIndependent
	// semanticsSeparateReasoning: cache inside input, reasoning separate from output (Gemini).
	semanticsSeparateReasoning
)

// Normalize returns the breakdown CPA would compute for a record from this
// provider and executor when the executor did not attach one itself.
func Normalize(provider, executorType string, d pluginapi.UsageDetail) Breakdown {
	sem := semanticsFor(provider, executorType)
	if d.CacheReadTokens == 0 && d.CachedTokens > 0 && d.InputTokens == 0 &&
		d.OutputTokens == 0 && d.ReasoningTokens == 0 && d.CacheCreationTokens == 0 && d.TotalTokens == 0 &&
		(sem == semanticsSubset || sem == semanticsSeparateReasoning) {
		d.CacheReadTokens = d.CachedTokens
	}
	return breakdownFor(d, sem)
}

func breakdownFor(d pluginapi.UsageDetail, sem semantics) Breakdown {
	if d.TotalTokens == 0 && d.InputTokens == 0 && d.OutputTokens == 0 {
		total, ok := unclassifiedLowerBound(d)
		if !ok {
			return inconsistent(d.TotalTokens, 0)
		}
		if total > 0 && (sem == semanticsUnknown || sem == semanticsSubset ||
			(sem == semanticsSeparateReasoning && (d.CacheReadTokens > 0 || d.CacheCreationTokens > 0 || d.CachedTokens > 0))) {
			return unclassified(total)
		}
	}
	switch sem {
	case semanticsSubset:
		return subset(d.InputTokens, d.CacheReadTokens, d.CacheCreationTokens, d.OutputTokens, d.ReasoningTokens, d.TotalTokens)
	case semanticsIndependent:
		return independent(d.InputTokens, d.CacheReadTokens, d.CacheCreationTokens, d.OutputTokens, d.ReasoningTokens, d.TotalTokens)
	case semanticsSeparateReasoning:
		return separateReasoning(d.InputTokens, d.CacheReadTokens, d.CacheCreationTokens, d.OutputTokens, d.ReasoningTokens, d.TotalTokens)
	default:
		total := d.TotalTokens
		if total == 0 {
			var ok bool
			if total, ok = unclassifiedLowerBound(d); !ok {
				return inconsistent(d.TotalTokens, 0)
			}
		}
		return unclassified(total)
	}
}

func subset(inputTotal, cacheRead, cacheWrite, outputTotal, reasoning, total int64) Breakdown {
	cacheTotal, okCache := sum(cacheRead, cacheWrite)
	expected, okExpected := sum(inputTotal, outputTotal)
	if !okCache || !okExpected || reasoning < 0 || cacheTotal > inputTotal || reasoning > outputTotal {
		return inconsistent(total, expected)
	}
	resolved, ok := resolveTotal(total, expected)
	if !ok {
		return inconsistent(total, expected)
	}
	return Breakdown{
		Uncached: inputTotal - cacheTotal, CacheRead: cacheRead, CacheWrite: cacheWrite,
		OutputNonReasoning: outputTotal - reasoning, Reasoning: reasoning,
		Total: resolved, Quality: QualityComplete,
	}
}

func independent(uncached, cacheRead, cacheWrite, nonReasoning, reasoning, total int64) Breakdown {
	inputTotal, okInput := sum(uncached, cacheRead, cacheWrite)
	outputTotal, okOutput := sum(nonReasoning, reasoning)
	expected, okExpected := sum(inputTotal, outputTotal)
	if !okInput || !okOutput || !okExpected {
		return inconsistent(total, expected)
	}
	resolved, ok := resolveTotal(total, expected)
	if !ok {
		return inconsistent(total, expected)
	}
	return Breakdown{
		Uncached: uncached, CacheRead: cacheRead, CacheWrite: cacheWrite,
		OutputNonReasoning: nonReasoning, Reasoning: reasoning,
		Total: resolved, Quality: QualityComplete,
	}
}

func separateReasoning(inputTotal, cacheRead, cacheWrite, nonReasoning, reasoning, total int64) Breakdown {
	cacheTotal, okCache := sum(cacheRead, cacheWrite)
	if !okCache || inputTotal < 0 || cacheTotal > inputTotal {
		return inconsistent(total, 0)
	}
	outputTotal, okOutput := sum(nonReasoning, reasoning)
	expected, okExpected := sum(inputTotal, outputTotal)
	if !okOutput || !okExpected {
		return inconsistent(total, expected)
	}
	resolved, ok := resolveTotal(total, expected)
	if !ok {
		return inconsistent(total, expected)
	}
	return Breakdown{
		Uncached: inputTotal - cacheTotal, CacheRead: cacheRead, CacheWrite: cacheWrite,
		OutputNonReasoning: nonReasoning, Reasoning: reasoning,
		Total: resolved, Quality: QualityComplete,
	}
}

func unclassified(total int64) Breakdown {
	if total <= 0 {
		if total < 0 {
			return Breakdown{Quality: QualityInconsistent}
		}
		return Breakdown{Quality: QualityComplete}
	}
	return Breakdown{Total: total, Unclassified: total, Quality: QualityUnclassified}
}

func inconsistent(total, fallback int64) Breakdown {
	resolved := total
	if resolved <= 0 {
		resolved = fallback
	}
	if resolved < 0 {
		resolved = 0
	}
	return Breakdown{Total: resolved, Unclassified: resolved, Quality: QualityInconsistent}
}

func unclassifiedLowerBound(d pluginapi.UsageDetail) (int64, bool) {
	cacheTokens, ok := sum(d.CacheReadTokens, d.CacheCreationTokens)
	if !ok || d.InputTokens < 0 || d.OutputTokens < 0 || d.ReasoningTokens < 0 || d.CachedTokens < 0 {
		return 0, false
	}
	inputTotal := max(d.InputTokens, cacheTokens, d.CachedTokens)
	outputTotal := max(d.OutputTokens, d.ReasoningTokens)
	return sum(inputTotal, outputTotal)
}

func semanticsFor(provider, executorType string) semantics {
	p := strings.ToLower(strings.TrimSpace(provider))
	e := strings.ToLower(strings.TrimSpace(executorType))
	value := strings.TrimSpace(p + " " + e)
	if value == "" || value == "unknown" || value == "unknown unknown" {
		return semanticsUnknown
	}
	if e == "openaicompatexecutor" || p == "openai-compatibility" || strings.HasPrefix(p, "openai-compatible-") {
		return semanticsSubset
	}
	if strings.Contains(value, "claude") || strings.Contains(value, "anthropic") {
		return semanticsIndependent
	}
	for _, marker := range []string{"gemini", "aistudio", "antigravity", "vertex", "interaction"} {
		if strings.Contains(value, marker) {
			return semanticsSeparateReasoning
		}
	}
	for _, marker := range []string{"openai", "codex", "xai", "grok", "kimi", "qwen", "deepseek", "openrouter"} {
		if strings.Contains(value, marker) {
			return semanticsSubset
		}
	}
	return semanticsUnknown
}

func resolveTotal(total, expected int64) (int64, bool) {
	if total < 0 || expected < 0 {
		return 0, false
	}
	if total == 0 {
		return expected, true
	}
	return total, total == expected
}

func sum(values ...int64) (int64, bool) {
	var total int64
	for _, v := range values {
		if v < 0 || total > int64(^uint64(0)>>1)-v {
			return 0, false
		}
		total += v
	}
	return total, true
}
