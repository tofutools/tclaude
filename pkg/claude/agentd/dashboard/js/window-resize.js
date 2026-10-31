// window-resize.js -- the edges and corners of a floating window. The companion
// and the board overlay share these, so both resize as a desktop window does.
//
// The zones sit in a frame beside the window rather than inside it. A window
// clips its content to its rounded corners, which would cut off the half of
// each zone that straddles the edge. The frame covers only those strips and
// lets every other pointer through to the window.

import { h } from 'preact';
import htm from 'htm';
import { COMPANION_FOOTER_ROOM, clampPlacement } from './companion-model.js';

const html = htm.bind(h);

// Corners come last: they are drawn over the edges they touch.
const RESIZE_ZONES = ['n', 's', 'e', 'w', 'nw', 'ne', 'sw', 'se'];

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
  const roomH = north ? bottom : south ? viewport.height - COMPANION_FOOTER_ROOM - start.y : sized.h;
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

export function ResizeFrame({ placement, className, onResizeStart }) {
  return html`<div class=${`window-resize-frame ${className}`} aria-hidden="true"
    style=${`left:${placement.x}px;top:${placement.y}px;width:${placement.w}px;height:${placement.h}px`}>
    ${RESIZE_ZONES.map((zone) => html`<div key=${zone} class=${`window-resize-zone ${zone}`}
      onPointerDown=${(event) => onResizeStart(event, zone)}></div>`)}
  </div>`;
}
