import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import { mapLimit } from './limits.js';
import { previousPeriod, recordedThrough } from './compare.js';

const page = readFileSync(new URL('./index.html', import.meta.url), 'utf8');
const loading = page.split('/* ---------- loading ---------- */')[1].split('/* ---------- auto refresh ---------- */')[0];
const wiring = page.slice(page.indexOf('for (const section of DETAIL_SECTIONS) {'), page.indexOf('let resizeTimer'));
const deferred = () => Promise.withResolvers();

function dashboard() {
  const nodes = new Map();
  const node = (id) => {
    if (!nodes.has(id)) nodes.set(id, {
      hidden: false, open: false, textContent: '', value: '', children: [], attrs: {}, listeners: {},
      replaceChildren(...children) { this.children = children; },
      setAttribute(k, v) { this.attrs[k] = v; },
      addEventListener(k, fn) { this.listeners[k] = fn; },
      querySelector(selector) { return node(id + ' ' + selector); },
    });
    return nodes.get(id);
  };
  const calls = [], credentialCalls = [], renders = [], painted = [], classes = new Set();
  const responses = {
    health: {}, summary: { totals: {}, providers: [], data_since: 1 },
    timeseries: {}, drift: { points: [] }, requests: { total: 0, calls: [] }, 'limit-burn': {}, 'limit-leftover': {}, affinity: {},
    breakdown: { rows: [] }, errors: { total: 0, rows: [] }, pricing: {},
    options: { providers: ['codex'], models: ['gpt-5'], clients: ['codex'], accounts: ['acc1'] },
  };
  const ctx = vm.createContext({
    state: { period: '7d', provider: '', model: '', client: '', account: '', apiKey: '', view: 'usage', driftGroup: 'response_model', by: 'model', reqSort: 'recent', reqOutcome: 'all', reqOffset: 0 },
    REQUESTS_PAGE: 50,
    data: {}, autoRefresh: { timer: 0 }, AuthError: class AuthError extends Error {},
    document: {
      body: { classList: { add: (c) => classes.add(c), remove: (c) => classes.delete(c) } },
      getElementById: node, querySelectorAll: () => [],
    },
    clearTimeout() {}, scheduleAutoRefresh() {}, pressed() {}, mapLimit, reqRefreshSeconds: () => 3,
    rangeParams: () => ({ from: 100, to: 200, tz: -300 }),
    previousPeriod: () => ({ from: 0, to: 100 }), recordedThrough: () => true,
    el: (tag, attrs) => ({ tag, ...attrs }),
    api: async (route, params) => { calls.push({ route, params }); return responses[route]; },
    management: async () => { credentialCalls.push('credentials'); return { files: [] }; },
    renderHealth() {},
    renderTotals: () => { renders.push('totals'); painted.push(ctx.data.params?.provider); },
    renderDaily: () => renders.push('daily'), renderModelDetail() {},
    renderBurn() {}, renderLeftover() {}, renderAffinity() {}, renderQuality() {}, renderCostWarning() {},
    renderDrift: () => renders.push('drift'), renderBreakdown: () => renders.push('breakdown'),
    renderLatency() {}, renderRequests: () => renders.push('requests'),
    forgetKey: () => { ctx.data = {}; renders.push('forget-key'); }, showKeyForm: () => renders.push('key-form'),
    loadLimits: async () => renders.push('limits'),
  });
  ctx.filterParams = () => ({ ...ctx.rangeParams(), provider: ctx.state.provider, model: ctx.state.model, client: ctx.state.client, account: ctx.state.account, api_key: ctx.state.apiKey });
  vm.runInContext(loading + wiring, ctx);
  return { ctx, calls, credentialCalls, renders, painted, node, classes, responses };
}

test('opening usage does not query collapsed analyses or an incomplete previous period', async () => {
  const { ctx, calls, node } = dashboard();
  ctx.recordedThrough = () => false;
  await ctx.refresh();
  expect(calls.map((c) => c.route)).toEqual(['health', 'summary', 'timeseries', 'options', 'drift', 'breakdown', 'limit-burn']);
  for (const section of ['latency', 'affinity', 'leftover', 'quality']) {
    expect(node(section).open).toBe(false);
    expect(node(section).attrs['aria-busy']).toBe('false');
  }
});

