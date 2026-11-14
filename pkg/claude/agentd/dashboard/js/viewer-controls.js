// viewer-controls.js — the zoom gestures and display modes the attachment
// viewers (image and Markdown) share.
//
// Zoom has to answer to the inputs the operator's hands already know, not only
// to the viewer's own buttons:
//
//   - Ctrl+wheel, which is also what Chrome and Firefox report for a trackpad
//     pinch on macOS (a wheel event with ctrlKey set and small deltas);
//   - Safari's pinch, which arrives as the non-standard gesture* events
//     instead;
//   - Ctrl/Cmd + = - 0, and the bare + - 0 keys, while the viewer has focus;
//   - on a touch screen, a two-finger pinch and a double tap (useTouchZoom).
//
// Every one of those is ALSO a browser page-zoom gesture, so the handlers must
// be able to cancel it. That is why the wheel listener is installed by hand
// with `passive: false` rather than through an onWheel prop: Preact would
// register it passive-by-default in the browsers that make wheel listeners on
// the document passive, and preventDefault would then be ignored.
//
// Display modes: a dialog can be maximized (fill the window, keep the page
// around it) or put in true fullscreen through the Fullscreen API. Escape in
// fullscreen is the browser's: it leaves fullscreen, and the next Escape closes
// the viewer as usual.

import { h } from 'preact';
import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';

const html = htm.bind(h);

export function clamp(value, lo, hi) { return Math.min(hi, Math.max(lo, value)); }

// The multiplier one zoom step (a button or a key press) applies.
export const ZOOM_STEP = 1.25;

// wheelZoomFactor turns one Ctrl+wheel event into a multiplicative zoom.
//
// A trackpad pinch reports small pixel deltas many times a second; a mouse
// wheel notch reports ~100. The exponential keeps a pinch proportional to the
// fingers' travel, and the clamp keeps one notch to a single comfortable step
// instead of a jump.
export function wheelZoomFactor(event) {
  let delta = Number(event.deltaY) || 0;
  if (event.deltaMode === 1) delta *= 16; // lines
  else if (event.deltaMode === 2) delta *= 400; // pages
  return Math.exp(-clamp(delta, -30, 30) * 0.01);
}

// zoomKeyAction maps a keydown to 'in' | 'out' | 'reset', or null when the key
// is not a zoom key. Alt is left alone: it composes characters on macOS.
export function zoomKeyAction(event) {
  if (event.altKey) return null;
  switch (event.key) {
    case '+': case '=': return 'in';
    case '-': case '_': return 'out';
    case '0': return 'reset';
    default: return null;
  }
}

