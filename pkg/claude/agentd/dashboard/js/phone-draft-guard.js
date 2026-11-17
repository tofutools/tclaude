// phone-draft-guard.js -- a phone sheet's guard for a short draft (a comment,
// a note, an agent's prompt), the message sheet's rule (phone-compose-sheet.js)
// for any sheet: a close the operator asked for (Back, Escape, the scrim, a
// swipe down, Cancel) asks "Discard?" while there is a draft. Asked again
// while the question is up, Back means Discard (the yes: Back alone can
// always leave a form) and any other close Keep.
//
//   const guard = useDraftGuard(!!text.trim(), busy, () => field.current);
//   ...
//   ${guard.asking && html`<${DiscardAsk} what="this comment" guard=${guard} close=${close} />`}
//
// Cancel buttons in such a sheet call requestClosePhoneSheet, so they are
// guarded too; the Body's own close() (after a send, or Discard) is not.

import { h } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { useSheetGuard } from './phone-sheet.js';
import { ButtonRow } from './phone-ui.js';

const html = htm.bind(h);

// useDraftGuard guards the sheet this Body renders in while `dirty`; `busy`
// (a send in flight) holds any close. Either may be a function, read at the
// moment of the close: a draft a child holds, reported as it is typed. focusField() is the field to give the
// focus back to once the question is gone and the draft kept, by Keep or by
// a close asked for again. Returns { asking, keep }: whether the question is
// up, and Keep.
export function useDraftGuard(dirty, busy = false, focusField = null) {
  const [asking, setAsking] = useState(false);
  const askingNow = useRef(false);
  askingNow.current = asking;
  const field = useRef(focusField);
  field.current = focusField;
  const wasAsking = useRef(false);
  useEffect(() => {
    if (wasAsking.current && !asking) field.current?.()?.focus?.();
    wasAsking.current = asking;
  }, [asking]);
  useSheetGuard((why) => {
    if (typeof busy === 'function' ? busy() : busy) return true;
    if (!(typeof dirty === 'function' ? dirty() : dirty)) return false;
    if (askingNow.current && why?.back) return false;
    askingNow.current = !askingNow.current;
    setAsking(askingNow.current);
    return true;
  });
  return { asking, keep: () => setAsking(false) };
}

// DiscardAsk is the question, in the sheet: Keep, or Discard, which closes it
// (the Body's own close). It takes the focus, as the message sheet's does;
// Keep gives it back to the field (useDraftGuard's focusField).
export function DiscardAsk({ what = 'this', guard, close }) {
  const row = useRef(null);
  useLayoutEffect(() => { row.current?.querySelector('button')?.focus(); }, []);
  const keep = () => guard.keep();
  return html`<div ref=${row} class="phone-compose-foot phone-draft-ask" role="alertdialog" aria-label=${`Discard ${what}?`}>
    <span class="phone-compose-ask">Discard ${what}?</span>
    <${ButtonRow} buttons=${[
      { key: 'keep', label: 'Keep', onClick: keep },
      { key: 'discard', label: 'Discard', tone: 'no', onClick: close },
    ]} />
  </div>`;
}
