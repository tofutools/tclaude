// phone-sheet.js -- the phone shell's bottom sheet: one modal sheet at a time,
// over the whole page, for the More menu and any phone view's choices (an
// agent's actions, say).
//
// A view opens one with openPhoneSheet({ label, Body, props }). The phone
// shell (phone-shell-island.js) renders it, outside <main>, so a sheet opened
// from a tab's own island still sits above everything and can make the rest
// of the page inert. Body renders with props plus close(); it reads live data
// through its props (signals), so a sheet stays current while open.
//
// While a sheet is open the shell, the tab under it and the own views are
// inert, Escape, the scrim and a drag down on its grab handle close it, and
// focus goes back to whatever opened it. A sheet opened from the bottom nav
// (More) passes overNav and slot: it sits above the nav, which stays in
// reach, and the nav lights that slot while it is open.
//
// A Body holding something the operator would lose (a draft) guards those
// closes with useSheetGuard: its guard runs instead of the close, and the
// Body closes itself once the operator chose. Body's own close() is never
// guarded. A Body marks what takes focus with data-sheet-focus (a message's
// text field); otherwise its first button does.

import { h } from 'preact';
import { useLayoutEffect, useRef } from 'preact/hooks';
import { signal } from '@preact/signals';
import htm from 'htm';

const html = htm.bind(h);

export const phoneSheet = signal(null);

export function openPhoneSheet({ label, Body, props = {}, overNav = false, slot = '' }) {
  phoneSheet.value = { label, Body, props, overNav, slot };
}

export function closePhoneSheet() {
  phoneSheet.value = null;
  guard = null;
}

// The open sheet's close guard (useSheetGuard), or null.
let guard = null;

// requestClosePhoneSheet is a close the operator asked for: Escape, the
// scrim, a swipe down, or a Body's Cancel. A guard may hold it.
export function requestClosePhoneSheet() {
  if (guard) guard();
  else closePhoneSheet();
}

// useSheetGuard guards the sheet this Body renders in: onRequestClose runs
// instead of a requested close and returns true when it held the close (a
// draft to ask about), false to let it go. It is installed as the Body
// mounts and always calls the latest onRequestClose, so a close the instant
// after a keystroke is guarded too.
export function useSheetGuard(onRequestClose) {
  const latest = useRef(onRequestClose);
  latest.current = onRequestClose;
  useLayoutEffect(() => {
    const mine = () => { if (!latest.current()) closePhoneSheet(); };
    guard = mine;
    return () => { if (guard === mine) guard = null; };
  }, []);
}

// What the sheet makes inert while it is open: everything a tap could reach
// behind it, the docked terminal too. The sheet itself renders outside all of
// these. A sheet over the nav leaves the nav out.
const BEHIND = 'main, .phone-bar, .phone-view, .phone-nav, .phone-term';
const BEHIND_ABOVE_NAV = 'main, .phone-bar, .phone-view, .phone-term';

// A drag on the grab handle closes the sheet once it went this far down (or
// a quarter of the sheet's height, whichever is more, up to SWIPE_MAX_PX), or
// when it was a flick: at least FLICK_MIN_PX at FLICK_PX_PER_MS or faster.
export const SWIPE_CLOSE_PX = 60;
const SWIPE_MAX_PX = 140;
const FLICK_MIN_PX = 24;
const FLICK_PX_PER_MS = 0.5;

// dragCloses says whether a drag of dy px down in ms milliseconds, on a
// sheet height px tall, closes it.
export function dragCloses(dy, ms, height = 0) {
  const far = Math.min(SWIPE_MAX_PX, Math.max(SWIPE_CLOSE_PX, height / 4));
  if (dy >= far) return true;
  return dy >= FLICK_MIN_PX && ms > 0 && dy / ms >= FLICK_PX_PER_MS;
}

const reducedMotion = (win) => !!win?.matchMedia?.('(prefers-reduced-motion: reduce)')?.matches;

