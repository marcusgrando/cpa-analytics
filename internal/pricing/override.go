package pricing

// PriceOverride sets USD prices per one million tokens for a model that the
// LiteLLM table lacks or prices wrongly.
type PriceOverride struct {
	Input      float64 `yaml:"input"`
	Output     float64 `yaml:"output"`
	CacheRead  float64 `yaml:"cache_read"`
	CacheWrite float64 `yaml:"cache_write"`
}
