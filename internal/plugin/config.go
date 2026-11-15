package plugin

import (
	"fmt"
	"strings"

	"github.com/marcus-grando-azion/cpa-analytics/internal/pricing"
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
	if !strings.HasPrefix(cfg.PricingURL, "https://") && !strings.HasPrefix(cfg.PricingURL, "http://") {
		return Config{}, fmt.Errorf("pricing_url must be an http(s) URL, got %q", cfg.PricingURL)
	}
	if cfg.RetentionDays < 0 {
		return Config{}, fmt.Errorf("retention_days must be zero or positive, got %d", cfg.RetentionDays)
	}
	for model, o := range cfg.PriceOverrides {
		if strings.TrimSpace(model) == "" {
			return Config{}, fmt.Errorf("price_overrides has an empty model name")
		}
		if o.Input < 0 || o.Output < 0 || o.CacheRead < 0 || o.CacheWrite < 0 {
			return Config{}, fmt.Errorf("price_overrides.%s has a negative price", model)
		}
	}
	return cfg, nil
}
