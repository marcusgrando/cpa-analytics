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
    segments: members.map((m) => ({ account: m.account, share: (weightOf(m) * m.used) / total })),
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
  module.exports = { claudeWeight, codexWeight, parseClaudeUsage, parseCodexUsage, accountWindows, poolWindow, pools, accountProblem, mapLimit, fmtDuration };
}
