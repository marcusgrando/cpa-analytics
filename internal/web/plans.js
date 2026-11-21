// Plan value and cost-benefit math: default subscription prices, potential
// leverage from weekly quota burn, realized leverage over the selected period,
// and token-per-dollar breakdowns. No DOM and no network, so bun tests load it
// directly. web.go inlines it into the dashboard page.

const PLAN_DAY_MS = 86400000;
const PLAN_MONTH_DAYS = 365.25 / 12;
const PLAN_WEEK_DAYS = 7;
const MIN_MEASURED_WEEKLY_PCT = 1;
const LOW_SAMPLE_WEEKLY_PCT = 3;

// Public list prices in USD per month for known subscription tiers. Users can
// override any plan or account in plugin config (plan_costs) or in the Plans
// tab for the current browser session.
const DEFAULT_PLAN_COSTS = {
  'claude:pro': 20,
  'claude:max 5x': 100,
  'claude:max 20x': 200,
  'codex:plus': 20,
  'codex:pro lite': 100,
  'codex:pro': 200,
  'codex:business': 30,
  'codex:team': 30,
  'antigravity:free': 0,
  'antigravity:pro': 20,
  'antigravity:ultra': 250,
  'gemini:free': 0,
  'devin:core': 20,
  'devin:team': 500,
};

function normProvider(provider) {
  const p = String(provider || '').trim().toLowerCase();
  return p === 'gemini-cli' ? 'gemini' : p;
}

function planKey(provider, plan) {
  const p = normProvider(provider) || 'other';
  const name = String(plan || '').trim();
  return name ? p + ':' + name : p + ':Unknown';
}

function lookupCostMap(map, key) {
  if (!map || typeof map !== 'object') return undefined;
  if (Object.prototype.hasOwnProperty.call(map, key)) return map[key];
  const lower = String(key || '').toLowerCase();
  for (const [k, v] of Object.entries(map)) {
    if (String(k).toLowerCase() === lower) return v;
  }
  return undefined;
}

function validMonthlyCost(v) {
  const n = Number(v);
  return v !== '' && v !== null && v !== undefined && Number.isFinite(n) && n >= 0 ? n : null;
}

// resolvePlanCost returns the monthly subscription price in USD and where it
// came from: session edit, plugin config, built-in default, pay-as-you-go API
// key, or unknown.
function resolvePlanCost(item, configCosts, sessionCosts) {
  const key = planKey(item.provider, item.plan);
  if (item.isApiKey) {
    return { monthlyCost: null, source: 'payg', key };
  }
  const checks = [
    [sessionCosts, item.authIndex, 'session'],
    [sessionCosts, key, 'session'],
    [configCosts, item.authIndex, 'config'],
    [configCosts, key, 'config'],
    [DEFAULT_PLAN_COSTS, key.toLowerCase(), 'default'],
  ];
  for (const [map, k, source] of checks) {
    if (!k) continue;
    const raw = lookupCostMap(map, k);
    const parsed = validMonthlyCost(raw);
    if (parsed !== null) return { monthlyCost: parsed, source, key };
  }
  return { monthlyCost: null, source: 'unknown', key };
}

// effectiveRangeDays scales subscription cost to the part of [from, to] that
// actually has recorded history, so selecting 30d after 3 days of recording
// does not understate realized value by 10x.
function effectiveRangeDays(from, to, dataSince) {
  const clamped = dataSince > 0 && dataSince > from && dataSince < to;
  const start = clamped ? dataSince : from;
  const days = Math.max((to - start) / PLAN_DAY_MS, 1 / 24);
  return { days, clamped };
}

function tokenBuckets(src) {
  const t = src || {};
  const input = Number(t.uncached_input) || 0;
  const cacheRead = Number(t.cache_read) || 0;
  const cacheWrite = Number(t.cache_write) || 0;
  const output = (Number(t.output) || 0) + (Number(t.reasoning) || 0);
  const unclassified = Number(t.unclassified) || 0;
  return { input, cacheRead, cacheWrite, output, total: input + cacheRead + cacheWrite + output + unclassified };
}

