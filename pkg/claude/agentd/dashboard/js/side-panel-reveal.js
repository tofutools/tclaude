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
//  - It floats over the content (body.dock-revealed); the page does not make
//    room for it. The Groups toolbar's global controls move into its head
//    while it is up, as they do when it is pinned (dock-actions.js).
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
//    (body.dock-peeking). Moving onto that strip reveals the panel at once;
//    the page scrollbar beside it, the one part of the edge zone the strip
//    does not cover, takes the usual dwell, which a press still cancels, so a
//    flick to grab the scrollbar is no worse than without the peek. A press on a strip
//    that is still only peeking means it slid under a still pointer, a wrong
//    prediction: the press ends the peek and is swallowed, so it acts on
//    nothing in the panel. Sustained motion keeps the peek; when the
//    prediction stops holding it retracts after PEEK_RETRACT_MS. A press, a
//    drag, Escape or a tab switch takes it back at once. The peek is its own
//    state with its own timer, never a reveal: it is never "over" and holds
//    nothing, so the reveal's hover-out rules (and their scrollbar loop) do
//    not run for it, and it decides nothing from the panel's box, which is
//    mid-slide while it peeks.
//  - The peek grows (v2, tcl-wdwbm2): from PEEK_MIN_PX it widens with the
//    pointer's progress towards where the open panel's left edge would be,
//    up to the panel's full width. A move that crosses that edge while the
//    prediction holds, and is not braking at all, reveals the panel; so does
//    a stop (no move for STOP_MS) inside the full panel's area or just left
//    of the peek. A braking move refuses the prediction, so a stop on content
//    under the would-be panel, which a pointer brakes into, does not reveal.
//    A prediction that stops holding shrinks the peek back to PEEK_MIN_PX
//    before it retracts. A motion is judged from where it started (the rest
//    point it left): the further out it starts the more rightward speed it
//    needs, and it has to have travelled MIN_TRAVEL_PX (side-panel-predict.js).
//    No band near the edge refuses outright (tcl-bg5qm1).
//  - A snap (tcl-bg5qm1): an arrival in the edge zone that travelled
//    ARRIVAL_MIN_TRAVEL_PX or more at the speed its start needs reveals the
//    panel at once, without the dwell; a snap is over in a few frames, too few
//    for the prediction. One that runs past the window's right edge counts
//    when its next frame would have reached the edge zone. A slow approach
//    still takes the dwell.
//  - The folded handle is no exception: resting on it takes the dwell like
//    anywhere in the edge zone. Pinning stays one click: on the handle before
//    the dwell ends, or on the revealed panel's handle.
//  - A debug overlay draws each prediction (side-panel-predict-debug.js):
//    ?side_panel_predict_debug=1 in the URL, or localStorage
//    tclaude.dash.sidePanelPredictDebug = "1".
//  - config dashboard.side_panel_edge_reveal (default on) turns it off, peek
//    included; dashboard.side_panel_predictive_peek and
//    dashboard.side_panel_growing_peek turn off the peek, or its growth, and
//    the other dashboard.side_panel_* values tune it (setSidePanelTuning).

import { HOVER_CLOSE_MS } from './popover-watch.js';
import {
  canRevealSidePanel, concealSidePanel, isSidePanelPeeking, isSidePanelRevealed, peekSidePanel, revealSidePanel, unpeekSidePanel,
} from './side-panel.js';
import {
  PREDICT, arrivalReveals, crossingHolds, peekWidthFor, predictEdgeLanding, recentSamples, stopReveals,
} from './side-panel-predict.js';
import { mountPredictDebug, predictDebug, predictDebugOn } from './side-panel-predict-debug.js';

// EDGE_REVEAL_PX is how close to the right edge the pointer has to be. It is
// measured from the content's edge (left of any page scrollbar), so the
// scrollbar strip always counts on top, and 32px is a target a fast flick can
// land on without taking a wide band of the page's right-hand content.
export const EDGE_REVEAL_PX = 32;
// EDGE_REVEAL_DWELL_MS is how long it has to stay there.
export const EDGE_REVEAL_DWELL_MS = 150;

let enabled = true;

