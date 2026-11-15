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
// inert, Escape, the scrim and a swipe down on its grab handle close it, and
// focus goes back to whatever opened it.
//
// A Body holding something the operator would lose (a draft) guards those
// closes with useSheetGuard: its guard runs instead of the close, and the
// Body closes itself once the operator chose. Body's own close() is never
// guarded. A Body marks what takes focus with data-sheet-focus (a message's
// text field); otherwise its first button does.

import { h } from 'preact';
import { useEffect, useRef } from 'preact/hooks';
import { signal } from '@preact/signals';
import htm from 'htm';

const html = htm.bind(h);

export const phoneSheet = signal(null);

export function openPhoneSheet({ label, Body, props = {} }) {
  phoneSheet.value = { label, Body, props };
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

// useSheetGuard installs a guard for the sheet this Body renders in, while
// active: a function that runs instead of a requested close.
export function useSheetGuard(active, onRequestClose) {
  const latest = useRef(onRequestClose);
  latest.current = onRequestClose;
  useEffect(() => {
    if (!active) return undefined;
    const mine = () => latest.current();
    guard = mine;
    return () => { if (guard === mine) guard = null; };
  }, [active]);
}

// What the sheet makes inert while it is open: everything a tap could reach
// behind it, the docked terminal too. The sheet itself renders outside all of
// these.
const BEHIND = 'main, .phone-bar, .phone-view, .phone-nav, .phone-term';

// How far down a grab handle drag goes before it closes the sheet.
export const SWIPE_CLOSE_PX = 60;

// Grab is the handle at a sheet's top: dragging it down far enough closes
// the sheet (onClose, so a guard may hold it). Screen readers have Escape and
// the sheet's own buttons.
function Grab({ onClose }) {
  const start = useRef(null);
  const down = (event) => {
    start.current = event.clientY;
    event.currentTarget.setPointerCapture?.(event.pointerId);
  };
  const up = (event) => {
    const from = start.current;
    start.current = null;
    if (from !== null && event.clientY - from >= SWIPE_CLOSE_PX) onClose();
  };
  return html`<div class="phone-sheet-grab" aria-hidden="true"
    onPointerDown=${down} onPointerUp=${up} onPointerCancel=${() => { start.current = null; }}></div>`;
}

function Sheet({ label, onClose, children }) {
  const sheet = useRef(null);
  useEffect(() => {
    const doc = sheet.current.ownerDocument;
    const opener = doc.activeElement;
    const behind = [...doc.querySelectorAll(BEHIND)];
    for (const element of behind) element.inert = true;
    const onKey = (event) => { if (event.key === 'Escape') onClose(); };
    doc.addEventListener('keydown', onKey);
    // The first action, not a link in the sheet's details.
    const first = sheet.current.querySelector('[data-sheet-focus]')
      || sheet.current.querySelector('button') || sheet.current.querySelector('a[href], input');
    first?.focus();
    return () => {
      doc.removeEventListener('keydown', onKey);
      for (const element of behind) element.inert = false;
      if (opener?.isConnected) opener.focus();
    };
  }, []);
  return html`<div class="phone-sheet-scrim" onClick=${onClose}></div>
    <div class="phone-sheet" role="dialog" aria-modal="true" aria-label=${label} ref=${sheet}>
      <${Grab} onClose=${onClose} />
      ${children}
    </div>`;
}

// PhoneSheetHost renders the open sheet, if any. The phone shell mounts it.
export function PhoneSheetHost() {
  const open = phoneSheet.value;
  if (!open) return null;
  const { label, Body, props } = open;
  return html`<${Sheet} key=${label} label=${label} onClose=${requestClosePhoneSheet}>
    <${Body} ...${props} close=${closePhoneSheet} />
  <//>`;
}
