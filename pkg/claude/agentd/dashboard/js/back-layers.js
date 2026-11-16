// back-layers.js -- the one registry of open modal layers that Back closes.
//
// The rule: while an overlay is open (a dialog, a form, a preview, a
// confirmation), Back is the overlay's. It closes the topmost one, through
// that overlay's own guarded close (a dirty form asks to discard first), and
// never reaches page navigation until every overlay is gone.
//
// Every modal layer registers here while it is open: the shared overlay
// (management-overlay.js, so every Dialog) does it for itself, and a
// hand-rolled one calls useBackLayer. The phone's history layer
// (phone-history.js) counts the
// registered layers as history steps and, on Back, asks the topmost one to
// close. Phone sheets and pages, and the routed overlays (a terminal, a
// task), are history steps of their own and do not register here. A jstest
// (back-layers.test.mjs) fails when an aria-modal overlay neither registers
// nor is one of those.
//
// Dependency-light on purpose: management-overlay.js and phone-history.js
// both import it, and neither imports the other.

import { signal } from '@preact/signals';
import { useLayoutEffect, useRef } from 'preact/hooks';

// backLayers is the open layers, oldest first. Each is { close } where
// close() closes the layer and resolves (or returns) false when it stayed
// open (a draft kept, a send in flight).
export const backLayers = signal([]);

// registerBackLayer adds an open layer on top. Returns the unregister
// function, which a layer calls once it is closed.
export function registerBackLayer(layer) {
  const entry = { close: layer.close };
  backLayers.value = [...backLayers.peek(), entry];
  return () => {
    const layers = backLayers.peek();
    if (layers.includes(entry)) backLayers.value = layers.filter((item) => item !== entry);
  };
}

// useBackLayer registers a hand-rolled overlay while `open`: close is what
// Back does to it (the same as its Cancel, Escape or backdrop).
export function useBackLayer(open, close) {
  const latest = useRef(close);
  latest.current = close;
  useLayoutEffect(() => {
    if (!open) return undefined;
    return registerBackLayer({ close: () => latest.current?.() });
  }, [!!open]);
}

// closeTopBackLayer asks the topmost layer to close. Resolves true when it
// closed, false when it stayed open or there is none.
export async function closeTopBackLayer() {
  const top = backLayers.peek().at(-1);
  if (!top) return false;
  try {
    return (await top.close()) !== false;
  } catch (_) {
    return false;
  }
}
