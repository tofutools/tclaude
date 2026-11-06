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
//    or a press (its scrollbar's) that started on it is under way. Escape, its
//    Hide button, unfolding it, a tab switch and the config switch send it
//    away at once; nothing else skips the delay.
//  - "On it" is geometric as well as the DOM: the page's own scrollbar sits
//    to the right of the panel, under the edge zone, and a pointer there
//    targets <html>, not the panel. Without that, the pointer that revealed
//    the panel was never over it, and it went and came back in a loop.
//  - A pointer heading for the edge fast peeks the panel first (tcl-o4nims):
//    side-panel-predict.js projects where the motion would stop, and when that
//    is at or past the edge zone, only the panel's leading edge slides in
//    (body.dock-peeking). Reaching that strip reveals the panel at once;
//    reaching the edge zone beside it (the page scrollbar) takes the usual
//    dwell, which a press still cancels, so a flick to grab the scrollbar is
//    no worse than without the peek. Sustained motion keeps the peek; when the
//    prediction stops holding it retracts after PEEK_RETRACT_MS. A press, a
//    drag, Escape or a tab switch takes it back at once. The peek is its own
//    state with its own timer, never a reveal: it is never "over" and holds
//    nothing, so the reveal's hover-out rules (and their scrollbar loop) do
//    not run for it, and it decides nothing from the panel's box, which is
//    mid-slide while it peeks.
//  - A debug overlay draws each prediction (side-panel-predict-debug.js):
//    ?side_panel_predict_debug=1 in the URL, or localStorage
//    tclaude.dash.sidePanelPredictDebug = "1".
//  - config dashboard.side_panel_edge_reveal (default on) turns it off, peek
//    included.

import { HOVER_CLOSE_MS } from './popover-watch.js';
import {
  canRevealSidePanel, concealSidePanel, isSidePanelPeeking, isSidePanelRevealed, peekSidePanel, revealSidePanel, unpeekSidePanel,
} from './side-panel.js';
import { PREDICT, predictEdgeLanding, recentSamples } from './side-panel-predict.js';
import { mountPredictDebug, predictDebug, predictDebugOn } from './side-panel-predict-debug.js';

// EDGE_REVEAL_PX is how close to the right edge the pointer has to be. It is
// measured from the content's edge (left of any page scrollbar), so the
// scrollbar strip always counts on top, and 32px is a target a fast flick can
// land on without taking a wide band of the page's right-hand content.
export const EDGE_REVEAL_PX = 32;
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

