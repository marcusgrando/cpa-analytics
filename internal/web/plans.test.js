import { describe, expect, test } from 'bun:test';
import {
  PLAN_MONTH_DAYS, PLAN_WEEK_DAYS,
  resolvePlanCost, effectiveRangeDays, computeAccountPlanRow, groupPlanRows, rankPlanRows, planCostsYAML,
} from './plans.js';

describe('plan cost resolution', () => {
  test('prefers session edits over plugin config over built-in defaults', () => {
    const item = { authIndex: 'acc-1', provider: 'claude', plan: 'Max 20x' };
    expect(resolvePlanCost(item, {}, {})).toEqual({ monthlyCost: 200, source: 'default', key: 'claude:Max 20x' });
    expect(resolvePlanCost(item, { 'claude:Max 20x': 180 }, {})).toEqual({ monthlyCost: 180, source: 'config', key: 'claude:Max 20x' });
    expect(resolvePlanCost(item, { 'claude:Max 20x': 180, 'acc-1': 150 }, {})).toEqual({ monthlyCost: 150, source: 'config', key: 'claude:Max 20x' });
    expect(resolvePlanCost(item, { 'acc-1': 150 }, { 'claude:Max 20x': 190 })).toEqual({ monthlyCost: 190, source: 'session', key: 'claude:Max 20x' });
    expect(resolvePlanCost(item, { 'acc-1': 150 }, { 'claude:Max 20x': 190, 'acc-1': 175 })).toEqual({ monthlyCost: 175, source: 'session', key: 'claude:Max 20x' });
  });

  test('normalizes gemini-cli to gemini and marks API keys as pay-as-you-go', () => {
    expect(resolvePlanCost({ authIndex: 'g1', provider: 'gemini-cli', plan: 'Free' }, {}, {})).toEqual({
      monthlyCost: 0, source: 'default', key: 'gemini:Free',
    });
    expect(resolvePlanCost({ authIndex: 'k1', provider: 'openai', plan: '', isApiKey: true }, {}, {})).toEqual({
      monthlyCost: null, source: 'payg', key: 'openai:Unknown',
    });
  });
});

describe('effectiveRangeDays', () => {
  test('clamps period cost to recorded history when recording began inside the range', () => {
    const day = 86400000;
    const to = 30 * day;
    const from = 0;
    expect(effectiveRangeDays(from, to, 25 * day)).toEqual({ days: 5, clamped: true });
    expect(effectiveRangeDays(from, to, 0)).toEqual({ days: 30, clamped: false });
  });
});

