// Provider colors. Claude, Codex and Gemini wear their brand colors, stepped
// for each theme in the page's CSS and validated together for color-blind
// separation. Any other provider takes the one spare color that still clears
// that check against all three, in order of first appearance, and the rest
// share the overflow gray, so the legend and tables carry their identity.

const BRAND_COLORS = {
  claude: 'var(--provider-claude)',
  codex: 'var(--provider-codex)',
  gemini: 'var(--provider-gemini)',
  'gemini-cli': 'var(--provider-gemini)',
  vertex: 'var(--provider-gemini)',
  aistudio: 'var(--provider-gemini)',
  antigravity: 'var(--provider-antigravity)',
  xai: 'var(--provider-xai)',
  kimi: 'var(--provider-kimi)',
  meta: 'var(--provider-meta)',
  devin: 'var(--provider-devin)',
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

// Several accounts of one provider share its color, so each further account,
// in auth index order, takes the next dash. Past the last dash an account
// falls to the overflow gray: a repeated dash would make two accounts look
// like one.
const ACCOUNT_DASHES = ['', '6 4', '2 3', '10 3 2 3'];

// accountStyles maps each account to its line color and dash pattern.
function accountStyles(accounts, colorOf) {
  const styles = new Map();
  const perProvider = new Map();
  const sorted = [...accounts].sort((a, b) => (a.account < b.account ? -1 : a.account > b.account ? 1 : 0));
  for (const { account, provider } of sorted) {
    const n = perProvider.get(provider) || 0;
    perProvider.set(provider, n + 1);
    styles.set(account, n < ACCOUNT_DASHES.length ? { color: colorOf(provider), dash: ACCOUNT_DASHES[n] } : { color: 'var(--other)', dash: '' });
  }
  return styles;
}

if (typeof module !== 'undefined') {
  module.exports = { accountStyles, providerPalette };
}
