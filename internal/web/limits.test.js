// Run with: bun test internal/web
// Fixtures follow the shapes CPA's api-call returned from the live endpoints
// on 2026-11-15, trimmed to the fields the page reads.
import { describe, expect, test } from 'bun:test';
import {
  claudeWeight, codexWeight, parseClaudeUsage, parseCodexUsage, accountWindows, poolWindow, pools, accountProblem, mapLimit, fmtDuration, parseClaudeSpend,
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
    expect(codexWeight('team')).toBeNull();
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
    expect(pool.segments).toEqual([{ account: 'a', share: 46.4 }, { account: 'b', share: 20 }]);
  });

  test('one account without a known plan makes every weight equal', () => {
    const pool = poolWindow([member('a', 20, 60, now + DAY), member('b', null, 20, now + DAY)], now);
    expect(pool.weighted).toBe(false);
    expect(pool.used).toBe(40);
    expect(pool.segments).toEqual([{ account: 'a', share: 30 }, { account: 'b', share: 10 }]);
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

  test('extra_usage is read when spend is missing', () => {
    const body = { extra_usage: { is_enabled: true, monthly_limit: 50000, used_credits: 1234.0, currency: 'USD', decimal_places: 2 } };
    expect(parseClaudeSpend(body)).toEqual({ used: 12.34, limit: 500, currency: 'USD' });
  });
});
