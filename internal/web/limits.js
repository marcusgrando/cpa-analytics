// Pooled subscription limits: parsing of the providers' usage responses and
// the pool and pace math. No DOM and no network, so bun tests load it as is.
// web.go inlines it into the dashboard page.

const LIMIT_HOUR = 3600000;
const LIMIT_DAY = 24 * LIMIT_HOUR;
const PACE_TOLERANCE = 5;

// Weights follow the plans' published multiples of the base plan.
const CODEX_WEIGHTS = { plus: 1, prolite: 5, pro: 20 };

function claudeWeight(profile) {
  const tier = String((profile && profile.organization && profile.organization.rate_limit_tier) || '');
  if (tier.includes('max_20x')) return 20;
  if (tier.includes('max_5x')) return 5;
  if (profile && profile.account && profile.account.has_claude_pro) return 1;
  return null;
}

function codexWeight(planType) {
  return Object.prototype.hasOwnProperty.call(CODEX_WEIGHTS, planType) ? CODEX_WEIGHTS[planType] : null;
}

function parseTime(iso) {
  const t = iso ? Date.parse(iso) : NaN;
  return Number.isFinite(t) ? t : null;
}

function limitWindow(id, label, used, resetsAt, lengthMs) {
  return { id, label, used: Number(used) || 0, resetsAt, lengthMs };
}

function parseClaudeUsage(body) {
  const windows = [];
  if (body && Array.isArray(body.limits)) {
    for (const l of body.limits) {
      if (!l) continue;
      const resetsAt = parseTime(l.resets_at);
      if (l.kind === 'session') windows.push(limitWindow('session', '5-hour', l.percent, resetsAt, 5 * LIMIT_HOUR));
      else if (l.kind === 'weekly_all') windows.push(limitWindow('weekly', 'Weekly', l.percent, resetsAt, 7 * LIMIT_DAY));
      else if (l.kind === 'weekly_scoped' && l.scope && l.scope.model && l.scope.model.display_name) {
        const model = l.scope.model.display_name;
        windows.push(limitWindow('weekly:' + model, 'Weekly, ' + model, l.percent, resetsAt, 7 * LIMIT_DAY));
      }
    }
  }
  if (!windows.length && body) {
    if (body.five_hour) windows.push(limitWindow('session', '5-hour', body.five_hour.utilization, parseTime(body.five_hour.resets_at), 5 * LIMIT_HOUR));
    if (body.seven_day) windows.push(limitWindow('weekly', 'Weekly', body.seven_day.utilization, parseTime(body.seven_day.resets_at), 7 * LIMIT_DAY));
  }
  if (!windows.length) throw new Error('The usage response has no usage windows.');
  // No session in progress means the whole 5-hour allowance is free.
  if (!windows.some((w) => w.id === 'session')) windows.unshift(limitWindow('session', '5-hour', 0, null, 5 * LIMIT_HOUR));
  return windows;
}

// parseClaudeSpend returns the extra usage bought past the plan limits, in
// major currency units, or null when extra usage is off. `spend` is the newer
// field; `extra_usage` carries the same amounts in older responses.
function parseClaudeSpend(body) {
  const spend = body && body.spend;
  if (spend) {
    if (!spend.enabled || !spend.used) return null;
    const major = (m) => Number(m.amount_minor) / 10 ** Number(m.exponent);
    return { used: major(spend.used), limit: spend.limit ? major(spend.limit) : null, currency: spend.used.currency };
  }
  const extra = body && body.extra_usage;
  if (!extra || !extra.is_enabled) return null;
  const major = (v) => Number(v) / 10 ** Number(extra.decimal_places);
  return { used: major(extra.used_credits), limit: extra.monthly_limit == null ? null : major(extra.monthly_limit), currency: extra.currency };
}

function codexWindow(raw) {
  const lengthMs = Number(raw.limit_window_seconds) * 1000;
  const resetsAt = Number(raw.reset_at) > 0 ? Number(raw.reset_at) * 1000 : null;
  if (lengthMs >= 30 * LIMIT_DAY) return limitWindow('monthly', 'Monthly', raw.used_percent, resetsAt, lengthMs);
  if (lengthMs >= 7 * LIMIT_DAY) return limitWindow('weekly', 'Weekly', raw.used_percent, resetsAt, lengthMs);
  return limitWindow('session', Math.round(lengthMs / LIMIT_HOUR) + '-hour', raw.used_percent, resetsAt, lengthMs);
}