// Grab is the handle at a sheet's top. The sheet follows a finger dragging
// it down; let go far or fast enough and the sheet closes (onClose, so a
// guard may hold it), otherwise it slides back (at once under reduced
// motion). Screen readers have Escape and the sheet's own buttons.
function Grab({ sheet, onClose }) {
  const drag = useRef(null);
  const place = (dy, animate) => {
    const el = sheet.current;
    if (!el) return;
    el.style.transition = animate && !reducedMotion(el.ownerDocument.defaultView) ? 'transform 180ms ease-out' : 'none';
    el.style.transform = dy > 0 ? `translateY(${dy}px)` : '';
  };
  const down = (event) => {
    drag.current = { y: event.clientY, t: event.timeStamp, dy: 0 };
    event.currentTarget.setPointerCapture?.(event.pointerId);
  };
  const move = (event) => {
    if (!drag.current) return;
    drag.current.dy = Math.max(0, event.clientY - drag.current.y);
    place(drag.current.dy, false);
  };
  const up = (event) => {
    const from = drag.current;
    drag.current = null;
    if (!from) return;
    const dy = Math.max(0, event.clientY - from.y);
    const height = sheet.current?.getBoundingClientRect?.().height || 0;
    // Back in place either way: a guard holding the close shows its question
    // in the sheet, which must not be left half off screen.
    place(0, !dragCloses(dy, event.timeStamp - from.t, height));
    if (dragCloses(dy, event.timeStamp - from.t, height)) onClose();
  };
  const cancel = () => {
    if (!drag.current) return;
    drag.current = null;
    place(0, true);
  };
  return html`<div class="phone-sheet-grab" aria-hidden="true"
    onPointerDown=${down} onPointerMove=${move} onPointerUp=${up} onPointerCancel=${cancel}></div>`;
}

function Sheet({ label, overNav = false, onClose, children }) {
  const sheet = useRef(null);
  // A layout effect: Escape, the inert page behind and the focus are in place
  // in the same commit as the sheet. A passive effect can run a frame or more
  // late on a busy page, and an Escape in between was lost, leaving the sheet
  // up.
  useLayoutEffect(() => {
    const box = sheet.current;
    const doc = box.ownerDocument;
    const opener = doc.activeElement;
    const behind = [...doc.querySelectorAll(overNav ? BEHIND_ABOVE_NAV : BEHIND)];
    for (const element of behind) element.inert = true;
    // Over the nav, the nav stays on top: a sheet dragged down slides under it.
    if (overNav) doc.documentElement.dataset.phoneSheet = 'over-nav';
    const onKey = (event) => { if (event.key === 'Escape') onClose(); };
    doc.addEventListener('keydown', onKey);
    // The first action, not a link in the sheet's details.
    const first = sheet.current.querySelector('[data-sheet-focus]')
      || sheet.current.querySelector('button') || sheet.current.querySelector('a[href], input');
    first?.focus();
    return () => {
      doc.removeEventListener('keydown', onKey);
      for (const element of behind) element.inert = false;
      if (overNav) delete doc.documentElement.dataset.phoneSheet;
      // Focus goes back to the opener, unless the operator already moved it
      // out of the sheet (a nav slot under a sheet over the nav).
      const moved = doc.activeElement && doc.activeElement !== doc.body && !box.contains(doc.activeElement);
      if (opener?.isConnected && !moved) opener.focus();
    };
  }, []);
  // Over the nav the sheet is not aria-modal: the nav stays in reach, for a
  // screen reader too.
  const above = overNav ? ' phone-sheet-over-nav' : '';
  return html`<div class=${`phone-sheet-scrim${above}`} onClick=${onClose}></div>
    <div class=${`phone-sheet${above}`} role="dialog" aria-modal=${overNav ? undefined : 'true'} aria-label=${label} ref=${sheet}>
      <${Grab} sheet=${sheet} onClose=${onClose} />
      ${children}
    </div>`;
}

// PhoneSheetHost renders the open sheet, if any. The phone shell mounts it.
export function PhoneSheetHost() {
  const open = phoneSheet.value;
  if (!open) return null;
  const { label, Body, props, overNav } = open;
  return html`<${Sheet} key=${label} label=${label} overNav=${!!overNav} onClose=${requestClosePhoneSheet}>
    <${Body} ...${props} close=${closePhoneSheet} />
  <//>`;
}