test('each disclosure loads on first expansion and reuses its result until refresh', async () => {
  const { ctx, calls, node } = dashboard();
  await ctx.refresh();
  calls.length = 0;
  for (const section of ['affinity', 'leftover', 'quality']) {
    node(section).open = true;
    await ctx.refreshDetail(section);
    node(section).open = false;
    await ctx.refreshDetail(section);
    node(section).open = true;
    await ctx.refreshDetail(section);
  }
  expect(calls.map((c) => c.route)).toEqual(['affinity', 'limit-leftover', 'pricing']);
  calls.length = 0;
  await ctx.refresh();
  expect(calls.map((c) => c.route)).toEqual(expect.arrayContaining(['affinity', 'limit-leftover', 'pricing']));
});

test('filter navigation reuses options, credentials and account limits within the displayed range', async () => {
  const { ctx, calls, credentialCalls, node } = dashboard();
  node('leftover').open = true;
  await ctx.refresh();
  calls.length = credentialCalls.length = 0;
  ctx.rangeParams = () => ({ from: 300, to: 400, tz: -300 });
  ctx.state.model = 'gpt-5';
  await ctx.refreshFilters();
  ctx.state.client = 'codex';
  await ctx.refreshFilters();
  ctx.state.apiKey = 'abcdef123456';
  await ctx.refreshFilters();
  expect(calls.every((c) => ['summary', 'timeseries', 'drift', 'breakdown'].includes(c.route))).toBe(true);
  expect(credentialCalls).toEqual([]);
  expect(ctx.data.params.from).toBe(100);
  expect(ctx.data.params.to).toBe(200);
  calls.length = 0;
  ctx.state.provider = 'claude';
  await ctx.refreshFilters();
  expect(calls.filter((c) => c.route.startsWith('limit-')).map((c) => c.route)).toEqual(['limit-burn', 'limit-leftover']);
  expect(calls.some((c) => c.route === 'options')).toBe(false);
});

test('refresh and period changes renew metadata and limit results', async () => {
  const { ctx, calls, credentialCalls } = dashboard();
  await ctx.refresh();
  for (const changePeriod of [false, true]) {
    calls.length = credentialCalls.length = 0;
    ctx.rangeParams = () => ({ from: 300, to: 400, tz: -300 });
    if (changePeriod) { ctx.state.period = '30d'; await ctx.refreshFilters(); }
    else await ctx.refresh();
    expect(calls.map((c) => c.route)).toEqual(expect.arrayContaining(['health', 'options', 'limit-burn']));
    expect(credentialCalls.length).toBe(1);
    expect(ctx.data.params.from).toBe(300);
  }
});

test('a collapsed detail is invalidated by refresh and uses the new filters when opened', async () => {
  const { ctx, calls, node } = dashboard();
  await ctx.refresh();
  node('affinity').open = true;
  await ctx.refreshDetail('affinity');
  node('affinity').open = false;
  ctx.state.model = 'new-model';
  await ctx.refreshFilters();
  calls.length = 0;
  node('affinity').open = true;
  await ctx.refreshDetail('affinity');
  expect(calls).toEqual([{ route: 'affinity', params: ctx.data.params }]);
  expect(calls[0].params.model).toBe('new-model');
});

test('expanding during a full load waits for it and does not duplicate the detail query', async () => {
  const { ctx, calls, node } = dashboard();
  const gate = deferred(), api = ctx.api;
  ctx.api = (route, params) => route === 'timeseries' ? gate.promise : api(route, params);
  const full = ctx.refresh();
  node('affinity').open = true;
  const detail = ctx.refreshDetail('affinity');
  expect(calls.some((c) => c.route === 'affinity')).toBe(false);
  gate.resolve({});
  await Promise.all([full, detail]);
  expect(calls.filter((c) => c.route === 'affinity').length).toBe(1);
});

