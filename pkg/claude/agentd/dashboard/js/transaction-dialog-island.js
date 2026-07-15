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
// non-dismissible request errors, and one shared lock across the primary plus
// an optional alternate mutation action.
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
  alternateLabel = '',
  alternateBusyLabel = alternateLabel,
  alternateClass = 'confirm-danger',
  alternateDisabled = false,
  alternateID = '',
  alternateTitle = '',
  busyAction = 'primary',
  metaID = '',
  errorID = '',
  cancelID = '',
  submitID = '',
  onClose,
  onSubmit,
  onAlternateSubmit,
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
  const submit = (action = 'primary') => {
    const alternate = action === 'alternate';
    if (busy || submitLock.current
      || (alternate ? alternateDisabled : submitDisabled)) return;
    const handler = alternate ? onAlternateSubmit : onSubmit;
    if (!handler) return;
    submitLock.current = true;
    handler();
  };
  const close = () => {
    if (!busy) onClose?.();
  };
  return html`
    <${Overlay}
      id=${id}
      labelledby=${labelledby}
      onClose=${close}
      onSubmitHotkey=${() => submit('primary')}
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
        ${alternateLabel ? html`<button
          id=${alternateID || `${baseID}-alternate`}
          class=${alternateClass}
          type="button"
          title=${alternateTitle || undefined}
          disabled=${busy || alternateDisabled}
          aria-busy=${busy && busyAction === 'alternate' ? 'true' : undefined}
          onClick=${() => submit('alternate')}
        >${busy && busyAction === 'alternate' ? alternateBusyLabel : alternateLabel}</button>` : null}
        <button
          ref=${submitRef}
          id=${submitID || `${baseID}-submit`}
          class=${primaryClass}
          type="button"
          disabled=${busy || submitDisabled}
          aria-busy=${busy && busyAction === 'primary' ? 'true' : undefined}
          onClick=${() => submit('primary')}
        >${busy && busyAction === 'primary' ? busyLabel : primaryLabel}</button>
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

function DeleteWorktreeChoice({ worktree, checked, disabled, onChange }) {
  if (!worktree?.path || worktree.kind === 'none') return null;
  const path = worktreePath(worktree);
  if (!worktree.removable) {
    let reason = html`not removable`;
    if (worktree.kind === 'main') reason = html`the repo’s main worktree, never removed`;
    else if (worktree.shared) {
      reason = html`<${Words} plain="shared with another agent" wizard="shared with another familiar"/>`;
    }
    return html`
      <label class="delete-agent-wt disabled" id="delete-agent-wt-row">
        <input type="checkbox" id="delete-agent-wt" checked=${false} disabled />
        <span id="delete-agent-wt-label">Git worktree kept${' '}
          <span class="wt-note">${path} — ${reason}</span>
        </span>
      </label>
    `;
  }
  return html`
    <label class=${`delete-agent-wt${disabled ? ' disabled' : ''}`} id="delete-agent-wt-row">
      <input
        type="checkbox"
        id="delete-agent-wt"
        checked=${checked}
        disabled=${disabled}
        onChange=${onChange}
      />
      <span id="delete-agent-wt-label">Also delete the git worktree${' '}
        <span class="wt-note">${path} — directory removed, branch kept</span>
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
  const busyLabel = html`<span class="btn-spinner" aria-hidden="true"></span>${retrying ? 'Retrying…' : 'Retiring…'}`;
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
      busyLabel=${busyLabel}
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

function ShutdownAgentDialog({ descriptor, actions, confirmDiscard }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [submittedRequest, setSubmittedRequest] = useState(null);
  const activeRef = useRef(true);
  useEffect(() => () => { activeRef.current = false; }, []);

  const submit = async (force) => {
    if (busy) return;
    const request = submittedRequest || Object.freeze({
      agent: descriptor.agent,
      label: descriptor.label,
      force: force === true,
    });
    if (submittedRequest && submittedRequest.force !== (force === true)) return;
    if (!submittedRequest) setSubmittedRequest(request);
    setError('');
    setBusy(true);
    try {
      await actions.shutdownAgent(request);
    } catch (cause) {
      if (activeRef.current) setError(cause?.message || String(cause));
    } finally {
      if (activeRef.current) setBusy(false);
    }
  };

  const retrying = !!submittedRequest;
  const forceChoice = submittedRequest?.force === true;
  return html`
    <${TransactionDialogFrame}
      id="shutdown-modal"
      labelledby="shutdown-title"
      title="Shut down agent?"
      meta=${descriptor.label || ''}
      metaID="shutdown-meta"
      error=${error}
      errorID="shutdown-error"
      busy=${busy}
      busyAction=${forceChoice ? 'alternate' : 'primary'}
      primaryLabel=${retrying && !forceChoice ? 'Retry soft exit' : 'Soft exit'}
      busyLabel=${html`<span class="btn-spinner" aria-hidden="true"></span>${retrying ? 'Retrying soft exit…' : 'Soft exiting…'}`}
      primaryClass=""
      submitDisabled=${retrying && forceChoice}
      submitID="shutdown-soft"
      alternateLabel=${retrying && forceChoice ? 'Retry force kill' : 'Force kill'}
      alternateBusyLabel=${html`<span class="btn-spinner" aria-hidden="true"></span>${retrying ? 'Retrying force kill…' : 'Force killing…'}`}
      alternateDisabled=${retrying && !forceChoice}
      alternateID="shutdown-force"
      alternateTitle="Immediately kills the tmux session; use if soft exit is stuck"
      cancelID="shutdown-cancel"
      onClose=${actions.close}
      onSubmit=${() => submit(false)}
      onAlternateSubmit=${() => submit(true)}
      confirmDiscard=${confirmDiscard}
    >
      <p>Soft exit injects /exit into tmux pane. Conv jsonl is preserved; in-flight tool calls are interrupted.</p>
    </${TransactionDialogFrame}>
  `;
}

