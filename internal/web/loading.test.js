import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const page = readFileSync(new URL('./index.html', import.meta.url), 'utf8');
const loading = page.split('/* ---------- loading ---------- */')[1].split('/* ---------- auto refresh ---------- */')[0];
const wiring = page.slice(page.indexOf("document.getElementById('drift-group').addEventListener"), page.indexOf('let resizeTimer'));
const deferred = () => Promise.withResolvers();

function dashboard() {
  const nodes = new Map();
  const node = (id) => {
    if (!nodes.has(id)) nodes.set(id, {
      hidden: false, textContent: '', value: '', children: [], attrs: {}, listeners: {},
      replaceChildren(...children) { this.children = children; },
      setAttribute(k, v) { this.attrs[k] = v; },
      addEventListener(k, fn) { this.listeners[k] = fn; },
    });
    return nodes.get(id);
  };
  const calls = [], renders = [], classes = new Set();
  const responses = {
    health: {}, summary: { totals: {}, providers: [], data_since: 1 },
    timeseries: {}, drift: { points: [] }, 'limit-burn': {}, 'limit-leftover': {}, affinity: {},
    breakdown: { rows: [] }, pricing: {}, options: { providers: ['codex'], models: ['gpt-5'], clients: ['codex'] },
  };
  const ctx = vm.createContext({
    state: { period: 7, provider: '', model: '', client: '', apiKey: '', view: 'usage', driftGroup: 'response_model', by: 'model' },
    data: {}, autoRefresh: { timer: 0 }, AuthError: class AuthError extends Error {},
    document: {
      body: { classList: { add: (c) => classes.add(c), remove: (c) => classes.delete(c) } },
      getElementById: node, querySelectorAll: () => [],
    },
    clearTimeout() {}, scheduleAutoRefresh() {}, pressed() {},
    rangeParams: () => ({ from: 100, to: 200, tz: -300 }),
    previousPeriod: () => ({ from: 0, to: 100 }), recordedThrough: () => true,
    el: (tag, attrs) => ({ tag, ...attrs }),
    api: async (route, params) => { calls.push({ route, params }); return responses[route]; },
    management: async () => ({ files: [] }),
    renderAll: () => renders.push('all'), renderHealth() {},
    renderDrift: () => renders.push('drift'), renderBreakdown: () => renders.push('breakdown'),
    forgetKey: () => renders.push('forget-key'), showKeyForm: () => renders.push('key-form'),
    loadLimits: async () => renders.push('limits'),
  });
  ctx.filterParams = () => ({ ...ctx.rangeParams(), provider: ctx.state.provider, model: ctx.state.model, client: ctx.state.client, api_key: ctx.state.apiKey });
  vm.runInContext(loading + wiring, ctx);
  return { ctx, calls, renders, node, classes, responses };
}

test('full load uses one lightweight options query in the same batch and keeps the selected value', async () => {
  const { ctx, calls, node } = dashboard();
  ctx.state.model = 'no-longer-in-range';
  const gate = deferred();
  const api = ctx.api;
  ctx.api = (route, params) => route === 'drift' ? gate.promise : api(route, params);
  const pending = ctx.refresh();
  expect(calls.filter((c) => c.route === 'options')).toEqual([{ route: 'options', params: { from: 100, to: 200, tz: -300 } }]);
  gate.resolve({ points: [] });
  await pending;
  expect(calls.filter((c) => c.route === 'breakdown').length).toBe(1);
  expect(calls.filter((c) => c.route === 'summary').length).toBe(2);
  expect(node('f-model').children.map((n) => n.value)).toEqual(['', 'gpt-5', 'no-longer-in-range']);
  expect(node('f-model').value).toBe('no-longer-in-range');
});

test('changing a grouping only fetches that section, using the displayed range and filters', async () => {
  const { ctx, calls, renders } = dashboard();
  ctx.state.provider = 'codex';
  await ctx.refresh();
  calls.length = renders.length = 0;
  ctx.rangeParams = () => ({ from: 300, to: 400, tz: -300 });
  ctx.state.driftGroup = 'client_version';
  await ctx.refreshGroup('drift');
  ctx.state.by = 'account';
  await ctx.refreshGroup('breakdown');
  expect(calls).toEqual([
    { route: 'drift', params: { from: 100, to: 200, tz: -300, provider: 'codex', model: '', client: '', api_key: '', group: 'client_version' } },
    { route: 'breakdown', params: { from: 100, to: 200, tz: -300, provider: 'codex', model: '', client: '', api_key: '', by: 'account' } },
  ]);
  expect(renders).toEqual(['drift', 'breakdown']);
});

test('the drift grouping control uses the partial load', async () => {
  const { ctx, node, calls } = dashboard();
  await ctx.refresh();
  calls.length = 0;
  node('drift-group').listeners.change({ target: { value: 'client_version' } });
  await Promise.resolve();
  expect(calls.map((c) => c.route)).toEqual(['drift']);
});

