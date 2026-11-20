// Log scales and hit testing for the latency scatter. No DOM, so bun tests
// load it as is.

// logDomain widens [min, max] to whole decades.
function logDomain(min, max) {
  const lo = Math.pow(10, Math.floor(Math.log10(min)));
  const hi = Math.pow(10, Math.ceil(Math.log10(max)));
  return [lo, hi > lo ? hi : lo * 10];
}

// logTicks lists 1, 2, 5 steps between lo and hi, or only the decades when
// the range spans more than three of them.
function logTicks(lo, hi) {
  const steps = Math.log10(hi / lo) > 3 ? [1] : [1, 2, 5];
  const ticks = [];
  for (let decade = lo; decade <= hi; decade *= 10) {
    for (const s of steps) {
      const v = Math.round(decade * s * 1e6) / 1e6;
      if (v >= lo && v <= hi) ticks.push(v);
    }
  }
  return ticks;
}

// nearestPoint returns the index of the point closest to (x, y) within
// radius, or -1.
function nearestPoint(points, x, y, radius) {
  let best = -1;
  let bestDist = radius * radius;
  points.forEach((p, i) => {
    const d = (p.x - x) ** 2 + (p.y - y) ** 2;
    if (d <= bestDist) { best = i; bestDist = d; }
  });
  return best;
}

if (typeof module !== 'undefined') { module.exports = { logDomain, logTicks, nearestPoint }; }
