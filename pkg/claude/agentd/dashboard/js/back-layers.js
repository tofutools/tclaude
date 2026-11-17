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

// backLayers is the open layers, oldest first. Each is { close, routed, seq }
// where close() closes the layer and resolves (or returns) false when it
// stayed open (a draft kept, a send in flight). A routed layer is already a
// location of its own (a library manager's ?manage=, Mail's ?file=): the
// router takes Back to it, so it is no extra step. seq orders it against the
// other layers Back weighs (a phone sheet: backSeq).
export const backLayers = signal([]);

let seq = 0;

// backSeq is the next opening order: a phone sheet takes one as it opens, so
// Back closes whichever of it and a layer came last.
export function backSeq() {
  seq += 1;
  return seq;
}

// registerBackLayer adds an open layer on top. Returns the unregister
// function, which a layer calls once it is closed.
export function registerBackLayer(layer) {
  const entry = { close: layer.close, routed: !!layer.routed, seq: backSeq() };
  backLayers.value = [...backLayers.peek(), entry];
  return () => {
    const layers = backLayers.peek();
    if (layers.includes(entry)) backLayers.value = layers.filter((item) => item !== entry);
  };
}

// useBackLayer registers a hand-rolled overlay while `open`: close is what
// Back does to it (the same as its Cancel, Escape or backdrop). routed says
// it is a location of its own already (see backLayers).
export function useBackLayer(open, close, { routed = false } = {}) {
  const latest = useRef(close);
  latest.current = close;
  useLayoutEffect(() => {
    if (!open) return undefined;
    return registerBackLayer({ close: (why) => latest.current?.(why), routed });
  }, [!!open, routed]);
}

// stepLayers is the open layers that are history steps of their own here:
// every one but the routed.
export function stepLayers(layers = backLayers.peek()) {
  return layers.filter((layer) => !layer.routed);
}

// closeTopBackLayer asks the topmost step layer to close, for Back. Resolves
// true when it closed, false when it stayed open or there is none. A layer
// marked `question` (raised by an earlier Back's close: phone-history.js) is
// told so: close({ question: true }).
export async function closeTopBackLayer() {
  const top = stepLayers().at(-1);
  if (!top) return false;
  try {
    return (await top.close({ question: !!top.question })) !== false;
  } catch (_) {
    return false;
  }
}