// bindEdgeReveal watches the pointer for the reveal and the peek. timers,
// now and finePointer are seams for tests; constants is the prediction's
// (PREDICT unless a test tunes it).
export function bindEdgeReveal(doc = globalThis.document, {
  timers = globalThis,
  now = () => (globalThis.performance?.now?.() ?? Date.now()),
  finePointer = () => !!(doc.defaultView || globalThis).matchMedia?.('(hover: hover) and (pointer: fine)')?.matches,
  constants = PREDICT,
  debug = null,
} = {}) {
  const win = doc.defaultView || globalThis;
  const dock = () => doc.getElementById('agent-dock');
  let dwell = null;
  let hide = null;
  let dragging = false; // an HTML5 drag anywhere
  let dragFromPanel = false;
  let overPanel = false;
  let pressFromPanel = false; // a button pressed on it (its scrollbar's) and not yet released
  let samples = []; // recent pointer moves, for the prediction
  let retract = null; // a peek's retract timer
  const debugOn = debug ?? predictDebugOn(win);
  if (debugOn) mountPredictDebug(doc);

  const cancelDwell = () => {
    if (dwell) timers.clearTimeout(dwell);
    dwell = null;
  };
  const cancelHide = () => {
    if (hide) timers.clearTimeout(hide);
    hide = null;
  };
  const cancelRetract = () => {
    if (retract) timers.clearTimeout(retract);
    retract = null;
  };
  // endPeek takes a peek back at once (a press, a drag, Escape).
  const endPeek = () => {
    cancelRetract();
    samples = [];
    unpeekSidePanel(doc);
  };
  // armRetract (re)starts the peek's retract: a prediction that still holds
  // restarts it, so sustained motion keeps the peek; one that stops holding
  // leaves it running out.
  const armRetract = () => {
    cancelRetract();
    retract = timers.setTimeout(() => {
      retract = null;
      unpeekSidePanel(doc);
    }, constants.PEEK_RETRACT_MS);
  };
  // revealFromPeek: the pointer reached the peeking strip, so the prediction
  // was right and the panel comes in whole, with no dwell.
  const revealFromPeek = () => {
    cancelRetract();
    samples = [];
    revealSidePanel(doc);
  };
  const held = () => {
    const panel = dock();
    const focused = doc.activeElement;
    return dragFromPanel || pressFromPanel || (!!panel?.contains(focused) && typing(focused)) || openMenu(panel, doc);
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
  // contentRight is the right edge of the page's content: innerWidth less any
  // page scrollbar.
  const contentRight = () => doc.documentElement?.clientWidth || win.innerWidth;
  const inEdgeZone = (event) => {
    if (event.clientX < contentRight() - EDGE_REVEAL_PX) return false;
    const rect = dock()?.getBoundingClientRect?.();
    return !rect?.height || (event.clientY >= rect.top && event.clientY <= rect.bottom);
  };

  // onPanel is whether the pointer is on the floating panel: on one of its
  // elements, or on the page scrollbar beside it. A pointer on the page
  // scrollbar targets <html>, so only that target counts in the panel's band
  // (its left edge to the window's right edge); a dialog or popover drawn over
  // the band is not the panel.
  const onPanel = (event) => {
    const panel = dock();
    if (panel?.contains(event.target)) return true;
    if (event.target !== doc.documentElement) return false;
    const rect = panel?.getBoundingClientRect?.();
    if (!rect?.width) return false;
    return event.clientX >= rect.left && event.clientY >= rect.top && event.clientY <= rect.bottom;
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
    // A move with no button held ends a press, even one whose pointerup the
    // page never saw (a scrollbar drag released elsewhere).
    if (!event.buttons) pressFromPanel = false;
    if (isSidePanelRevealed()) {
      samples = [];
      overPanel = onPanel(event);
      if (overPanel) cancelHide();
      else scheduleHide();
      return;
    }
    // The folded handle sits in the edge zone: aiming at it must not slide the
    // panel in under the click.
    const onHandle = !!doc.getElementById('dock-toggle')?.contains(event.target);
    const allowed = enabled && !event.buttons && !dragging && !onHandle && finePointer() && canRevealSidePanel(doc);
    if (!allowed) {
      cancelDwell();
      if (isSidePanelPeeking()) endPeek();
      samples = [];
      return;
    }
    if (isSidePanelPeeking() && dock()?.contains(event.target)) {
      cancelDwell();
      revealFromPeek();
      return;
    }
    if (!inEdgeZone(event)) {
      cancelDwell();
      predict(event);
      return;
    }
    // In the edge zone the dwell decides, peeking or not; a peek waits for it
    // rather than running out under it.
    if (isSidePanelPeeking()) cancelRetract();
    if (!dwell) {
      dwell = timers.setTimeout(() => {
        dwell = null;
        if (enabled && !dragging) revealSidePanel(doc);
      }, EDGE_REVEAL_DWELL_MS);
    }
  });
  // predict records the move and peeks, keeps or lets go of a peek. Only moves
  // the reveal would act on count, and the band is the panel's vertical
  // extent, which a peek's slide does not change.
  const predict = (event) => {
    const t = now();
    samples = recentSamples([...samples, { t, x: event.clientX, y: event.clientY }], t, constants);
    const rect = dock()?.getBoundingClientRect?.();
    const band = rect?.height ? { top: rect.top, bottom: rect.bottom } : null;
    const edgeX = contentRight() - EDGE_REVEAL_PX;
    const prediction = predictEdgeLanding(samples, edgeX, band, constants);
    if (debugOn) predictDebug.value = { prediction, edgeX };
    if (!prediction.peek) {
      if (isSidePanelPeeking() && !retract) armRetract();
      return;
    }
    peekSidePanel(doc);
    if (isSidePanelPeeking()) armRetract();
  };

  // A press cancels a reveal still waiting out its dwell, and takes a peek
  // back, unless it is on the peeking strip, which reveals the panel. It
  // closes nothing else, so it is not the shared outside-press rule
  // (ui-popover-dismiss.test.mjs).
  doc.addEventListener('pointerdown', (event) => {
    cancelDwell();
    if (isSidePanelPeeking()) {
      if (dock()?.contains(event.target) && !doc.getElementById('dock-toggle')?.contains(event.target)) revealFromPeek();
      else endPeek();
    }
    if (isSidePanelRevealed() && onPanel(event)) pressFromPanel = true;
  }, true);
  const pressOver = () => {
    if (!pressFromPanel) return;
    pressFromPanel = false;
    if (!overPanel) scheduleHide();
  };
  doc.addEventListener('pointerup', pressOver, true);
  doc.addEventListener('pointercancel', pressOver, true);
  doc.documentElement?.addEventListener('pointerleave', () => {
    cancelDwell();
    samples = [];
    if (isSidePanelPeeking() && !retract) armRetract();
    overPanel = false;
    scheduleHide();
  });
  doc.addEventListener('dragstart', (event) => {
    dragging = true;
    cancelDwell();
    if (isSidePanelPeeking()) endPeek();
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
  panel?.addEventListener('pointerenter', (event) => {
    if (isSidePanelPeeking()) {
      if (!event.buttons && !doc.getElementById('dock-toggle')?.contains(event.target)) revealFromPeek();
      return;
    }
    if (!isSidePanelRevealed()) return;
    overPanel = true;
    cancelHide();
  });
  doc.addEventListener('focusout', (event) => {
    if (dock()?.contains(event.target) && !overPanel) scheduleHide();
  });
  doc.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && !event.defaultPrevented && isSidePanelPeeking()) {
      endPeek();
      return;
    }
    if (event.key !== 'Escape' || event.defaultPrevented || !isSidePanelRevealed() || openMenu(dock(), doc)) return;
    cancelHide();
    concealSidePanel(doc);
  });
}
