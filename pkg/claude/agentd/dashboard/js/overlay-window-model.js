// overlay-window-model.js -- the pure half of the shared overlay window
// (overlay-window.js): where a floating or snapped window sits, and how a drag
// changes that. A placement is { x, y, w, h, snap }, where snap is '' for a
// floating window, or the zone it is snapped to, or 'max'. A saved placement may
// also carry panels, the size the operator gave each zone: { [zone]: { w, h } }.
//
// Everything here is DOM-free so it can be tested directly.

// droppedAt is where a window dragged out lands: under the pointer, by its title.
export const droppedAt = ({ clientX, clientY }) => ({ x: clientX - 120, y: clientY - 14 });

export const OVERLAY_MIN_W = 360;
export const OVERLAY_MIN_H = 220;
// Distance from a viewport edge at which a drag offers to snap: a share of the
// viewport's width, so it suits every screen, within these limits.
export const OVERLAY_SNAP_EDGE_SHARE = 0.03;
export const OVERLAY_SNAP_EDGE_MIN = 40;
export const OVERLAY_SNAP_EDGE_MAX = 80;
// How far along an edge, from its end, a drag counts as the corner.
export const OVERLAY_CORNER_SHARE = 0.11;
export const OVERLAY_CORNER_MIN = 120;
export const OVERLAY_CORNER_MAX = 240;
// The share of the viewport a snapped window takes.
export const OVERLAY_SNAP_SHARE = 0.42;
// Room kept for the fixed footer bar below a placement.
export const OVERLAY_FOOTER_ROOM = 29;

// Each snap zone and the edges and corner of it that are free to resize.
const SNAP_ZONES = Object.freeze({
  left: ['e'], right: ['w'], bottom: ['n'],
  'top-left': ['e', 's', 'se'], 'top-right': ['w', 's', 'sw'],
  'bottom-left': ['e', 'n', 'ne'], 'bottom-right': ['w', 'n', 'nw'],
});
const isSnapZone = (zone) => Object.hasOwn(SNAP_ZONES, zone);

// freeEdges names the handles a window snapped to zone has.
export function freeEdges(zone) {
  return SNAP_ZONES[zone] || [];
}

function parsePanels(raw) {
  if (!raw || typeof raw !== 'object') return null;
  const panels = Object.entries(raw).filter(([zone, size]) => isSnapZone(zone) && Number.isFinite(size?.w) && Number.isFinite(size?.h))
    .map(([zone, { w, h }]) => [zone, { w, h }]);
  return panels.length ? Object.fromEntries(panels) : null;
}

export function parsePlacement(raw) {
  try {
    const p = JSON.parse(raw || 'null');
    if (!p || typeof p !== 'object') return null;
    const nums = ['x', 'y', 'w', 'h'].every((k) => Number.isFinite(p[k]));
    if (!nums) return null;
    const snap = isSnapZone(p.snap) || p.snap === 'max' ? p.snap : '';
    const panels = parsePanels(p.panels);
    const placement = { x: p.x, y: p.y, w: p.w, h: p.h, snap, ...(panels && { panels }) };
    return snap === 'max' && isSnapZone(p.restoreSnap) ? { ...placement, restoreSnap: p.restoreSnap } : placement;
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

const clamped = (min, value, max) => Math.min(max, Math.max(min, value));

// snapReach is how near an edge and a corner a drag must come in this viewport.
export function snapReach(viewport) {
  return {
    edge: clamped(OVERLAY_SNAP_EDGE_MIN, viewport.width * OVERLAY_SNAP_EDGE_SHARE, OVERLAY_SNAP_EDGE_MAX),
    corner: clamped(OVERLAY_CORNER_MIN, viewport.width * OVERLAY_CORNER_SHARE, OVERLAY_CORNER_MAX),
  };
}

// snapZone names the zone a dragged window would snap to for a pointer position,
// or '' to keep floating. Along a side, top is where the room starts, so the top
// corners sit just under the nav bar.
export function snapZone(pointer, viewport, top = 0) {
  const bottom = viewport.height - OVERLAY_FOOTER_ROOM;
  const { edge, corner } = snapReach(viewport);
  const right = pointer.x >= viewport.width - edge;
  const left = pointer.x <= edge;
  // A corner stops at a third of a short side, whose middle stays the edge's.
  if (left || right) {
    const side = right ? 'right' : 'left';
    const reach = Math.min(corner, (bottom - top) / 3);
    if (pointer.y <= top + reach) return `top-${side}`;
    return pointer.y >= bottom - reach ? `bottom-${side}` : side;
  }
  if (pointer.y < bottom - edge) return '';
  if (pointer.x <= corner) return 'bottom-left';
  return pointer.x >= viewport.width - corner ? 'bottom-right' : 'bottom';
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

// zoneRect is the rectangle a snapped window occupies: a full-height side panel
// below the top bar, a full-width bottom drawer above the footer, a quarter in a
// corner, or for a maximized window all the room between the top bar and footer.
// size is what the operator resized this zone to; it stays within the viewport.
export function zoneRect(zone, viewport, top = 0, size = null) {
  const bottom = viewport.height - OVERLAY_FOOTER_ROOM;
  const room = Math.max(OVERLAY_MIN_H, bottom - top);
  if (zone === 'max') return { x: 0, y: top, w: viewport.width, h: room, snap: zone };
  if (!isSnapZone(zone)) return null;
  const width = () => Math.min(viewport.width, Math.max(OVERLAY_MIN_W, Math.round(size?.w ?? viewport.width * OVERLAY_SNAP_SHARE)));
  const height = (share) => Math.min(room, Math.max(OVERLAY_MIN_H, Math.round(size?.h ?? (bottom - top) * share)));
  if (zone === 'bottom') {
    const h = height(OVERLAY_SNAP_SHARE);
    return { x: 0, y: bottom - h, w: viewport.width, h, snap: zone };
  }
  const w = width();
  const x = zone.endsWith('right') ? viewport.width - w : 0;
  if (zone === 'left' || zone === 'right') return { x, y: top, w, h: room, snap: zone };
  const h = height(0.5);
  return { x, y: zone.startsWith('top') ? top : bottom - h, w, h, snap: zone };
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

// snappedResize is a drag of one of a snapped window's free edges, or the corner
// between two, named by its compass letters. The opposite side stays glued to
// its place however far the drag overshoots.
export function snappedResize(start, zone, dx, dy, viewport, top = 0) {
  const west = zone.includes('w');
  const north = zone.includes('n');
  const dw = west ? -dx : zone.includes('e') ? dx : 0;
  const dh = north ? -dy : zone.includes('s') ? dy : 0;
  const room = Math.max(OVERLAY_MIN_H, viewport.height - OVERLAY_FOOTER_ROOM - top);
  const w = Math.min(viewport.width, Math.max(OVERLAY_MIN_W, start.w + dw));
  const h = Math.min(room, Math.max(OVERLAY_MIN_H, start.h + dh));
  return { ...start, x: west ? start.x + start.w - w : start.x, y: north ? start.y + start.h - h : start.y, w, h };
}
