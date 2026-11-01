// overlay-window.js -- the one window every floating dashboard window sits in.
// The companion and the board overlay bring their content; this owns the rest:
// where the window is (placement and persistence), dragging, resizing from any
// edge or corner, edge snapping and maximize, minimize and open animations, the
// native-look controls, which window is in front, and handing focus back after
// a click on its chrome.
//
//   useOverlayWindow   the state and the gestures, one call per window
//   OverlayWindow      the DOM: snap guide, window, title bar, resize frame
//
// See overlay-window-model.js for the geometry.
//
// The resize zones sit in a frame beside the window rather than inside it. A
// window clips its content to its rounded corners, which would cut off the half
// of each zone that straddles the edge. The frame covers only those strips and
// lets every other pointer through to the window.

import { h } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import { signal } from '@preact/signals';
import htm from 'htm';
import {
  clampPlacement, fitPlacement, parsePlacement, resizedPlacement, savedAfterResize, snappedResize, snapZone, viewportOf, zoneRect,
} from './overlay-window-model.js';
import { WindowControls } from './overlay-window-controls.js';

const html = htm.bind(h);

// Corners come last: they are drawn over the edges they touch.
const RESIZE_ZONES = ['n', 's', 'e', 'w', 'nw', 'ne', 'sw', 'se'];
const TEAR_OFF_PX = 4;

// The id of the window in front. Every window shares one z-index layer, where
// DOM order decides, so the companion is in front until another is raised. A
// window with lifts steps above that layer when it is the one in front.
export const frontOverlayWindow = signal('');

export function raiseOverlayWindow(id) {
  frontOverlayWindow.value = id;
}

// snapTopOf is where a snapped side panel starts: just below the nav bar.
function snapTopOf(win) {
  try {
    const bottom = win?.document?.querySelector?.('nav')?.getBoundingClientRect?.().bottom;
    return Number.isFinite(bottom) && bottom > 0 ? Math.round(bottom) : 0;
  } catch {
    return 0;
  }
}

export function reducedMotion(win) {
  try {
    return !!win?.matchMedia?.('(prefers-reduced-motion: reduce)')?.matches;
  } catch {
    return false;
  }
}

export function readPref(prefs, key) {
  try {
    return prefs?.getItem?.(key) ?? null;
  } catch {
    return null;
  }
}

export function writePref(prefs, key, value) {
  try {
    prefs?.setItem?.(key, value);
  } catch { /* prefs unavailable: the session keeps working */ }
}