function parseCodexUsage(body) {
  const rl = body && body.rate_limit;
  const windows = rl ? [rl.primary_window, rl.secondary_window].filter((w) => w && Number(w.limit_window_seconds) > 0).map(codexWindow) : [];
  if (!windows.length) throw new Error('The usage response has no usage windows.');
  return { plan: body.plan_type || '', windows };
}

const ANTIGRAVITY_TIERS = {
  'g1-ultra-tier': { plan: 'Ultra', weight: 20 },
  'g1-pro-tier': { plan: 'Pro', weight: 5 },
  'free-tier': { plan: 'Free', weight: 1 },
};

// parseAntigravityPlan reads the tier and Google One AI credit balance from
// Antigravity's loadCodeAssist response.
function parseAntigravityPlan(body) {
  const paidId = String((body && body.paidTier && body.paidTier.id) || '').toLowerCase();
  const curId = String((body && body.currentTier && body.currentTier.id) || '').toLowerCase();
  const tierId = paidId || curId;
  const matched = ANTIGRAVITY_TIERS[tierId] || { plan: (body && body.paidTier && body.paidTier.name) || tierId, weight: null };
  let credits = null;
  const list = body && body.paidTier && Array.isArray(body.paidTier.availableCredits) ? body.paidTier.availableCredits : [];
  for (const c of list) {
    if (!c || String(c.creditType || '').toUpperCase() !== 'GOOGLE_ONE_AI') continue;
    const amount = Number(c.creditAmount);
    if (Number.isFinite(amount)) credits = { amount, min: Number(c.minimumCreditAmountForUsage) || 0 };
  }
  const project = String((body && body.cloudaicompanionProject) || '');
  return { ...matched, credits, project };
}

function clampPct(v) {
  return Math.min(100, Math.max(0, Number(v) || 0));
}

function antigravityModelFamily(id) {
  const lower = String(id || '').toLowerCase();
  if (lower.startsWith('tab_')) return null;
  if (lower.includes('claude') || lower.includes('gpt-oss')) return { id: 'claude-oss', label: 'Claude & OSS' };
  if (lower.includes('gemini')) {
    return lower.includes('flash') ? { id: 'gemini-flash', label: 'Gemini Flash' } : { id: 'gemini-pro', label: 'Gemini Pro' };
  }
  return { id: 'other', label: 'Other models' };
}

// parseAntigravityUsage groups per-model quotaInfo from fetchAvailableModels
// into clean quota pools. When Gemini Pro and Gemini Flash share the same
// remaining fraction and reset time, they collapse into one Gemini pool.
function parseAntigravityUsage(body) {
  const models = body && body.models && typeof body.models === 'object' ? Object.entries(body.models) : [];
  const byFamily = new Map();
  for (const [id, info] of models) {
    const q = info && info.quotaInfo;
    if (!q || q.remainingFraction === undefined || q.remainingFraction === null) continue;
    const rem = Number(q.remainingFraction);
    if (!Number.isFinite(rem)) continue;
    const fam = antigravityModelFamily(id);
    if (!fam) continue;
    const used = clampPct((1 - rem) * 100);
    const resetsAt = parseTime(q.resetTime);
    const cur = byFamily.get(fam.id);
    if (!cur || used > cur.used || (used === cur.used && resetsAt && (!cur.resetsAt || resetsAt < cur.resetsAt))) {
      byFamily.set(fam.id, limitWindow(fam.id, fam.label, used, resetsAt, 5 * LIMIT_HOUR));
    }
  }
  const pro = byFamily.get('gemini-pro');
  const flash = byFamily.get('gemini-flash');
  if (pro && flash && Math.abs(pro.used - flash.used) < 1 && Math.abs((pro.resetsAt || 0) - (flash.resetsAt || 0)) <= 5 * 60000) {
    byFamily.delete('gemini-pro');
    byFamily.delete('gemini-flash');
    byFamily.set('gemini', limitWindow('gemini', 'Gemini', Math.max(pro.used, flash.used), pro.resetsAt || flash.resetsAt, 5 * LIMIT_HOUR));
  }
  const order = ['gemini', 'gemini-pro', 'gemini-flash', 'claude-oss', 'other'];
  const windows = order.filter((k) => byFamily.has(k)).map((k) => byFamily.get(k));
  if (!windows.length) throw new Error('The usage response has no usage windows.');
  return windows;
}