function scaleTokens(t, factor) {
  if (!(factor > 0) || !Number.isFinite(factor)) return null;
  return {
    input: t.input * factor,
    cacheRead: t.cacheRead * factor,
    cacheWrite: t.cacheWrite * factor,
    output: t.output * factor,
    total: t.total * factor,
  };
}

// computeAccountPlanRow turns one account's realized totals, weekly quota burn
// and resolved monthly price into potential and realized cost-benefit figures.
function computeAccountPlanRow(item, opts) {
  const days = opts.effectiveDays;
  const { monthlyCost, source, key } = resolvePlanCost(item, opts.configCosts, opts.sessionCosts);
  const realizedTokens = tokenBuckets(item);
  const measuredTokens = tokenBuckets(item.measured_tokens);
  const costUSD = Number(item.cost_usd) || 0;
  const weeklyPct = Number(item.weekly_pct) || 0;
  const measuredCostUSD = Number(item.measured_cost_usd) || 0;

  const periodCost = monthlyCost !== null ? monthlyCost * (days / PLAN_MONTH_DAYS) : null;
  const weeklyCost = monthlyCost !== null ? monthlyCost * (PLAN_WEEK_DAYS / PLAN_MONTH_DAYS) : null;

  const realizedLeverage = source === 'payg' ? (costUSD > 0 ? 1 : null)
    : (periodCost !== null && periodCost > 0 ? costUSD / periodCost : null);
  const realizedTokensPerDollar = source === 'payg' && costUSD > 0 ? scaleTokens(realizedTokens, 1 / costUSD)
    : (periodCost !== null && periodCost > 0 ? scaleTokens(realizedTokens, 1 / periodCost) : null);

  let potential = null;
  if (weeklyPct >= MIN_MEASURED_WEEKLY_PCT && measuredCostUSD > 0) {
    const usdPerPct = measuredCostUSD / weeklyPct;
    const fullCycleUSD = usdPerPct * 100;
    const tokensPerPct = scaleTokens(measuredTokens, 1 / weeklyPct);
    const leverage = weeklyCost !== null && weeklyCost > 0 ? fullCycleUSD / weeklyCost : null;
    const tokensPerDollar = weeklyCost !== null && weeklyCost > 0 ? scaleTokens(measuredTokens, (100 / weeklyPct) / weeklyCost) : null;
    potential = {
      weeklyPct,
      usdPerPct,
      fullCycleUSD,
      leverage,
      effectiveRate: leverage > 0 ? 1 / leverage : null,
      tokensPerPct,
      tokensPerDollar,
      lowSample: weeklyPct < LOW_SAMPLE_WEEKLY_PCT,
    };
  }

  const utilizationPct = potential && potential.leverage > 0 && realizedLeverage !== null
    ? (realizedLeverage / potential.leverage) * 100
    : null;

  return {
    id: item.authIndex,
    authIndex: item.authIndex,
    name: item.name || item.authIndex,
    provider: normProvider(item.provider) || 'other',
    plan: item.isApiKey ? 'API key' : (item.plan || 'Unknown'),
    planKey: key,
    isApiKey: Boolean(item.isApiKey),
    accountCount: 1,
    monthlyCost,
    costSource: source,
    periodCost,
    weeklyCost,
    requests: Number(item.requests) || 0,
    unpricedTokens: Number(item.unpriced_tokens) || 0,
    cacheHitRate: Number(item.cache_hit_rate) || 0,
    topModel: item.top_model || '',
    topModelShare: Number(item.top_model_share) || 0,
    realized: {
      costUSD,
      leverage: realizedLeverage,
      effectiveRate: realizedLeverage > 0 ? 1 / realizedLeverage : null,
      tokens: realizedTokens,
      tokensPerDollar: realizedTokensPerDollar,
    },
    potential,
    utilizationPct,
    measuredRaw: { weeklyPct, costUSD: measuredCostUSD, tokens: measuredTokens },
  };
}

