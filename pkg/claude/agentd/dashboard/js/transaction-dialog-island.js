import { h, render } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
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
  metaID = '',
  errorID = '',
  cancelID = '',
  submitID = '',
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
      ${meta ? html`<div class="modal-meta" id=${metaID || undefined}>${meta}</div>` : null}
      ${children}
      <div class="cleanup-error" id=${errorID || undefined} role=${error ? 'alert' : undefined}>${error}</div>
      <div class="modal-buttons">
        <button id=${cancelID || `${baseID}-cancel`} type="button" disabled=${busy} onClick=${close}>Cancel</button>
        <span class="spacer"></span>
        <button
          ref=${submitRef}
          id=${submitID || `${baseID}-submit`}
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

function Words({ plain, wizard }) {
  return html`<span class="theme-copy-regular">${plain}</span><span class="theme-copy-wizard">${wizard}</span>`;
}

function worktreePath(worktree) {
  return worktree.path + (worktree.branch ? ` · ${worktree.branch}` : '');
}

function RetireWorktreeChoice({ worktree, shutdown, checked, disabled, onChange }) {
  if (!worktree?.path || worktree.kind === 'none') return null;
  const path = worktreePath(worktree);
  if (!worktree.removable) {
    let reason = html`not removable`;
    if (worktree.kind === 'main') reason = html`the repo’s main worktree, never removed`;
    else if (worktree.shared) {
      reason = html`<${Words} plain="shared with another agent" wizard="shared with another familiar"/>`;
    }
    return html`
      <label class="delete-agent-wt disabled" id="retire-wt-row">
        <input type="checkbox" id="retire-wt" checked=${false} disabled />
        <span id="retire-wt-label">Git worktree kept
          <span class="wt-note">${path} — ${reason}</span>
        </span>
      </label>
    `;
  }
  const note = shutdown
    ? html`${path} — removed after the agent exits`
    : html`${path} — requires shutting down the session first`;
  return html`
    <label class=${`delete-agent-wt${disabled ? ' disabled' : ''}`} id="retire-wt-row">
      <input
        type="checkbox"
        id="retire-wt"
        checked=${checked}
        disabled=${disabled}
        onChange=${onChange}
      />
      <span id="retire-wt-label">${shutdown ? 'Also delete' : 'Delete'} the git worktree + branch
        <span class="wt-note">${note}</span>
      </span>
    </label>
  `;
}

