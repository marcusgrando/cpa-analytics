package plugin

import (
	"fmt"
	"net/http"
	"time"

	"github.com/marcusgrando/cpa-analytics/internal/pricing"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	pricingRefreshEvery = 24 * time.Hour
	modelsDevPricingURL = "https://models.dev/api.json"
	openRouterModelsURL = "https://openrouter.ai/api/v1/models"
)

// fetchViaHost downloads a URL through host.http.do so CPA's proxy settings apply.
func (p *Plugin) fetchViaHost(url string) ([]byte, error) {
	return p.fetchViaHostStop(url, nil)
}

func (p *Plugin) fetchViaHostStop(url string, stop <-chan struct{}) ([]byte, error) {
	resp, err := doHostHTTP(p.host, pluginapi.HTTPRequest{Method: http.MethodGet, URL: url}, httpTimeout, stop)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
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

// startPricing (re)starts the background refresher for url. Callers hold p.lifecycleMu.
func (p *Plugin) startPricing(url string, kv pricing.KV) {
	p.mu.Lock()
	old, oldStop := p.refresher, p.pricingStop
	p.refresher, p.pricingStop = nil, nil
	p.mu.Unlock()
	if oldStop != nil {
		close(oldStop)
	}
	old.Stop()
	stop := make(chan struct{})
	r := pricing.NewRefresher(func(u string) ([]byte, error) {
		return p.fetchViaHostStop(u, stop)
	}, kv, url, pricingRefreshEvery).WithFallbacks(
		pricing.FallbackSource{Name: "modelsdev", URL: modelsDevPricingURL, Parse: pricing.ParseModelsDev},
		pricing.FallbackSource{Name: "openrouter", URL: openRouterModelsURL, Parse: pricing.ParseOpenRouter},
	)
	r.Start()
	p.mu.Lock()
	p.refresher = r
	p.pricingStop = stop
	p.mu.Unlock()
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
