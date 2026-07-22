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

// dragScreenPoint is the release point in screen coordinates, which is what a
// detached window has to be positioned in.
export function dragScreenPoint(event) {
  const x = Number(event?.screenX);
  const y = Number(event?.screenY);
  if (!Number.isFinite(x) || !Number.isFinite(y)) return null;
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

// A tab dragged off the strip lands in a window of its own rather than another
// tab of the window it just left — dragging a terminal *out* should get it out
// of the way, which a tab behind the dashboard does not. Only window features
// make a browser choose a window over a tab; passing none (the ⧉ tab button and
// the tab context menu) still opens an ordinary tab.
//
// Every browser treats the features as a request. A user setting that forces
// tabs, or a screen too small for the asked-for size, quietly wins; the handoff
// itself does not care which one it got.
export const DETACH_WINDOW_MIN = Object.freeze({ width: 480, height: 300 });
// Popup chrome the terminal does not get to use. Sizing asks for the pane's own
// height plus this, so the terminal arrives with the columns and rows it had
// and nothing reflows on the way out.
export const DETACH_WINDOW_CHROME_HEIGHT = 40;

function clampAxis(length, start, min, availLength, availStart) {
  const span = Math.max(min, Math.min(Math.round(length), availLength));
  const limit = availStart + availLength - span;
  return { span, start: Math.round(Math.max(availStart, Math.min(start, limit))) };
}

// detachWindowFeatures sizes the detached window to the pane the terminal is
// leaving and puts it where the drag was released, clamped onto the screen.
// It returns '' when either measurement is missing, which asks for a plain tab
// rather than guessing at a geometry.
export function detachWindowFeatures({ size, at, screen } = {}) {
  const width = Number(size?.width);
  const height = Number(size?.height);
  const screenWidth = Number(screen?.availWidth);
  const screenHeight = Number(screen?.availHeight);
  if (![width, height, screenWidth, screenHeight].every((value) => Number.isFinite(value) && value > 0)) {
    return '';
  }
  const screenLeft = Number.isFinite(Number(screen?.availLeft)) ? Number(screen.availLeft) : 0;
  const screenTop = Number.isFinite(Number(screen?.availTop)) ? Number(screen.availTop) : 0;
  const wantLeft = Number(at?.x);
  const wantTop = Number(at?.y);
  const horizontal = clampAxis(
    width, Number.isFinite(wantLeft) ? wantLeft : screenLeft,
    DETACH_WINDOW_MIN.width, screenWidth, screenLeft,
  );
  const vertical = clampAxis(
    height + DETACH_WINDOW_CHROME_HEIGHT, Number.isFinite(wantTop) ? wantTop : screenTop,
    DETACH_WINDOW_MIN.height, screenHeight, screenTop,
  );
  return `popup=yes,width=${horizontal.span},height=${vertical.span}`
    + `,left=${horizontal.start},top=${vertical.start}`;
}