// parseGeminiQuota parses buckets[] from Gemini CLI's retrieveUserQuota response.
function parseGeminiQuota(body) {
  const buckets = body && Array.isArray(body.buckets) ? body.buckets : [];
  const byFamily = new Map();
  for (const b of buckets) {
    if (!b || b.remainingFraction === undefined || b.remainingFraction === null) continue;
    const rem = Number(b.remainingFraction);
    if (!Number.isFinite(rem)) continue;
    const id = String(b.modelId || '');
    const lower = id.toLowerCase();
    const fam = lower.includes('flash') ? { id: 'gemini-flash', label: 'Gemini Flash' }
      : lower.includes('pro') ? { id: 'gemini-pro', label: 'Gemini Pro' }
      : { id: 'gemini', label: id || 'Gemini' };
    const used = clampPct((1 - rem) * 100);
    const resetsAt = parseTime(b.resetTime);
    const cur = byFamily.get(fam.id);
    if (!cur || used > cur.used) byFamily.set(fam.id, limitWindow(fam.id, fam.label, used, resetsAt, LIMIT_DAY));
  }
  const windows = [...byFamily.values()];
  if (!windows.length) throw new Error('The usage response has no usage windows.');
  return windows;
}

function parsePctSignal(raw) {
  const n = parseFloat(String(raw || '').replace('%', '').trim());
  return Number.isFinite(n) ? n : null;
}

// parseDevinSignals reads Devin's daily and weekly quota directly from CPA's
// credential quota.signals, needing no extra upstream call.
function parseDevinSignals(account) {
  const signals = (account && account.quota && account.quota.signals) || {};
  const windows = [];
  const dailyRem = parsePctSignal(signals.daily_quota_remaining_percent);
  if (dailyRem !== null) windows.push(limitWindow('daily', 'Daily', clampPct(100 - dailyRem), parseTime(signals.daily_quota_reset_at), LIMIT_DAY));
  const weeklyRem = parsePctSignal(signals.weekly_quota_remaining_percent);
  if (weeklyRem !== null) windows.push(limitWindow('weekly', 'Weekly', clampPct(100 - weeklyRem), parseTime(signals.weekly_quota_reset_at), 7 * LIMIT_DAY));
  return { plan: signals.plan || '', windows };
}

function kimiWindowLengthMs(w) {
  const d = Number(w && w.duration);
  if (!(d > 0)) return 0;
  const unit = String((w && w.timeUnit) || '').toUpperCase();
  if (unit.includes('MINUTE')) return d * 60000;
  if (unit.includes('HOUR')) return d * LIMIT_HOUR;
  if (unit.includes('DAY')) return d * LIMIT_DAY;
  if (unit.includes('WEEK')) return d * 7 * LIMIT_DAY;
  return d * 1000;
}

function kimiDetailPct(detail) {
  if (!detail) return null;
  const limit = Number(detail.limit);
  if (!(limit > 0)) return null;
  if (detail.used !== undefined && detail.used !== null) {
    const u = Number(detail.used);
    if (Number.isFinite(u)) return clampPct((u / limit) * 100);
  }
  if (detail.remaining !== undefined && detail.remaining !== null) {
    const r = Number(detail.remaining);
    if (Number.isFinite(r)) return clampPct(((limit - r) / limit) * 100);
  }
  return null;
}

