// Run with: bun test internal/web
import { expect, test } from 'bun:test';
import { besideColumn } from './tooltip.js';

const viewport = { width: 1280, height: 800 };
const box = { width: 250, height: 240 };

test('opens to the right of the hovered column, so the column marks stay visible', () => {
  const column = { left: 600, right: 660 };
  const at = besideColumn(column, box, viewport, 300);
  expect(at.x).toBe(668);
  expect(at.x).toBeGreaterThanOrEqual(column.right);
});

test('flips to the left of the column near the right edge, never over it', () => {
  const column = { left: 1000, right: 1140 };
  const at = besideColumn(column, box, viewport, 300);
  expect(at.x).toBe(742);
  expect(at.x + box.width).toBeLessThanOrEqual(column.left);
});

test('centers on the pointer and stays inside the viewport vertically', () => {
  const column = { left: 600, right: 660 };
  expect(besideColumn(column, box, viewport, 300).y).toBe(180);
  expect(besideColumn(column, box, viewport, 20).y).toBe(8);
  expect(besideColumn(column, box, viewport, 790).y).toBe(552);
});

test('falls back to the left edge when the column leaves no room on either side', () => {
  const at = besideColumn({ left: 100, right: 1200 }, box, viewport, 300);
  expect(at.x).toBe(8);
});
