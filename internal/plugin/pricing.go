package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/marcusgrando/cpa-analytics/internal/pricing"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const pricingRefreshEvery = 24 * time.Hour

// fetchViaHost downloads a URL through host.http.do so CPA's proxy settings apply.
func (p *Plugin) fetchViaHost(url string) ([]byte, error) {
	if p.host == nil {
		return nil, errors.New("host callbacks unavailable")
	}
	req, err := json.Marshal(pluginapi.HTTPRequest{Method: "GET", URL: url})
	if err != nil {
		return nil, err
	}
	raw, err := p.host.Call(pluginabi.MethodHostHTTPDo, req)
	if err != nil {
		return nil, err
	}
	var resp pluginapi.HTTPResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode host.http.do response: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s returned HTTP %d", url, resp.StatusCode)
	}
	return resp.Body, nil
}

// prices returns the current table with the configured overrides applied.
func (p *Plugin) prices() *pricing.Table {
	p.mu.Lock()
	r, overrides := p.refresher, p.cfg.PriceOverrides
	p.mu.Unlock()
	if r == nil {
		return pricing.WithOverrides(&pricing.Table{}, overrides)
	}
	tbl, _ := r.Current()
	return pricing.WithOverrides(tbl, overrides)
}

// startPricing (re)starts the background refresher for url.
func (p *Plugin) startPricing(url string, kv pricing.KV) {
	p.mu.Lock()
	old := p.refresher
	p.refresher = nil
	p.mu.Unlock()
	old.Stop()
	r := pricing.NewRefresher(p.fetchViaHost, kv, url, pricingRefreshEvery)
	p.mu.Lock()
	p.refresher = r
	p.mu.Unlock()
	r.Start()
}

// pricingStatus reports the refresher state, or just the URL before it starts.
func (p *Plugin) pricingStatus() pricing.Status {
	p.mu.Lock()
	r, url := p.refresher, p.cfg.PricingURL
	p.mu.Unlock()
	if r == nil {
		return pricing.Status{URL: url}
	}
	return r.Status()
}
