// Provider colors. Claude, Codex and Gemini wear their brand colors, stepped
// for each theme in the page's CSS and validated together for color-blind
// separation. Any other provider takes the one spare color that still clears
// that check against all three, in order of first appearance, and the rest
// share the overflow gray, so the legend and tables carry their identity.

const BRAND_COLORS = {
  claude: 'var(--provider-claude)',
  codex: 'var(--provider-codex)',
  gemini: 'var(--provider-gemini)',
};

// providerPalette returns a function that gives each provider a stable color.
function providerPalette() {
  const others = new Map();
  return (provider) => {
    if (BRAND_COLORS[provider]) return BRAND_COLORS[provider];
    if (!others.has(provider)) others.set(provider, others.size === 0 ? 'var(--provider-next)' : 'var(--other)');
    return others.get(provider);
  };
}

if (typeof module !== 'undefined') {
  module.exports = { providerPalette };
}
