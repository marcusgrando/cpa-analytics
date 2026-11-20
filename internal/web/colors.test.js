// Run with: bun test internal/web
import { describe, expect, test } from 'bun:test';
import { accountStyles, providerPalette } from './colors.js';

test('known CPA providers always wear their assigned colors', () => {
  const color = providerPalette();
  expect(color('xai')).toBe('var(--provider-xai)');
  expect(color('antigravity')).toBe('var(--provider-antigravity)');
  expect(color('kimi')).toBe('var(--provider-kimi)');
  expect(color('meta')).toBe('var(--provider-meta)');
  expect(color('devin')).toBe('var(--provider-devin)');
  expect(color('gemini')).toBe('var(--provider-gemini)');
  expect(color('gemini-cli')).toBe('var(--provider-gemini)');
  expect(color('codex')).toBe('var(--provider-codex)');
  expect(color('claude')).toBe('var(--provider-claude)');
});

test('the first unknown provider takes the spare color and the rest share the overflow gray', () => {
  const color = providerPalette();
  expect(color('openrouter')).toBe('var(--provider-next)');
  expect(color('deepseek')).toBe('var(--other)');
  expect(color('qwen')).toBe('var(--other)');
});

test('a provider keeps its color on every call', () => {
  const color = providerPalette();
  color('openrouter');
  color('deepseek');
  expect(color('openrouter')).toBe('var(--provider-next)');
});

describe('account styles', () => {
  test('an account wears its provider color, and each further account of that provider a new dash', () => {
    const styles = accountStyles([
      { account: 'b', provider: 'claude' },
      { account: 'x', provider: 'codex' },
      { account: 'a', provider: 'claude' },
    ], providerPalette());
    expect(styles.get('a')).toEqual({ color: 'var(--provider-claude)', dash: '' });
    expect(styles.get('b')).toEqual({ color: 'var(--provider-claude)', dash: '6 4' });
    expect(styles.get('x')).toEqual({ color: 'var(--provider-codex)', dash: '' });
  });

  test('accounts past the last dash share the overflow gray instead of repeating a dash', () => {
    const accounts = ['a', 'b', 'c', 'd', 'e'].map((account) => ({ account, provider: 'claude' }));
    const styles = accountStyles(accounts, providerPalette());
    expect(styles.get('d').dash).toBe('10 3 2 3');
    expect(styles.get('e')).toEqual({ color: 'var(--other)', dash: '' });
  });
});