// useOverlayWindow runs one window. prefKey is where its placement is saved and
// defaultPlacement(viewport) is where it goes before anything is. Without snaps
// it only floats. spotted(base, viewport, top) may name a place a floating
// window opens at instead of its saved one, onPlaced hears that the operator
// placed the window themselves, and onRefocus is asked to take the keyboard
// back after a click on the window's chrome. open is false while the window is
// minimized.
export function useOverlayWindow({ win, prefs, prefKey, defaultPlacement, snaps = false, open = true, spotted = null, onPlaced = null, onRefocus = null }) {
  const windowRef = useRef(null);
  // The rectangle the window grows out of when it next opens.
  const openFrom = useRef(null);
  // The cleanup of an in-flight drag or resize, run if the window unmounts.
  const gesture = useRef(null);
  const panelBeforeMaximize = useRef(null);
  const saved = () => parsePlacement(readPref(prefs, prefKey));
  const snapTop = () => snapTopOf(win);

  function floatingPlacement() {
    const viewport = viewportOf(win);
    const base = { ...(saved() || defaultPlacement(viewport)), snap: '' };
    return spotted?.(base, viewport, snapTop()) || fitPlacement(base, viewport);
  }

  const [placement, setPlacement] = useState(() => {
    const zone = snaps ? saved()?.snap : '';
    return zone ? zoneRect(zone, viewportOf(win), snapTop()) : floatingPlacement();
  });
  const [guide, setGuide] = useState(null);
  useEffect(() => () => gesture.current?.(), []);

  // Reopen where the operator left it, if that still fits.
  useLayoutEffect(() => {
    if (open) setPlacement((p) => (p.snap ? p : floatingPlacement()));
  }, [open]);

  // Grow the window out of the rectangle it was opened from.
  useLayoutEffect(() => {
    if (!open) return;
    const from = openFrom.current;
    openFrom.current = null;
    const node = windowRef.current;
    if (!from || !node || reducedMotion(win) || typeof node.animate !== 'function') return;
    // The effect above only queued the move, so the node isn't there yet.
    const target = placement.snap ? placement : floatingPlacement();
    const to = { left: target.x, top: target.y, width: target.w, height: target.h };
    if (!to.width || !to.height) return;
    node.animate([
      { transform: `translate(${from.left - to.left}px, ${from.top - to.top}px) scale(${from.width / to.width}, ${from.height / to.height})`, opacity: 0.5, borderRadius: '20px' },
      { transform: 'none', opacity: 1, borderRadius: '10px' },
    ], { duration: 180, easing: 'cubic-bezier(.2,.8,.2,1)' });
  }, [open]);

  // Keep a floating window on screen when the browser resizes, working from the
  // saved placement so a larger browser restores what a smaller one shrank.
  useEffect(() => {
    const onResize = () => {
      panelBeforeMaximize.current = null;
      setPlacement((p) => (p.snap ? zoneRect(p.snap, viewportOf(win), snapTop()) : floatingPlacement()));
    };
    win?.addEventListener?.('resize', onResize);
    return () => win?.removeEventListener?.('resize', onResize);
  }, [win]);

  // A move keeps the saved size: the window may only be small for this viewport.
  function withSavedSize(p) {
    const size = saved();
    return size ? { ...p, w: size.w, h: size.h } : p;
  }

  function savePlacement(p) {
    writePref(prefs, prefKey, JSON.stringify(p));
  }

  function saveMovedWindow(p) {
    savePlacement(p);
    onPlaced?.();
  }

  function track(event, onMove, onEnd) {
    const handle = event.currentTarget;
    handle.setPointerCapture?.(event.pointerId);
    const stop = () => {
      handle.removeEventListener('pointermove', onMove);
      handle.removeEventListener('pointerup', up);
      handle.removeEventListener('pointercancel', up);
      gesture.current = null;
    };
    const up = () => {
      stop();
      onEnd();
    };
    handle.addEventListener('pointermove', onMove);
    handle.addEventListener('pointerup', up);
    handle.addEventListener('pointercancel', up);
    gesture.current = stop;
  }

  function onTitlePointerDown(event) {
    if (event.button !== 0 || event.target.closest?.('button, input, [role="tab"]')) return;
    if (!windowRef.current) return;
    event.preventDefault();
    let start = placement;
    const sx = event.clientX;
    const sy = event.clientY;
    let zone = '';
    let last = start;
    let moved = false;
    track(event, (m) => {
      if (start.snap) {
        // A click leaves a snapped window alone. Tearing it off takes a real
        // move, and restores a floating size under the pointer.
        if (Math.hypot(m.clientX - sx, m.clientY - sy) < TEAR_OFF_PX) return;
        const viewport = viewportOf(win);
        const floating = clampPlacement({ ...defaultPlacement(viewport), ...(saved() || {}), snap: '' }, viewport);
        start = { ...floating, x: sx - floating.w / 2, y: sy - 14, snap: '' };
      }
      moved = true;
      last = clampPlacement({ ...start, x: start.x + m.clientX - sx, y: start.y + m.clientY - sy, snap: '' }, viewportOf(win));
      setPlacement(last);
      if (!snaps) return;
      zone = snapZone({ x: m.clientX, y: m.clientY }, viewportOf(win));
      setGuide(zone ? zoneRect(zone, viewportOf(win), snapTop()) : null);
    }, () => {
      setGuide(null);
      if (zone) {
        setPlacement(zoneRect(zone, viewportOf(win), snapTop()));
        saveMovedWindow({ ...withSavedSize(last), snap: zone });
      } else if (moved) {
        // A click shows the fitted placement, which is not the operator's to keep.
        saveMovedWindow(withSavedSize(last));
      }
    });
  }

  function onResizePointerDown(event, zone = '') {
    if (event.button !== 0) return;
    event.preventDefault();
    event.stopPropagation();
    const start = placement;
    const sx = event.clientX;
    const sy = event.clientY;
    let last = start;
    let moved = false;
    track(event, (m) => {
      moved = true;
      const dx = m.clientX - sx;
      const dy = m.clientY - sy;
      // A snapped panel resizes along its free edge only.
      last = start.snap ? snappedResize(start, dx, dy, viewportOf(win)) : resizedPlacement(start, zone, dx, dy, viewportOf(win));
      setPlacement(last);
    }, () => {
      if (!start.snap && moved) saveMovedWindow(savedAfterResize(last, zone, saved()));
    });
  }

  // A mouse press leaves focus on the control, where the next key meant for the
  // window's content would press it again. Keyboard activation (detail 0) stays.
  function releaseFocus(event) {
    if (!event?.detail) return;
    event.currentTarget?.blur?.();
    onRefocus?.();
  }

  // A click on the title bar or other empty space leaves focus on the page.
  // Give it back to the content, so typing works and the border returns.
  // Controls keep their click, which may have opened a dialog, and a text
  // selection survives so it can be copied.
  function onWindowClick(event) {
    if (!onRefocus) return;
    if (event.target.closest?.('button, a, input, textarea, select, label')) return;
    if (win?.getSelection?.()?.isCollapsed === false) return;
    const active = win?.document?.activeElement;
    if (!active || active === win.document.body) onRefocus();
  }

  // Restore goes back to the panel the window was maximized from, or floating.
  // The pref keeps which edge that was; a resized panel's size lives only here.
  function toggleMaximize(event) {
    const { restoreSnap = '', ...rest } = saved() || placement;
    if (placement.snap === 'max') {
      const panel = panelBeforeMaximize.current;
      const next = !restoreSnap ? floatingPlacement()
        : panel?.snap === restoreSnap ? panel : zoneRect(restoreSnap, viewportOf(win), snapTop());
      setPlacement(next);
      savePlacement({ ...rest, snap: restoreSnap });
    } else {
      panelBeforeMaximize.current = placement.snap ? placement : null;
      setPlacement(zoneRect('max', viewportOf(win), snapTop()));
      savePlacement({ ...rest, snap: 'max', ...(placement.snap && { restoreSnap: placement.snap }) });
    }
    releaseFocus(event);
  }

  // shrinkTo plays the window down into a rectangle, then calls done. With
  // nowhere to go, or no animation wanted, it just calls done.
  function shrinkTo(rect, done) {
    const node = windowRef.current;
    if (!node || !rect || reducedMotion(win) || typeof node.animate !== 'function') {
      done();
      return;
    }
    const from = node.getBoundingClientRect();
    const animation = node.animate([
      { transform: 'none', opacity: 1 },
      { transform: `translate(${rect.left - from.left}px, ${rect.top - from.top}px) scale(${rect.width / from.width}, ${rect.height / from.height})`, opacity: 0.4 },
    ], { duration: 160, easing: 'cubic-bezier(.4,0,.8,.4)', fill: 'forwards' });
    animation.onfinish = () => { done(); animation.cancel(); };
  }

  return { placement, guide, windowRef, openFrom, onTitlePointerDown, onResizePointerDown, onWindowClick, releaseFocus, toggleMaximize, shrinkTo };
}

