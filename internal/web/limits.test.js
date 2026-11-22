// Run with: bun test internal/web
// Fixtures follow the shapes CPA's api-call returned from the live endpoints
// on 2026-11-15, trimmed to the fields the page reads.
import { describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import {
  claudeWeight, codexWeight, parseClaudeUsage, parseCodexUsage,
  parseAntigravityPlan, parseAntigravityUsage, parseAntigravityQuotaSummary, parseGeminiQuota, parseDevinSignals, parseKimiUsage, summarizeCredentialStatus,
  accountWindows, poolWindow, pools, accountProblem, mapLimit, fmtDuration, parseClaudeSpend,
} from './limits.js';

const HOUR = 3600000;
const DAY = 24 * HOUR;
const now = Date.parse('2026-11-15T03:00:00Z');

const claudeExhausted = {
  five_hour: { utilization: 0.0, resets_at: null },
  seven_day: { utilization: 100.0, resets_at: '2026-11-16T03:59:59.849583+00:00' },
  limits: [
    { kind: 'session', group: 'session', percent: 0, resets_at: null, scope: null },
    { kind: 'weekly_all', group: 'weekly', percent: 100, resets_at: '2026-11-16T03:59:59.849583+00:00', scope: null },
    { kind: 'weekly_scoped', group: 'weekly', percent: 97, resets_at: '2026-11-16T03:59:59.849726+00:00', scope: { model: { id: null, display_name: 'Fable' }, surface: null } },
  ],
};

const claudeIdleSession = {
  five_hour: null,
  seven_day: { utilization: 58.0, resets_at: '2026-11-17T00:00:00.491321+00:00' },
  limits: [
    { kind: 'weekly_all', group: 'weekly', percent: 58, resets_at: '2026-11-17T00:00:00.491321+00:00', scope: null },
    { kind: 'weekly_scoped', group: 'weekly', percent: 50, resets_at: '2026-11-17T00:00:00.491461+00:00', scope: { model: { id: null, display_name: 'Fable' }, surface: null } },
  ],
};

const codexWeeklyOnly = {
  plan_type: 'prolite',
  rate_limit: {
    primary_window: { used_percent: 23, limit_window_seconds: 604800, reset_after_seconds: 292302, reset_at: 1795005193 },
    secondary_window: null,
  },
};

describe('plan weights', () => {
  test('Claude weight comes from the organization rate limit tier', () => {
    expect(claudeWeight({ organization: { organization_type: 'claude_max', rate_limit_tier: 'default_claude_max_20x' } })).toBe(20);
    expect(claudeWeight({ organization: { organization_type: 'claude_team', rate_limit_tier: 'default_claude_max_5x' } })).toBe(5);
    expect(claudeWeight({ account: { has_claude_pro: true }, organization: { rate_limit_tier: 'default_claude_ai' } })).toBe(1);
  });

  test('an unknown Claude plan has no weight', () => {
    expect(claudeWeight({ organization: { rate_limit_tier: 'enterprise_custom' } })).toBeNull();
    expect(claudeWeight(null)).toBeNull();
  });

  test('Codex weight comes from the plan type', () => {
    expect(codexWeight('plus')).toBe(1);
    expect(codexWeight('prolite')).toBe(5);
    expect(codexWeight('pro')).toBe(20);
    // Business (formerly Team), Enterprise and Edu seats get Plus-level limits.
    for (const seat of ['business', 'team', 'enterprise', 'edu']) expect(codexWeight(seat)).toBe(1);
    expect(codexWeight('free')).toBeNull();
    expect(codexWeight(undefined)).toBeNull();
  });
});

describe('parsing', () => {
  test('Claude limits become session, weekly and per-model weekly windows', () => {
    expect(parseClaudeUsage(claudeExhausted)).toEqual([
      { id: 'session', label: '5-hour', used: 0, resetsAt: null, lengthMs: 5 * HOUR },
      { id: 'weekly', label: 'Weekly', used: 100, resetsAt: Date.parse('2026-11-16T03:59:59.849Z'), lengthMs: 7 * DAY },
      { id: 'weekly:Fable', label: 'Weekly, Fable', used: 97, resetsAt: Date.parse('2026-11-16T03:59:59.849Z'), lengthMs: 7 * DAY },
    ]);
  });

  // An account with no session in progress still has the whole 5-hour
  // allowance, so it joins the session pool at 0%.
  test('a Claude account with no session limit reports an idle session', () => {
    const windows = parseClaudeUsage(claudeIdleSession);
    expect(windows[0]).toEqual({ id: 'session', label: '5-hour', used: 0, resetsAt: null, lengthMs: 5 * HOUR });
    expect(windows.map((w) => w.id)).toEqual(['session', 'weekly', 'weekly:Fable']);
  });

  test('Claude falls back to five_hour and seven_day without a limits list', () => {
    expect(parseClaudeUsage({ five_hour: { utilization: 12.5, resets_at: '2026-11-15T05:00:00Z' }, seven_day: { utilization: 40, resets_at: '2026-11-20T00:00:00Z' } })).toEqual([
      { id: 'session', label: '5-hour', used: 12.5, resetsAt: Date.parse('2026-11-15T05:00:00Z'), lengthMs: 5 * HOUR },
      { id: 'weekly', label: 'Weekly', used: 40, resetsAt: Date.parse('2026-11-20T00:00:00Z'), lengthMs: 7 * DAY },
    ]);
  });

  test('Claude falls back when the limits list has nothing usable', () => {
    const body = { limits: [null, { kind: 'monthly_spend', percent: 3 }], seven_day: { utilization: 40, resets_at: '2026-11-20T00:00:00Z' } };
    expect(parseClaudeUsage(body).map((w) => [w.id, w.used])).toEqual([['session', 0], ['weekly', 40]]);
  });

  test('Codex windows are named by their length', () => {
    expect(parseCodexUsage(codexWeeklyOnly)).toEqual({
      plan: 'prolite',
      windows: [{ id: 'weekly', label: 'Weekly', used: 23, resetsAt: 1795005193000, lengthMs: 7 * DAY }],
    });
    const both = parseCodexUsage({ plan_type: 'plus', rate_limit: {
      primary_window: { used_percent: 40, limit_window_seconds: 18000, reset_at: 1795000000 },
      secondary_window: { used_percent: 10, limit_window_seconds: 2592000, reset_at: 1796000000 },
    } });
    expect(both.windows.map((w) => [w.id, w.label])).toEqual([['session', '5-hour'], ['monthly', 'Monthly']]);
  });

  test('a body without any window is rejected', () => {
    expect(() => parseClaudeUsage({ error: 'nope' })).toThrow('no usage windows');
    expect(() => parseCodexUsage({ plan_type: 'plus', rate_limit: null })).toThrow('no usage windows');
  });
});

describe('weekly exhaustion', () => {
  const session = (used, resetsAt) => ({ id: 'session', label: '5-hour', used, resetsAt, lengthMs: 5 * HOUR });
  const weekly = (used, resetsAt) => ({ id: 'weekly', label: 'Weekly', used, resetsAt, lengthMs: 7 * DAY });

  test('a window refills what it used at its reset', () => {
    const [s] = accountWindows([session(40, now + HOUR), weekly(30, now + DAY)], now);
    expect(s.used).toBe(40);
    expect(s.blocked).toBe(false);
    expect(s.restore).toEqual({ at: now + HOUR, refill: 40 });
  });

  // With the weekly allowance spent, the session window cannot be used, and
  // an idle session comes back whole when the week resets.
  test('an exhausted week blocks an idle session until the weekly reset', () => {
    const [s] = accountWindows([session(0, null), weekly(100, now + DAY)], now);
    expect(s.used).toBe(100);
    expect(s.blocked).toBe(true);
    expect(s.restore).toEqual({ at: now + DAY, refill: 100 });
  });

  test('a session that resets before the week comes back whole at the weekly reset', () => {
    const [s] = accountWindows([session(30, now + HOUR), weekly(100, now + DAY)], now);
    expect(s.restore).toEqual({ at: now + DAY, refill: 100 });
  });

  test('a spent session that resets after the week comes back at its own reset', () => {
    const [s] = accountWindows([session(100, now + 2 * DAY), weekly(100, now + DAY)], now);
    expect(s.restore).toEqual({ at: now + 2 * DAY, refill: 100 });
  });

  test('a partly used session that resets after the week gets its unused part at the weekly reset', () => {
    const [s] = accountWindows([session(30, now + 2 * DAY), weekly(100, now + DAY)], now);
    expect(s.restore).toEqual({ at: now + DAY, refill: 70 });
  });

  test('a reset already in the past restores nothing', () => {
    const [s, w] = accountWindows([session(40, now - 1000), weekly(100, now - 1000)], now);
    expect(s.restore).toBeNull();
    expect(w.restore).toBeNull();
  });
});

describe('pool', () => {
  const member = (account, weight, used, resetsAt, lengthMs = 7 * DAY) => ({ account, weight, used, resetsAt, lengthMs, restore: resetsAt > now && used > 0 ? { at: resetsAt, refill: used } : null });

  test('plan weights decide each account share of the pool', () => {
    // Max 20x at 58%, Max 5x at 100%: (20*58 + 5*100) / 25 = 66.4%.
    const pool = poolWindow([member('a', 20, 58, now + 2 * DAY), member('b', 5, 100, now + DAY)], now);
    expect(pool.weighted).toBe(true);
    expect(pool.used).toBeCloseTo(66.4, 6);
    expect(pool.accounts.map((x) => [x.account, x.capacity])).toEqual([['a', 80], ['b', 20]]);
  });

  test('each account reports how much of its own window has elapsed', () => {
    const pool = poolWindow([member('a', 20, 58, now + 2 * DAY), member('b', 5, 100, null)], now);
    expect(pool.accounts[0].elapsed).toBeCloseTo(500 / 7, 6);
    expect(pool.accounts[1].elapsed).toBe(0);
  });

  test('one account without a known plan makes every weight equal', () => {
    const pool = poolWindow([member('a', 20, 60, now + DAY), member('b', null, 20, now + DAY)], now);
    expect(pool.weighted).toBe(false);
    expect(pool.used).toBe(40);
    expect(pool.accounts.map((x) => x.capacity)).toEqual([50, 50]);
  });

  test('pace compares usage with the time elapsed in each window', () => {
    // Both windows are 6 of 7 days in: expected 85.7%.
    const late = poolWindow([member('a', 1, 50, now + DAY), member('b', 1, 70, now + DAY)], now);
    expect(late.expected).toBeCloseTo(600 / 7, 6);
    expect(late.pace).toBe('under');
    const early = poolWindow([member('a', 1, 50, now + 6 * DAY)], now);
    expect(early.pace).toBe('ahead');
    const even = poolWindow([member('a', 1, 47, now + 4 * DAY)], now);
    expect(even.pace).toBe('on');
  });

  // A blocked session holds no consumption, so it counts in the used share
  // but not in the pace.
  test('blocked accounts are left out of the pace', () => {
    const blocked = { ...member('b', 5, 100, null, 5 * HOUR), blocked: true };
    const pool = poolWindow([member('a', 20, 0, null, 5 * HOUR), blocked], now);
    expect(pool.used).toBe(20);
    expect(pool.pace).toBe('on');
    expect(poolWindow([blocked], now).pace).toBeNull();
  });

  test('a window with no reset has not started', () => {
    const pool = poolWindow([member('a', 1, 0, null, 5 * HOUR)], now);
    expect(pool.expected).toBe(0);
    expect(pool.pace).toBe('on');
  });

  test('restores are listed soonest first as a share of the pool', () => {
    const pool = poolWindow([member('a', 20, 58, now + 2 * DAY), member('b', 5, 100, now + DAY)], now);
    expect(pool.restores).toEqual([{ account: 'b', at: now + DAY, percent: 20 }, { account: 'a', at: now + 2 * DAY, percent: 46 }]);
  });
});

describe('pools', () => {
  test('accounts are pooled per window, in the order windows first appear', () => {
    const result = pools([
      { account: 'max20', weight: 20, windows: parseClaudeUsage(claudeIdleSession) },
      { account: 'team5', weight: 5, windows: parseClaudeUsage(claudeExhausted) },
    ], now);
    expect(result.map((p) => [p.id, p.members.length])).toEqual([['session', 2], ['weekly', 2], ['weekly:Fable', 2]]);
    const session = result[0];
    // The team account's idle session is blocked by its spent week.
    expect(session.used).toBe(20);
    expect(session.restores).toEqual([{ account: 'team5', at: Date.parse('2026-11-16T03:59:59.849Z'), percent: 20 }]);
  });
});

describe('account problems', () => {
  test('an expired token tells whether CPA renews it or a sign-in is needed', () => {
    expect(accountProblem(401, false)).toBe('Token expired. CPA renews it on the next proxied request.');
    expect(accountProblem(401, true)).toBe('Sign in again from the management center.');
    expect(accountProblem(403, false)).toBe('Token expired. CPA renews it on the next proxied request.');
  });

  test('other upstream statuses are reported as they are', () => {
    expect(accountProblem(429, false)).toBe('The provider rate limited the usage check. Try again later.');
    expect(accountProblem(500, false)).toBe('The provider answered HTTP 500.');
    expect(accountProblem(200, false)).toBeNull();
  });
});

test('mapLimit keeps order and never runs more than the limit at once', async () => {
  let running = 0, peak = 0;
  const out = await mapLimit([1, 2, 3, 4, 5, 6], 2, async (n) => {
    running++; peak = Math.max(peak, running);
    await new Promise((r) => setTimeout(r, 5 * (7 - n)));
    running--;
    return n * 10;
  });
  expect(out).toEqual([10, 20, 30, 40, 50, 60]);
  expect(peak).toBe(2);
});

test('durations round before they are split into units', () => {
  expect(fmtDuration(59.6 * 60000)).toBe('1 h 0 min');
  expect(fmtDuration(HOUR + 59 * 60000 + 40000)).toBe('2 h 0 min');
  expect(fmtDuration(DAY + 23 * HOUR + 40 * 60000)).toBe('2 d 0 h');
  expect(fmtDuration(20000)).toBe('1 min');
  expect(fmtDuration(3 * DAY + 9 * HOUR + 10 * 60000)).toBe('3 d 9 h');
});

describe('Claude extra usage', () => {
  const money = (amount) => ({ amount_minor: amount, currency: 'USD', exponent: 2 });

  test('spend is read in major units', () => {
    const body = { spend: { used: money(50000), limit: money(50000), percent: 100, enabled: true } };
    expect(parseClaudeSpend(body)).toEqual({ used: 500, limit: 500, currency: 'USD' });
  });

  test('spend without a limit has no limit', () => {
    expect(parseClaudeSpend({ spend: { used: money(1234), limit: null, enabled: true } })).toEqual({ used: 12.34, limit: null, currency: 'USD' });
  });

  test('disabled extra usage reads as none', () => {
    expect(parseClaudeSpend({ spend: { used: money(0), limit: null, enabled: false } })).toBeNull();
    expect(parseClaudeSpend({ extra_usage: { is_enabled: false, used_credits: null } })).toBeNull();
    expect(parseClaudeSpend({})).toBeNull();
  });

  test('extra_usage is read when spend is missing, defaulting missing decimal_places to 2', () => {
    const body = { extra_usage: { is_enabled: true, monthly_limit: 50000, used_credits: 1234.0, currency: 'USD' } };
    expect(parseClaudeSpend(body)).toEqual({ used: 12.34, limit: 500, currency: 'USD' });
    expect(parseClaudeSpend({ spend: { used: { amount_minor: 500, currency: 'USD' }, limit: null, enabled: true } })).toEqual({
      used: 5, limit: null, currency: 'USD',
    });
  });
});

describe('other providers', () => {
  test('Antigravity plan reads paidTier and Google One AI credits', () => {
    const p = parseAntigravityPlan({
      currentTier: { id: 'free-tier' },
      paidTier: { id: 'g1-pro-tier', availableCredits: [{ creditType: 'GOOGLE_ONE_AI', creditAmount: '125', minimumCreditAmountForUsage: '1' }] },
      cloudaicompanionProject: 'proj-1',
    });
    expect(p).toEqual({ plan: 'Pro', weight: 5, windowMs: 5 * HOUR, credits: { amount: 125, min: 1 }, project: 'proj-1' });
  });

  test('Antigravity free tier refreshes weekly instead of every five hours', () => {
    expect(parseAntigravityPlan({ currentTier: { id: 'free-tier' } }).windowMs).toBe(7 * DAY);
  });

  // Shapes from a live Pro account read at 2026-11-20T17:21:55Z.
  const AG_NOW = Date.parse('2026-11-20T17:21:55Z');

  test('Antigravity windows are named by their length, like the other providers', () => {
    const windows = parseAntigravityUsage({
      models: {
        'gemini-3-flash': { quotaInfo: { remainingFraction: 0.5254414, resetTime: '2026-11-20T21:47:38Z' } },
        'gemini-2.5-pro': { quotaInfo: { remainingFraction: 0.5254414, resetTime: '2026-11-20T21:47:38Z' } },
        'claude-opus-5-5-high': { quotaInfo: { remainingFraction: 0.9, resetTime: '2026-11-20T21:43:02Z' } },
      },
    }, 5 * HOUR, AG_NOW);
    expect(windows.map((w) => [w.id, w.label, w.lengthMs])).toEqual([
      ['session:gemini', '5-hour, Gemini', 5 * HOUR],
      ['session:claude-oss', '5-hour, Claude & OSS', 5 * HOUR],
    ]);
  });

  test('an untouched Antigravity window has not started, since its reset is only now plus five hours', () => {
    const [w] = parseAntigravityUsage({
      models: { 'claude-opus-5-5-high': { quotaInfo: { remainingFraction: 1, resetTime: '2026-11-20T22:21:55Z' } } },
    }, 5 * HOUR, AG_NOW);
    expect(w).toEqual({ id: 'session:claude-oss', label: '5-hour, Claude & OSS', used: 0, resetsAt: null, lengthMs: 5 * HOUR });
  });

  test('an Antigravity window that resets after more than five hours is the weekly cap', () => {
    const [w] = parseAntigravityUsage({
      models: {
        'gemini-3-flash': { quotaInfo: { remainingFraction: 0, resetTime: '2026-11-23T09:00:00Z' } },
        'gemini-2.5-pro': { quotaInfo: { remainingFraction: 0, resetTime: '2026-11-23T09:00:00Z' } },
      },
    }, 5 * HOUR, AG_NOW);
    expect(w).toEqual({ id: 'weekly:gemini', label: 'Weekly, Gemini', used: 100, resetsAt: Date.parse('2026-11-23T09:00:00Z'), lengthMs: 7 * DAY });
  });

  test('Antigravity free tier windows are weekly', () => {
    const [w] = parseAntigravityUsage({
      models: {
        'gemini-3-flash': { quotaInfo: { remainingFraction: 0.5, resetTime: '2026-11-24T09:00:00Z' } },
        'gemini-2.5-pro': { quotaInfo: { remainingFraction: 0.5, resetTime: '2026-11-24T09:00:00Z' } },
      },
    }, 7 * DAY, AG_NOW);
    expect([w.id, w.label, w.used]).toEqual(['weekly:gemini', 'Weekly, Gemini', 50]);
  });

  test('Antigravity usage collapses identical Gemini buckets and skips tab completions', () => {
    const windows = parseAntigravityUsage({
      models: {
        'tab_flash_lite_preview': { quotaInfo: { remainingFraction: 1 } },
        'gemini-2.5-pro': { quotaInfo: { remainingFraction: 0.64, resetTime: '2026-11-20T16:47:38Z' } },
        'gemini-3-flash': { quotaInfo: { remainingFraction: 0.64, resetTime: '2026-11-20T16:47:38Z' } },
        'claude-opus-5-5-high': { quotaInfo: { remainingFraction: 0.9, resetTime: '2026-11-20T21:43:02Z' } },
      },
    }, 5 * HOUR, Date.parse('2026-11-20T16:45:00Z'));
    expect(windows.map((w) => [w.id, Math.round(w.used)])).toEqual([['session:gemini', 36], ['session:claude-oss', 10]]);
  });

  test('Antigravity keeps Gemini Pro and Flash separate when their usage diverges', () => {
    const windows = parseAntigravityUsage({
      models: {
        'gemini-4-neon': { quotaInfo: { remainingFraction: 0.2, resetTime: '2026-11-20T16:00:00Z' } },
        'gemini-3-flash': { quotaInfo: { remainingFraction: 0.9, resetTime: '2026-11-20T16:00:00Z' } },
      },
    }, 5 * HOUR, Date.parse('2026-11-20T12:00:00Z'));
    expect(windows.map((w) => [w.id, Math.round(w.used)])).toEqual([['session:gemini-pro', 80], ['session:gemini-flash', 10]]);
  });

  // retrieveUserQuotaSummary as a live Pro account returned it at 2026-11-20T18:59:53Z.
  const AG_SUMMARY = { groups: [
    { displayName: 'Gemini Models', buckets: [
      { bucketId: 'gemini-weekly', window: 'weekly', resetTime: '2026-11-27T11:47:38Z', remainingFraction: 0.7600026 },
      { bucketId: 'gemini-5h', window: '5h', resetTime: '2026-11-20T21:47:38Z', remainingFraction: 0.5472385 },
    ] },
    { displayName: 'Claude and GPT models', buckets: [
      { bucketId: '3p-weekly', window: 'weekly', resetTime: '2026-11-27T18:59:53Z', remainingFraction: 1 },
      { bucketId: '3p-5h', window: '5h', resetTime: '2026-11-20T23:59:53Z', remainingFraction: 1 },
    ] },
    { displayName: 'Gemini 4 Argon', buckets: [
      { bucketId: 'gemini-argon-wtu-weekly', window: 'weekly', resetTime: '2026-11-27T13:09:09Z', remainingFraction: 0.99835056 },
    ] },
  ] };

  test('Antigravity quota summary gives each group its 5-hour and weekly windows', () => {
    const windows = parseAntigravityQuotaSummary(AG_SUMMARY);
    expect(windows.map((w) => [w.id, w.label, Math.round(w.used * 10) / 10, w.resetsAt, w.lengthMs])).toEqual([
      ['session:gemini', '5-hour, Gemini', 45.3, Date.parse('2026-11-20T21:47:38Z'), 5 * HOUR],
      ['weekly:gemini', 'Weekly, Gemini', 24, Date.parse('2026-11-27T11:47:38Z'), 7 * DAY],
      ['session:claude-and-gpt', '5-hour, Claude and GPT', 0, null, 5 * HOUR],
      ['weekly:claude-and-gpt', 'Weekly, Claude and GPT', 0, null, 7 * DAY],
      ['weekly:gemini-4-argon', 'Weekly, Gemini 4 Argon', 0.2, Date.parse('2026-11-27T13:09:09Z'), 7 * DAY],
    ]);
  });

  test('Antigravity quota summary ignores null remainingFraction, trailing whitespace and empty slugs', () => {
    const windows = parseAntigravityQuotaSummary({
      groups: [
        { displayName: '   ', buckets: [{ window: 'weekly', remainingFraction: 0.5 }] },
        { displayName: '---', buckets: [{ window: 'weekly', remainingFraction: 0.5 }] },
        { displayName: 'Gemini Models  ', buckets: [
          { window: '5h', remainingFraction: null, resetTime: '2026-11-20T21:00:00Z' },
          { window: 'weekly', remainingFraction: 0.75, resetTime: '2026-11-27T11:00:00Z' },
        ] },
      ],
    });
    expect(windows.map((w) => [w.id, w.label, w.used])).toEqual([['weekly:gemini', 'Weekly, Gemini', 25]]);
  });

  test('an Antigravity quota summary without groups fails so the page falls back to fetchAvailableModels', () => {
    expect(() => parseAntigravityQuotaSummary({ groups: [] })).toThrow('no usage windows');
    expect(() => parseAntigravityQuotaSummary({})).toThrow('no usage windows');
  });

  test('a spent Antigravity weekly limit blocks the 5-hour window of its own group only', () => {
    const now = Date.parse('2026-11-20T19:00:00Z');
    const windows = [
      { id: 'session:gemini', label: '5-hour, Gemini', used: 30, resetsAt: now + HOUR, lengthMs: 5 * HOUR },
      { id: 'weekly:gemini', label: 'Weekly, Gemini', used: 100, resetsAt: now + 3 * DAY, lengthMs: 7 * DAY },
      { id: 'session:claude-and-gpt', label: '5-hour, Claude and GPT', used: 10, resetsAt: now + HOUR, lengthMs: 5 * HOUR },
      { id: 'weekly:claude-and-gpt', label: 'Weekly, Claude and GPT', used: 20, resetsAt: now + 3 * DAY, lengthMs: 7 * DAY },
    ];
    const out = accountWindows(windows, now);
    expect(out.map((w) => [w.id, w.blocked, w.used])).toEqual([
      ['session:gemini', true, 100], ['weekly:gemini', false, 100], ['session:claude-and-gpt', false, 10], ['weekly:claude-and-gpt', false, 20],
    ]);
    expect(out[0].restore).toEqual({ at: now + 3 * DAY, refill: 100 });
  });

  test('Gemini CLI quota parses buckets by model family', () => {
    const windows = parseGeminiQuota({
      buckets: [
        { modelId: 'gemini-2.5-pro', remainingFraction: 0.75, resetTime: '2026-11-21T00:00:00Z' },
        { modelId: 'gemini-2.5-flash', remainingFraction: 0.95, resetTime: '2026-11-21T00:00:00Z' },
      ],
    });
    expect(windows.map((w) => [w.id, w.label, Math.round(w.used)])).toEqual([['daily:gemini-pro', 'Daily, Gemini Pro', 25], ['daily:gemini-flash', 'Daily, Gemini Flash', 5]]);
  });

  test('Devin quota reads directly from CPA credential signals', () => {
    const res = parseDevinSignals({
      quota: {
        signals: {
          plan: 'core',
          daily_quota_remaining_percent: '60%',
          weekly_quota_remaining_percent: '25%',
          daily_quota_reset_at: '2026-11-21T00:00:00Z',
          weekly_quota_reset_at: '2026-11-25T00:00:00Z',
        },
      },
    });
    expect(res.plan).toBe('core');
    expect(res.windows.map((w) => [w.id, w.used])).toEqual([['daily', 40], ['weekly', 75]]);
  });

  test('Kimi usage parses weekly and short windows', () => {
    const windows = parseKimiUsage({
      usage: { limit: 100, used: 30, resetTime: '2026-11-25T00:00:00Z' },
      limits: [{ window: { duration: 300, timeUnit: 'TIME_UNIT_MINUTE' }, detail: { limit: 100, remaining: 85, resetTime: '2026-11-20T20:00:00Z' } }],
    });
    expect(windows.map((w) => [w.id, w.label, w.used])).toEqual([['weekly', 'Weekly', 30], ['session', '5-hour', 15]]);
  });

  test('credential status summarizes active cooldowns, blocks and recent counts', () => {
    const st = summarizeCredentialStatus({
      status: 'error',
      status_message: 'credential_quota',
      quota_blocked: true,
      cooldowns: [
        { scope: 'credential', reason: 'credential_quota', retry_at: '2026-11-16T00:00:00Z' },
        { scope: 'model', model_key: 'old', reason: 'quota', retry_at: '2026-11-14T00:00:00Z' },
      ],
      recent_requests: [{ success: 10, failed: 2 }, { success: 5, failed: 1 }],
    }, now);
    expect(st.state).toBe('quota_blocked');
    expect(st.cooldowns.length).toBe(1);
    expect(st.recentSuccess).toBe(15);
    expect(st.recentFailed).toBe(3);
  });
});

describe('limits rendering', () => {
  const page = readFileSync(new URL('./index.html', import.meta.url), 'utf8');
  const limitsLib = readFileSync(new URL('./limits.js', import.meta.url), 'utf8');
  const limitsSection = page.split('/* ---------- limits ---------- */')[1].split('/* ---------- latency ---------- */')[0];

  function renderView(limitsData) {
    const body = { children: [], replaceChildren(...kids) { this.children = kids.filter(Boolean); } };
    const makeEl = (tag, attrs = {}, ...children) => {
      const node = {
        tag,
        attrs: { ...attrs },
        className: attrs.class || '',
        children: [],
        append(...kids) { this.children.push(...kids.filter((k) => k !== null && k !== undefined && k !== false)); },
      };
      if (attrs.text !== undefined) node.children.push(String(attrs.text));
      node.append(...children);
      return node;
    };
    const ctx = vm.createContext({
      HOUR, DAY,
      el: makeEl,
      fmtInt: (n) => String(Math.round(n)),
      document: { getElementById: (id) => (id === 'limits-body' ? body : { hidden: false, textContent: '' }) },
      AuthError: class AuthError extends Error {},
      api: async () => null,
      management: async () => ({ files: [] }),
    });
    vm.runInContext(limitsLib + '\n' + limitsSection + '\nglobalThis.__render = (d) => { limitsData = d; renderLimits(); };', ctx);
    ctx.__render(limitsData);
    return body.children;
  }

  const walk = (nodes, pred, out = []) => {
    for (const n of nodes) {
      if (!n || typeof n !== 'object') continue;
      if (pred(n)) out.push(n);
      if (Array.isArray(n.children)) walk(n.children, pred, out);
    }
    return out;
  };
  const textOf = (node) => {
    if (typeof node === 'string') return node;
    if (!node || !Array.isArray(node.children)) return '';
    return node.children.map(textOf).join(' ');
  };
  const hasClass = (node, cls) => String(node.className || '').split(/\s+/).includes(cls);

  test('single-account provider omits duplicate account rows and puts identity in subtitle', () => {
    const sections = renderView({
      checkedAt: now,
      disabled: 0,
      statusOnly: [],
      history: {
        accounts: [{
          auth_index: 'c1',
          points: [
            { t: now - 24 * HOUR, weekly_pct: 70, weekly_reset: now + 4 * DAY },
            { t: now - 3 * HOUR, weekly_pct: 90, weekly_reset: now + 4 * DAY },
          ],
        }],
      },
      results: [{
        account: { auth_index: 'c1', provider: 'codex', email: 'eu@example.com' },
        plan: 'Pro Lite',
        weight: 5,
        windows: [{ id: 'weekly', label: 'Weekly', used: 93, resetsAt: now + 4 * DAY, lengthMs: 7 * DAY }],
      }],
    });
    expect(sections.length).toBe(1);
    expect(walk(sections, (n) => hasClass(n, 'acct')).length).toBe(0);
    expect(walk(sections, (n) => hasClass(n, 'meter')).length).toBe(1);
    const sub = walk(sections, (n) => hasClass(n, 'sub'))[0];
    expect(textOf(sub)).toBe('eu@example.com, Pro Lite');
    const head = walk(sections, (n) => hasClass(n, 'pool-head'))[0];
    expect(textOf(head)).toContain('93% used');
    expect(textOf(head)).toContain('Resets in');
    expect(textOf(head)).not.toContain('of the pool');
    const stats = walk(sections, (n) => hasClass(n, 'trend-stats'))[0];
    expect(textOf(stats)).toContain('At limit:');
    expect(textOf(stats)).not.toContain('First at limit:');
    expect(textOf(stats)).not.toContain('Whole pool at limit:');
    expect(textOf(stats)).not.toContain('of the pool per hour');
  });

  test('single-account blocked, limit-reached and not-started states render on the single bar', () => {
    const sections = renderView({
      checkedAt: now,
      disabled: 0,
      statusOnly: [],
      history: null,
      results: [{
        account: { auth_index: 'a1', provider: 'antigravity', email: 'user@example.com' },
        plan: 'Pro',
        weight: 5,
        windows: [
          { id: 'session:gemini', label: '5-hour, Gemini', used: 20, resetsAt: now + HOUR, lengthMs: 5 * HOUR },
          { id: 'weekly:gemini', label: 'Weekly, Gemini', used: 100, resetsAt: now + 2 * DAY, lengthMs: 7 * DAY },
          { id: 'session:other', label: '5-hour, Other', used: 0, resetsAt: null, lengthMs: 5 * HOUR },
        ],
      }],
    });
    expect(walk(sections, (n) => hasClass(n, 'acct')).length).toBe(0);
    const poolsRendered = walk(sections, (n) => hasClass(n, 'pool'));
    expect(poolsRendered.length).toBe(3);
    // Blocked 5-hour window uses var(--other) and explains why it is blocked.
    const blockedFill = walk([poolsRendered[0]], (n) => hasClass(n, 'fill'))[0];
    expect(blockedFill.attrs.style).toContain('var(--other)');
    expect(textOf(poolsRendered[0])).toContain('Blocked by the weekly limit');
    // Exhausted weekly window uses var(--critical) and shows Limit reached.
    const spentFill = walk([poolsRendered[1]], (n) => hasClass(n, 'fill'))[0];
    expect(spentFill.attrs.style).toContain('var(--critical)');
    expect(textOf(poolsRendered[1])).toContain('Limit reached');
    // Untouched window shows Not started.
    expect(textOf(poolsRendered[2])).toContain('Not started');
  });

  test('multi-account provider keeps account rows for shared windows and names lone-account windows', () => {
    const sections = renderView({
      checkedAt: now,
      disabled: 0,
      statusOnly: [],
      history: null,
      results: [
        {
          account: { auth_index: 'u1', provider: 'claude', email: 'first@example.com' },
          plan: 'Max 20x',
          weight: 20,
          windows: [
            { id: 'weekly', label: 'Weekly', used: 40, resetsAt: now + 3 * DAY, lengthMs: 7 * DAY },
            { id: 'weekly:Fable', label: 'Weekly, Fable', used: 25, resetsAt: now + 3 * DAY, lengthMs: 7 * DAY },
          ],
        },
        {
          account: { auth_index: 'u2', provider: 'claude', email: 'second@example.com' },
          plan: 'Max 5x',
          weight: 5,
          windows: [
            { id: 'weekly', label: 'Weekly', used: 80, resetsAt: now + 2 * DAY, lengthMs: 7 * DAY },
          ],
        },
      ],
    });
    const poolsRendered = walk(sections, (n) => hasClass(n, 'pool'));
    expect(poolsRendered.length).toBe(2);
    // Shared Weekly window has the pool bar plus 2 per-account rows.
    expect(walk([poolsRendered[0]], (n) => hasClass(n, 'acct')).length).toBe(2);
    expect(textOf(poolsRendered[0])).toContain('of the pool used');
    // Single-member scoped window omits duplicate .acct row and names its owner.
    expect(walk([poolsRendered[1]], (n) => hasClass(n, 'acct')).length).toBe(0);
    expect(textOf(poolsRendered[1])).toContain('first@example.com, Max 20x');
  });

  test('Antigravity weekly pools render consumption and projection charts per group', () => {
    const sections = renderView({
      checkedAt: now,
      disabled: 0,
      statusOnly: [],
      history: {
        accounts: [
          {
            auth_index: 'ag1',
            provider: 'antigravity',
            group: 'gemini',
            group_label: 'Gemini',
            points: [
              { t: now - 24 * HOUR, weekly_pct: 10, weekly_reset: now + 5 * DAY },
              { t: now - 3 * HOUR, weekly_pct: 25, weekly_reset: now + 5 * DAY },
            ],
          },
          {
            auth_index: 'ag1',
            provider: 'antigravity',
            group: 'claude-and-gpt',
            group_label: 'Claude and GPT',
            points: [
              { t: now - 24 * HOUR, weekly_pct: 5, weekly_reset: now + 5 * DAY },
              { t: now - 3 * HOUR, weekly_pct: 15, weekly_reset: now + 5 * DAY },
            ],
          },
        ],
      },
      results: [{
        account: { auth_index: 'ag1', provider: 'antigravity', email: 'ag@example.com' },
        plan: 'Pro',
        weight: 5,
        windows: [
          { id: 'session:gemini', label: '5-hour, Gemini', used: 40, resetsAt: now + 2 * HOUR, lengthMs: 5 * HOUR },
          { id: 'weekly:gemini', label: 'Weekly, Gemini', used: 30, resetsAt: now + 5 * DAY, lengthMs: 7 * DAY },
          { id: 'weekly:claude-and-gpt', label: 'Weekly, Claude and GPT', used: 20, resetsAt: now + 5 * DAY, lengthMs: 7 * DAY },
        ],
      }],
    });
    const poolsRendered = walk(sections, (n) => hasClass(n, 'pool'));
    expect(poolsRendered.length).toBe(3);
    // 5-hour session window has no trend chart; both weekly group windows do.
    expect(walk([poolsRendered[0]], (n) => hasClass(n, 'trend-stats')).length).toBe(0);
    expect(walk([poolsRendered[1]], (n) => hasClass(n, 'trend-stats')).length).toBe(1);
    expect(walk([poolsRendered[2]], (n) => hasClass(n, 'trend-stats')).length).toBe(1);
    expect(walk(sections, (n) => hasClass(n, 'trend')).length).toBe(2);
  });

  test('multi-account pool at 100% renders critical red Limit reached and Antigravity empty trend explains polling', () => {
    const sections = renderView({
      checkedAt: now,
      disabled: 0,
      statusOnly: [],
      history: { accounts: [] },
      results: [
        {
          account: { auth_index: 'ag1', provider: 'antigravity', email: 'one@example.com' },
          plan: 'Pro',
          weight: 5,
          windows: [{ id: 'weekly:gemini', label: 'Weekly, Gemini', used: 100, resetsAt: now + 2 * DAY, lengthMs: 7 * DAY }],
        },
        {
          account: { auth_index: 'ag2', provider: 'antigravity', email: 'two@example.com' },
          plan: 'Pro',
          weight: 5,
          windows: [{ id: 'weekly:gemini', label: 'Weekly, Gemini', used: 100, resetsAt: now + 3 * DAY, lengthMs: 7 * DAY }],
        },
      ],
    });
    const pool = walk(sections, (n) => hasClass(n, 'pool'))[0];
    const bigFill = walk([pool], (n) => hasClass(n, 'fill'))[0];
    expect(bigFill.attrs.style).toContain('var(--critical)');
    expect(textOf(pool)).toContain('Limit reached');
    expect(textOf(pool)).toContain('polled every 5 minutes');
  });

  test('short recorded history under 24 h shows Recorded since in trend stats', () => {
    const sections = renderView({
      checkedAt: now,
      disabled: 0,
      statusOnly: [],
      history: {
        accounts: [{
          auth_index: 'ag1',
          provider: 'antigravity',
          group: 'gemini',
          group_label: 'Gemini',
          points: [
            { t: now - 2 * HOUR, weekly_pct: 1.6, weekly_reset: now + 6 * DAY },
            { t: now - 15 * 60000, weekly_pct: 2.8, weekly_reset: now + 6 * DAY },
          ],
        }],
      },
      results: [{
        account: { auth_index: 'ag1', provider: 'antigravity', email: 'ag@example.com' },
        plan: 'Ultra',
        weight: 20,
        windows: [{ id: 'weekly:gemini', label: 'Weekly, Gemini', used: 3.1, resetsAt: now + 6 * DAY, lengthMs: 7 * DAY }],
      }],
    });
    const stats = walk(sections, (n) => hasClass(n, 'trend-stats'))[0];
    expect(textOf(stats)).toContain('Recorded since');
  });
});