// REVEAL_DEFAULTS is the whole tuning table: the prediction's (PREDICT) and
// the edge zone's, and the peek's two switches.
export const REVEAL_DEFAULTS = Object.freeze({
  ...PREDICT,
  EDGE_PX: EDGE_REVEAL_PX,
  DWELL_MS: EDGE_REVEAL_DWELL_MS,
  PREDICTIVE: true,
  GROWING: true,
});
let tuning = REVEAL_DEFAULTS;

// setSidePanelTuning follows the snapshot's side_panel_tuning (config
// dashboard.side_panel_*, defaulted and clamped by the daemon). Speeds there
// are px/s and the braking threshold a percent; absent values keep the
// defaults. Turning the peek off takes a peek back at once.
export function setSidePanelTuning(t, doc = globalThis.document) {
  const num = (v, scale = 1) => (typeof v === 'number' && Number.isFinite(v) ? v / scale : undefined);
  const pick = (key, v) => (v === undefined ? REVEAL_DEFAULTS[key] : v);
  const next = {
    ...REVEAL_DEFAULTS,
    PREDICTIVE: t?.predictive_peek !== false,
    GROWING: t?.growing_peek !== false,
    EDGE_PX: pick('EDGE_PX', num(t?.edge_zone_px)),
    DWELL_MS: pick('DWELL_MS', num(t?.dwell_ms)),
    PEEK_RETRACT_MS: pick('PEEK_RETRACT_MS', num(t?.retract_ms)),
    MIN_VX: pick('MIN_VX', num(t?.predict_min_speed, 1000)),
    RUNWAY_FULL_PX: pick('RUNWAY_FULL_PX', num(t?.predict_speed_distance_px)),
    MIN_VX_FLOOR: pick('MIN_VX_FLOOR', num(t?.predict_min_speed_floor, 1000)),
    MIN_SAMPLES: pick('MIN_SAMPLES', num(t?.predict_min_readings)),
    MIN_TRAVEL_PX: pick('MIN_TRAVEL_PX', num(t?.min_travel_px)),
    BRAKE_RATIO: pick('BRAKE_RATIO', num(t?.brake_percent, 100)),
    PEEK_MIN_PX: pick('PEEK_MIN_PX', num(t?.peek_min_px)),
  };
  tuning = Object.freeze(next);
  if (!tuning.PREDICTIVE && doc && isSidePanelPeeking()) {
    unpeekSidePanel(doc);
    doc.body?.style?.removeProperty?.('--dock-peek-w');
  }
}

