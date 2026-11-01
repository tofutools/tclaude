// overlay-window-model.js -- the pure half of the shared overlay window
// (overlay-window.js): where a floating or snapped window sits, and how a drag
// changes that. A placement is { x, y, w, h, snap }, where snap is '' for a
// floating window, or the edge it is snapped to, or 'max'.
//
// Everything here is DOM-free so it can be tested directly.

export const OVERLAY_MIN_W = 360;
export const OVERLAY_MIN_H = 220;
// Distance from a viewport edge at which a drag offers to snap.
export const OVERLAY_SNAP_EDGE = 28;
// The share of the viewport a snapped window takes.
export const OVERLAY_SNAP_SHARE = 0.42;
// Room kept for the fixed footer bar below a placement.
export const OVERLAY_FOOTER_ROOM = 29;

export function parsePlacement(raw) {
  try {
    const p = JSON.parse(raw || 'null');
    if (!p || typeof p !== 'object') return null;
    const nums = ['x', 'y', 'w', 'h'].every((k) => Number.isFinite(p[k]));
    if (!nums) return null;
    const snap = ['left', 'right', 'bottom', 'max'].includes(p.snap) ? p.snap : '';
    const placement = { x: p.x, y: p.y, w: p.w, h: p.h, snap };
    return snap === 'max' && ['left', 'right', 'bottom'].includes(p.restoreSnap) ? { ...placement, restoreSnap: p.restoreSnap } : placement;
  } catch {
    return null;
  }
}

// clampPlacement keeps a floating window reachable: at least its title bar
// stays on screen whatever the viewport did since it was saved.
export function clampPlacement(p, viewport) {
  const maxW = Math.max(OVERLAY_MIN_W, viewport.width - 8);
  const maxH = Math.max(OVERLAY_MIN_H, viewport.height - OVERLAY_FOOTER_ROOM - 8);
  const w = Math.round(Math.min(maxW, Math.max(OVERLAY_MIN_W, p.w)));
  const h = Math.round(Math.min(maxH, Math.max(OVERLAY_MIN_H, p.h)));
  const x = Math.round(Math.min(viewport.width - 80, Math.max(80 - w, p.x)));
  const y = Math.round(Math.min(viewport.height - OVERLAY_FOOTER_ROOM - 36, Math.max(0, p.y)));
  return { x, y, w, h, snap: p.snap || '' };
}

// fitPlacement is where a window opens: exactly its saved place when that fits,
// otherwise moved in, and smaller only if the viewport cannot hold it. Unlike
// clampPlacement it keeps the whole title bar on screen, because the close and
// minimize controls sit at its right end. A viewport narrower than the minimum
// width keeps that right end. The result is never saved, so a larger browser
// brings back the operator's own size.
export function fitPlacement(p, viewport) {
  const { w, h, snap } = clampPlacement(p, viewport);
  const right = viewport.width - w;
  const x = Math.round(Math.max(Math.min(0, right), Math.min(p.x, right)));
  const y = Math.round(Math.max(0, Math.min(p.y, viewport.height - OVERLAY_FOOTER_ROOM - h)));
  return { x, y, w, h, snap };
}

// snapZone names the edge a dragged window would snap to for a pointer
// position, or '' to keep floating.
export function snapZone(pointer, viewport) {
  if (pointer.x >= viewport.width - OVERLAY_SNAP_EDGE) return 'right';
  if (pointer.x <= OVERLAY_SNAP_EDGE) return 'left';
  if (pointer.y >= viewport.height - OVERLAY_FOOTER_ROOM - OVERLAY_SNAP_EDGE) return 'bottom';
  return '';
}

// viewportOf is the area a fixed window can use. innerWidth and innerHeight
// include page scrollbars, which would cover a window snapped to that edge.
export function viewportOf(win) {
  const root = win?.document?.documentElement;
  return {
    width: root?.clientWidth || win?.innerWidth || 1280,
    height: root?.clientHeight || win?.innerHeight || 800,
  };
}

// zoneRect is the rectangle a snapped window occupies: a full-height side
// panel below the top bar, a full-width bottom drawer above the footer, or for
// a maximized window all the room between the two.
export function zoneRect(zone, viewport, top = 0) {
  const bottom = viewport.height - OVERLAY_FOOTER_ROOM;
  if (zone === 'max') return { x: 0, y: top, w: viewport.width, h: Math.max(OVERLAY_MIN_H, bottom - top), snap: zone };
  if (zone === 'right' || zone === 'left') {
    const w = Math.min(viewport.width, Math.max(OVERLAY_MIN_W, Math.round(viewport.width * OVERLAY_SNAP_SHARE)));
    return { x: zone === 'right' ? viewport.width - w : 0, y: top, w, h: Math.max(OVERLAY_MIN_H, bottom - top), snap: zone };
  }
  if (zone === 'bottom') {
    const h = Math.max(OVERLAY_MIN_H, Math.round((bottom - top) * OVERLAY_SNAP_SHARE));
    return { x: 0, y: bottom - h, w: viewport.width, h, snap: zone };
  }
  return null;
}

// resizedPlacement is a drag of one edge or corner, named by its compass
// letters. The opposite side stays put, including where the minimum size or the
// viewport stops the drag. A window already past the viewport limit can still
// shrink, and does not jump back to it.
export function resizedPlacement(start, zone, dx, dy, viewport) {
  const west = zone.includes('w');
  const east = zone.includes('e');
  const north = zone.includes('n');
  const south = zone.includes('s');
  const right = start.x + start.w;
  const bottom = start.y + start.h;
  const sized = clampPlacement({
    ...start,
    w: start.w + (west ? -dx : east ? dx : 0),
    h: start.h + (north ? -dy : south ? dy : 0),
  }, viewport);
  const roomW = west ? right : east ? viewport.width - start.x : sized.w;
  const roomH = north ? bottom : south ? viewport.height - OVERLAY_FOOTER_ROOM - start.y : sized.h;
  const w = Math.min(sized.w, Math.max(start.w, roomW));
  const height = Math.min(sized.h, Math.max(start.h, roomH));
  return clampPlacement({
    ...sized,
    x: west ? right - w : start.x,
    y: north ? bottom - height : start.y,
    w,
    h: height,
  }, viewport);
}

// savedAfterResize is what a finished resize remembers. A window fitted to a small
// viewport shows less than its saved size, so the side the drag did not touch
// keeps the saved value.
export function savedAfterResize(last, zone, saved) {
  if (!saved) return last;
  return {
    ...last,
    w: /[ew]/.test(zone) ? last.w : saved.w,
    h: /[ns]/.test(zone) ? last.h : saved.h,
  };
}

// snappedResize is a drag of a snapped panel's free edge. The panel stays glued
// to its edge however far the drag overshoots.
export function snappedResize(start, dx, dy, viewport) {
  let next = start;
  if (start.snap === 'right') next = { ...start, x: start.x + dx, w: start.w - dx };
  else if (start.snap === 'left') next = { ...start, w: start.w + dx };
  else if (start.snap === 'bottom') next = { ...start, y: start.y + dy, h: start.h - dy };
  const w = Math.min(viewport.width, Math.max(OVERLAY_MIN_W, next.w));
  const h = Math.max(OVERLAY_MIN_H, next.h);
  next = { ...next, w, h };
  if (start.snap === 'right') next.x = viewport.width - w;
  if (start.snap === 'bottom') next.y = start.y + start.h - h;
  return next;
}