// useZoomGestures wires the zoom inputs above to one element (the dialog), for
// as long as `enabled` holds. onZoom(factor, point) gets a multiplier and the
// client point to keep still — null for keyboard zoom, which zooms about the
// middle. onKey(action) gets the keyboard actions, so a viewer can make
// 'reset' mean its own default.
//
// The keys are heard on the document, gated by shouldHandle (the viewer being
// the topmost overlay), not on the dialog: clicking a control that then
// unmounts or disables itself drops focus to <body>, and a dialog-scoped
// listener would hand the next Ctrl/Cmd + back to the browser's page zoom.
export function useZoomGestures({ targetRef, enabled, onZoom, onKey, shouldHandle = () => true }) {
  const zoomRef = useRef(onZoom);
  const keyRef = useRef(onKey);
  const handleRef = useRef(shouldHandle);
  zoomRef.current = onZoom;
  keyRef.current = onKey;
  handleRef.current = shouldHandle;

  useEffect(() => {
    const node = targetRef.current;
    if (!enabled || !node) return undefined;

    const onWheel = (event) => {
      if (!event.ctrlKey) return; // a plain wheel scrolls / pans as usual
      event.preventDefault();
      zoomRef.current?.(wheelZoomFactor(event), { x: event.clientX, y: event.clientY });
    };
    // Safari: event.scale is cumulative from gesturestart, so each change
    // applies only its ratio to the previous one.
    //
    // iOS Safari fires these for a touch pinch as well as the touch events
    // useTouchZoom zooms by; while fingers are down the touch path owns the
    // zoom, so a pinch is not applied twice. (A macOS trackpad pinch fires
    // no touch events.) The page zoom is still cancelled either way.
    let lastScale = 1;
    let fingers = 0;
    const onTouches = (event) => { fingers = event.touches?.length || 0; };
    const onGestureStart = (event) => { event.preventDefault(); lastScale = 1; };
    const onGestureChange = (event) => {
      event.preventDefault();
      const scale = Number(event.scale) || 1;
      if (fingers === 0) zoomRef.current?.(scale / lastScale, { x: event.clientX, y: event.clientY });
      lastScale = scale;
    };
    const onGestureEnd = (event) => { event.preventDefault(); };
    const onKeyDown = (event) => {
      if (event.defaultPrevented || !handleRef.current()) return;
      const action = zoomKeyAction(event);
      if (!action) return;
      event.preventDefault(); // Ctrl/Cmd + = would otherwise zoom the page
      keyRef.current?.(action);
    };

    node.addEventListener('wheel', onWheel, { passive: false });
    for (const type of ['touchstart', 'touchend', 'touchcancel']) node.addEventListener(type, onTouches, { passive: true });
    node.addEventListener('gesturestart', onGestureStart);
    node.addEventListener('gesturechange', onGestureChange);
    node.addEventListener('gestureend', onGestureEnd);
    document.addEventListener('keydown', onKeyDown);
    return () => {
      node.removeEventListener('wheel', onWheel, { passive: false });
      for (const type of ['touchstart', 'touchend', 'touchcancel']) node.removeEventListener(type, onTouches, { passive: true });
      node.removeEventListener('gesturestart', onGestureStart);
      node.removeEventListener('gesturechange', onGestureChange);
      node.removeEventListener('gestureend', onGestureEnd);
      document.removeEventListener('keydown', onKeyDown);
    };
  }, [enabled, targetRef]);
}

// Touch. A phone or touch screen has no Ctrl+wheel, and the browser's own
// pinch zooms the whole page instead of the image or text in the viewer (and
// on the desktop page, panning that zoomed page over the viewer's stage is
// then trapped by its contained scroll). So the viewer answers touch itself:
//
//   - two fingers pinch-zoom about their midpoint (each move applies the
//     ratio of the fingers' new span to their last one);
//   - a double tap toggles the zoom at the tapped point, as a double click
//     does with a mouse;
//   - one finger is left to the browser: it scrolls the stage natively, with
//     momentum, which pans a zoomed image or scrolls a document.
//
// Touch events, not pointer events: a non-passive touchmove can cancel the
// browser's two-finger gesture while one-finger scrolling stays native. The
// stage also sets touch-action: pan-x pan-y, which keeps the browser from
// pinch-zooming or double-tap-zooming the page over it.

export const DOUBLE_TAP_MS = 300;
// How far a tap may drift and still count as a tap, and how far apart two
// taps may land and still make a double tap (CSS px).
export const TAP_SLOP = 10;
export const DOUBLE_TAP_SLOP = 30;

// pinchStep turns two fingers' last and new positions ([{x, y}, {x, y}]) into
// a zoom: the factor to apply and the point to keep still (the new midpoint).
// Null when either span is degenerate.
export function pinchStep(before, after) {
  const span = (p) => Math.hypot(p[1].x - p[0].x, p[1].y - p[0].y);
  const from = span(before);
  const to = span(after);
  if (!(from > 0) || !(to > 0)) return null;
  return { factor: to / from, point: { x: (after[0].x + after[1].x) / 2, y: (after[0].y + after[1].y) / 2 } };
}

// tapTracker classifies single-finger touches: end() returns 'double' when a
// touch completes a double tap, 'tap' for a lone tap, null otherwise. Times
// are milliseconds, positions client px.
export function tapTracker() {
  let start = null;
  let lastTap = null;
  return {
    start(x, y, t) { start = { x, y, t }; },
    cancel() { start = null; lastTap = null; },
    end(x, y, t) {
      const begun = start;
      start = null;
      if (!begun || Math.hypot(x - begun.x, y - begun.y) > TAP_SLOP || t - begun.t > DOUBLE_TAP_MS) {
        lastTap = null;
        return null;
      }
      if (lastTap && t - lastTap.t <= DOUBLE_TAP_MS && Math.hypot(x - lastTap.x, y - lastTap.y) <= DOUBLE_TAP_SLOP) {
        lastTap = null;
        return 'double';
      }
      lastTap = { x, y, t };
      return 'tap';
    },
  };
}