test('a stale expanded detail cannot paint or overlap the next full load', async () => {
  const { ctx, node, calls } = dashboard();
  await ctx.refresh();
  const gate = deferred(), api = ctx.api;
  ctx.api = (route, params) => route === 'affinity' ? gate.promise : api(route, params);
  node('affinity').open = true;
  const detail = ctx.refreshDetail('affinity');
  await new Promise(setImmediate);
  calls.length = 0;
  node('affinity').open = false;
  ctx.state.model = 'new-model';
  const full = ctx.refreshFilters();
  await new Promise(setImmediate);
  expect(calls).toEqual([]);
  gate.resolve({ stale: true });
  await Promise.all([full, detail]);
  expect(ctx.data.affinity).toBeUndefined();
  expect(ctx.data.params.model).toBe('new-model');
});

test('detail errors keep the overview usable and can retry on reopening', async () => {
  const { ctx, node, renders } = dashboard();
  await ctx.refresh();
  const api = ctx.api;
  ctx.api = async () => { throw new Error('offline'); };
  node('quality').open = true;
  await ctx.refreshDetail('quality');
  expect(node('quality .section-status').textContent).toContain('Could not load');
  expect(node('app').hidden).toBe(false);
  expect(renders).toContain('totals');
  ctx.api = api;
  await ctx.refreshDetail('quality');
  expect(node('quality .section-status').hidden).toBe(true);
  expect(node('load-error').textContent).toBe('');
});

test('credential authentication errors stop loading and discard reusable data', async () => {
  const { ctx, renders } = dashboard();
  ctx.management = async () => { throw new ctx.AuthError('key expired'); };
  await ctx.refresh();
  expect(renders).toContain('key-form');
  expect(ctx.data).toEqual({});
});

test('forgetting a key clears usage metadata, account profiles and visible health', () => {
  const { ctx, node } = dashboard();
  ctx.data = { credentials: [{ label: 'previous account' }], options: { clients: ['previous client'] } };
  ctx.limitsData = { results: ['previous limit'] };
  ctx.claudeProfiles = new Map([['previous account', {}]]);
  ctx.antigravityPlans = new Map([['previous account', {}]]);
  node('health').children = ['previous error'];
  vm.runInContext(page.slice(page.indexOf('function forgetKey()'), page.indexOf('function showKeyForm(')), ctx);
  ctx.forgetKey();
  expect(ctx.data).toEqual({});
  expect(ctx.limitsData).toBeNull();
  expect(ctx.claudeProfiles.size).toBe(0);
  expect(ctx.antigravityPlans.size).toBe(0);
  expect(node('health').children).toEqual([]);
});

test('the previous period is queried only when recording covers its entire range', async () => {
  for (const [since, count] of [[50, 2], [150, 1], [0, 1]]) {
    const { ctx, calls, responses } = dashboard();
    ctx.rangeParams = () => ({ from: 200, to: 300, tz: 0 });
    ctx.previousPeriod = previousPeriod;
    ctx.recordedThrough = recordedThrough;
    responses.summary.data_since = since;
    await ctx.refresh();
    expect(calls.filter((c) => c.route === 'summary').length).toBe(count);
  }
});

test('toggle controls fetch their own detail and rapid expansions stay serialized', async () => {
  const { ctx, calls, node } = dashboard();
  await ctx.refresh();
  calls.length = 0;
  const gate = deferred(), api = ctx.api;
  ctx.api = (route, params) => {
    if (route === 'affinity') { calls.push({ route, params }); return gate.promise; }
    return api(route, params);
  };
  node('affinity').open = true;
  const a = node('affinity').listeners.toggle();
  const duplicate = node('affinity').listeners.toggle();
  node('quality').open = true;
  const b = node('quality').listeners.toggle();
  await new Promise(setImmediate);
  expect(calls.map((c) => c.route)).toEqual(['affinity']);
  gate.resolve({});
  await Promise.all([a, duplicate, b]);
  expect(calls.map((c) => c.route)).toEqual(['affinity', 'pricing']);
});

test('a failed primary load cannot start a detail with obsolete filters', async () => {
  const { ctx, calls, node } = dashboard();
  await ctx.refresh();
  const api = ctx.api;
  ctx.api = async () => { throw new Error('offline'); };
  ctx.state.client = 'new-client';
  await ctx.refreshFilters();
  calls.length = 0;
  ctx.api = api;
  node('affinity').open = true;
  await ctx.refreshDetail('affinity');
  expect(calls).toEqual([]);
});

