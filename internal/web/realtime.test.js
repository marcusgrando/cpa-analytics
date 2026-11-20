// Run with: bun test internal/web
import { expect, test } from 'bun:test';
import { realtimeFigures } from './realtime.js';

const now = { minutes: 15, requests: 30, failed: 0, tokens: 15000, cost_usd: 1.5, cache_hit_rate: 0.8, median_ttft_ms: 500, median_latency_ms: 3000 };
const today = { minutes: 600, requests: 600, failed: 6, tokens: 300000, cost_usd: 30, cache_hit_rate: 0.9, median_ttft_ms: 400, median_latency_ms: 2000 };
const byId = (figures) => Object.fromEntries(figures.map((f) => [f.id, f]));

test('rates are per minute, cost per hour, and change is relative to today', () => {
  const f = byId(realtimeFigures({ now, today }));
  expect(f.requests).toEqual({ id: 'requests', now: 2, today: 1, change: 1, unit: 'relative' });
  expect(f.tokens).toEqual({ id: 'tokens', now: 1000, today: 500, change: 1, unit: 'relative' });
  expect(f.cost).toEqual({ id: 'cost', now: 6, today: 3, change: 1, unit: 'relative' });
  expect(f.ttft.change).toBeCloseTo(0.25);
  expect(f.latency.change).toBeCloseTo(0.5);
  expect(f.failures).toEqual({ id: 'failures', now: 0, today: 0.01, change: -1, unit: 'relative' });
});

test('the cache hit rate changes in percentage points', () => {
  const f = byId(realtimeFigures({ now, today }));
  expect(f.cache.unit).toBe('points');
  expect(f.cache.change).toBeCloseTo(-0.1);
});

test('a quiet window has no cache rate or medians, and nothing to compare', () => {
  const quiet = { minutes: 15, requests: 0, failed: 0, tokens: 0, cost_usd: 0, cache_hit_rate: 0, median_ttft_ms: 0, median_latency_ms: 0 };
  const f = byId(realtimeFigures({ now: quiet, today }));
  expect(f.requests.now).toBe(0);
  expect(f.requests.change).toBe(-1);
  for (const id of ['cache', 'ttft', 'latency']) {
    expect(f[id].now).toBeNull();
    expect(f[id].change).toBeNull();
  }
});

test('without traffic today there is no change to show', () => {
  const empty = { minutes: 600, requests: 0, failed: 0, tokens: 0, cost_usd: 0, cache_hit_rate: 0, median_ttft_ms: 0, median_latency_ms: 0 };
  for (const fig of realtimeFigures({ now, today: empty })) expect(fig.change).toBeNull();
});