function RetireAgentDialog({ descriptor, actions, confirmDiscard }) {
  const [shutdown, setShutdown] = useState(true);
  const [deleteWorktree, setDeleteWorktree] = useState(false);
  const [worktree, setWorktree] = useState(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [submittedChoice, setSubmittedChoice] = useState(null);
  const activeRef = useRef(true);
  const shutdownRef = useRef(true);
  const submittedRef = useRef(null);
  const probeGeneration = useRef(0);
  const probeAbort = useRef(null);
  shutdownRef.current = shutdown;
  submittedRef.current = submittedChoice;

  useEffect(() => () => { activeRef.current = false; }, []);
  useEffect(() => {
    const generation = ++probeGeneration.current;
    const controller = new AbortController();
    probeAbort.current = controller;
    actions.loadAgentWorktree(descriptor.conv, { signal: controller.signal }).then(
      (next) => {
        if (!activeRef.current || controller.signal.aborted
          || generation !== probeGeneration.current || submittedRef.current) return;
        setWorktree(next || null);
        setDeleteWorktree(!!next?.removable && shutdownRef.current);
      },
      (cause) => {
        // Worktree discovery is advisory. Preserve the safe hidden/off default
        // for ordinary failures; AbortError is the expected close/reopen path.
        if (cause?.name !== 'AbortError' && activeRef.current
          && generation === probeGeneration.current && !submittedRef.current) {
          setWorktree(null);
          setDeleteWorktree(false);
        }
      },
    );
    return () => {
      controller.abort();
      if (probeAbort.current === controller) probeAbort.current = null;
    };
  }, [descriptor.conv]);

  const locked = !!submittedChoice;
  const changeShutdown = (event) => {
    if (locked || busy) return;
    const next = event.currentTarget.checked;
    shutdownRef.current = next;
    setShutdown(next);
    setDeleteWorktree(!!worktree?.removable && next);
  };
  const submit = async () => {
    if (busy) return;
    const choice = submittedChoice || Object.freeze({
      shutdown,
      deleteWorktree: !!worktree?.removable && shutdown && deleteWorktree,
    });
    if (!submittedChoice) {
      submittedRef.current = choice;
      setSubmittedChoice(choice);
      probeAbort.current?.abort();
    }
    setError('');
    setBusy(true);
    try {
      const outcome = await actions.retireAgent({
        conv: descriptor.conv,
        label: descriptor.label,
        ...choice,
      });
      if (outcome?.dangling) {
        await actions.handoffDangling({
          ...outcome,
          conv: descriptor.conv,
          label: descriptor.label,
        });
      }
    } catch (cause) {
      if (activeRef.current) setError(cause?.message || String(cause));
    } finally {
      if (activeRef.current) setBusy(false);
    }
  };

  const title = html`<span class="retire-title-regular">Retire this agent?</span><span class="retire-title-wizard">Banish this familiar?</span>`;
  const retrying = !!submittedChoice;
  return html`
    <${TransactionDialogFrame}
      id="retire-modal"
      labelledby="retire-title"
      title=${title}
      meta=${descriptor.label || ''}
      metaID="retire-meta"
      error=${error}
      errorID="retire-error"
      busy=${busy}
      primaryLabel=${retrying ? 'Retry' : 'Retire'}
      busyLabel=${retrying ? 'Retrying…' : 'Retiring…'}
      cancelID="retire-cancel"
      submitID="retire-ok"
      onClose=${actions.close}
      onSubmit=${submit}
      confirmDiscard=${confirmDiscard}
    >
      <p><${Words}
        plain="Demotes the agent to a plain conversation: revokes its group memberships and permission grants so it stops being an agent. The conversation itself stays on disk and can be reinstated later — this is the non-destructive soft-delete."
        wizard="Returns the familiar to a plain conversation: revokes its party memberships and boons so it stops being a familiar. The conversation scroll stays on disk and can be restored later — banishment is not destruction."
      /></p>
      <label class="delete-agent-wt" id="retire-shutdown-row">
        <input
          type="checkbox"
          id="retire-shutdown"
          checked=${shutdown}
          disabled=${busy || locked}
          onChange=${changeShutdown}
        />
        <span>Also shut down the running session
          <span class="wt-note">soft-exits the tmux pane (/exit) — the conversation is kept either way</span>
        </span>
      </label>
      <${RetireWorktreeChoice}
        worktree=${worktree}
        shutdown=${shutdown}
        checked=${deleteWorktree}
        disabled=${busy || locked || !shutdown}
        onChange=${(event) => {
          if (!busy && !locked) setDeleteWorktree(event.currentTarget.checked);
        }}
      />
    </${TransactionDialogFrame}>
  `;
}

export function TransactionDialogApp({ state, actions, confirmDiscard }) {
  const current = state.dialog.value;
  if (!current) return null;
  if (current.descriptor.kind === 'retire-agent') {
    return html`<${RetireAgentDialog}
      key=${current.key}
      descriptor=${current.descriptor}
      actions=${actions}
      confirmDiscard=${confirmDiscard}
    />`;
  }
  return null;
}

export function mountTransactionDialogIsland({
  host, state, actions, confirmDiscard, registerCleanup,
}) {
  render(html`<${TransactionDialogApp}
    state=${state}
    actions=${actions}
    confirmDiscard=${confirmDiscard}
  />`, host);
  const unregister = registerTransactionDialogController(state);
  registerCleanup(() => {
    unregister();
    state.dispose();
    render(null, host);
  });
}
