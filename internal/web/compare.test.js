// Run with: bun test internal/web
import { expect, test } from 'bun:test';
import { relativeChange, pointChange, previousPeriod, recordedThrough } from './compare.js';

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