export function ResizeFrame({ placement, className, onResizeStart, onPointerDownCapture }) {
  return html`<div class=${`window-resize-frame ${className}`} aria-hidden="true" onPointerDownCapture=${onPointerDownCapture}
    style=${`left:${placement.x}px;top:${placement.y}px;width:${placement.w}px;height:${placement.h}px`}>
    ${RESIZE_ZONES.map((zone) => html`<div key=${zone} class=${`window-resize-zone ${zone}`}
      onPointerDown=${(event) => onResizeStart(event, zone)}></div>`)}
  </div>`;
}

// OverlayWindow draws what useOverlayWindow (the window prop) runs. id names it
// for stacking; a window last in the DOM passes lifts={false}, as it is already
// on top of its layer and whatever shares that layer keeps its place. mounted keeps a hidden window in the page, for content that
// must stay alive while minimized. lead and title are the title bar's content,
// and controls, { look, closeLabel, closeTitle, onMinimize, onClose }, adds the
// native buttons, which the classic look leaves to the content.
export function OverlayWindow({ id, window: overlay, lifts = true, open = true, mounted = open, className, frameClass, titlebarClass, label, lead = null, title, controls = null, children }) {
  const { placement, guide } = overlay;
  const front = lifts && frontOverlayWindow.value === id ? ' overlay-window-front' : '';
  const raise = () => raiseOverlayWindow(id);
  const { look, ...buttons } = controls || {};
  const windowControls = look && look !== 'classic' && html`<${WindowControls} look=${look} maximized=${placement.snap === 'max'}
    ...${buttons} onMaximize=${overlay.toggleMaximize} />`;
  return html`
    ${guide && html`<div class="overlay-snap-guide" aria-hidden="true" style=${`left:${guide.x}px;top:${guide.y}px;width:${guide.w}px;height:${guide.h}px`}></div>`}
    ${mounted && html`<section ref=${overlay.windowRef}
      class=${`overlay-window ${className}${placement.snap ? ` snapped snapped-${placement.snap}` : ''}${front}`}
      style=${`left:${placement.x}px;top:${placement.y}px;width:${placement.w}px;height:${placement.h}px`}
      role="dialog" aria-label=${label} aria-modal="false" aria-hidden=${open ? undefined : 'true'}
      onClick=${overlay.onWindowClick} onPointerDownCapture=${raise}>
      <header class=${titlebarClass} onPointerDown=${overlay.onTitlePointerDown}>
        ${look === 'macos' ? windowControls : lead}
        ${title}
        ${look !== 'macos' && windowControls}
      </header>
      ${children}
      ${placement.snap && placement.snap !== 'max' && html`<div class="overlay-window-edge" aria-hidden="true" onPointerDown=${(event) => overlay.onResizePointerDown(event)}></div>`}
    </section>`}
    ${open && !placement.snap && html`<${ResizeFrame} placement=${placement} className=${`${frameClass}${front}`}
      onResizeStart=${overlay.onResizePointerDown} onPointerDownCapture=${raise} />`}`;
}
