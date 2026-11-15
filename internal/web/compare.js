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

if (typeof module !== 'undefined') {
  module.exports = { relativeChange, pointChange, previousPeriod, recordedThrough };
}
