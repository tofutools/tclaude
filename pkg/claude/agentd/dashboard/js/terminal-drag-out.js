// Geometry for the "drag a terminal out of its home region" gesture.
//
// HTML5 drag and drop never tells a page that a drag ended over some other
// browser window, so detach (dashboard tab strip -> own browser tab) and
// reattach (solo pop-out -> dashboard) have to be inferred from where the drag
// ended: released outside the region that owns the terminal, far enough out
// that a sloppy reorder cannot trigger it, and with no drop target inside that
// region having accepted the drag.
//
// The rule is deliberately conservative. Anything it cannot recognise — a
// cancelled drag, a coordinate the browser did not report — resolves to "not a
// drag-out", so the gesture degrades to the explicit ⧉ tab / ↩ dashboard
// buttons instead of half-firing.

export const DRAG_OUT_MARGIN = 56;

// dragOutPoint returns the release point of a drag event, or null when the
// event carries no usable coordinate.
export function dragOutPoint(event) {
  const x = Number(event?.clientX);
  const y = Number(event?.clientY);
  if (!Number.isFinite(x) || !Number.isFinite(y)) return null;
  // Escape-cancelled drags report the viewport origin, and neither home region
  // ever sits in that corner — the tab strip is inside the dashboard chrome and
  // the solo header spans the top of its own page but is never zero-height.
  if (x === 0 && y === 0) return null;
  return { x, y };
}

export function draggedOut(point, rect, margin = DRAG_OUT_MARGIN) {
  if (!point || !rect) return false;
  const right = Number.isFinite(rect.right) ? rect.right : rect.left + rect.width;
  const bottom = Number.isFinite(rect.bottom) ? rect.bottom : rect.top + rect.height;
  if (![rect.left, rect.top, right, bottom].every(Number.isFinite)) return false;
  return point.x < rect.left - margin || point.x > right + margin
    || point.y < rect.top - margin || point.y > bottom + margin;
}

// dragLeftRegion is the whole rule in one call: it takes the raw dragend (or
// dragover) event and the region element that owns the terminal.
export function dragLeftRegion(event, region, margin = DRAG_OUT_MARGIN) {
  const rect = region?.getBoundingClientRect?.();
  return draggedOut(dragOutPoint(event), rect, margin);
}