// useTouchZoom wires the touch gestures above to one element (the viewer's
// stage) while `enabled` holds: onZoom(factor, point) as useZoomGestures
// calls it, and onDoubleTap(point) for a double tap. Without onDoubleTap a
// double tap is left to the browser (in text, it selects a word).
export function useTouchZoom({ targetRef, enabled, onZoom, onDoubleTap, shouldHandle = () => true }) {
  const zoomRef = useRef(onZoom);
  const tapRef = useRef(onDoubleTap);
  const handleRef = useRef(shouldHandle);
  zoomRef.current = onZoom;
  tapRef.current = onDoubleTap;
  handleRef.current = shouldHandle;

  useEffect(() => {
    const node = targetRef.current;
    if (!enabled || !node) return undefined;
    const taps = tapTracker();
    let fingers = null; // the last two positions while pinching
    const points = (list) => [0, 1].map((i) => ({ x: list[i].clientX, y: list[i].clientY }));

    // targetTouches: only the fingers on the stage. A thumb resting on the
    // header must not turn a one-finger drag into a pinch.
    const onStart = (event) => {
      if (!handleRef.current()) return;
      const touches = event.targetTouches;
      if (touches.length === 1) {
        taps.start(touches[0].clientX, touches[0].clientY, event.timeStamp);
        fingers = null;
      } else {
        taps.cancel();
        fingers = touches.length === 2 ? points(touches) : null;
      }
    };
    const onMove = (event) => {
      if (event.targetTouches.length !== 2 || !fingers) return;
      // A second finger that lands after the first has started a native
      // scroll cannot stop it (the move is no longer cancelable); zooming
      // then would fight the scroller, so that touch sequence only scrolls.
      if (!event.cancelable) { fingers = null; return; }
      event.preventDefault();
      const next = points(event.targetTouches);
      const step = pinchStep(fingers, next);
      fingers = next;
      if (step && Math.abs(step.factor - 1) > 1e-3) zoomRef.current?.(step.factor, step.point);
    };
    const onEnd = (event) => {
      const touches = event.targetTouches;
      if (touches.length === 2) { fingers = points(touches); return; }
      fingers = null;
      if (touches.length > 0) { taps.cancel(); return; }
      const touch = event.changedTouches?.[0];
      if (!touch || !tapRef.current || !handleRef.current()) return;
      if (taps.end(touch.clientX, touch.clientY, event.timeStamp) !== 'double') return;
      // No synthesized mouse events (and so no dblclick) after a handled
      // double tap: the zoom must toggle once, not twice.
      if (event.cancelable) event.preventDefault();
      tapRef.current?.({ x: touch.clientX, y: touch.clientY });
    };
    const onCancel = () => { fingers = null; taps.cancel(); };

    node.addEventListener('touchstart', onStart, { passive: true });
    node.addEventListener('touchmove', onMove, { passive: false });
    node.addEventListener('touchend', onEnd, { passive: false });
    node.addEventListener('touchcancel', onCancel);
    return () => {
      node.removeEventListener('touchstart', onStart, { passive: true });
      node.removeEventListener('touchmove', onMove, { passive: false });
      node.removeEventListener('touchend', onEnd, { passive: false });
      node.removeEventListener('touchcancel', onCancel);
    };
  }, [enabled, targetRef]);
}

function fullscreenSupported() {
  return typeof document !== 'undefined'
    && document.fullscreenEnabled !== false
    && typeof document.documentElement?.requestFullscreen === 'function';
}