describe('cost-benefit and ranking', () => {
  const weekCost200 = 200 * (PLAN_WEEK_DAYS / PLAN_MONTH_DAYS);

  test('computes potential leverage, realized leverage and token-per-dollar breakdown', () => {
    const row = computeAccountPlanRow({
      authIndex: 'c1', name: 'claude-main', provider: 'claude', plan: 'Max 20x',
      cost_usd: 300, uncached_input: 10_000_000, cache_read: 90_000_000, cache_write: 5_000_000,
      output: 2_000_000, reasoning: 1_000_000, requests: 200, top_model: 'claude-opus-4-6', top_model_share: 0.85,
      weekly_pct: 50, measured_cost_usd: 300,
      measured_tokens: { uncached_input: 10_000_000, cache_read: 90_000_000, cache_write: 5_000_000, output: 2_000_000, reasoning: 1_000_000 },
    }, { effectiveDays: PLAN_WEEK_DAYS, configCosts: {}, sessionCosts: {} });

    expect(row.potential.fullCycleUSD).toBeCloseTo(600, 6);
    expect(row.potential.leverage).toBeCloseTo(600 / weekCost200, 6);
    expect(row.realized.leverage).toBeCloseTo(300 / weekCost200, 6);
    expect(row.utilizationPct).toBeCloseTo(50, 6);
    // Output includes reasoning: 3M at 50% => 6M per full cycle => 6M / weekCost200 per $1.
    expect(row.potential.tokensPerDollar.output).toBeCloseTo(6_000_000 / weekCost200, 2);
    expect(row.potential.tokensPerDollar.cacheRead).toBeCloseTo(180_000_000 / weekCost200, 2);
  });

  test('pools multiple accounts of the same plan weighted by measured quota movement', () => {
    const opts = { effectiveDays: PLAN_WEEK_DAYS, configCosts: {}, sessionCosts: {} };
    const a1 = computeAccountPlanRow({
      authIndex: 'a1', provider: 'codex', plan: 'Pro', cost_usd: 400,
      weekly_pct: 50, measured_cost_usd: 400, measured_tokens: { output: 4_000_000 },
    }, opts);
    const a2 = computeAccountPlanRow({
      authIndex: 'a2', provider: 'codex', plan: 'Pro', cost_usd: 40,
      weekly_pct: 10, measured_cost_usd: 40, measured_tokens: { output: 400_000 },
    }, opts);
    const [group] = groupPlanRows([a1, a2], opts);
    expect(group.accountCount).toBe(2);
    expect(group.totalMonthlyCost).toBe(400);
    expect(group.monthlyCost).toBe(200);
    // Pooled: $440 / 60% = $7.333/pct => $733.33 per full cycle for one $200/mo plan.
    expect(group.potential.fullCycleUSD).toBeCloseTo((440 / 60) * 100, 4);
    expect(group.potential.leverage).toBeCloseTo(((440 / 60) * 100) / weekCost200, 4);
    // Realized: $440 across 2 accounts ($400/mo total => 2 * weekCost200).
    expect(group.realized.leverage).toBeCloseTo(440 / (2 * weekCost200), 4);
  });

  test('ranks paid plans by potential leverage, includes headerless providers by realized leverage, and keeps free and payg ordered cleanly', () => {
    const opts = { effectiveDays: PLAN_WEEK_DAYS, configCosts: {}, sessionCosts: {} };
    const codexPro = computeAccountPlanRow({
      authIndex: 'cx', provider: 'codex', plan: 'Pro', cost_usd: 300,
      weekly_pct: 40, measured_cost_usd: 300,
    }, opts); // potential $750/wk on $46/wk => ~16.3x
    const claudeMax = computeAccountPlanRow({
      authIndex: 'cl', provider: 'claude', plan: 'Max 20x', cost_usd: 250,
      weekly_pct: 50, measured_cost_usd: 250,
    }, opts); // potential $500/wk on $46/wk => ~10.9x
    const antigravityPro = computeAccountPlanRow({
      authIndex: 'ag', provider: 'antigravity', plan: 'Pro', cost_usd: 80,
      weekly_pct: 0, measured_cost_usd: 0,
    }, opts); // no quota headers, realized $80 on $4.6/wk => ~17.4x
    const geminiFree = computeAccountPlanRow({
      authIndex: 'gm', provider: 'gemini', plan: 'Free', cost_usd: 15,
    }, opts);
    const apiKey = computeAccountPlanRow({
      authIndex: 'ak', provider: 'openai', plan: '', isApiKey: true, cost_usd: 50,
    }, opts);

    const ranked = rankPlanRows([claudeMax, apiKey, geminiFree, codexPro, antigravityPro], 'potential');
    expect(ranked.map((r) => r.authIndex)).toEqual(['ag', 'cx', 'cl', 'gm', 'ak']);

    const byRealized = rankPlanRows([claudeMax, codexPro, antigravityPro], 'realized');
    expect(byRealized.map((r) => r.authIndex)).toEqual(['ag', 'cx', 'cl']);
  });

  test('formats plan_costs YAML snippet cleanly', () => {
    expect(planCostsYAML({ 'codex:Pro': 200, 'claude:Max 20x': 180 })).toBe(
      'plan_costs:\n  "claude:Max 20x": 180\n  "codex:Pro": 200',
    );
    expect(planCostsYAML({})).toBe('');
  });
});
