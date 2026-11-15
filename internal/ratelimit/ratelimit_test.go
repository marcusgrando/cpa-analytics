package ratelimit

import (
	"net/http"
	"testing"
	"time"
)

var at = time.Unix(1_800_000_000, 0)

func header(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestClaudeUtilizationIsAFraction(t *testing.T) {
	got := FromHeaders(header(
		"anthropic-ratelimit-unified-5h-utilization", "0.42",
		"anthropic-ratelimit-unified-5h-reset", "1800003600",
		"anthropic-ratelimit-unified-7d-utilization", "0.075",
		"anthropic-ratelimit-unified-7d-reset", "1800300000",
	), at)
	want := Snapshot{
		Session: &Window{UsedPct: 42, ResetAt: 1_800_003_600_000},
		Weekly:  &Window{UsedPct: 7.5, ResetAt: 1_800_300_000_000},
	}
	assertSnapshot(t, got, want)
}

// Codex sends a primary and a secondary window. Which one is the week is
// told by the window length, not by the name.
func TestCodexWindowsAreClassifiedByLength(t *testing.T) {
	got := FromHeaders(header(
		"X-Codex-Primary-Used-Percent", "12",
		"X-Codex-Primary-Window-Minutes", "300",
		"X-Codex-Primary-Reset-At", "1800010000",
		"X-Codex-Secondary-Used-Percent", "3.5",
		"X-Codex-Secondary-Window-Minutes", "10080",
		"X-Codex-Secondary-Reset-After-Seconds", "86400",
	), at)
	want := Snapshot{
		Session: &Window{UsedPct: 12, ResetAt: 1_800_010_000_000},
		// Without Reset-At the reset is counted from the response time.
		Weekly: &Window{UsedPct: 3.5, ResetAt: at.Add(24 * time.Hour).UnixMilli()},
	}
	assertSnapshot(t, got, want)

	weeklyOnly := FromHeaders(header(
		"X-Codex-Primary-Used-Percent", "40",
		"X-Codex-Primary-Window-Minutes", "10080",
		"X-Codex-Primary-Reset-At", "1800300000",
	), at)
	assertSnapshot(t, weeklyOnly, Snapshot{Weekly: &Window{UsedPct: 40, ResetAt: 1_800_300_000_000}})
}

// Values that cannot be read are left out. They are never clamped or guessed.
func TestUnreadableValuesAreLeftOut(t *testing.T) {
	cases := map[string]http.Header{
		"no headers":            nil,
		"negative utilization":  header("anthropic-ratelimit-unified-7d-utilization", "-0.1", "anthropic-ratelimit-unified-7d-reset", "1800300000"),
		"utilization not a num": header("anthropic-ratelimit-unified-7d-utilization", "high"),
		"utilization NaN":       header("anthropic-ratelimit-unified-7d-utilization", "NaN"),
		"codex without length":  header("X-Codex-Primary-Used-Percent", "40", "X-Codex-Primary-Reset-At", "1800300000"),
		"codex monthly window":  header("X-Codex-Primary-Used-Percent", "40", "X-Codex-Primary-Window-Minutes", "43200"),
		"codex 2-day window":    header("X-Codex-Primary-Used-Percent", "40", "X-Codex-Primary-Window-Minutes", "2880"),
		"codex bad percent":     header("X-Codex-Primary-Used-Percent", "Inf", "X-Codex-Primary-Window-Minutes", "10080"),
		"code review window":    header("X-Codex-Code-Review-Primary-Used-Percent", "40", "X-Codex-Code-Review-Primary-Window-Minutes", "10080"),
		"scoped claude window":  header("anthropic-ratelimit-unified-7d_opus-utilization", "0.5"),
	}
	for name, h := range cases {
		if got := FromHeaders(h, at); got.Session != nil || got.Weekly != nil {
			t.Errorf("%s: got %+v / %+v, want nothing", name, got.Session, got.Weekly)
		}
	}
}

// A usable percent with an unreadable reset keeps the percent: the reset only
// tells cycles apart.
func TestUnreadableResetKeepsThePercent(t *testing.T) {
	got := FromHeaders(header(
		"anthropic-ratelimit-unified-7d-utilization", "0.3",
		"anthropic-ratelimit-unified-7d-reset", "soon",
	), at)
	assertSnapshot(t, got, Snapshot{Weekly: &Window{UsedPct: 30}})
}

func assertSnapshot(t *testing.T, got, want Snapshot) {
	t.Helper()
	for _, w := range []struct {
		name      string
		got, want *Window
	}{{"session", got.Session, want.Session}, {"weekly", got.Weekly, want.Weekly}} {
		switch {
		case w.want == nil && w.got != nil:
			t.Errorf("%s = %+v, want none", w.name, *w.got)
		case w.want != nil && w.got == nil:
			t.Errorf("%s missing, want %+v", w.name, *w.want)
		case w.want != nil && *w.got != *w.want:
			t.Errorf("%s = %+v, want %+v", w.name, *w.got, *w.want)
		}
	}
}