// groupPlanRows rolls accounts of the same (provider, plan) into one row so
// multiple accounts on the same tier produce one sample-weighted plan score.
function groupPlanRows(accountRows, opts) {
  const days = opts.effectiveDays;
  const groups = new Map();
  for (const row of accountRows) {
    const gk = (row.isApiKey ? 'payg:' : 'sub:') + row.provider + ':' + row.plan.toLowerCase();
    if (!groups.has(gk)) groups.set(gk, []);
    groups.get(gk).push(row);
  }
  const out = [];
  for (const members of groups.values()) {
    const first = members[0];
    const isApiKey = first.isApiKey;
    const allKnown = !isApiKey && members.every((m) => m.monthlyCost !== null);
    const totalMonthlyCost = allKnown ? members.reduce((s, m) => s + m.monthlyCost, 0) : null;
    const avgMonthlyCost = allKnown ? totalMonthlyCost / members.length : null;
    const periodCost = totalMonthlyCost !== null ? totalMonthlyCost * (days / PLAN_MONTH_DAYS) : null;
    const avgWeeklyCost = avgMonthlyCost !== null ? avgMonthlyCost * (PLAN_WEEK_DAYS / PLAN_MONTH_DAYS) : null;

    const costUSD = members.reduce((s, m) => s + m.realized.costUSD, 0);
    const requests = members.reduce((s, m) => s + m.requests, 0);
    const unpricedTokens = members.reduce((s, m) => s + m.unpricedTokens, 0);
    const tokens = {
      input: members.reduce((s, m) => s + m.realized.tokens.input, 0),
      cacheRead: members.reduce((s, m) => s + m.realized.tokens.cacheRead, 0),
      cacheWrite: members.reduce((s, m) => s + m.realized.tokens.cacheWrite, 0),
      output: members.reduce((s, m) => s + m.realized.tokens.output, 0),
      total: members.reduce((s, m) => s + m.realized.tokens.total, 0),
    };
    const totalInput = tokens.input + tokens.cacheRead + tokens.cacheWrite;
    const cacheHitRate = totalInput > 0 ? tokens.cacheRead / totalInput : 0;

    const realizedLeverage = isApiKey ? (costUSD > 0 ? 1 : null)
      : (periodCost !== null && periodCost > 0 ? costUSD / periodCost : null);
    const realizedTokensPerDollar = isApiKey && costUSD > 0 ? scaleTokens(tokens, 1 / costUSD)
      : (periodCost !== null && periodCost > 0 ? scaleTokens(tokens, 1 / periodCost) : null);

    // Pool measured weekly spans across accounts of the same plan so a
    // low-traffic account does not skew the plan average.
    const measuredPct = members.reduce((s, m) => s + m.measuredRaw.weeklyPct, 0);
    const measuredCost = members.reduce((s, m) => s + m.measuredRaw.costUSD, 0);
    const measuredTokens = {
      input: members.reduce((s, m) => s + m.measuredRaw.tokens.input, 0),
      cacheRead: members.reduce((s, m) => s + m.measuredRaw.tokens.cacheRead, 0),
      cacheWrite: members.reduce((s, m) => s + m.measuredRaw.tokens.cacheWrite, 0),
      output: members.reduce((s, m) => s + m.measuredRaw.tokens.output, 0),
      total: members.reduce((s, m) => s + m.measuredRaw.tokens.total, 0),
    };
    let potential = null;
    if (measuredPct >= MIN_MEASURED_WEEKLY_PCT && measuredCost > 0) {
      const usdPerPct = measuredCost / measuredPct;
      const fullCycleUSD = usdPerPct * 100;
      const tokensPerPct = scaleTokens(measuredTokens, 1 / measuredPct);
      const leverage = avgWeeklyCost !== null && avgWeeklyCost > 0 ? fullCycleUSD / avgWeeklyCost : null;
      const tokensPerDollar = avgWeeklyCost !== null && avgWeeklyCost > 0 ? scaleTokens(measuredTokens, (100 / measuredPct) / avgWeeklyCost) : null;
      potential = {
        weeklyPct: measuredPct,
        usdPerPct,
        fullCycleUSD,
        leverage,
        effectiveRate: leverage > 0 ? 1 / leverage : null,
        tokensPerPct,
        tokensPerDollar,
        lowSample: measuredPct < LOW_SAMPLE_WEEKLY_PCT,
      };
    }

    const utilizationPct = potential && potential.leverage > 0 && realizedLeverage !== null
      ? (realizedLeverage / potential.leverage) * 100
      : null;

    // Pick the top model from the busiest account in the group.
    const busiest = members.reduce((best, m) => ((m.realized.costUSD || m.realized.tokens.total) > (best.realized.costUSD || best.realized.tokens.total) ? m : best), members[0]);
    const sources = new Set(members.map((m) => m.costSource));
    const costSource = sources.size === 1 ? first.costSource : (sources.has('session') ? 'session' : sources.has('config') ? 'config' : 'mixed');

    out.push({
      id: first.planKey,
      provider: first.provider,
      plan: first.plan,
      planKey: first.planKey,
      isApiKey,
      accountCount: members.length,
      accounts: members,
      monthlyCost: avgMonthlyCost,
      totalMonthlyCost,
      costSource,
      periodCost,
      weeklyCost: avgWeeklyCost,
      requests,
      unpricedTokens,
      cacheHitRate,
      topModel: busiest.topModel,
      topModelShare: busiest.topModelShare,
      realized: {
        costUSD,
        leverage: realizedLeverage,
        effectiveRate: realizedLeverage > 0 ? 1 / realizedLeverage : null,
        tokens,
        tokensPerDollar: realizedTokensPerDollar,
      },
      potential,
      utilizationPct,
    });
  }
  return out;
}