function DeleteAgentDialog({ descriptor, actions, confirmDiscard }) {
  const [worktree, setWorktree] = useState(null);
  const [deleteWorktree, setDeleteWorktree] = useState(false);
  const [probing, setProbing] = useState(true);
  const [probeError, setProbeError] = useState('');
  const [probeVersion, setProbeVersion] = useState(0);
  const [busy, setBusy] = useState(false);
  const [mutationError, setMutationError] = useState('');
  const [submittedRequest, setSubmittedRequest] = useState(null);
  const activeRef = useRef(true);
  const submittedRef = useRef(null);
  const probeGeneration = useRef(0);
  const probeAbort = useRef(null);
  submittedRef.current = submittedRequest;

  useEffect(() => () => { activeRef.current = false; }, []);
  useEffect(() => {
    const generation = ++probeGeneration.current;
    const controller = new AbortController();
    probeAbort.current = controller;
    setProbing(true);
    setProbeError('');
    setWorktree(null);
    setDeleteWorktree(false);
    actions.loadAgentWorktree(descriptor.agent, { signal: controller.signal }).then(
      (next) => {
        if (!activeRef.current || controller.signal.aborted
          || generation !== probeGeneration.current || submittedRef.current) return;
        setWorktree(next || null);
        setDeleteWorktree(next?.removable === true);
        setProbing(false);
      },
      (cause) => {
        if (!activeRef.current || controller.signal.aborted
          || generation !== probeGeneration.current || submittedRef.current) return;
        setWorktree(null);
        setDeleteWorktree(false);
        setProbing(false);
        if (cause?.name !== 'AbortError') {
          setProbeError(`Worktree check failed: ${cause?.message || cause}`);
        }
      },
    );
    return () => {
      controller.abort();
      if (probeAbort.current === controller) probeAbort.current = null;
    };
  }, [descriptor.agent, probeVersion]);

  const retryProbe = () => {
    if (busy || probing || submittedRequest) return;
    setProbeVersion((current) => current + 1);
  };
  const submit = async () => {
    if (busy) return;
    const request = submittedRequest || Object.freeze({
      agent: descriptor.agent,
      label: descriptor.label,
      deleteWorktree: worktree?.removable === true && deleteWorktree === true,
    });
    if (!submittedRequest) {
      submittedRef.current = request;
      setSubmittedRequest(request);
      probeAbort.current?.abort();
    }
    setProbeError('');
    setMutationError('');
    setBusy(true);
    try {
      await actions.deleteAgent(request);
    } catch (cause) {
      if (activeRef.current) setMutationError(cause?.message || String(cause));
    } finally {
      if (activeRef.current) setBusy(false);
    }
  };

  const retrying = !!submittedRequest;
  const title = html`<span class="theme-copy-regular">Permanently delete this agent?</span><span class="theme-copy-wizard">Permanently erase this familiar?</span>`;
  const initialLabel = html`<span class="theme-copy-regular">Delete forever</span><span class="theme-copy-wizard">Erase forever</span>`;
  return html`
    <${TransactionDialogFrame}
      id="delete-agent-modal"
      labelledby="delete-agent-title"
      title=${title}
      meta=${descriptor.label || descriptor.agent}
      metaID="delete-agent-meta"
      error=${mutationError || probeError}
      errorID="delete-agent-error"
      busy=${busy}
      primaryLabel=${retrying ? 'Retry delete' : initialLabel}
      busyLabel=${html`<span class="btn-spinner" aria-hidden="true"></span>${retrying ? 'Retrying delete…' : 'Deleting…'}`}
      submitID="delete-agent-ok"
      cancelID="delete-agent-cancel"
      onClose=${actions.close}
      onSubmit=${submit}
      confirmDiscard=${confirmDiscard}
    >
      <p><${Words}
        plain="Wipes the conversation history (.jsonl) from disk and drops every group / membership / ownership / permission row for this agent. This cannot be undone."
        wizard="Burns the conversation scroll (.jsonl) and erases every party membership, ownership mark, and boon bound to this familiar. This cannot be undone."
      /></p>
      <${DeleteWorktreeChoice}
        worktree=${worktree}
        checked=${deleteWorktree}
        disabled=${busy || retrying}
        onChange=${(event) => {
          if (!busy && !retrying) setDeleteWorktree(event.currentTarget.checked);
        }}
      />
      ${probeError && !retrying ? html`<div class="transaction-probe-retry">
        <button
          id="delete-agent-wt-retry"
          type="button"
          disabled=${probing || busy}
          onClick=${retryProbe}
        >Retry worktree check</button>
      </div>` : null}
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
  if (current.descriptor.kind === 'shutdown-agent') {
    return html`<${ShutdownAgentDialog}
      key=${current.key}
      descriptor=${current.descriptor}
      actions=${actions}
      confirmDiscard=${confirmDiscard}
    />`;
  }
  if (current.descriptor.kind === 'delete-agent') {
    return html`<${DeleteAgentDialog}
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
