// Run with: bun test internal/web
import { expect, test } from 'bun:test';
import { relativeChange, pointChange, previousPeriod, recordedThrough, changeDrivers, markerImpact } from './compare.js';

test('a relative change is rounded to whole percents and signed', () => {
  expect(relativeChange(112, 100)).toBe('+12%');
  expect(relativeChange(80, 100)).toBe('-20%');
  expect(relativeChange(100.2, 100)).toBe('no change');
});

test('there is nothing to compare with an empty previous period', () => {
  expect(relativeChange(5, 0)).toBeNull();
  expect(relativeChange(0, 0)).toBeNull();
});

test('a rate changes by percentage points', () => {
  expect(pointChange(0.967, 0.95)).toBe('+1.7 pp');
  expect(pointChange(0.8, 0.95)).toBe('-15.0 pp');
  expect(pointChange(0.95, 0.9502)).toBe('no change');
});

test('a rate without a previous value has nothing to compare', () => {
  expect(pointChange(0.9, null)).toBeNull();
});

test('the previous period has the same length and ends where the current one starts', () => {
  expect(previousPeriod({ from: 1000, to: 1700 })).toEqual({ from: 300, to: 1000 });
});

test('a period is compared only when recording had begun by its start', () => {
  expect(recordedThrough({ from: 300, to: 1000 }, 100)).toBe(true);
  expect(recordedThrough({ from: 300, to: 1000 }, 300)).toBe(true);
  // Recording started inside the period: its totals would read as growth.
  expect(recordedThrough({ from: 300, to: 1000 }, 301)).toBe(false);
  // Nothing recorded at all.
  expect(recordedThrough({ from: 300, to: 1000 }, 0)).toBe(false);
});

test('changeDrivers lists the behavioural levers that moved between periods', () => {
  const cur = { requests: 130, median_input_per_request: 200000, median_output_per_request: 400, processed_tokens: 1000, cache_hit_rate: 0.96, median_requests_per_session: 20 };
  const prev = { requests: 100, median_input_per_request: 160000, median_output_per_request: 400, processed_tokens: 1000, cache_hit_rate: 0.98, median_requests_per_session: 10 };
  expect(changeDrivers(cur, prev)).toEqual([
    { label: 'requests', change: '+30%' },
    { label: 'input/req', change: '+25%' },
    { label: 'cache hit', change: '-2.0 pp' },
    { label: 'req/session', change: '+100%' },
  ]);
});

test('markerImpact weights daily medians by request volume before and after the marker', () => {
  const points = [
    { t: 100, requests: 100, cache_hit_rate: 0.95, median: { input: 100000, output_total: 400, cost_usd: 0.05 } },
    { t: 200, requests: 100, cache_hit_rate: 0.97, median: { input: 150000, output_total: 500, cost_usd: 0.075 } },
  ];
  expect(markerImpact(points, { t: 200 }, 'median')).toEqual({
    beforeReqs: 100,
    afterReqs: 100,
    input: '+50%',
    outputTotal: '+25%',
    costUsd: '+50%',
    cacheHit: '+2.0 pp',
  });
  expect(markerImpact(points, { t: 100 }, 'median')).toBeNull();
});
