// Run with: bun test internal/web
import { expect, test } from 'bun:test';
import { sparkline, sumBuckets } from './sparkline.js';

test('sumBuckets adds every group of a bucket and fills empty buckets with zeros', () => {
  const points = [
    { t: 1, key: 'codex', requests: 2, failed: 1, cost_usd: 0.5 },
    { t: 1, key: 'claude', requests: 3, failed: 0, cost_usd: 1.5 },
    { t: 3, key: 'codex', requests: 1, failed: 2, cost_usd: 0.25 },
  ];
  expect(sumBuckets(points, [1, 2, 3])).toEqual([
    { requests: 5, failed: 1, cost_usd: 2 },
    {},
    { requests: 1, failed: 2, cost_usd: 0.25 },
  ]);
});

test('sparkline spans the full width and scales from zero to the largest value', () => {
  expect(sparkline([0, 5, 10], 24, 3)).toEqual({ d: 'M0 21L50 12L100 3', end: { x: 100, y: 3 } });
});

test('a bucket with nothing to measure breaks the line and the end is the last measured bucket', () => {
  expect(sparkline([1, null, 1, null], 24, 3)).toEqual({ d: 'M0 3M66.67 3', end: { x: 66.67, y: 3 } });
});

test('all zeros draw a flat line on the baseline', () => {
  expect(sparkline([0, 0], 24, 3)).toEqual({ d: 'M0 21L100 21', end: { x: 100, y: 21 } });
});

test('fewer than two buckets or no measured value draws nothing', () => {
  expect(sparkline([5], 24, 3)).toBeNull();
  expect(sparkline([null, null], 24, 3)).toBeNull();
});
