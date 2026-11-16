// phone-hold.js -- touch and hold on a phone: useHold(onHold) for a row or
// card that opens something more (an agent's menu) when held.
//
// A hold is HOLD_MS with the finger down and still. While it builds, the
// element carries .phone-holding, which phone-agents.css draws as the row
// lighting up and a bar filling along its foot (a visual cue; no haptics). Moving more than HOLD_SLOP_PX,
// a scroll (the browser cancels the pointer), lifting early or a second finger
// cancels it, and the tap goes through as a tap. A hold is never the only way
// in: the element's ⋯ opens the same thing.
//
// The browser has a long-press of its own, which would fire too under the
// finger still down, on whatever the hold just opened there: a text
// selection, or a link's menu. So a holdable element (.phone-holdable) takes
// no selection or callout, and once a hold fires, until that touch ends
// (armAfterHold): the page is armed (data-hold-armed on the root, under
// which phone.css makes the page, the new sheet included, take no pointer),
// a contextmenu or selectstart is swallowed and the selection cleared. Then
// the one click the lift makes is swallowed too, wherever it lands. The next
// press ends all of it, so a deliberate tap on the sheet goes through, and
// its text is selectable again.

import { useEffect, useRef, useState } from 'preact/hooks';

export const HOLD_MS = 500;
export const HOLD_SLOP_PX = 10;
// LIFT_MS is how long after the finger lifts the click its lift makes is
// still swallowed (a lift may make none). HELD_MAX_MS ends the armed state
// of a touch whose end never arrives (a lost touchend), so the page cannot
// stay inert.
export const LIFT_MS = 400;
export const HELD_MAX_MS = 15000;
export const ARMED_ATTR = 'data-hold-armed';

// Not pointercancel: a browser cancels the pointer as its own long-press
// starts, with the finger still down.
const ENDS = ['pointerup', 'touchend', 'touchcancel'];

function clearSelection(doc) {
  try { doc.getSelection?.()?.removeAllRanges?.(); } catch { /* nothing selected */ }
}

// armAfterHold keeps the browser's own long-press from acting on what a hold
// just opened under the finger, until that touch ends (see the top). It
// returns the function that ends it early.
export function armAfterHold(doc = globalThis.document) {
  if (!doc?.addEventListener) return () => {};
  const root = doc.documentElement;
  const eat = (event) => { event.preventDefault(); event.stopImmediatePropagation(); };
  const eatSelect = (event) => { eat(event); clearSelection(doc); };
  const eatClick = (event) => { eat(event); disarm(); };
  let timer = null;
  const held = [['contextmenu', eat], ['selectstart', eatSelect]];
  const release = () => {
    root?.removeAttribute?.(ARMED_ATTR);
    for (const [type, fn] of held) doc.removeEventListener(type, fn, true);
    for (const type of ENDS) doc.removeEventListener(type, lift, true);
  };
  // The finger is up: the page takes presses again, and its one click is
  // still to come.
  const lift = () => {
    release();
    clearSelection(doc);
    clearTimeout(timer);
    timer = setTimeout(disarm, LIFT_MS);
  };
  const disarm = () => {
    clearTimeout(timer);
    release();
    doc.removeEventListener('click', eatClick, true);
    doc.removeEventListener('pointerdown', disarm, true);
  };
  root?.setAttribute?.(ARMED_ATTR, '');
  clearSelection(doc);
  for (const [type, fn] of held) doc.addEventListener(type, fn, true);
  for (const type of ENDS) doc.addEventListener(type, lift, true);
  doc.addEventListener('click', eatClick, true);
  doc.addEventListener('pointerdown', disarm, true);
  timer = setTimeout(disarm, HELD_MAX_MS);
  return disarm;
}

// useHold returns { holding, handlers }: spread handlers on the element and
// give it .phone-holding while holding. onHold(event-free) runs on a hold; a
// falsy onHold makes the handlers inert.
export function useHold(onHold, { ms = HOLD_MS } = {}) {
  const [holding, setHolding] = useState(false);
  const start = useRef(null);
  const timer = useRef(null);
  const swallow = useRef(false);
  const latest = useRef(onHold);
  latest.current = onHold;
  const stop = () => {
    if (timer.current) { clearTimeout(timer.current); timer.current = null; }
    start.current = null;
    setHolding(false);
  };
  useEffect(() => () => { if (timer.current) clearTimeout(timer.current); }, []);
  if (!onHold) return { holding: false, handlers: {} };
  const handlers = {
    onPointerDown: (event) => {
      // The primary button or a finger; a second finger is a pinch, not a hold.
      if (event.button > 0 || (start.current && start.current.id !== event.pointerId)) { stop(); return; }
      swallow.current = false;
      start.current = { x: event.clientX, y: event.clientY, id: event.pointerId, doc: event.currentTarget?.ownerDocument };
      setHolding(true);
      if (timer.current) clearTimeout(timer.current);
      timer.current = setTimeout(() => {
        timer.current = null;
        const doc = start.current?.doc;
        start.current = null;
        swallow.current = true;
        armAfterHold(doc);
        setHolding(false);
        latest.current?.();
      }, ms);
    },
    onPointerMove: (event) => {
      const at = start.current;
      if (!at || at.id !== event.pointerId) return;
      if (Math.abs(event.clientX - at.x) > HOLD_SLOP_PX || Math.abs(event.clientY - at.y) > HOLD_SLOP_PX) stop();
    },
    onPointerUp: stop,
    onPointerCancel: stop,
    onPointerLeave: stop,
    onClickCapture: (event) => {
      if (!swallow.current) return;
      swallow.current = false;
      event.preventDefault();
      // Immediate: the element's own onClick listens on it too, and at the
      // target a plain stop would not keep that one from running.
      event.stopImmediatePropagation();
    },
    // Android's long-press menu (and a desktop right-click on a phone layout).
    onContextMenu: (event) => event.preventDefault(),
  };
  return { holding, handlers };
}

// The phone shell registers how an agent's menu opens (preact-loader.js), so
// a list mounted outside the shell's own views (Open sessions, under the
// terminals tab) can open it too: agentMenuOpener() is that function, taking
// { agentId, name }, or null before the shell has loaded.
let opener = null;
export function setAgentMenuOpener(open) { opener = open || null; }
export const agentMenuOpener = () => opener;
