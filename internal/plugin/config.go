package plugin

import (
	"fmt"
	"math"
	"net/url"
	"strings"

	"github.com/marcusgrando/cpa-analytics/internal/pricing"
	"gopkg.in/yaml.v3"
)

const (
	defaultDBPath     = "/CLIProxyAPI/data/cpa-analytics.db"
	defaultPricingURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
)

// Config is the plugin-owned subtree under plugins.configs.cpa-analytics.
type Config struct {
	DBPath         string                           `yaml:"db_path"`
	RetentionDays  int                              `yaml:"retention_days"`
	PriceOverrides map[string]pricing.PriceOverride `yaml:"price_overrides"`
	PricingURL     string                           `yaml:"pricing_url"`
	PlanCosts      map[string]float64               `yaml:"plan_costs"`
}

// ParseConfig reads the plugin config YAML sent by the host and applies defaults.
func ParseConfig(raw []byte) (Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse plugin config: %w", err)
	}
	cfg.DBPath = strings.TrimSpace(cfg.DBPath)
	if cfg.DBPath == "" {
		cfg.DBPath = defaultDBPath
	}
	cfg.PricingURL = strings.TrimSpace(cfg.PricingURL)
	if cfg.PricingURL == "" {
		cfg.PricingURL = defaultPricingURL
	}
	parsedURL, err := url.Parse(cfg.PricingURL)
	if err != nil || (parsedURL.Scheme != "https" && parsedURL.Scheme != "http") || parsedURL.Host == "" {
		return Config{}, fmt.Errorf("pricing_url must be an http(s) URL, got %q", cfg.PricingURL)
	}
	if cfg.RetentionDays < 0 {
		return Config{}, fmt.Errorf("retention_days must be zero or positive, got %d", cfg.RetentionDays)
	}
	for model, o := range cfg.PriceOverrides {
		if strings.TrimSpace(model) == "" {
			return Config{}, fmt.Errorf("price_overrides has an empty model name")
		}
		if invalidPrice(o.Input) || invalidPrice(o.Output) || invalidOptionalPrice(o.CacheRead) || invalidOptionalPrice(o.CacheWrite) {
			return Config{}, fmt.Errorf("price_overrides.%s must have non-negative finite prices", model)
		}
	}
	for key, cost := range cfg.PlanCosts {
		if strings.TrimSpace(key) == "" {
			return Config{}, fmt.Errorf("plan_costs has an empty key")
		}
		if invalidPrice(cost) {
			return Config{}, fmt.Errorf("plan_costs.%s must be a non-negative number", key)
		}
	}
	return cfg, nil
}

func invalidPrice(v float64) bool { return math.IsNaN(v) || math.IsInf(v, 0) || v < 0 }

func invalidOptionalPrice(price *float64) bool { return price != nil && invalidPrice(*price) }
