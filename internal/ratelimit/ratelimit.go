// Package ratelimit reads the subscription limit usage that Claude and Codex
// report in their response headers. CPA passes the upstream response headers
// in every usage record, so each request carries a snapshot of its account's
// limits. Formats follow CPA's own readers at the pinned tag:
// sdk/cliproxy/auth/selector.go for Claude and
// internal/runtime/executor/helps/codex_quota.go for Codex.
package ratelimit

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Window is one limit window: the percent used and when it resets, in Unix
// milliseconds, 0 when the reset could not be read.
type Window struct {
	UsedPct float64
	ResetAt int64
}

// Snapshot holds the session (5-hour) and weekly windows a response reported.
// A window the response did not report, or reported unreadably, is nil.
type Snapshot struct {
	Session *Window
	Weekly  *Window
}

const (
	sessionMaxMinutes = 24 * 60
	weekMinutes       = 7 * 24 * 60
	monthMinutes      = 30 * 24 * 60
)

// FromHeaders reads a snapshot from upstream response headers. respondedAt
// anchors Codex resets given as seconds from the response.
func FromHeaders(h http.Header, respondedAt time.Time) Snapshot {
	var s Snapshot
	if h == nil {
		return s
	}
	s.Session = claudeWindow(h, "5h")
	s.Weekly = claudeWindow(h, "7d")
	var sessionMinutes int64
	for _, name := range []string{"Primary", "Secondary"} {
		w, minutes := codexWindow(h, name, respondedAt)
		switch {
		case w == nil:
		case minutes <= sessionMaxMinutes:
			if s.Session == nil || minutes < sessionMinutes {
				s.Session = w
				sessionMinutes = minutes
			}
		case minutes >= weekMinutes && minutes < monthMinutes:
			s.Weekly = w
		}
	}
	return s
}

// claudeWindow reads Anthropic-Ratelimit-Unified-<window>-Utilization, a
// fraction of the limit, and -Reset, in epoch seconds.
func claudeWindow(h http.Header, window string) *Window {
	prefix := "Anthropic-Ratelimit-Unified-" + window + "-"
	used, ok := nonNegative(h.Get(prefix + "Utilization"))
	if !ok {
		return nil
	}
	w := &Window{UsedPct: used * 100}
	if reset, ok := epochSeconds(h.Get(prefix + "Reset")); ok {
		w.ResetAt = reset
	}
	return w
}

// codexWindow reads X-Codex-<name>-Used-Percent (0 to 100), -Window-Minutes,
// and -Reset-At (epoch seconds) or -Reset-After-Seconds. It returns the
// window length in minutes, since that is what tells the windows apart.
func codexWindow(h http.Header, name string, respondedAt time.Time) (*Window, int64) {
	prefix := "X-Codex-" + name + "-"
	used, ok := nonNegative(h.Get(prefix + "Used-Percent"))
	if !ok {
		return nil, 0
	}
	minutes, err := strconv.ParseInt(strings.TrimSpace(h.Get(prefix+"Window-Minutes")), 10, 64)
	if err != nil || minutes <= 0 {
		return nil, 0
	}
	w := &Window{UsedPct: used}
	if reset, ok := epochSeconds(h.Get(prefix + "Reset-At")); ok {
		w.ResetAt = reset
	} else if after, err := strconv.ParseInt(strings.TrimSpace(h.Get(prefix+"Reset-After-Seconds")), 10, 64); err == nil && after >= 0 && after <= math.MaxInt64/int64(time.Second) {
		w.ResetAt = respondedAt.Add(time.Duration(after) * time.Second).UnixMilli()
	}
	return w, minutes
}

func nonNegative(raw string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, false
	}
	return v, true
}

// epochSeconds returns a positive epoch-seconds value in milliseconds.
func epochSeconds(raw string) (int64, bool) {
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v * 1000, true
}
