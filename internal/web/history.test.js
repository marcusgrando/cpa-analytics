// Run with: bun test internal/web
import { describe, expect, test } from 'bun:test';
import { readingAt, poolHistory, burnRate, projectPool } from './limits.js';

const HOUR = 3600000;
const DAY = 24 * HOUR;
const now = Date.parse('2026-11-15T12:00:00Z');
const reset = now + 2 * DAY;
const pt = (t, pct, r = reset) => ({ t, weekly_pct: pct, weekly_reset: r });

describe('readings', () => {
  const points = [pt(now - 10 * HOUR, 20, now - 2 * HOUR), pt(now - 1 * HOUR, 5)];
  test('an account keeps its last reading until the next one', () => {
    expect(readingAt(points, now - 5 * HOUR)).toBe(20);
    expect(readingAt(points, now)).toBe(5);
  });
  test('a reading counts as zero once its reset has passed', () => {
    expect(readingAt(points, now - 90 * 60000)).toBe(0);
  });
  test('before the first reading the account is unknown', () => {
    expect(readingAt(points, now - 11 * HOUR)).toBeNull();
  });
  test('a reading without a reset never expires', () => {
    expect(readingAt([pt(now - 9 * DAY, 30, 0)], now)).toBe(30);
  });
});

describe('pool history', () => {
  const accounts = [
    { account: 'a', weight: 20, points: [pt(now - 4 * HOUR, 50)] },
    { account: 'b', weight: 5, points: [pt(now - 2 * HOUR, 10)] },
  ];
  test('pools the accounts known at each time by plan weight', () => {
    const got = poolHistory(accounts, [now - 5 * HOUR, now - 3 * HOUR, now]);
    expect(got).toEqual([
      { t: now - 5 * HOUR, used: null },
      { t: now - 3 * HOUR, used: 50 },
      { t: now, used: (20 * 50 + 5 * 10) / 25 },
    ]);
  });
  test('falls back to equal weights when a plan is unknown, like the live pool', () => {
    const got = poolHistory([accounts[0], { ...accounts[1], weight: null }], [now]);
    expect(got[0].used).toBe(30);
  });
});

describe('burn rate', () => {
  test('is the points gained per hour over the window', () => {
    const points = [pt(now - 5 * HOUR, 10), pt(now - 2 * HOUR, 16), pt(now - 30 * 60000, 22)];
    // At now - 3 h the account read 10. It reads 22 at now.
    expect(burnRate(points, now, 3 * HOUR)).toBeCloseTo(4);
  });
  test('measures from the first reading of a cycle that began inside the window', () => {
    const points = [pt(now - 5 * HOUR, 90, now - 2 * HOUR), pt(now - 1 * HOUR, 4, now + 7 * DAY), pt(now, 6, now + 7 * DAY)];
    expect(burnRate(points, now, 3 * HOUR)).toBeCloseTo(2);
  });
  test('is zero when the account has not been used since its reset', () => {
    expect(burnRate([pt(now - 5 * HOUR, 40, now - HOUR)], now, 3 * HOUR)).toBe(0);
  });
  test('is unknown with a single reading and nothing before the window', () => {
    expect(burnRate([pt(now - HOUR, 40)], now, 3 * HOUR)).toBeNull();
    expect(burnRate([], now, 3 * HOUR)).toBeNull();
  });
  test('never goes negative when a reset refills the percent', () => {
    expect(burnRate([pt(now - 5 * HOUR, 60), pt(now, 10)], now, 3 * HOUR)).toBe(0);
  });
});

describe('projection', () => {
  const week = 7 * DAY;
  test('each account keeps its rate, stops at the limit and starts over at its reset', () => {
    const members = [
      { account: 'a', weight: 1, used: 90, resetsAt: now + 20 * HOUR, lengthMs: week, rate: 1 },
      { account: 'b', weight: 1, used: 0, resetsAt: now + 3 * DAY, lengthMs: week, rate: 0.5 },
    ];
    const got = projectPool(members, now, 2 * DAY, HOUR);
    const at = (h) => got.points.find((p) => p.t === now + h * HOUR).used;
    expect(at(0)).toBe(45);
    // a reaches its limit after 10 h, b has gained 5 points.
    expect(at(10)).toBe((100 + 5) / 2);
    // a resets at 20 h and has gained 4 points by 24 h.
    expect(at(24)).toBe((4 + 12) / 2);
    expect(got.firstFull).toEqual({ account: 'a', at: now + 10 * HOUR });
    expect(got.poolFull).toBeNull();
  });
  test('reports when the whole pool reaches its limit', () => {
    const members = [
      { account: 'a', weight: 3, used: 50, resetsAt: now + 6 * DAY, lengthMs: week, rate: 10 },
      { account: 'b', weight: 1, used: 80, resetsAt: now + 6 * DAY, lengthMs: week, rate: 5 },
    ];
    const got = projectPool(members, now, DAY, HOUR);
    expect(got.firstFull).toEqual({ account: 'b', at: now + 4 * HOUR });
    expect(got.poolFull).toBe(now + 5 * HOUR);
  });
  test('an account with an unknown rate stays where it is', () => {
    const got = projectPool([{ account: 'a', weight: 1, used: 30, resetsAt: now + 6 * DAY, lengthMs: week, rate: null }], now, DAY, HOUR);
    expect(got.points.every((p) => p.used === 30)).toBe(true);
    expect(got.firstFull).toBeNull();
  });
  test('an account already at its limit is the first one full, now', () => {
    const got = projectPool([{ account: 'a', weight: 1, used: 100, resetsAt: now + DAY, lengthMs: week, rate: 0 }], now, HOUR, HOUR);
    expect(got.firstFull).toEqual({ account: 'a', at: now });
  });
});
