// endLabels picks which lines get their name at the end of the line. Lines
// come in rank order with the y of their last point. A label that would sit
// closer than minGap to a higher-ranked one is dropped, not moved, since a
// moved label no longer touches its line. The legend still names every line.
function endLabels(ends, limit, minGap) {
  const kept = [];
  for (const end of ends) {
    if (kept.length === limit) break;
    if (kept.every((k) => Math.abs(k.y - end.y) >= minGap)) kept.push(end);
  }
  return kept.map((k) => k.key);
}

if (typeof module !== 'undefined') { module.exports = { endLabels }; }
