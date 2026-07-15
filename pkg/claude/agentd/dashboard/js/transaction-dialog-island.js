import { h, render } from 'preact';
import { useEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { registerTransactionDialogController } from './transaction-dialog-controller.js';

const html = htm.bind(h);

// Shared chrome for the lifecycle, destructive, bulk-selection, cleanup, and
// worktree-cleanup dialogs. Feature components keep their own controlled form
// state, while this frame supplies the transaction invariants: one focus
// boundary, dirty confirmation, guarded backdrop drags, blocked busy dismissal,
// non-dismissible request errors, and a retry-capable primary action.
export function TransactionDialogFrame({
  id,
  labelledby,
  title,
  meta = '',
  busy = false,
  dirty = false,
  error = '',
  primaryLabel,
  busyLabel = primaryLabel,
  primaryClass = 'confirm-danger',
  submitDisabled = false,
  onClose,
  onSubmit,
  confirmDiscard,
  children,
}) {
  const submitRef = useRef(null);
  const submitLock = useRef(false);
  const baseID = id.endsWith('-modal') ? id.slice(0, -6) : id;
  useEffect(() => {
    // A completed/failed transaction publishes busy=false and explicitly
    // re-arms the same frozen dialog for retry. Until that edge, keep a
    // synchronous lock as well as the rendered disabled state so two click
    // events in one render cannot start parallel requests.
    if (!busy) submitLock.current = false;
  }, [busy]);
  const submit = () => {
    if (busy || submitDisabled || submitLock.current) return;
    submitLock.current = true;
    onSubmit?.();
  };
  const close = () => {
    if (!busy) onClose?.();
  };
  return html`
    <${Overlay}
      id=${id}
      labelledby=${labelledby}
      onClose=${close}
      onSubmitHotkey=${submit}
      dirty=${dirty}
      blocked=${busy}
      confirmDiscard=${confirmDiscard}
      guardBackdropDrag=${true}
      initialFocusRef=${submitRef}
      dialogClass="modal"
    >
      <h3 id=${labelledby}>${title}</h3>
      ${meta ? html`<div class="modal-meta">${meta}</div>` : null}
      ${children}
      <div class="cleanup-error" role=${error ? 'alert' : undefined}>${error}</div>
      <div class="modal-buttons">
        <button id=${`${baseID}-cancel`} type="button" disabled=${busy} onClick=${close}>Cancel</button>
        <span class="spacer"></span>
        <button
          ref=${submitRef}
          id=${`${baseID}-submit`}
          class=${primaryClass}
          type="button"
          disabled=${busy || submitDisabled}
          aria-busy=${busy ? 'true' : undefined}
          onClick=${submit}
        >${busy ? busyLabel : primaryLabel}</button>
      </div>
    </${Overlay}>
  `;
}

export function TransactionDialogApp({ state }) {
  // Concrete keyed renderers arrive in the following atomic checkpoints. Read
  // the signal now so the mounted root already owns the production lifecycle;
  // no legacy launcher is redirected until its corresponding renderer exists.
  void state.dialog.value;
  return null;
}

export function mountTransactionDialogIsland({ host, state, registerCleanup }) {
  render(html`<${TransactionDialogApp} state=${state} />`, host);
  const unregister = registerTransactionDialogController(state);
  registerCleanup(() => {
    unregister();
    state.dispose();
    render(null, host);
  });
}
