// Tooltip placement for chart columns. The tooltip opens beside the hovered
// column, never over it, so it does not hide the marks it describes.

const TOOLTIP_GAP = 8;

// besideColumn returns the tooltip's top-left corner for a column rect, the
// tooltip's size, the viewport size and the pointer's vertical position.
function besideColumn(column, box, viewport, pointerY) {
  let x = column.right + TOOLTIP_GAP;
  if (x + box.width > viewport.width - TOOLTIP_GAP) x = column.left - TOOLTIP_GAP - box.width;
  if (x < TOOLTIP_GAP) x = TOOLTIP_GAP;
  const y = Math.min(Math.max(pointerY - box.height / 2, TOOLTIP_GAP), viewport.height - box.height - TOOLTIP_GAP);
  return { x, y: Math.max(TOOLTIP_GAP, y) };
}

if (typeof module !== 'undefined') {
  module.exports = { besideColumn };
}
