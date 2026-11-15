// Run with: bun test internal/web
import { expect, test } from 'bun:test';
import { providerPalette } from './colors.js';

test('Claude, Codex and Gemini always wear their brand colors', () => {
  const color = providerPalette();
  expect(color('xai')).toBe('var(--provider-next)');
  expect(color('gemini')).toBe('var(--provider-gemini)');
  expect(color('codex')).toBe('var(--provider-codex)');
  expect(color('claude')).toBe('var(--provider-claude)');
});

test('the first other provider takes the spare color and the rest share the overflow gray', () => {
  const color = providerPalette();
  expect(color('kimi')).toBe('var(--provider-next)');
  expect(color('xai')).toBe('var(--other)');
  expect(color('meta')).toBe('var(--other)');
});

test('a provider keeps its color on every call', () => {
  const color = providerPalette();
  color('kimi');
  color('xai');
  expect(color('kimi')).toBe('var(--provider-next)');
});
