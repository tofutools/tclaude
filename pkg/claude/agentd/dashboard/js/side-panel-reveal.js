// side-panel-reveal.js -- a folded side panel floats in when the pointer
// reaches the right edge, and goes again when the pointer leaves it.
//
// Only for a panel that opted in (registerSidePanel's autoReveal: the Groups
// palette and the Collab list) and only while it is folded: an unfolded panel
// behaves as before, and the handle stays the way to unfold (pin) it, and the
// keyboard and screen-reader way in. The reveal is a pointer convenience:
//
//  - It needs a fine, hovering pointer, no button held and no drag under way,
//    and a short dwell in the edge zone, so a quick pass to the scrollbar or
//    the end of a drag does not pop it.
//  - It floats over the content (body.dock-revealed); nothing reflows.
//  - It goes the standard hover-out delay after the pointer leaves it, but
//    not while it holds focus, while one of its menus is open, or while a drag
//    that started in it is under way. Escape sends it away.
//  - config dashboard.side_panel_edge_reveal (default on) turns it off.

import { HOVER_CLOSE_MS } from './popover-watch.js';
import { canRevealSidePanel, concealSidePanel, isSidePanelRevealed, revealSidePanel } from './side-panel.js';

// EDGE_REVEAL_PX is how close to the right edge the pointer has to be.
export const EDGE_REVEAL_PX = 10;
// EDGE_REVEAL_DWELL_MS is how long it has to stay there.
export const EDGE_REVEAL_DWELL_MS = 150;

let enabled = true;

// setEdgeRevealEnabled follows config dashboard.side_panel_edge_reveal; off,
// a floating panel goes at once.
export function setEdgeRevealEnabled(on, doc = globalThis.document) {
  enabled = on !== false;
  if (!enabled) concealSidePanel(doc);
}

// openMenu is a menu the panel has open: a menu button inside it showing its
// menu, or any menu in the page (a row's context menu is placed outside the
// panel, and only the panel's pointer could have opened it while it floats).
function openMenu(dock, doc) {
  return !!dock?.querySelector('[aria-haspopup][aria-expanded="true"]')
    || [...doc.querySelectorAll('[role="menu"]')].some((menu) => !menu.hidden);
}

// bindEdgeReveal watches the pointer for the reveal. timers and finePointer
// are seams for tests.
export function bindEdgeReveal(doc = globalThis.document, {
  timers = globalThis,
  finePointer = () => !!(doc.defaultView || globalThis).matchMedia?.('(hover: hover) and (pointer: fine)')?.matches,
} = {}) {
  const win = doc.defaultView || globalThis;
  const dock = () => doc.getElementById('agent-dock');
  let dwell = null;
  let hide = null;
  let dragging = false; // an HTML5 drag anywhere
  let dragFromPanel = false;
  let overPanel = false;

  const cancelDwell = () => {
    if (dwell) timers.clearTimeout(dwell);
    dwell = null;
  };
  const cancelHide = () => {
    if (hide) timers.clearTimeout(hide);
    hide = null;
  };
  const held = () => {
    const panel = dock();
    return dragFromPanel || !!panel?.contains(doc.activeElement) || openMenu(panel, doc);
  };
  // scheduleHide sends a floating panel away after the hover-out delay, unless
  // it is held then; whatever ends the hold (the pointer moving on, focus
  // leaving, the drag ending) asks again.
  const scheduleHide = () => {
    if (!isSidePanelRevealed() || hide) return;
    hide = timers.setTimeout(() => {
      hide = null;
      if (!overPanel && !held()) concealSidePanel(doc);
    }, HOVER_CLOSE_MS);
  };
  const inEdgeZone = (event) => {
    if (event.clientX < win.innerWidth - EDGE_REVEAL_PX) return false;
    const rect = dock()?.getBoundingClientRect?.();
    return !rect?.height || (event.clientY >= rect.top && event.clientY <= rect.bottom);
  };

  doc.addEventListener('pointermove', (event) => {
    if (event.pointerType && event.pointerType !== 'mouse') return;
    if (isSidePanelRevealed()) {
      overPanel = !!dock()?.contains(event.target);
      if (overPanel) cancelHide();
      else scheduleHide();
      return;
    }
    if (!enabled || event.buttons || dragging || !inEdgeZone(event) || !finePointer() || !canRevealSidePanel(doc)) {
      cancelDwell();
      return;
    }
    if (!dwell) {
      dwell = timers.setTimeout(() => {
        dwell = null;
        if (enabled && !dragging) revealSidePanel(doc);
      }, EDGE_REVEAL_DWELL_MS);
    }
  });
  doc.addEventListener('pointerdown', cancelDwell);
  doc.documentElement?.addEventListener('pointerleave', () => {
    cancelDwell();
    overPanel = false;
    scheduleHide();
  });
  doc.addEventListener('dragstart', (event) => {
    dragging = true;
    cancelDwell();
    dragFromPanel = isSidePanelRevealed() && !!dock()?.contains(event.target);
  });
  doc.addEventListener('dragend', () => {
    dragging = false;
    if (!dragFromPanel) return;
    dragFromPanel = false;
    scheduleHide();
  });
  doc.addEventListener('focusout', (event) => {
    if (dock()?.contains(event.target) && !overPanel) scheduleHide();
  });
  doc.addEventListener('keydown', (event) => {
    if (event.key !== 'Escape' || event.defaultPrevented || !isSidePanelRevealed() || openMenu(dock(), doc)) return;
    cancelHide();
    concealSidePanel(doc);
  });
}