// useViewerDisplay gives an open dialog its maximize and fullscreen modes.
// `display` is what the dialog's data-display attribute carries, which is all
// the stylesheet needs; fullscreen state is read back from the document, so a
// browser-initiated exit (Escape, F11) is reflected without our involvement.
// The F key is heard on the document for the reason useZoomGestures gives.
export function useViewerDisplay({ dialogRef, open, shouldHandle = () => true }) {
  const handleRef = useRef(shouldHandle);
  handleRef.current = shouldHandle;
  const [maximized, setMaximized] = useState(false);
  const [fullscreen, setFullscreen] = useState(false);
  const canFullscreen = fullscreenSupported();

  useEffect(() => {
    const node = dialogRef.current;
    if (!open || !node) return undefined;
    const sync = () => setFullscreen(document.fullscreenElement === node);
    // F toggles fullscreen, like most media viewers. Unmodified only, so it
    // never shadows a browser shortcut.
    const onKeyDown = (event) => {
      if (event.defaultPrevented || event.ctrlKey || event.metaKey || event.altKey) return;
      if (event.isComposing || event.repeat || !handleRef.current()) return;
      if (event.target?.closest?.('input, textarea, select, [contenteditable]:not([contenteditable="false"])')) return;
      if (event.key === 't' || event.key === 'T') {
        if (document.fullscreenElement === node) return;
        event.preventDefault();
        setMaximized((value) => !value);
        return;
      }
      if (event.key !== 'f' && event.key !== 'F') return;
      if (!canFullscreen || !handleRef.current()) return;
      event.preventDefault();
      if (document.fullscreenElement === node) document.exitFullscreen?.().catch(() => {});
      else node.requestFullscreen?.().catch(() => {});
    };
    document.addEventListener('fullscreenchange', sync);
    document.addEventListener('keydown', onKeyDown);
    return () => {
      document.removeEventListener('fullscreenchange', sync);
      document.removeEventListener('keydown', onKeyDown);
      if (document.fullscreenElement === node) document.exitFullscreen?.().catch(() => {});
      setFullscreen(false);
    };
  }, [canFullscreen, dialogRef, open]);

  const toggleMaximize = useCallback(() => setMaximized((value) => !value), []);
  const toggleFullscreen = useCallback(() => {
    const node = dialogRef.current;
    if (!node || !canFullscreen) return;
    if (document.fullscreenElement === node) document.exitFullscreen?.().catch(() => {});
    else node.requestFullscreen?.().catch(() => {});
  }, [canFullscreen, dialogRef]);

  let display = 'normal';
  if (fullscreen) display = 'fullscreen';
  else if (maximized) display = 'maximized';
  return { display, maximized, fullscreen, canFullscreen, toggleMaximize, toggleFullscreen };
}

const ICONS = {
  maximize: html`<path d="M3 3h10v10H3z" />`,
  restore: html`<path d="M5 3h8v8M3 5h8v8H3z" />`,
  fullscreen: html`<path d="M2 6V2h4M10 2h4v4M14 10v4h-4M6 14H2v-4" />`,
  exitFullscreen: html`<path d="M6 2v4H2M14 6h-4V2M10 14v-4h4M2 10h4v4" />`,
};

function Icon({ name }) {
  return html`<svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true"
    fill="none" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round">${ICONS[name]}</svg>`;
}

// ViewerDisplayButtons is the maximize / fullscreen pair for a viewer header.
// Maximize is hidden while fullscreen: the window size underneath is moot.
export function ViewerDisplayButtons({ maximized, fullscreen, canFullscreen, toggleMaximize, toggleFullscreen }) {
  return html`
    ${!fullscreen && html`<button data-ui-exempt type="button" class="viewer-display-button viewer-maximize"
      onClick=${toggleMaximize} aria-pressed=${maximized}
      aria-label=${maximized ? 'Restore the viewer size' : 'Maximize the viewer'}
      title=${maximized ? 'Restore size (T)' : 'Maximize to the window (T)'}>
      <${Icon} name=${maximized ? 'restore' : 'maximize'} />
    </button>`}
    ${canFullscreen && html`<button data-ui-exempt type="button" class="viewer-display-button viewer-fullscreen"
      onClick=${toggleFullscreen} aria-pressed=${fullscreen}
      aria-label=${fullscreen ? 'Exit full screen' : 'Full screen'}
      title=${fullscreen ? 'Exit full screen (F or Esc)' : 'Full screen (F)'}>
      <${Icon} name=${fullscreen ? 'exitFullscreen' : 'fullscreen'} />
    </button>`}
  `;
}
