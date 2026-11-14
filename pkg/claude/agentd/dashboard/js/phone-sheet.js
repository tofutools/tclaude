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
// inert, Escape and the scrim close it, and focus goes back to whatever opened
// it.

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
}

// What the sheet makes inert while it is open: everything a tap could reach
// behind it. The sheet itself renders outside all of these.
const BEHIND = 'main, .phone-bar, .phone-view, .phone-nav';

function Sheet({ label, onClose, children }) {
  const sheet = useRef(null);
  useEffect(() => {
    const doc = sheet.current.ownerDocument;
    const opener = doc.activeElement;
    const behind = [...doc.querySelectorAll(BEHIND)];
    for (const element of behind) element.inert = true;
    const onKey = (event) => { if (event.key === 'Escape') onClose(); };
    doc.addEventListener('keydown', onKey);
    sheet.current.querySelector('button, a[href], input')?.focus();
    return () => {
      doc.removeEventListener('keydown', onKey);
      for (const element of behind) element.inert = false;
      if (opener?.isConnected) opener.focus();
    };
  }, []);
  return html`<div class="phone-sheet-scrim" onClick=${onClose}></div>
    <div class="phone-sheet" role="dialog" aria-modal="true" aria-label=${label} ref=${sheet}>
      <div class="phone-sheet-grab" aria-hidden="true"></div>
      ${children}
    </div>`;
}

// PhoneSheetHost renders the open sheet, if any. The phone shell mounts it.
export function PhoneSheetHost() {
  const open = phoneSheet.value;
  if (!open) return null;
  const { label, Body, props } = open;
  return html`<${Sheet} key=${label} label=${label} onClose=${closePhoneSheet}>
    <${Body} ...${props} close=${closePhoneSheet} />
  <//>`;
}
