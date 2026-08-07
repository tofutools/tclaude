// dialog-resize.js — a draggable size for the attachment viewers, remembered
// across sessions.
//
// The image and Markdown viewers open at one fixed size, which is the wrong
// size for half the files they show: a tall report wants height, a wide
// screenshot wants width, and the operator's monitor is not the one the CSS
// default was picked on. This gives both dialogs a corner grip and remembers
// where it was left.
//
// The size is applied as the CSS custom properties --dialog-w / --dialog-h
// rather than as an inline width/height. That is what lets the stylesheet keep
// the last word: the dialog rules read the property through a min() against the
// viewport, and the narrow-screen rules that make the viewer full-screen still
// win, which an inline style would have overridden.
//
// Persistence goes through dashPrefs (server-side, SQLite) rather than
// localStorage, for the reason prefs.js explains: the dashboard's loopback port
// changes every daemon start, which would partition a localStorage copy away.
// The gesture math doubles the pointer delta because the dialog is centred in
// its overlay, so a drag grows it from both edges at once and the grip only
// travels half the size change.

import { h } from 'preact';
import { useCallback, useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { dashPrefs } from './prefs.js';

const html = htm.bind(h);

// Floors: below these the header actions wrap and the stage stops being worth
// looking at. The ceiling is the viewport, less the overlay's own padding.
const MIN_W = 380, MIN_H = 260;
const VIEWPORT_MARGIN = 32;
// One keyboard press. Coarse enough to cross a dialog in a few presses, fine
// enough to land on a size deliberately.
const KEY_STEP = 48;

function clamp(value, lo, hi) { return Math.min(hi, Math.max(lo, value)); }

// The largest size worth storing. A pref bigger than the screen is not wrong —
// the CSS min() clamps it on the way in — but it would make a drag on a small
// screen start from a number the operator cannot see.
function viewportCeiling() {
  const width = typeof window === 'undefined' ? 0 : window.innerWidth;
  const height = typeof window === 'undefined' ? 0 : window.innerHeight;
  return {
    w: Math.max(MIN_W, (width || MIN_W + VIEWPORT_MARGIN) - VIEWPORT_MARGIN),
    h: Math.max(MIN_H, (height || MIN_H + VIEWPORT_MARGIN) - VIEWPORT_MARGIN),
  };
}

// A stored size the operator can no longer have chosen — a corrupt value, or a
// pref from a monitor twice this one — is not honoured blindly, but a smaller
// window is a temporary condition: the raw value is kept and only the applied
// style is clamped, by the CSS min().
function loadSize(prefKey) {
  try {
    const saved = JSON.parse(dashPrefs.getItem(prefKey));
    if (saved && typeof saved === 'object') {
      const w = Number(saved.w);
      const h = Number(saved.h);
      // Finite as well as positive: "Infinity" survives Number() and > 0, and
      // would reach the stylesheet as the custom property `Infinitypx`.
      if (Number.isFinite(w) && Number.isFinite(h) && w > 0 && h > 0) {
        return { w: Math.max(MIN_W, w), h: Math.max(MIN_H, h) };
      }
    }
  } catch {
    // Missing or corrupt — the CSS default stands.
  }
  return null;
}

// The size is per PREF KEY, not per component. Every notification card with an
// attachment mounts its viewer up front, so a size held in component state
// would stick only to the one card whose grip was dragged: opening any other
// card's viewer would show the stylesheet default, and dragging it would start
// from that default and overwrite the stored size. This module-level cell is
// the session's authoritative copy, and every mounted viewer follows it.
const sizes = new Map();          // prefKey -> {w, h} | null (null = CSS default)
const subscribers = new Set();    // (prefKey) => void

function currentSize(prefKey) {
  if (!sizes.has(prefKey)) sizes.set(prefKey, loadSize(prefKey));
  return sizes.get(prefKey);
}

function publishSize(prefKey, size) {
  sizes.set(prefKey, size);
  for (const notify of subscribers) notify(prefKey);
}

// useDialogResize gives a centred modal dialog a remembered size.
//
// `dialogRef` is the same ref the dialog element already carries (the one
// useDialogFocus hands out): the gesture measures the live box rather than
// trusting state, so a drag that starts from the CSS default — or from a size
// the viewport clamped — still tracks the pointer exactly.
export function useDialogResize({ dialogRef, prefKey }) {
  // dashPrefs is a synchronous cache that boot has already filled, so the
  // stored size is there for the first render — no flash of the default.
  const [size, setSize] = useState(() => currentSize(prefKey));
  // The gesture reads through a ref as well, because the handlers it installs
  // close over the state from the render they started in.
  const sizeRef = useRef(size);
  sizeRef.current = size;

  // Follow the shared cell, so a size dragged on one viewer is the size every
  // other viewer of the same kind opens at.
  useEffect(() => {
    const notify = (key) => { if (key === prefKey) setSize(currentSize(prefKey)); };
    subscribers.add(notify);
    notify(prefKey);
    return () => { subscribers.delete(notify); };
  }, [prefKey]);

  const applySize = useCallback((next) => {
    sizeRef.current = next;
    publishSize(prefKey, next);
  }, [prefKey]);

  const persist = useCallback(() => {
    const current = sizeRef.current;
    try {
      if (current) dashPrefs.setItem(prefKey, JSON.stringify(current));
      else dashPrefs.removeItem(prefKey);
    } catch {
      // Best-effort persistence; the size already applies to this session.
    }
  }, [prefKey]);

  // measure reports the size a gesture should grow from: the live box when the
  // DOM can be measured, else whatever is applied. linkedom (the jstest DOM)
  // has no layout, so the fallback keeps the gesture testable.
  const measure = useCallback(() => {
    const node = dialogRef?.current;
    const rect = node?.getBoundingClientRect?.();
    if (rect?.width > 0 && rect?.height > 0) return { w: rect.width, h: rect.height };
    return sizeRef.current ? { ...sizeRef.current } : { w: MIN_W, h: MIN_H };
  }, [dialogRef]);

  const resizeBy = useCallback((dw, dh, start = measure()) => {
    const ceiling = viewportCeiling();
    applySize({
      w: clamp(start.w + dw, MIN_W, ceiling.w),
      h: clamp(start.h + dh, MIN_H, ceiling.h),
    });
  }, [applySize, measure]);

  const reset = useCallback(() => {
    applySize(null);
    persist();
  }, [applySize, persist]);

  const onPointerDown = useCallback((event) => {
    if (event.button !== 0) return; // left button only, like the mail gutters
    event.preventDefault();
    const grip = event.currentTarget;
    const start = measure();
    const startX = event.clientX, startY = event.clientY;
    let moved = false;
    grip.setPointerCapture?.(event.pointerId);
    // preventDefault above suppressed the click's default focus action, and the
    // grip's own label promises arrow keys. Focus it so that promise holds
    // straight after a drag, not only after tabbing to it.
    grip.focus?.();

    const onMove = (moveEvent) => {
      moved = true;
      // Doubled: the dialog is centred, so it grows away from the pointer by
      // as much as it grows toward it.
      resizeBy((moveEvent.clientX - startX) * 2, (moveEvent.clientY - startY) * 2, start);
      // Persisted as the drag runs, not only on release, so a dialog that
      // unmounts mid-gesture (Escape, a list refresh) still keeps the size.
      // dashPrefs coalesces this to at most one write per 400ms rather than one
      // per move — a handful of upserts across a drag, and the last move always
      // lands in the pending batch, so the stored value is the final one.
      persist();
    };
    const onUp = () => {
      grip.removeEventListener('pointermove', onMove);
      grip.removeEventListener('pointerup', onUp);
      grip.removeEventListener('pointercancel', onUp);
      if (grip.hasPointerCapture?.(event.pointerId)) grip.releasePointerCapture?.(event.pointerId);
      if (moved) persist(); // a bare click leaves the stored size alone
    };
    grip.addEventListener('pointermove', onMove);
    grip.addEventListener('pointerup', onUp);
    grip.addEventListener('pointercancel', onUp);
  }, [measure, persist, resizeBy]);

  // The grip is a real control, so the size is reachable without a pointer.
  const onKeyDown = useCallback((event) => {
    const steps = {
      ArrowRight: [KEY_STEP, 0], ArrowLeft: [-KEY_STEP, 0],
      ArrowDown: [0, KEY_STEP], ArrowUp: [0, -KEY_STEP],
    };
    const step = steps[event.key];
    if (step) {
      event.preventDefault();
      // Stop here rather than let the dialog's Escape/scroll handling see an
      // arrow key that was meant for the grip.
      event.stopPropagation();
      resizeBy(step[0], step[1]);
      persist();
      return;
    }
    if (event.key === 'Home') {
      event.preventDefault();
      event.stopPropagation();
      reset();
    }
  }, [persist, reset, resizeBy]);

  const dialogStyle = useMemo(
    () => (size ? { '--dialog-w': `${Math.round(size.w)}px`, '--dialog-h': `${Math.round(size.h)}px` } : undefined),
    [size],
  );

  return {
    dialogStyle,
    resizerProps: { onPointerDown, onKeyDown, onDoubleClick: reset },
  };
}

// DialogResizer is the corner grip itself. It sits inside the dialog, over the
// bottom-right corner of whatever the last row happens to be.
export function DialogResizer({ onPointerDown, onKeyDown, onDoubleClick, label = 'Resize this viewer' }) {
  return html`<button type="button" class="dialog-resizer"
    onPointerDown=${onPointerDown} onKeyDown=${onKeyDown} onDblClick=${onDoubleClick}
    aria-label=${`${label} (arrow keys resize, Home restores the default size)`}
    title="Drag to resize · double-click to restore the default size">
    <span class="dialog-resizer-grip" aria-hidden="true"></span>
  </button>`;
}