// parseKimiUsage parses Kimi Coding's /coding/v1/usages response.
function parseKimiUsage(body) {
  const windows = [];
  if (body && body.usage) {
    const pct = kimiDetailPct(body.usage);
    if (pct !== null) windows.push(limitWindow('weekly', 'Weekly', pct, parseTime(body.usage.resetTime), 7 * LIMIT_DAY));
  }
  if (body && Array.isArray(body.limits)) {
    for (const item of body.limits) {
      const pct = kimiDetailPct(item && item.detail);
      if (pct === null) continue;
      const lengthMs = kimiWindowLengthMs(item.window);
      const resetsAt = parseTime(item.detail.resetTime);
      const id = lengthMs >= 7 * LIMIT_DAY ? 'weekly' : lengthMs > 0 ? 'session' : 'limit';
      const label = lengthMs >= 7 * LIMIT_DAY ? 'Weekly' : lengthMs >= LIMIT_HOUR ? Math.round(lengthMs / LIMIT_HOUR) + '-hour' : 'Window';
      if (!windows.some((w) => w.id === id)) windows.push(limitWindow(id, label, pct, resetsAt, lengthMs || 5 * LIMIT_HOUR));
    }
  }
  if (!windows.length) throw new Error('The usage response has no usage windows.');
  return windows;
}

// summarizeCredentialStatus extracts CPA-side health, active cooldowns and
// recent traffic counts for any credential regardless of provider.
function summarizeCredentialStatus(account, now) {
  const cooldowns = (Array.isArray(account && account.cooldowns) ? account.cooldowns : [])
    .map((c) => ({
      scope: c.scope || 'credential',
      model: c.model_key || '',
      reason: c.reason || '',
      retryAt: parseTime(c.retry_at),
    }))
    .filter((c) => !c.retryAt || c.retryAt > now)
    .sort((a, b) => (a.retryAt || Infinity) - (b.retryAt || Infinity));
  const recent = Array.isArray(account && account.recent_requests) ? account.recent_requests : [];
  const recentSuccess = recent.reduce((s, r) => s + (Number(r.success) || 0), 0);
  const recentFailed = recent.reduce((s, r) => s + (Number(r.failed) || 0), 0);
  const state = account && account.requires_sign_in ? 'sign_in'
    : account && account.quota_blocked ? 'quota_blocked'
    : account && account.status === 'error' ? 'error'
    : cooldowns.length > 0 ? 'cooldown'
    : 'active';
  return {
    state,
    status: (account && account.status) || '',
    message: (account && account.status_message) || '',
    cooldowns,
    recentSuccess,
    recentFailed,
  };
}

// accountWindows adds what each window gives back and when. With the weekly
// allowance spent, the session window cannot be used until the week resets.
function accountWindows(windows, now) {
  const week = windows.find((w) => w.id === 'weekly');
  const weekSpent = week && week.used >= 100;
  return windows.map((w) => {
    const future = (t) => t !== null && t > now;
    if (w.id !== 'session' || !weekSpent) {
      return { ...w, blocked: false, restore: future(w.resetsAt) && w.used > 0 ? { at: w.resetsAt, refill: w.used } : null };
    }
    let restore = null;
    if (future(week.resetsAt)) {
      if (w.resetsAt === null || w.resetsAt <= week.resetsAt) restore = { at: week.resetsAt, refill: 100 };
      else if (w.used >= 100) restore = { at: w.resetsAt, refill: 100 };
      else restore = { at: week.resetsAt, refill: 100 - w.used };
    }
    return { ...w, used: 100, blocked: true, restore };
  });
}

