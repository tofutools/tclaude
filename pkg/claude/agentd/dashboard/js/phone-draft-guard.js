// phone-draft-guard.js -- a phone sheet's guard for a short draft (a comment,
// a note, an agent's prompt), the message sheet's rule (phone-compose-sheet.js)
// for any sheet: a close the operator asked for (Back, Escape, the scrim, a
// swipe down, Cancel) asks "Discard?" while there is a draft, and asked again
// while the question is up it means Keep. Nothing written goes without a yes.
//
//   const guard = useDraftGuard(!!text.trim(), busy);
//   ...
//   ${guard.asking && html`<${DiscardAsk} what="this comment" guard=${guard} close=${close} />`}
//
// Cancel buttons in such a sheet call requestClosePhoneSheet, so they are
// guarded too; the Body's own close() (after a send, or Discard) is not.

import { h } from 'preact';
import { useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { useSheetGuard } from './phone-sheet.js';
import { ButtonRow } from './phone-ui.js';

const html = htm.bind(h);

// useDraftGuard guards the sheet this Body renders in while `dirty`; `busy`
// (a send in flight) holds any close. Returns { asking, keep }: whether the
// question is up, and Keep.
export function useDraftGuard(dirty, busy = false) {
  const [asking, setAsking] = useState(false);
  useSheetGuard(() => {
    if (busy) return true;
    if (!dirty) return false;
    setAsking((now) => !now);
    return true;
  });
  return { asking, keep: () => setAsking(false) };
}

// DiscardAsk is the question, in the sheet: Keep, or Discard, which closes it
// (the Body's own close). It takes the focus, as the message sheet's does,
// and Keep gives it back to `field` when there is one.
export function DiscardAsk({ what = 'this', guard, close, field = null }) {
  const row = useRef(null);
  useLayoutEffect(() => { row.current?.querySelector('button')?.focus(); }, []);
  const keep = () => {
    guard.keep();
    setTimeout(() => field?.current?.focus?.(), 0);
  };
  return html`<div ref=${row} class="phone-compose-foot phone-draft-ask" role="alertdialog" aria-label=${`Discard ${what}?`}>
    <span class="phone-compose-ask">Discard ${what}?</span>
    <${ButtonRow} buttons=${[
      { key: 'keep', label: 'Keep', onClick: keep },
      { key: 'discard', label: 'Discard', tone: 'no', onClick: close },
    ]} />
  </div>`;
}
