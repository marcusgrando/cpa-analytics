// Comparison of a period with the one before it. No DOM, so bun tests load it
// as is. web.go inlines it into the dashboard page.

// Below this, a change reads as "no change" rather than a rounded zero.
const CHANGE_FLOOR = 0.005;

// relativeChange formats current against previous, or null when the previous
// period is empty and there is nothing to compare.
function relativeChange(current, previous) {
  if (!(previous > 0)) return null;
  const change = (current - previous) / previous;
  if (Math.abs(change) < CHANGE_FLOOR) return 'no change';
  return (change > 0 ? '+' : '-') + Math.round(Math.abs(change) * 100) + '%';
}

// pointChange formats the difference of two rates (0 to 1) in percentage points.
function pointChange(current, previous) {
  if (previous === null || previous === undefined) return null;
  const change = current - previous;
  if (Math.abs(change) < CHANGE_FLOOR / 10) return 'no change';
  return (change > 0 ? '+' : '-') + (Math.abs(change) * 100).toFixed(1) + ' pp';
}

// previousPeriod returns the window of the same length that ends where range starts.
function previousPeriod(range) {
  return { from: range.from - (range.to - range.from), to: range.from };
}

// recordedThrough tells whether recording had begun when period started.
// dataSince is the first recorded row, 0 with none. A partly recorded period
// would read as growth.
function recordedThrough(period, dataSince) {
  return dataSince > 0 && dataSince <= period.from;
}

// changeDrivers decomposes period-over-period change into the behavioural
// levers that move spend: request volume, prompt input per request, total
// output per request, cache hit efficiency and session depth.
function changeDrivers(current, previous) {
  if (!current || !previous) return [];
  const items = [];
  const addRel = (label, cur, prev) => {
    const ch = relativeChange(cur, prev);
    if (ch && ch !== 'no change') items.push({ label, change: ch });
  };
  addRel('requests', current.requests, previous.requests);
  addRel('input/req', current.median_input_per_request, previous.median_input_per_request);
  addRel('output/req', current.median_output_per_request, previous.median_output_per_request);
  if (previous.processed_tokens > 0 && current.processed_tokens > 0) {
    const ch = pointChange(current.cache_hit_rate, previous.cache_hit_rate);
    if (ch && ch !== 'no change') items.push({ label: 'cache hit', change: ch });
  }
  addRel('req/session', current.median_requests_per_session, previous.median_requests_per_session);
  return items;
}

// markerImpact compares per-request drift points strictly before marker.t with
// points at or after marker.t, weighted by daily request volume, so the owner
// can see immediately whether a new client version, model or effort moved spend.
function markerImpact(points, marker, stat = 'median') {
  if (!Array.isArray(points) || !marker) return null;
  const agg = (slice) => {
    let reqs = 0, inputReqs = 0, input = 0, outTotal = 0, cost = 0, hit = 0;
    for (const p of slice) {
      const n = Number(p.requests) || 0;
      const st = p[stat];
      if (!(n > 0) || !st) continue;
      reqs += n;
      input += (st.input || 0) * n;
      outTotal += (st.output_total !== undefined ? st.output_total : (st.output || 0) + (st.reasoning || 0)) * n;
      cost += (st.cost_usd || 0) * n;
      const inN = p.input_requests !== undefined ? Number(p.input_requests) || 0 : n;
      if (inN > 0) {
        inputReqs += inN;
        hit += (p.cache_hit_rate || 0) * inN;
      }
    }
    if (!reqs) return null;
    return { reqs, input: input / reqs, outputTotal: outTotal / reqs, costUsd: cost / reqs, cacheHit: inputReqs > 0 ? hit / inputReqs : null };
  };
  const before = agg(points.filter((p) => p.t < marker.t));
  const after = agg(points.filter((p) => p.t >= marker.t));
  if (!before || !after) return null;
  return {
    beforeReqs: before.reqs,
    afterReqs: after.reqs,
    input: relativeChange(after.input, before.input),
    outputTotal: relativeChange(after.outputTotal, before.outputTotal),
    costUsd: relativeChange(after.costUsd, before.costUsd),
    cacheHit: before.cacheHit !== null && after.cacheHit !== null ? pointChange(after.cacheHit, before.cacheHit) : null,
  };
}

if (typeof module !== 'undefined') {
  module.exports = { relativeChange, pointChange, previousPeriod, recordedThrough, changeDrivers, markerImpact };
}