function metricScore(row, sortBy) {
  if (sortBy === 'realized') return row.realized.leverage;
  if (sortBy === 'output' || sortBy === 'input' || sortBy === 'cacheRead' || sortBy === 'cacheWrite') {
    const p = row.potential && row.potential.tokensPerDollar ? row.potential.tokensPerDollar[sortBy] : null;
    if (p !== null && p > 0) return p;
    return row.realized.tokensPerDollar ? row.realized.tokensPerDollar[sortBy] : null;
  }
  // Default: potential leverage when quota headers exist, else realized leverage.
  if (row.potential && row.potential.leverage !== null) return row.potential.leverage;
  return row.realized.leverage;
}

// rankPlanRows orders paid subscription rows by the chosen cost-benefit metric
// (highest first), followed by free tiers (by API value extracted), API keys
// (1.0x baseline) and plans with no price configured yet.
function rankPlanRows(rows, sortBy = 'potential') {
  const bucket = (r) => {
    if (r.isApiKey) return 2;
    if (r.monthlyCost === 0) return 1;
    if (r.monthlyCost === null) return 3;
    const s = metricScore(r, sortBy);
    return s !== null && s > 0 ? 0 : 4;
  };
  return [...rows].sort((a, b) => {
    const ba = bucket(a), bb = bucket(b);
    if (ba !== bb) return ba - bb;
    if (ba === 0) {
      const sa = metricScore(a, sortBy) || 0;
      const sb = metricScore(b, sortBy) || 0;
      if (sb !== sa) return sb - sa;
    }
    if (b.realized.costUSD !== a.realized.costUSD) return b.realized.costUSD - a.realized.costUSD;
    return (a.planKey || a.id).localeCompare(b.planKey || b.id);
  });
}

function planCostsYAML(overrides) {
  const entries = Object.entries(overrides || {})
    .filter(([, v]) => validMonthlyCost(v) !== null)
    .sort((a, b) => a[0].localeCompare(b[0]));
  if (!entries.length) return '';
  return 'plan_costs:\n' + entries.map(([k, v]) => `  ${JSON.stringify(k)}: ${Number(v)}`).join('\n');
}

if (typeof module !== 'undefined') {
  module.exports = {
    PLAN_MONTH_DAYS, PLAN_WEEK_DAYS, DEFAULT_PLAN_COSTS,
    normProvider, planKey, resolvePlanCost, effectiveRangeDays,
    computeAccountPlanRow, groupPlanRows, rankPlanRows, planCostsYAML,
  };
}
