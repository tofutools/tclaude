// phone-hold.js -- touch and hold on a phone: useHold(onHold) for a row or
// card that opens something more (an agent's menu) when held.
//
// A hold is HOLD_MS with the finger down and still. While it builds, the
// element carries .phone-holding, which phone-agents.css draws as the row
// lighting up and a bar filling along its foot (a visual cue; no haptics). Moving more than HOLD_SLOP_PX,
// a scroll (the browser cancels the pointer), lifting early or a second finger
// cancels it, and the tap goes through as a tap. Once it fires, the click the
// lift would make is swallowed, and so is the browser's own long-press menu.
// A hold is never the only way in: the element's ⋯ opens the same thing.

import { useEffect, useRef, useState } from 'preact/hooks';

export const HOLD_MS = 500;
export const HOLD_SLOP_PX = 10;

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
      start.current = { x: event.clientX, y: event.clientY, id: event.pointerId };
      setHolding(true);
      if (timer.current) clearTimeout(timer.current);
      timer.current = setTimeout(() => {
        timer.current = null;
        start.current = null;
        swallow.current = true;
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
