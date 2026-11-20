// Run with: bun test internal/web
import { expect, test } from 'bun:test';
import { logDomain, logTicks, nearestPoint } from './scatter.js';

test('the log domain widens to whole decades', () => {
  expect(logDomain(150, 4200)).toEqual([100, 10000]);
  expect(logDomain(100, 100)).toEqual([100, 1000]);
});

test('ticks follow 1, 2, 5 within three decades and decades beyond', () => {
  expect(logTicks(100, 10000)).toEqual([100, 200, 500, 1000, 2000, 5000, 10000]);
  expect(logTicks(10, 1e6)).toEqual([10, 100, 1000, 1e4, 1e5, 1e6]);
});

test('the nearest point within the radius is found, or none', () => {
  const points = [{ x: 0, y: 0 }, { x: 10, y: 0 }];
  expect(nearestPoint(points, 8, 1, 6)).toBe(1);
  expect(nearestPoint(points, 50, 50, 6)).toBe(-1);
});
