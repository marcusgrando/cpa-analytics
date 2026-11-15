package client

import (
	"testing"
	"time"
)

func TestTracesTakeKeepsEntryForRetries(t *testing.T) {
	now := time.Unix(1000, 0)
	tr := NewTraces(10*time.Minute, 10, func() time.Time { return now })
	tr.Put("a", "codex_cli_rs/1.0.0")
	for i := 0; i < 2; i++ {
		ua, ok := tr.Take("a")
		if !ok || ua != "codex_cli_rs/1.0.0" {
			t.Fatalf("Take #%d = %q, %v", i, ua, ok)
		}
	}
	hits, misses := tr.Stats()
	if hits != 2 || misses != 0 {
		t.Fatalf("stats = %d, %d", hits, misses)
	}
}

func TestTracesExpireAfterTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	tr := NewTraces(10*time.Minute, 10, func() time.Time { return now })
	tr.Put("a", "x/1")
	now = now.Add(10*time.Minute + time.Second)
	if _, ok := tr.Take("a"); ok {
		t.Fatal("expired entry returned")
	}
	if _, misses := tr.Stats(); misses != 1 {
		t.Fatalf("misses = %d", misses)
	}
}

func TestTracesEvictOldestAtCapacity(t *testing.T) {
	now := time.Unix(1000, 0)
	tr := NewTraces(time.Hour, 2, func() time.Time { return now })
	tr.Put("a", "1")
	tr.Put("b", "2")
	tr.Put("c", "3")
	if _, ok := tr.Take("a"); ok {
		t.Fatal("oldest entry not evicted")
	}
	if _, ok := tr.Take("c"); !ok {
		t.Fatal("newest entry missing")
	}
	if tr.Len() != 2 {
		t.Fatalf("Len = %d", tr.Len())
	}
}

func TestTracesIgnoreEmptyTrace(t *testing.T) {
	tr := NewTraces(time.Hour, 2, time.Now)
	tr.Put("", "x/1")
	if tr.Len() != 0 {
		t.Fatal("empty trace stored")
	}
	if _, ok := tr.Take(""); ok {
		t.Fatal("empty trace found")
	}
}

// A WebSocket connection keeps one trace for every turn, so each lookup
// restarts the TTL. A session with turns less than a TTL apart stays joined.
func TestTracesTakeRestartsTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	tr := NewTraces(10*time.Minute, 10, func() time.Time { return now })
	tr.Put("ws", "codex_cli_rs/1.0.0")
	for i := 0; i < 3; i++ {
		now = now.Add(9 * time.Minute)
		if _, ok := tr.Take("ws"); !ok {
			t.Fatalf("turn %d after %v lost the trace", i, 9*time.Minute*time.Duration(i+1))
		}
	}
}
