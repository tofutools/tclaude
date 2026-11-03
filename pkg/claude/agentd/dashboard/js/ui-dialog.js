// Dialog: the standard dashboard dialog. ManagementOverlay owns the shell's
// behaviour (focus, Escape, backdrop, dirty/discard, hotkeys, resize); Dialog
// adds the parts every dialog used to hand-write around it:
//
//   - the title, an <h3> whose id the overlay is labelled by;
//   - the error slot, always role="alert" (it collapses while empty);
//   - the .modal-buttons footer: cancel and footerStart on the left, a
//     spacer, then footer on the right (after, such as a CLI note, follows
//     the footer);
//   - the guarded close, shared through context, so a Cancel or Close button
//     (DialogClose) or any other control (useDialogClose) enters the same
//     close transaction as Escape and the backdrop.
//
// Ids derive from the overlay id: "group-create-modal" titles itself
// "group-create-title", and its error and cancel ids are "group-create-error"
// and "group-create-cancel". Pass titleId / errorId / cancelId to keep a
// dialog's existing ids where they differ. Every other prop goes to
// ManagementOverlay.

import { createContext, h } from 'preact';
import { useContext } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay, useGuardedOverlayClose } from './management-overlay.js';
import { Button } from './ui-components.js';

const html = htm.bind(h);
const DialogCloseContext = createContext(() => Promise.resolve(false));

// useDialogClose returns the dialog's guarded close: it resolves true once
// the dialog closed, false when a guard (busy, dirty and kept) stopped it.
export function useDialogClose() {
  return useContext(DialogCloseContext);
}

// DialogClose: a footer button that closes the dialog through its guard.
export function DialogClose({ children = 'Cancel', ...rest }) {
  const close = useDialogClose();
  return html`<${Button} ...${rest} onClick=${() => { void close(); }}>${children}<//>`;
}

function idStem(id) {
  return String(id || '').replace(/-modal$/, '');
}

export function Dialog({
  id,
  title,
  titleId,
  error,
  errorId,
  errorClass = '',
  cancel = null,
  cancelId,
  footerStart = null,
  footer = null,
  after = null,
  busy = false,
  blocked,
  guard,
  children,
  ...overlay
}) {
  const own = useGuardedOverlayClose();
  const { requestClose, registerClose } = guard || own;
  const stem = idStem(id);
  const labelledby = overlay.labelledby ?? (title != null ? titleId || `${stem}-title` : undefined);
  const hasFooter = cancel != null || footerStart != null || footer != null;
  return html`<${ManagementOverlay}
    ...${overlay}
    id=${id}
    labelledby=${labelledby}
    blocked=${blocked ?? busy}
    registerClose=${registerClose}
  ><${DialogCloseContext.Provider} value=${requestClose}>
    ${title != null && html`<h3 id=${titleId || `${stem}-title`}>${title}</h3>`}
    ${children}
    ${error !== undefined && html`<div
      class=${errorClass ? `cron-create-error ui-error-box ${errorClass}` : 'cron-create-error ui-error-box'}
      id=${errorId || `${stem}-error`}
      role="alert"
    >${error}</div>`}
    ${hasFooter && html`<div class="modal-buttons">
      ${cancel != null && html`<${DialogClose} id=${cancelId || `${stem}-cancel`} disabled=${busy}>${cancel}<//>`}
      ${footerStart}
      <span class="spacer"></span>
      ${footer}
    </div>`}
    ${after}
  <//><//>`;
}