// sidePanelTuning is the live tuning (setSidePanelTuning's result).
export function sidePanelTuning() {
  return tuning;
}

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
// now and finePointer are seams for tests; constants overrides the live
// tuning (setSidePanelTuning) where a test tunes it.
export function bindEdgeReveal(doc = globalThis.document, {
  timers = globalThis,
  now = () => (globalThis.performance?.now?.() ?? Date.now()),
  finePointer = () => !!(doc.defaultView || globalThis).matchMedia?.('(hover: hover) and (pointer: fine)')?.matches,
  constants = null,
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
  let last = null; // the last pointermove's position
  let swallowClick = false; // the click after a press a peek swallowed
  let motion = null; // where the current rightward motion started: { x, t }
  let lastT = 0; // when the last pointermove came
  let peekStartX = 0; // where the pointer was when the peek started
  let peekW = 0; // the peek's width now
  let stop = null; // a peek's stop timer
  const C = () => (constants ? { ...tuning, ...constants } : tuning);
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
  const cancelStop = () => {
    if (stop) timers.clearTimeout(stop);
    stop = null;
  };
  // setPeekWidth sizes the peek (body --dock-peek-w); 0 clears it.
  const setPeekWidth = (w) => {
    peekW = w;
    if (w) doc.body?.style?.setProperty?.('--dock-peek-w', `${w}px`);
    else doc.body?.style?.removeProperty?.('--dock-peek-w');
  };
  const unpeek = () => {
    cancelStop();
    unpeekSidePanel(doc);
    setPeekWidth(0);
  };
  // endPeek takes a peek back at once (a press, a drag, Escape).
  const endPeek = () => {
    cancelRetract();
    samples = [];
    unpeek();
  };
  // armRetract (re)starts the peek's retract: a prediction that still holds
  // restarts it, so sustained motion keeps the peek; one that stops holding
  // leaves it running out.
  const armRetract = () => {
    cancelRetract();
    retract = timers.setTimeout(() => {
      retract = null;
      unpeek();
    }, C().PEEK_RETRACT_MS);
  };
  // revealFromPeek: the pointer reached the peeking strip, crossed into the
  // full panel's area or stopped there, so the prediction was right and the
  // panel comes in whole, with no dwell.
  const revealFromPeek = () => {
    cancelRetract();
    cancelStop();
    samples = [];
    motion = null;
    revealSidePanel(doc);
    setPeekWidth(0);
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
    if (event.clientX < contentRight() - C().EDGE_PX) return false;
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
    // Chrome sends a move at the same position when the layout changes under
    // a still pointer (the strip sliding in): that is not the pointer going
    // anywhere.
    const moved = !last || last.x !== event.clientX || last.y !== event.clientY;
    const t = now();
    // The motion restarts at any leftward move (so a rightward one starts from
    // its low point) and after a pause (from where it rested, a frame before
    // this move).
    if (!motion || !last || event.clientX < last.x) motion = { x: event.clientX, t };
    else if (t - lastT > C().WINDOW_MS) motion = { x: last.x, t: t - C().FRAME_MS };
    last = { x: event.clientX, y: event.clientY };
    lastT = t;
    if (isSidePanelRevealed()) {
      samples = [];
      motion = null;
      overPanel = onPanel(event);
      if (overPanel) cancelHide();
      else scheduleHide();
      return;
    }
    const allowed = enabled && !event.buttons && !dragging && finePointer() && canRevealSidePanel(doc);
    if (!allowed) {
      cancelDwell();
      if (isSidePanelPeeking()) endPeek();
      samples = [];
      return;
    }
    if (isSidePanelPeeking() && dock()?.contains(event.target)) {
      cancelDwell();
      if (moved) revealFromPeek();
      return;
    }
    if (!inEdgeZone(event)) {
      cancelDwell();
      predict(event);
      return;
    }
    // A snap arriving here reveals at once; otherwise the dwell decides,
    // peeking or not, and a peek waits for it rather than running out under
    // it, or stopping it short.
    if (snapArrived(event.clientX, t)) {
      cancelDwell();
      revealNow();
      return;
    }
    if (isSidePanelPeeking()) {
      cancelRetract();
      cancelStop();
    }
    if (!dwell) {
      dwell = timers.setTimeout(() => {
        dwell = null;
        if (enabled && !dragging) revealSidePanel(doc);
      }, C().DWELL_MS);
    }
  });
  // fullLeftNow is where the open panel's left edge would be: the dock keeps
  // its full width while folded (only translated).
  const fullLeftNow = () => {
    const width = dock()?.getBoundingClientRect?.()?.width || 0;
    const right = contentRight();
    return width ? right - width : right - C().EDGE_PX;
  };
  // snapArrived is whether the motion that brought the pointer to x at t is a
  // snap (arrivalReveals).
  const snapArrived = (x, t) => !!motion && arrivalReveals({ startX: motion.x, startT: motion.t, x, t, fullLeft: fullLeftNow() }, C());
  const revealNow = () => {
    cancelRetract();
    cancelStop();
    samples = [];
    motion = null;
    revealSidePanel(doc);
    setPeekWidth(0);
    if (debugOn) predictDebug.value = { prediction: { peek: true, reason: 'snap', vx: 0, px: 0, landingX: NaN }, edgeX: contentRight() - C().EDGE_PX, fullLeft: fullLeftNow() };
  };
  // predict records the move and peeks, grows, keeps or lets go of a peek.
  // Only moves the reveal would act on count, and the band is the panel's
  // vertical extent, which a peek's slide does not change. The motion's
  // motion restarts when it turns left or pauses.
  const predict = (event) => {
    const c = C();
    if (!c.PREDICTIVE) return;
    const t = now();
    const x = event.clientX;
    const y = event.clientY;
    const prev = samples[samples.length - 1];
    // A turn or a pause starts a new motion: its old moves are not this one's.
    if (prev && (x < prev.x || t - prev.t > c.WINDOW_MS)) samples = [];
    samples = recentSamples([...samples, { t, x, y }], t, c);
    const rect = dock()?.getBoundingClientRect?.();
    const band = rect?.height ? { top: rect.top, bottom: rect.bottom } : null;
    const right = contentRight();
    const edgeX = right - c.EDGE_PX;
    // The dock keeps its full width while folded (only translated), so its
    // width says where the open panel's left edge would be.
    const panelW = rect?.width || 0;
    const fullLeft = panelW ? right - panelW : edgeX;
    const prediction = predictEdgeLanding(samples, edgeX, band, c, { x: motion?.x ?? x, fullLeft });
    if (debugOn) predictDebug.value = { prediction, edgeX, fullLeft };
    if (!prediction.peek) {
      cancelStop();
      if (isSidePanelPeeking()) {
        if (peekW > c.PEEK_MIN_PX) setPeekWidth(c.PEEK_MIN_PX);
        if (!retract) armRetract();
      }
      return;
    }
    const inBand = !band || (y >= band.top && y <= band.bottom);
    // The growing peek's reveals (crossing, stopping) need a move that is not
    // braking at all: content under the would-be panel is what a braking
    // move there is aimed at.
    const steady = c.GROWING && crossingHolds(prediction, c);
    if (steady && inBand && x >= fullLeft) {
      revealFromPeek();
      return;
    }
    if (!isSidePanelPeeking()) {
      peekStartX = x;
      peekW = 0;
    }
    const width = c.GROWING ? peekWidthFor({ startX: peekStartX, x, fullLeft, edgeX, panelW, prev: peekW }, c) : c.PEEK_MIN_PX;
    setPeekWidth(panelW ? Math.min(width, panelW) : width);
    peekSidePanel(doc);
    if (!isSidePanelPeeking()) {
      setPeekWidth(0);
      return;
    }
    armRetract();
    // A stop right after a steady move: one inside the full panel's area, or
    // just short of the peek, means the pointer got where it was going.
    cancelStop();
    if (!steady) return;
    stop = timers.setTimeout(() => {
      stop = null;
      if (!isSidePanelPeeking() || !last) return;
      const stillInBand = !band || (last.y >= band.top && last.y <= band.bottom);
      if (stillInBand && stopReveals(last.x, fullLeft, right - peekW, c)) revealFromPeek();
    }, c.STOP_MS);
  };

  // A press cancels a reveal still waiting out its dwell, and takes a peek
  // back. One on the peeking strip is swallowed with its click: the strip
  // slid under a still pointer, and the press was meant for what was there.
  // It closes nothing else, so it is not the shared outside-press rule
  // (ui-popover-dismiss.test.mjs).
  doc.addEventListener('pointerdown', (event) => {
    cancelDwell();
    swallowClick = false;
    if (isSidePanelPeeking()) {
      if (dock()?.contains(event.target)) {
        event.preventDefault();
        event.stopPropagation();
        swallowClick = true;
      }
      endPeek();
    }
    if (isSidePanelRevealed() && onPanel(event)) pressFromPanel = true;
  }, true);
  const pressOver = () => {
    if (!pressFromPanel) return;
    pressFromPanel = false;
    if (!overPanel) scheduleHide();
  };
  doc.addEventListener('click', (event) => {
    if (!swallowClick) return;
    swallowClick = false;
    event.preventDefault();
    event.stopPropagation();
  }, true);
  doc.addEventListener('pointerup', pressOver, true);
  doc.addEventListener('pointercancel', pressOver, true);
  doc.documentElement?.addEventListener('pointerleave', () => {
    cancelDwell();
    cancelStop();
    // A snap that runs past the window's right edge, within the panel's
    // height, never sends a move inside the edge zone: it counts when its next
    // frame would have got there.
    const c = C();
    const rect = dock()?.getBoundingClientRect?.();
    const inBand = !rect?.height || (last && last.y >= rect.top && last.y <= rect.bottom);
    if (motion && last && inBand && enabled && !dragging && !isSidePanelRevealed() && finePointer() && canRevealSidePanel(doc)) {
      const speed = (last.x - motion.x) / Math.max(1, lastT - motion.t);
      const next = last.x + speed * c.FRAME_MS;
      const edgeX = contentRight() - c.EDGE_PX;
      if (next >= edgeX && snapArrived(next, lastT + c.FRAME_MS)) {
        revealNow();
        overPanel = false;
        scheduleHide();
        return;
      }
    }
    samples = [];
    motion = null;
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
  // A peek is never revealed by an enter alone: the strip sliding under a still
  // pointer enters it too. A real move onto it reveals (pointermove).
  panel?.addEventListener('pointerenter', () => {
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