// poolWindow combines one window across accounts. Members carry account,
// weight (null when the plan is unknown), used, resetsAt, lengthMs and restore.
function poolWindow(members, now) {
  const weighted = members.every((m) => m.weight > 0);
  const weightOf = (m) => (weighted ? m.weight : 1);
  const total = members.reduce((s, m) => s + weightOf(m), 0);
  const elapsed = (m) => (m.resetsAt === null ? 0 : Math.min(1, Math.max(0, (m.lengthMs - (m.resetsAt - now)) / m.lengthMs)));
  const used = members.reduce((s, m) => s + weightOf(m) * m.used, 0) / total;
  // Pace compares usage with time, and a blocked window is not being used.
  const active = members.filter((m) => !m.blocked);
  const activeTotal = active.reduce((s, m) => s + weightOf(m), 0);
  const activeUsed = active.reduce((s, m) => s + weightOf(m) * m.used, 0) / activeTotal;
  const expected = active.length ? (100 * active.reduce((s, m) => s + weightOf(m) * elapsed(m), 0)) / activeTotal : 0;
  const gap = activeUsed - expected;
  return {
    weighted,
    used,
    expected,
    pace: !active.length ? null : gap > PACE_TOLERANCE ? 'ahead' : gap < -PACE_TOLERANCE ? 'under' : 'on',
    // capacity is the account's share of the pool, elapsed how far its window has run, both in percent.
    accounts: members.map((m) => ({ account: m.account, capacity: (100 * weightOf(m)) / total, elapsed: 100 * elapsed(m) })),
    restores: members.filter((m) => m.restore)
      .map((m) => ({ account: m.account, at: m.restore.at, percent: Math.round((m.restore.refill * weightOf(m)) / total) }))
      .sort((a, b) => a.at - b.at),
  };
}

// pools groups accounts ({account, weight, windows}) by window id.
function pools(accounts, now) {
  const byId = new Map();
  for (const a of accounts) {
    for (const w of accountWindows(a.windows, now)) {
      if (!byId.has(w.id)) byId.set(w.id, { id: w.id, label: w.label, members: [] });
      byId.get(w.id).members.push({ account: a.account, weight: a.weight, ...w });
    }
  }
  return [...byId.values()].map((p) => ({ ...p, ...poolWindow(p.members, now) }));
}

// accountProblem explains an upstream status from CPA's api-call. CPA renews
// Claude and Codex tokens when it proxies a request, not on api-call.
function accountProblem(status, requiresSignIn) {
  if (status === 401 || status === 403) {
    return requiresSignIn ? 'Sign in again from the management center.' : 'Token expired. CPA renews it on the next proxied request.';
  }
  if (status === 429) return 'The provider rate limited the usage check. Try again later.';
  if (status < 200 || status > 299) return 'The provider answered HTTP ' + status + '.';
  return null;
}

// fmtDuration rounds first, so 59.6 minutes reads "1 h 0 min", not "60 min".
function fmtDuration(ms) {
  const minutes = Math.max(1, Math.round(ms / 60000));
  if (minutes < 60) return minutes + ' min';
  if (minutes < 24 * 60) return Math.floor(minutes / 60) + ' h ' + (minutes % 60) + ' min';
  const hours = Math.round(ms / LIMIT_HOUR);
  return Math.floor(hours / 24) + ' d ' + (hours % 24) + ' h';
}

// History and projection of the weekly pool. Readings come from the plugin's
// limit-history route: {t, weekly_pct, weekly_reset}, oldest first, with
// weekly_reset 0 when unknown.

// RESET_JITTER matches the plugin's tolerance for resets a provider
// recomputes per response.
const RESET_JITTER = LIMIT_HOUR;

function lastReading(points, t) {
  let last = null;
  for (const p of points) {
    if (p.t > t) break;
    last = p;
  }
  return last;
}

// readingAt is an account's weekly percent at t: its last reading, or 0 once
// that reading's reset has passed, or null before its first reading.
function readingAt(points, t) {
  const p = lastReading(points, t);
  if (!p) return null;
  return p.weekly_reset > 0 && p.weekly_reset <= t ? 0 : p.weekly_pct;
}

// weightsOf applies the live pool's rule: plan weights only when every plan is known.
function weightsOf(members) {
  const weighted = members.every((m) => m.weight > 0);
  return members.map((m) => (weighted ? m.weight : 1));
}

// poolHistory pools accounts ({account, weight, lengthMs, points}) at each
// time, from the first reading of any account on. Before an account's own
// first reading CPA sent it no traffic, so it holds that first value, though
// never back past the start of that reading's cycle.
function poolHistory(accounts, times) {
  const weights = weightsOf(accounts);
  const firsts = accounts.map((a) => a.points[0] || null);
  const since = Math.min(...firsts.filter(Boolean).map((p) => p.t));
  const valueAt = (a, first, t) => {
    const v = readingAt(a.points, t);
    if (v !== null || !first || t < since) return v;
    const cycleStart = first.weekly_reset > 0 && a.lengthMs > 0 ? first.weekly_reset - a.lengthMs : -Infinity;
    return t >= cycleStart ? first.weekly_pct : null;
  };
  return times.map((t) => {
    let sum = 0;
    let total = 0;
    accounts.forEach((a, i) => {
      const v = valueAt(a, firsts[i], t);
      if (v === null) return;
      sum += weights[i] * v;
      total += weights[i];
    });
    return { t, used: total > 0 ? sum / total : null };
  });
}

