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

// shownMenu is a menu actually on screen. The page keeps closed menus in the
// DOM, hidden by a class (.action-menu:not(.open)) rather than the hidden
// attribute, so only one that lays out counts.
function shownMenu(menu) {
  if (menu.hidden || menu.closest?.('[hidden]')) return false;
  return menu.classList.contains('open') || (menu.getClientRects?.().length || 0) > 0;
}

// openMenu is a menu the panel has open: a menu button inside it showing its
// menu, or any menu on screen (a row's context menu is placed outside the
// panel, and only the panel's pointer could have opened it while it floats).
function openMenu(dock, doc) {
  return !!dock?.querySelector('[aria-haspopup][aria-expanded="true"]')
    || [...doc.querySelectorAll('[role="menu"]')].some(shownMenu);
}

// typing is a text-entry control: focus there (the Find box) holds the panel,
// focus a click left on a button does not.
function typing(el) {
  if (el?.isContentEditable) return true;
  const tag = el?.tagName;
  if (tag === 'TEXTAREA' || tag === 'SELECT') return true;
  return tag === 'INPUT' && !/^(button|checkbox|color|file|hidden|image|radio|range|reset|submit)$/.test(el.type || '');
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
    const focused = doc.activeElement;
    return dragFromPanel || (!!panel?.contains(focused) && typing(focused)) || openMenu(panel, doc);
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

  // No pointer events fire during an HTML5 drag, so a pointermove means any
  // drag is over, even one whose dragend never reached the document because
  // the drop re-rendered its source.
  const dragOver = () => {
    if (!dragging) return;
    dragging = false;
    if (!dragFromPanel) return;
    dragFromPanel = false;
    scheduleHide();
  };

  doc.addEventListener('pointermove', (event) => {
    dragOver();
    if (event.pointerType && event.pointerType !== 'mouse') return;
    if (isSidePanelRevealed()) {
      overPanel = !!dock()?.contains(event.target);
      if (overPanel) cancelHide();
      else scheduleHide();
      return;
    }
    // The folded handle sits in the edge zone: aiming at it must not slide the
    // panel in under the click.
    const onHandle = !!doc.getElementById('dock-toggle')?.contains(event.target);
    if (!enabled || event.buttons || dragging || onHandle || !inEdgeZone(event) || !finePointer() || !canRevealSidePanel(doc)) {
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
  doc.addEventListener('dragend', dragOver);
  doc.addEventListener('drop', dragOver, true);
  // The pointer can leave the panel straight into a frame (a page or a board),
  // after which the page sees no pointermove: the panel's own leave says so.
  const panel = dock();
  panel?.addEventListener('pointerleave', () => {
    overPanel = false;
    scheduleHide();
  });
  panel?.addEventListener('pointerenter', () => {
    if (!isSidePanelRevealed()) return;
    overPanel = true;
    cancelHide();
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
