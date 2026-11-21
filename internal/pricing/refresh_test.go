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
	if _, _, ok := tbl.Lookup("gpt-5.5", "", 0); !ok || !at.Equal(now) {
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
	if _, _, ok := tbl.Lookup("gpt-5.5", "", 0); !ok || at.IsZero() {
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
	if _, _, ok := tbl.Lookup("gpt-5.5", "", 0); ok {
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
	if _, _, ok := tbl.Lookup("gpt-5.5", "", 0); !ok {
		t.Fatal("good table replaced by garbage")
	}
	if r.Status().LastError == "" {
		t.Fatal("parse error not reported")
	}
}

func TestRefresherMergesFallbacksAndToleratesFallbackErrors(t *testing.T) {
	litellmRaw, _ := os.ReadFile("testdata/litellm_sample.json")
	modelsDevRaw := []byte(`{"google":{"models":{"gemini-4-neon":{"cost":{"input":3,"output":15,"cache_read":0.15}}}}}`)
	kv := memKV{}
	r := NewRefresher(func(u string) ([]byte, error) {
		switch u {
		case "https://primary":
			return litellmRaw, nil
		case "https://modelsdev":
			return modelsDevRaw, nil
		default:
			return nil, errors.New("fallback down")
		}
	}, kv, "https://primary", time.Hour).WithFallbacks(
		FallbackSource{Name: "modelsdev", URL: "https://modelsdev", Parse: ParseModelsDev},
		FallbackSource{Name: "openrouter", URL: "https://openrouter", Parse: ParseOpenRouter},
	)
	r.Refresh()
	if r.Status().LastError != "" {
		t.Fatalf("primary succeeded but got error: %s", r.Status().LastError)
	}
	tbl, _ := r.Current()
	if _, _, ok := tbl.Lookup("gpt-5.5", "", 0); !ok {
		t.Fatal("missing primary model")
	}
	if _, _, ok := tbl.Lookup("gemini-4-neon-xhigh", "", 0); !ok {
		t.Fatal("missing fallback model")
	}
}