test('simultaneous changes to different sections both render', async () => {
  const { ctx, renders } = dashboard();
  await ctx.refresh();
  renders.length = 0;
  const drift = deferred(), breakdown = deferred();
  ctx.api = (route) => route === 'drift' ? drift.promise : breakdown.promise;
  const a = ctx.refreshGroup('drift'), b = ctx.refreshGroup('breakdown');
  breakdown.resolve({ by: 'account', rows: [] });
  await b;
  drift.resolve({ group: 'client_version', points: [] });
  await a;
  expect(renders).toEqual(['breakdown', 'drift']);
});

test('an older group response cannot overwrite the latest group', async () => {
  const { ctx, renders } = dashboard();
  await ctx.refresh();
  renders.length = 0;
  const old = deferred(), latest = deferred();
  ctx.api = (_route, p) => p.group === 'client_version' ? old.promise : latest.promise;
  ctx.state.driftGroup = 'client_version';
  const a = ctx.refreshGroup('drift');
  ctx.state.driftGroup = 'reasoning_effort';
  const b = ctx.refreshGroup('drift');
  latest.resolve({ group: 'reasoning_effort', points: [] });
  await b;
  old.resolve({ group: 'client_version', points: [] });
  await a;
  expect(ctx.data.drift.group).toBe('reasoning_effort');
  expect(renders).toEqual(['drift']);
});

test('a full refresh supersedes partial data and authentication errors', async () => {
  const { ctx, renders, node } = dashboard();
  await ctx.refresh();
  const old = deferred(), api = ctx.api;
  ctx.api = () => old.promise;
  const pending = ctx.refreshGroup('drift');
  ctx.api = api;
  ctx.state.provider = 'claude';
  await ctx.refresh();
  renders.length = 0;
  old.reject(new ctx.AuthError('expired old request'));
  await pending;
  expect(renders).toEqual([]);
  expect(node('load-error').textContent).toBe('');
});

test('rapid filter changes finish the active batch and only run the latest pending selection', async () => {
  const { ctx, calls, renders, classes } = dashboard();
  const first = deferred(), api = ctx.api;
  let blocked = false;
  ctx.api = (route, params) => {
    if (route === 'drift' && !blocked) { blocked = true; return first.promise; }
    return api(route, params);
  };
  const a = ctx.refresh();
  ctx.state.provider = 'codex';
  const b = ctx.refresh();
  ctx.state.provider = 'claude';
  const c = ctx.refresh();
  expect(calls.filter((c) => c.route === 'timeseries').length).toBe(1);
  first.resolve({ points: [] });
  await Promise.all([a, b, c]);
  expect(calls.filter((c) => c.route === 'timeseries').map((c) => c.params.provider)).toEqual(['', 'claude']);
  expect(renders).toEqual(['all']);
  expect(classes.size).toBe(0);
});

test('a late credentials response cannot paint an obsolete full load', async () => {
  const { ctx, renders } = dashboard();
  const credentials = deferred();
  ctx.management = () => credentials.promise;
  const first = ctx.refresh();
  await new Promise(setImmediate);
  ctx.state.provider = 'claude';
  const second = ctx.refresh();
  credentials.resolve({ files: [] });
  await Promise.all([first, second]);
  expect(renders).toEqual(['all']);
});

test('a failed filter change cannot reuse the previous selection for a partial load', async () => {
  const { ctx, calls } = dashboard();
  await ctx.refresh();
  const api = ctx.api;
  ctx.api = async () => { throw new Error('offline'); };
  ctx.state.provider = 'claude';
  await ctx.refresh();
  ctx.api = api;
  calls.length = 0;
  await ctx.refreshGroup('breakdown');
  expect(calls.find((c) => c.route === 'timeseries')?.params.provider).toBe('claude');
});

test('a failing batch still settles before the next selection starts', async () => {
  const { ctx, calls } = dashboard();
  const gate = deferred(), api = ctx.api;
  ctx.api = (route, params) => {
    if (route === 'summary') return Promise.reject(new Error('storage unavailable'));
    if (route === 'drift') return gate.promise;
    return api(route, params);
  };
  const first = ctx.refresh();
  await Promise.resolve();
  ctx.state.provider = 'claude';
  ctx.api = api;
  const second = ctx.refresh();
  await Promise.resolve();
  expect(calls.filter((c) => c.route === 'timeseries').length).toBe(1);
  gate.resolve({ points: [] });
  await Promise.all([first, second]);
  expect(calls.filter((c) => c.route === 'timeseries').length).toBe(2);
});

test('current partial failures stay visible and clear the busy state', async () => {
  const { ctx, node } = dashboard();
  await ctx.refresh();
  ctx.api = async () => { throw new Error('storage unavailable'); };
  await ctx.refreshGroup('breakdown');
  expect(node('load-error').textContent).toContain('storage unavailable');
  expect(node('breakdown').attrs['aria-busy']).toBe('false');
});
