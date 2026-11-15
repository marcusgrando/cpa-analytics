// Sparklines for the totals tiles. The page draws them in a 100-unit-wide
// viewBox stretched to the tile, so x is a percentage of the width.

// sumBuckets adds the numeric fields of every group in each bucket of xs.
// A bucket with no points is an empty object.
function sumBuckets(points, xs) {
  const sums = new Map(xs.map((t) => [t, {}]));
  for (const p of points) {
    const sum = sums.get(p.t);
    if (!sum) continue;
    for (const [field, value] of Object.entries(p)) {
      if (field !== 't' && typeof value === 'number') sum[field] = (sum[field] || 0) + value;
    }
  }
  return xs.map((t) => sums.get(t));
}

// recordedValues drops the values of buckets that ended before recording
// began, so the line does not show unrecorded days as idle ones.
function recordedValues(values, xs, size, since) {
  return values.map((v, i) => (xs[i] + size <= since ? null : v));
}

// sparkline returns the path from zero to the largest value, the last
// measured point and each bucket's point. A null value is a bucket with
// nothing to measure and breaks the line. It returns null when there is
// nothing to draw.
function sparkline(values, height, pad) {
  const measured = values.filter((v) => v !== null);
  if (values.length < 2 || !measured.length) return null;
  const max = Math.max(...measured);
  const round = (n) => Math.round(n * 100) / 100;
  let d = '';
  let end = null;
  let drawing = false;
  const points = values.map((v, i) => {
    if (v === null) { drawing = false; return null; }
    const point = { x: round((i / (values.length - 1)) * 100), y: round(height - pad - (max > 0 ? v / max : 0) * (height - 2 * pad)) };
    d += (drawing ? 'L' : 'M') + point.x + ' ' + point.y;
    drawing = true;
    end = point;
    return point;
  });
  return { d, end, points };
}

if (typeof module !== 'undefined') { module.exports = { recordedValues, sparkline, sumBuckets }; }
