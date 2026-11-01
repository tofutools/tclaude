// companion-chip-drag.js -- moving the minimized chips between their snap spots
// (tcl-avebwq). A press on any chip that travels becomes a drag of the whole
// row: outlines show every spot, the nearest one lights up, and release settles
// the row there. Anything shorter stays a click, and Esc puts the row back.
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import { CHIP_SPOTS, nearestChipSpot } from './companion-model.js';
import { viewportOf } from './overlay-window-model.js';

const DRAG_START_PX = 5;
const SETTLE_MS = 260;
// Overshoots a little, for the spring.
const SETTLE_EASING = 'cubic-bezier(.3,1.5,.5,1)';

export function useChipDrag({ win, spot, onDrop, onStart, reducedMotion }) {
  const [drag, setDrag] = useState(null); // {near, width, height}
  const ref = useRef(null);
  const settleFrom = useRef(null);
  const swallowClick = useRef(false);
  const release = useRef(null);
  useEffect(() => () => release.current?.(), []);

  // Glide from where the row was let go to where the layout put it.
  useLayoutEffect(() => {
    const from = settleFrom.current;
    const node = ref.current;
    if (drag || !from || !node) return;
    settleFrom.current = null;
    if (reducedMotion() || typeof node.animate !== 'function') return;
    const to = node.getBoundingClientRect();
    node.animate({ translate: [`${from.left - to.left}px ${from.top - to.top}px`, '0 0'] }, { duration: SETTLE_MS, easing: SETTLE_EASING });
  }, [drag, spot]);

  function slotsOnScreen() {
    return [...(win?.document?.querySelectorAll?.('[data-chip-spot]') || [])]
      .map((slot) => ({ spot: slot.dataset.chipSpot, rect: slot.getBoundingClientRect() }));
  }

  function onPointerDown(event) {
    const node = ref.current;
    const chip = event.target.closest?.('.companion-chip');
    if (event.button !== 0 || !node || !chip) return;
    const { pointerId, clientX: sx, clientY: sy } = event;
    let start = null;
    let near = spot;
    let settled = false;
    const land = (target) => {
      settled = true;
      settleFrom.current = node.getBoundingClientRect();
      node.style.translate = '';
      setDrag(null);
      if (target !== spot) onDrop(target);
    };
    const move = (m) => {
      if (m.pointerId !== pointerId || settled) return;
      if (!start) {
        if (Math.hypot(m.clientX - sx, m.clientY - sy) < DRAG_START_PX) return;
        start = node.getBoundingClientRect();
        swallowClick.current = true;
        chip.setPointerCapture?.(pointerId);
        onStart();
        setDrag({ near, width: Math.ceil(start.width), height: Math.ceil(start.height) });
      }
      // The row stays whole on screen however far the pointer goes.
      const vp = viewportOf(win);
      const dx = Math.min(vp.width - start.right, Math.max(-start.left, m.clientX - sx));
      const dy = Math.min(vp.height - start.bottom, Math.max(-start.top, m.clientY - sy));
      node.style.translate = `${dx}px ${dy}px`;
      // Unclamped: a row held on screen could never reach a spot nearer the edge
      // than half its own width.
      const next = nearestChipSpot({ x: start.left + start.width / 2 + m.clientX - sx, y: start.top + start.height / 2 + m.clientY - sy }, slotsOnScreen());
      if (next && next !== near) {
        near = next;
        setDrag((current) => current && { ...current, near });
      }
    };
    const onKey = (key) => {
      if (key.key !== 'Escape' || !start || settled) return;
      key.preventDefault();
      key.stopPropagation();
      land(spot);
    };
    const stop = () => {
      win?.removeEventListener?.('pointermove', move);
      win?.removeEventListener?.('pointerup', up);
      win?.removeEventListener?.('pointercancel', cancel);
      win?.removeEventListener?.('keydown', onKey, true);
      release.current = null;
    };
    const up = (u) => {
      if (u.pointerId !== pointerId) return;
      stop();
      if (start && !settled) land(near);
      // The click this release causes lands on the chip, on the capture path.
      setTimeout(() => { swallowClick.current = false; }, 0);
    };
    const cancel = (c) => {
      if (c.pointerId !== pointerId) return;
      stop();
      if (start && !settled) land(spot);
      swallowClick.current = false;
    };
    win?.addEventListener?.('pointermove', move);
    win?.addEventListener?.('pointerup', up);
    win?.addEventListener?.('pointercancel', cancel);
    win?.addEventListener?.('keydown', onKey, true);
    release.current = stop;
  }

  function onClickCapture(event) {
    if (!swallowClick.current) return;
    swallowClick.current = false;
    event.preventDefault();
    event.stopPropagation();
  }

  const slots = drag && CHIP_SPOTS.map((name) => ({ spot: name, near: name === drag.near, width: drag.width, height: drag.height }));
  return { ref, slots, onPointerDown, onClickCapture };
}
