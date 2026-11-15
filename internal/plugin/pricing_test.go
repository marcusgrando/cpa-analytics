package plugin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type fakeHost struct {
	calls  []string
	status int
	body   []byte
}

func (h *fakeHost) Call(method string, req []byte) ([]byte, error) {
	h.calls = append(h.calls, method)
	if method != pluginabi.MethodHostHTTPDo {
		return []byte("{}"), nil
	}
	var r pluginapi.HTTPRequest
	if err := json.Unmarshal(req, &r); err != nil {
		return nil, err
	}
	if r.Method != "GET" || !strings.HasPrefix(r.URL, "https://") {
		return nil, nil
	}
	return json.Marshal(pluginapi.HTTPResponse{StatusCode: h.status, Body: h.body})
}

func TestFetchViaHostReturnsBodyOn200(t *testing.T) {
	h := &fakeHost{status: 200, body: []byte(`{"gpt-5.5":{}}`)}
	p := New(h)
	body, err := p.fetchViaHost("https://example.invalid/prices.json")
	if err != nil || string(body) != `{"gpt-5.5":{}}` {
		t.Fatalf("body=%q err=%v", body, err)
	}
}

func TestFetchViaHostRejectsNon200(t *testing.T) {
	p := New(&fakeHost{status: 429, body: []byte("slow down")})
	if _, err := p.fetchViaHost("https://example.invalid/prices.json"); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchWithoutHostFails(t *testing.T) {
	if _, err := New(nil).fetchViaHost("https://example.invalid/p.json"); err == nil {
		t.Fatal("expected error")
	}
}
