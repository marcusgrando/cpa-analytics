// Run with: bun test internal/web
import { expect, test } from 'bun:test';
import { endLabels } from './endlabels.js';

test('labels follow rank order up to the limit', () => {
  const ends = [{ key: 'a', y: 10 }, { key: 'b', y: 40 }, { key: 'c', y: 70 }];
  expect(endLabels(ends, 2, 14)).toEqual(['a', 'b']);
});

test('a label that would crowd a higher-ranked one is dropped, not moved', () => {
  const ends = [{ key: 'a', y: 100 }, { key: 'b', y: 106 }, { key: 'c', y: 90 }, { key: 'd', y: 130 }];
  expect(endLabels(ends, 4, 14)).toEqual(['a', 'd']);
});

test('labels exactly the minimum gap apart both stay', () => {
  expect(endLabels([{ key: 'a', y: 50 }, { key: 'b', y: 64 }], 4, 14)).toEqual(['a', 'b']);
});
