function driftMeasurement(p, metric, stat) {
  const countField = { ttft_ms: 'ttft_requests', tokens_per_sec: 'speed_requests', cache_hit_rate: 'input_requests' }[metric];
  const n = countField ? p[countField] : p.requests;
  return { t: p.t, n, value: metric === 'cache_hit_rate' ? p.cache_hit_rate : p[stat][metric] };
}

// Each series compares its own first and last qualifying buckets. A missing
// baseline is distinct from a measured zero, especially for token counters.
function driftSeries(points, metric, stat, minimum) {
  const rate = metric === 'cache_hit_rate';
  const groups = new Map();
  for (const p of points) {
    const point = driftMeasurement(p, metric, stat);
    if (!(point.n >= minimum)) continue;
    if (!groups.has(p.key)) groups.set(p.key, { key: p.key, volume: 0, points: [] });
    const series = groups.get(p.key);
    series.volume += point.n;
    series.points.push(point);
  }
  for (const series of groups.values()) {
    series.points.sort((a, b) => a.t - b.t);
    const base = series.points[0].value;
    for (const p of series.points) {
      const difference = (p.value - base) * 100;
      p.change = rate ? difference : base > 0 ? difference / base : null;
    }
    series.change = series.points.length > 1 ? series.points.at(-1).change : null;
  }
  return [...groups.values()].sort((a, b) => b.volume - a.volume || a.key.localeCompare(b.key));
}

if (typeof module !== 'undefined') { module.exports = { driftSeries, driftMeasurement }; }