test('a queued full refresh still renews metadata after a later filter change', async () => {
  const { ctx, calls } = dashboard();
  await ctx.refresh();
  const gate = deferred(), api = ctx.api;
  ctx.api = (route, params) => route === 'timeseries' ? gate.promise : api(route, params);
  const first = ctx.refreshFilters();
  await new Promise(setImmediate);
  ctx.api = api;
  const full = ctx.refresh();
  ctx.state.model = 'gpt-5';
  const last = ctx.refreshFilters();
  calls.length = 0;
  gate.resolve({});
  await Promise.all([first, full, last]);
  expect(calls.map((c) => c.route)).toContain('options');
});

test('filter changes cannot turn an active full refresh into reuse of an older snapshot', async () => {
  const { ctx, calls } = dashboard();
  await ctx.refresh();
  const gate = deferred(), api = ctx.api;
  ctx.rangeParams = () => ({ from: 300, to: 400, tz: -300 });
  ctx.api = (route, params) => route === 'timeseries' ? gate.promise : api(route, params);
  const full = ctx.refresh();
  await new Promise(setImmediate);
  ctx.api = api;
  ctx.state.model = 'gpt-5';
  const filter = ctx.refreshFilters();
  calls.length = 0;
  gate.resolve({});
  await Promise.all([full, filter]);
  expect(ctx.data.params.to).toBe(400);
  expect(calls.map((c) => c.route)).toContain('options');
});

test('chart sections are visible before measuring them on loads and filter reuse', async () => {
  const { ctx, node } = dashboard();
  const visible = [];
  ctx.renderDrift = () => visible.push(!node('drift .section-content').hidden);
  ctx.renderBurn = () => visible.push(!node('burn .section-content').hidden);
  await ctx.refresh();
  ctx.state.model = 'gpt-5';
  await ctx.refreshFilters();
  expect(visible).toEqual([true, true, true, true]);
});

test('health shows only collection or pricing failures, while unpriced tokens remain visible without pricing details', () => {
  const { ctx, node } = dashboard();
  ctx.fmtCompact = ctx.fmtInt = String;
  vm.runInContext(page.slice(page.indexOf('function renderCostWarning()'), page.indexOf('/* ---------- limits ---------- */')), ctx);
  ctx.renderHealth({ writer: { written: 10, dropped: 0 }, trace_hits: 9, trace_misses: 2, pricing: {} });
  expect(node('health').children).toEqual([]);
  ctx.renderHealth({ storage_error: 'disk full', writer: { dropped: 3, last_error: 'write failed' }, pricing: { last_error: 'offline' } });
  expect(node('health').children.length).toBe(4);
  ctx.data = { summary: { totals: { cost_quality: { unpriced_tokens: 42 } } } };
  ctx.renderCostWarning();
  expect(node('cost-warning').hidden).toBe(false);
  expect(node('cost-warning').textContent).toContain('42 tokens');
  ctx.data.summary.totals.cost_quality.unpriced_tokens = 0;
  ctx.renderCostWarning();
  expect(node('cost-warning').hidden).toBe(true);
});

test('full load uses one lightweight options query and keeps the selected value', async () => {
  const { ctx, calls, node } = dashboard();
  ctx.state.model = 'no-longer-in-range';
  const gate = deferred();
  const api = ctx.api;
  ctx.api = (route, params) => route === 'drift' ? gate.promise : api(route, params);
  const pending = ctx.refresh();
  await new Promise(setImmediate);
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
    { route: 'drift', params: { from: 100, to: 200, tz: -300, provider: 'codex', model: '', client: '', account: '', api_key: '', group: 'client_version', bucket: 'day' } },
    { route: 'breakdown', params: { from: 100, to: 200, tz: -300, provider: 'codex', model: '', client: '', account: '', api_key: '', by: 'account' } },
  ]);
  expect(renders).toEqual(['drift', 'breakdown']);
});

test('switching breakdown to errors queries the errors route using the displayed range', async () => {
  const { ctx, calls, renders } = dashboard();
  await ctx.refresh();
  calls.length = renders.length = 0;
  ctx.state.by = 'errors';
  await ctx.refreshGroup('breakdown');
  expect(calls).toEqual([
    { route: 'errors', params: { from: 100, to: 200, tz: -300, provider: '', model: '', client: '', account: '', api_key: '' } },
  ]);
  expect(renders).toEqual(['breakdown']);
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
  const { ctx, calls, painted, classes } = dashboard();
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
  expect([...new Set(painted)]).toEqual(['claude']);
  expect(classes.size).toBe(0);
});

