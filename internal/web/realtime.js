// The "Right now" figures: the last minutes against the viewer's day so far,
// from the realtime route. No DOM, so bun tests load it as is.

// realtimeFigures returns each figure's value in the window, today's value and
// the change. Counts become rates over each stretch's minutes. A median or a
// cache rate with nothing measured is null, and so is any change without both
// sides.
function realtimeFigures({ now, today }) {
  const perMinute = (s, v) => (s.minutes > 0 ? v / s.minutes : 0);
  const measured = (v) => (v > 0 ? v : null);
  const relative = (a, b) => (a === null || b === null || b === 0 ? null : (a - b) / b);
  const points = (a, b) => (a === null || b === null ? null : a - b);
  const figure = (id, read, unit = 'relative') => {
    const a = read(now);
    const b = read(today);
    return { id, now: a, today: b, change: unit === 'points' ? points(a, b) : relative(a, b), unit };
  };
  return [
    figure('requests', (s) => perMinute(s, s.requests)),
    figure('tokens', (s) => perMinute(s, s.tokens)),
    figure('cost', (s) => perMinute(s, s.cost_usd) * 60),
    figure('cache', (s) => (s.tokens > 0 ? s.cache_hit_rate : null), 'points'),
    figure('ttft', (s) => measured(s.median_ttft_ms)),
    figure('latency', (s) => measured(s.median_latency_ms)),
    figure('failures', (s) => perMinute(s, s.failed)),
  ];
}

if (typeof module !== 'undefined') { module.exports = { realtimeFigures }; }