// withLiveReading ends an account's recorded readings with the one the view
// read live ({used, resetsAt}), so the recorded line meets the bar at now.
function withLiveReading(points, live, now) {
  if (live.used === null || live.used === undefined) return points;
  return [...points, { t: now, weekly_pct: live.used, weekly_reset: live.resetsAt || 0 }];
}

const sameCycle = (a, b) => Math.abs(a.weekly_reset - b.weekly_reset) <= RESET_JITTER;

// burnRate is the weekly points an account gained per hour over the window
// ending at now. It measures from the window start when the reading there
// belongs to the current cycle, otherwise from the cycle's first reading in
// the window. Null when one reading is all there is to go on.
function burnRate(points, now, windowMs) {
  const latest = lastReading(points, now);
  if (!latest) return null;
  if (latest.weekly_reset > 0 && latest.weekly_reset <= now) return 0;
  const start = now - windowMs;
  const before = lastReading(points, start);
  let base;
  if (before && sameCycle(before, latest) && !(before.weekly_reset > 0 && before.weekly_reset <= start)) {
    base = { t: start, pct: before.weekly_pct };
  } else {
    const first = points.find((p) => p.t > start && p.t <= now && sameCycle(p, latest));
    if (!first || first === latest) return null;
    base = { t: first.t, pct: first.weekly_pct };
  }
  return Math.max(0, (latest.weekly_pct - base.pct) / ((now - base.t) / LIMIT_HOUR));
}

// projectedUsed runs one account forward at its rate: it stops at the limit
// and starts over from 0 at each reset, a window length apart.
function projectedUsed(m, now, t) {
  let base = m.used;
  let from = now;
  let reset = m.resetsAt;
  while (reset !== null && reset > from && t >= reset) {
    base = 0;
    from = reset;
    reset += m.lengthMs;
  }
  return Math.min(100, base + ((m.rate || 0) * (t - from)) / LIMIT_HOUR);
}

// projectPool projects the weekly pool if every account keeps its current
// rate. Members carry account, weight, used, resetsAt, lengthMs and rate
// (points per hour, null when unknown, which holds the account still).
function projectPool(members, now, horizonMs, stepMs) {
  const weights = weightsOf(members);
  const total = weights.reduce((s, w) => s + w, 0);
  const points = [];
  let firstFull = null;
  let poolFull = null;
  for (let t = now; t <= now + horizonMs; t += stepMs) {
    const used = members.map((m) => projectedUsed(m, now, t));
    const pooled = used.reduce((s, u, i) => s + weights[i] * u, 0) / total;
    points.push({ t, used: pooled });
    const full = used.findIndex((u) => u >= 100);
    if (!firstFull && full >= 0) firstFull = { account: members[full].account, at: t };
    if (poolFull === null && used.every((u) => u >= 100)) poolFull = t;
  }
  return { points, firstFull, poolFull };
}

async function mapLimit(items, limit, fn) {
  const out = new Array(items.length);
  let next = 0;
  const worker = async () => {
    while (next < items.length) {
      const i = next++;
      out[i] = await fn(items[i], i);
    }
  };
  await Promise.all(Array.from({ length: Math.min(limit, items.length) }, worker));
  return out;
}

if (typeof module !== 'undefined') {
  module.exports = {
    claudeWeight, codexWeight, parseClaudeUsage, parseCodexUsage,
    parseAntigravityPlan, parseAntigravityUsage, parseGeminiQuota, parseDevinSignals, parseKimiUsage, summarizeCredentialStatus,
    accountWindows, poolWindow, pools, accountProblem, mapLimit, fmtDuration, parseClaudeSpend,
    readingAt, poolHistory, burnRate, projectPool, withLiveReading,
  };
}