test('a late credentials response cannot paint an obsolete full load', async () => {
  const { ctx, painted } = dashboard();
  const credentials = deferred();
  ctx.management = () => credentials.promise;
  const first = ctx.refresh();
  await new Promise(setImmediate);
  painted.length = 0;
  ctx.state.provider = 'claude';
  const second = ctx.refresh();
  credentials.resolve({ files: [] });
  await Promise.all([first, second]);
  expect([...new Set(painted)]).toEqual(['claude']);
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
    if (route === 'timeseries') { calls.push({ route, params }); return gate.promise; }
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

test('summary and daily usage render before slow details or credentials finish', async () => {
  const { ctx, renders, node, classes } = dashboard();
  const details = deferred(), credentials = deferred(), api = ctx.api;
  ctx.api = (route, params) => ['summary', 'timeseries', 'health'].includes(route) ? api(route, params) : details.promise;
  ctx.management = () => credentials.promise;
  node('app').hidden = true;
  const pending = ctx.refresh();
  await new Promise(setImmediate);
  expect(node('app').hidden).toBe(false);
  expect(renders).toContain('totals');
  expect(renders).toContain('daily');
  expect(renders).not.toContain('drift');
  expect(classes.has('loading')).toBe(false);
  expect(node('drift').attrs['aria-busy']).toBe('true');
  expect(node('drift .section-content').hidden).toBe(true);
  details.resolve({ providers: [], models: [], clients: [] });
  credentials.resolve({ files: [] });
  await pending;
});

test('only two data queries run at once and details do not compete with the first paint', async () => {
  const { ctx, calls } = dashboard();
  const primary = deferred(), api = ctx.api;
  let active = 0, peak = 0;
  ctx.api = async (route, params) => {
    if (route === 'health') return api(route, params);
    active++;
    peak = Math.max(peak, active);
    try {
      if (route === 'summary' || route === 'timeseries') await primary.promise;
      else await new Promise(setImmediate);
      return await api(route, params);
    } finally { active--; }
  };
  const pending = ctx.refresh();
  await new Promise(setImmediate);
  expect(active).toBe(2);
  expect(calls.map((c) => c.route)).toEqual(['health']);
  primary.resolve();
  await pending;
  expect(peak).toBe(2);
});

test('one failed detail keeps the primary data and other details visible', async () => {
  const { ctx, renders, node } = dashboard();
  const api = ctx.api;
  ctx.api = (route, params) => route === 'drift' ? Promise.reject(new Error('storage unavailable')) : api(route, params);
  await ctx.refresh();
  expect(renders).toContain('totals');
  expect(renders).toContain('breakdown');
  expect(node('load-error').textContent).toContain('storage unavailable');
  expect(node('drift').attrs['aria-busy']).toBe('false');
  expect(node('drift .section-status').textContent).toContain('Could not load');
  expect(node('drift .section-content').hidden).toBe(true);
  expect(node('breakdown .section-content').hidden).toBe(false);
});

test('superseded first paint never starts its queued detail queries', async () => {
  const { ctx, calls, painted } = dashboard();
  const primary = deferred(), api = ctx.api;
  ctx.api = (route, params) => route === 'timeseries' && params.provider === '' ? primary.promise : api(route, params);
  const first = ctx.refresh();
  ctx.state.provider = 'claude';
  const last = ctx.refresh();
  primary.resolve({});
  await Promise.all([first, last]);
  expect(calls.filter((c) => c.route === 'drift').map((c) => c.params.provider)).toEqual(['claude']);
  expect([...new Set(painted)]).toEqual(['claude']);
});

test('authentication failure in a detail stops queued work and requests a key', async () => {
  const { ctx, calls, renders } = dashboard();
  const api = ctx.api;
  ctx.api = (route, params) => route === 'drift' ? Promise.reject(new ctx.AuthError('key expired')) : api(route, params);
  await ctx.refresh();
  expect(renders).toContain('forget-key');
  expect(renders).toContain('key-form');
  expect(calls.some((c) => c.route === 'limit-burn')).toBe(false);
});

test('short periods pick minute and hour buckets for timeseries and drift', async () => {
  const { ctx, calls } = dashboard();
  for (const [period, bucket] of [['15m', 'minute'], ['1h', 'minute'], ['6h', 'hour'], ['24h', 'hour'], ['7d', 'day']]) {
    calls.length = 0;
    ctx.state.period = period;
    await ctx.refresh();
    expect(calls.find((c) => c.route === 'timeseries')?.params.bucket).toBe(bucket);
    expect(calls.find((c) => c.route === 'drift')?.params.bucket).toBe(bucket);
  }
});

test('chart controls and resize are safe before their detail data arrives', () => {
  const ctx = vm.createContext({ data: {} });
  const drift = page.slice(page.indexOf('function renderDrift()'), page.indexOf('function burnAccountName('));
  const burn = page.slice(page.indexOf('function renderBurn()'), page.indexOf('function renderLeftover()'));
  const views = page.slice(page.indexOf('/* ---------- latency ---------- */'), page.indexOf('/* ---------- loading ---------- */'));
  vm.runInContext(drift + burn + views, ctx);
  expect(() => ctx.renderDrift()).not.toThrow();
  expect(() => ctx.renderBurn()).not.toThrow();
  expect(() => ctx.renderLatency()).not.toThrow();
  expect(() => ctx.renderRequests()).not.toThrow();
});

test('drift chart keeps model and harness markers separated and omits unplotted marker clutter', () => {
  const nodes = new Map();
  const makeNode = (id, tag = 'div', attrs = {}) => {
    const n = {
      id, tag, attrs: { ...attrs }, hidden: false, textContent: '', clientWidth: 800,
      children: [], classList: { add() {}, remove() {} },
      append(...kids) { this.children.push(...kids.filter(Boolean)); },
      replaceChildren(...kids) { this.children = kids.filter(Boolean); },
      setAttribute(k, v) { this.attrs[k] = v; },
      getAttribute(k) { return this.attrs[k]; },
      getComputedTextLength() { return (this.textContent || String(this.attrs.text || '')).length * 6; },
      addEventListener() {},
      remove() { this.removed = true; },
    };
    if (attrs.text !== undefined) n.textContent = String(attrs.text);
    return n;
  };
  const getNode = (id) => {
    if (!nodes.has(id)) nodes.set(id, makeNode(id));
    return nodes.get(id);
  };
  const day1 = Date.UTC(2026, 10, 18), day2 = Date.UTC(2026, 10, 19);
  let capturedTooltipRows = [];
  const ctx = vm.createContext({
    MINUTE: 60000, HOUR: 3600000, DAY: 86400000,
    BUCKET_MS: { minute: 60000, hour: 3600000, day: 86400000 },
    SERIES_SLOTS: 2, END_LABELS: 4,
    state: { driftGroup: 'response_model', driftMetric: 'input', driftStat: 'median' },
    data: { params: { from: day1, to: day2, tz: 0 }, drift: null },
    document: {
      getElementById: getNode,
      querySelector: () => ({ textContent: 'Input incl. cache' }),
    },
    el: (tag, attrs = {}, ...kids) => { const n = makeNode('', tag, attrs); n.append(...kids); return n; },
    svgEl: (tag, attrs = {}) => makeNode('', tag, attrs),
    buckets: () => [day1, day2],
    frame: (container) => {
      const svg = makeNode('svg', 'svg');
      container.replaceChildren(svg);
      return { svg, x: (i) => 60 + i * 200, y: () => 100, band: 100, m: { top: 20, right: 20 }, width: 800, plotH: 200 };
    },
    labelSpacer: () => () => true,
    endLabels: (ends) => ends.map((e) => e.key),
    hitColumns: (_g, xs, onShow) => { capturedTooltipRows = xs.map((_, i) => onShow(i).rows); },
    legend: () => {},
    table: () => makeNode('', 'table'),
    colorFor: () => 'var(--s1)',
    markerImpact: () => null,
    fmtDay: (t) => new Date(t).toISOString().slice(0, 10),
    fmtHour: () => '00:00',
    fmtCompact: String, fmtInt: String, fmtPct: String, fmtUSD: String, fmtSeconds: String,
  });
  const driftCode = page.slice(page.indexOf('const MARKER_KINDS'), page.indexOf('function burnAccountName('));
  vm.runInContext(driftCode, ctx);

  const pt = (t, key, reqs) => ({
    t, key, requests: reqs, cache_hit_rate: 0.9,
    median: { input: 1000, uncached_input: 100, cache_read: 900, cache_write: 0, output: 100, reasoning: 0, output_total: 100, cost_usd: 0.1, latency_ms: 500, ttft_ms: 100, tokens_per_sec: 50 },
    p90: { input: 2000, uncached_input: 200, cache_read: 1800, cache_write: 0, output: 200, reasoning: 0, output_total: 200, cost_usd: 0.2, latency_ms: 900, ttft_ms: 200, tokens_per_sec: 40 },
  });
  // Model view: receives a plotted model marker, an unplotted model marker, and a stray client marker.
  ctx.data.drift = {
    group: 'response_model', bucket: 'day',
    points: [pt(day1, 'model-a', 100), pt(day2, 'model-a', 100), pt(day2, 'model-b', 50), pt(day2, 'model-tiny', 1)],
    markers: [
      { t: day2, kind: 'client_version', value: 'claude-code/2.1.329' },
      { t: day2, kind: 'response_model', value: 'model-b' },
      { t: day2, kind: 'response_model', value: 'model-tiny' },
    ],
  };
  ctx.renderDrift();
  const modelMarkerLabels = getNode('drift-chart').children[0].children
    .filter((c) => c.attrs.class === 'marker-label' && !c.removed)
    .map((c) => c.textContent);
  expect(modelMarkerLabels).toEqual(['model-b']);
  expect(capturedTooltipRows[1].filter((r) => r.value === 'new').map((r) => r.label)).toEqual(['model-b']);
  expect(getNode('drift-changes-summary').textContent).toBe('Changes (2)');

  // Client (harness) view: shows continuous harness lines and marks upgrades of plotted harnesses only.
  ctx.state.driftGroup = 'client';
  ctx.data.drift = {
    group: 'client', bucket: 'day',
    points: [pt(day1, 'claude-code', 100), pt(day2, 'claude-code', 100), pt(day1, 'codex', 50), pt(day2, 'codex', 50), pt(day2, 'tiny-sdk', 1)],
    markers: [
      { t: day2, kind: 'client_version', value: 'claude-code/2.1.329' },
      { t: day2, kind: 'client_version', value: 'tiny-sdk/1.0.0' },
      { t: day2, kind: 'response_model', value: 'model-b' },
    ],
  };
  ctx.renderDrift();
  const clientMarkerLabels = getNode('drift-chart').children[0].children
    .filter((c) => c.attrs.class === 'marker-label' && !c.removed)
    .map((c) => c.textContent);
  expect(clientMarkerLabels).toEqual(['claude-code/2.1.329']);
  expect(capturedTooltipRows[1].filter((r) => r.value === 'new').map((r) => r.label)).toEqual(['claude-code/2.1.329']);
});

test('requests tab loads calls and quiet live ticks fetch only requests without flashing loading', async () => {
  const { ctx, calls, credentialCalls, renders, node } = dashboard();
  ctx.state.view = 'requests';
  await ctx.refresh();
  expect(calls.map((c) => c.route)).toEqual(['requests', 'options']);
  expect(credentialCalls.length).toBe(1);
  expect(renders).toContain('requests');
  expect(node('requests .section-content').hidden).toBe(false);

  calls.length = credentialCalls.length = renders.length = 0;
  ctx.rangeParams = () => ({ from: 200, to: 300, tz: -300 });
  await ctx.refresh(true);
  expect(calls).toEqual([{
    route: 'requests',
    params: { from: 200, to: 300, tz: -300, provider: '', model: '', client: '', account: '', api_key: '', sort: 'recent', outcome: 'all', offset: 0, limit: 50 },
  }]);
  expect(credentialCalls.length).toBe(0);
  expect(renders).toEqual(['requests']);
  expect(node('requests .section-content').hidden).toBe(false);
});
