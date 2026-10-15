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
//   - Ctrl/Cmd + = - 0, and the bare + - 0 keys, while the viewer has focus.
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
    let lastScale = 1;
    const onGestureStart = (event) => { event.preventDefault(); lastScale = 1; };
    const onGestureChange = (event) => {
      event.preventDefault();
      const scale = Number(event.scale) || 1;
      zoomRef.current?.(scale / lastScale, { x: event.clientX, y: event.clientY });
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
    node.addEventListener('gesturestart', onGestureStart);
    node.addEventListener('gesturechange', onGestureChange);
    node.addEventListener('gestureend', onGestureEnd);
    document.addEventListener('keydown', onKeyDown);
    return () => {
      node.removeEventListener('wheel', onWheel, { passive: false });
      node.removeEventListener('gesturestart', onGestureStart);
      node.removeEventListener('gesturechange', onGestureChange);
      node.removeEventListener('gestureend', onGestureEnd);
      document.removeEventListener('keydown', onKeyDown);
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
    ${!fullscreen && html`<button type="button" class="viewer-display-button viewer-maximize"
      onClick=${toggleMaximize} aria-pressed=${maximized}
      aria-label=${maximized ? 'Restore the viewer size' : 'Maximize the viewer'}
      title=${maximized ? 'Restore size (T)' : 'Maximize to the window (T)'}>
      <${Icon} name=${maximized ? 'restore' : 'maximize'} />
    </button>`}
    ${canFullscreen && html`<button type="button" class="viewer-display-button viewer-fullscreen"
      onClick=${toggleFullscreen} aria-pressed=${fullscreen}
      aria-label=${fullscreen ? 'Exit full screen' : 'Full screen'}
      title=${fullscreen ? 'Exit full screen (F or Esc)' : 'Full screen (F)'}>
      <${Icon} name=${fullscreen ? 'exitFullscreen' : 'fullscreen'} />
    </button>`}
  `;
}
