import { expect, test } from 'bun:test';
import { driftSeries } from './drift.js';

const pt = (t, key, requests, value) => ({
  t, key, requests, median: { input: value, ttft_ms: value }, p90: { input: value * 2 },
  cache_hit_rate: value, input_requests: requests, ttft_requests: requests,
});
const series = (points, metric = 'input', stat = 'median', minimum = 5) => driftSeries(points, metric, stat, minimum);

test('the first and last qualifying buckets set change and sample, even with a one-call spike', () => {
  const points = [pt(3, 'a', 6, 50), pt(1, 'a', 5, 100), pt(0, 'a', 1, 5000), pt(2, 'a', 1, 10000)];
  const [a] = series(points, 'input', 'p90');
  expect(a.points.map(p => [p.t, p.value, p.change, p.n])).toEqual([[1, 200, 0, 5], [3, 100, -50, 6]]);
  expect(a.change).toBe(-50);
  expect(series(points, 'input', 'median', 1)[0].points).toHaveLength(4);
});

test('rank by volume among groups with measurements, with ties in key order', () => {
  const points = [pt(1, 'b', 9, 100), pt(1, 'a', 9, 0), pt(2, 'a', 7, 100), pt(1, 'c', 9, 1), pt(1, 'tiny', 2, 1)];
  const ranked = series(points);
  expect(ranked.map(s => s.key)).toEqual(['a', 'b', 'c']);
  expect(ranked.map(s => s.change)).toEqual([null, null, null]);
  expect(ranked[0].points.map(p => p.change)).toEqual([null, null]);
});

test('cache rate compares in percentage points even from zero, and requires measured input', () => {
  const [s] = series([pt(1, 'a', 5, 0), pt(2, 'a', 5, 0.25), {...pt(3, 'a', 5, 0), input_requests: 0}], 'cache_hit_rate', 'p90');
  expect(s.points.map(p => p.change)).toEqual([0, 25]);
  expect(s.change).toBe(25);
});

test('time metrics use the number of measured calls for filtering and tooltips', () => {
  const points = [{...pt(1, 'a', 1000, 700), ttft_requests: 1}, {...pt(2, 'a', 10, 30), ttft_requests: 6}];
  expect(series(points, 'ttft_ms')[0].points.map(p => p.n)).toEqual([6]);
});
