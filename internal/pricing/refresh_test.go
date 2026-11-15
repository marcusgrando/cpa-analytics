package pricing

import (
	"errors"
	"os"
	"testing"
	"time"
)

type memKV map[string][]byte

func (m memKV) SaveKV(k string, v []byte) error { m[k] = v; return nil }
func (m memKV) LoadKV(k string) ([]byte, error) { return m[k], nil }

func TestRefresherFetchesAndPersists(t *testing.T) {
	raw, _ := os.ReadFile("testdata/litellm_sample.json")
	kv := memKV{}
	now := time.Unix(5000, 0)
	r := NewRefresher(func(string) ([]byte, error) { return raw, nil }, kv, "https://example.invalid/p.json", time.Hour)
	r.now = func() time.Time { return now }
	r.Refresh()
	tbl, at := r.Current()
	if _, _, ok := tbl.Lookup("gpt-5.5", ""); !ok || !at.Equal(now) {
		t.Fatalf("table not loaded, at=%v", at)
	}
	if len(kv[kvKey]) == 0 {
		t.Fatal("table not persisted")
	}
	if r.Status().LastError != "" {
		t.Fatalf("status = %+v", r.Status())
	}
}

func TestRefresherFallsBackToStoredCopy(t *testing.T) {
	raw, _ := os.ReadFile("testdata/litellm_sample.json")
	kv := memKV{}
	first := NewRefresher(func(string) ([]byte, error) { return raw, nil }, kv, "u", time.Hour)
	first.Refresh()

	second := NewRefresher(func(string) ([]byte, error) { return nil, errors.New("offline") }, kv, "u", time.Hour)
	second.Refresh()
	tbl, at := second.Current()
	if _, _, ok := tbl.Lookup("gpt-5.5", ""); !ok || at.IsZero() {
		t.Fatal("stored copy not used")
	}
	if second.Status().LastError == "" {
		t.Fatal("fetch error not reported")
	}
}

func TestRefresherWithNothingServesEmptyTable(t *testing.T) {
	r := NewRefresher(func(string) ([]byte, error) { return nil, errors.New("offline") }, memKV{}, "u", time.Hour)
	r.Refresh()
	tbl, at := r.Current()
	if tbl == nil || !at.IsZero() {
		t.Fatalf("tbl=%v at=%v", tbl, at)
	}
	if _, _, ok := tbl.Lookup("gpt-5.5", ""); ok {
		t.Fatal("empty table priced a model")
	}
}

func TestRefresherKeepsTableWhenFetchReturnsGarbage(t *testing.T) {
	raw, _ := os.ReadFile("testdata/litellm_sample.json")
	kv := memKV{}
	calls := 0
	r := NewRefresher(func(string) ([]byte, error) {
		calls++
		if calls == 1 {
			return raw, nil
		}
		return []byte("<html>rate limited</html>"), nil
	}, kv, "u", time.Hour)
	r.Refresh()
	r.Refresh()
	tbl, _ := r.Current()
	if _, _, ok := tbl.Lookup("gpt-5.5", ""); !ok {
		t.Fatal("good table replaced by garbage")
	}
	if r.Status().LastError == "" {
		t.Fatal("parse error not reported")
	}
}
