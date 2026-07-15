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
  dialogClass = 'modal',
  hideCancel = false,
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
      dialogClass=${dialogClass}
    >
      <h3 id=${labelledby}>${title}</h3>
      ${meta ? html`<div class="modal-meta" id=${metaID || undefined}>${meta}</div>` : null}
      ${children}
      <div class="cleanup-error" id=${errorID || undefined} role=${error ? 'alert' : undefined}>${error}</div>
      <div class="modal-buttons">
        ${hideCancel ? null : html`<button id=${cancelID || `${baseID}-cancel`} type="button" disabled=${busy} onClick=${close}>Cancel</button>`}
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

function BulkRetireResult({ descriptor, response }) {
  const group = descriptor.kind === 'retire-group-preview';
  const rows = group ? (response?.members || []) : (response?.outcomes || []);
  return html`<div class="cleanup-list" id="retire-preview-list">
    ${rows.length ? rows.map((row) => {
      const status = group ? row.action : row.result;
      const worktree = group ? row.worktree : null;
      return html`<div class="cleanup-row" key=${row.conv_id || row.agent_id}>
        <span class=${`cleanup-badge ${status || ''}`}>${status || 'unknown'}</span>
        <span class="title">${row.title || row.conv_id || '(untitled)'}</span>
        <span class="id">${String(row.conv_id || '').slice(0, 8)}</span>
        <span class="meta">${row.detail || ''}</span>
        ${worktree ? html`<span class=${`cleanup-badge ${worktree.action || ''}`}>
          worktree ${worktree.action || 'unknown'}
        </span><span class="meta">${worktree.detail || ''}</span>` : null}
      </div>`;
    }) : html`<div class="cleanup-empty">Nothing to do.</div>`}
  </div>`;
}

function bulkRetireSummary(descriptor, response) {
  if (descriptor.kind === 'retire-group-preview') {
    const members = response?.members || [];
    const retired = members.filter((member) => member.action === 'retired').length;
    const failed = members.filter((member) => member.action === 'error').length;
    const skipped = members.filter((member) => String(member.action || '').startsWith('skipped')).length;
    return { retired, skipped, failed };
  }
  return {
    retired: Number(response?.retired || 0),
    skipped: Number(response?.skipped || 0),
    failed: Number(response?.failed || 0),
  };
}

function summaryText(summary) {
  const parts = [`${summary.retired} retired`];
  if (summary.skipped) parts.push(`${summary.skipped} skipped`);
  if (summary.failed) parts.push(`${summary.failed} failed`);
  return parts.join(' · ');
}

function BulkRetireDialog({ descriptor, actions, confirmDiscard }) {
  const candidates = descriptor.candidates || [];
  const [query, setQuery] = useState('');
  const [selected, setSelected] = useState(
    () => new Set(candidates.map((candidate) => candidate.conv_id)),
  );
  const [shutdown, setShutdown] = useState(true);
  const [deleteWorktrees, setDeleteWorktrees] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [submittedRequest, setSubmittedRequest] = useState(null);
  const [result, setResult] = useState(null);
  const activeRef = useRef(true);
  useEffect(() => () => { activeRef.current = false; }, []);

  const normalizedQuery = query.trim().toLowerCase();
  const matchesFilter = (candidate) => !normalizedQuery
    || String(candidate.title || '').toLowerCase().includes(normalizedQuery)
    || String(candidate.conv_id || '').toLowerCase().includes(normalizedQuery);
  const visibleCandidates = candidates.filter(matchesFilter);
  const selectedCandidates = candidates.filter((candidate) => selected.has(candidate.conv_id));
  const locked = !!submittedRequest || !!result;
  const dirty = !result && (
    query !== '' || selected.size !== candidates.length
    || !shutdown || !deleteWorktrees || !!submittedRequest
  );
  const regularTitle = descriptor.kind === 'retire-group-preview'
    ? `Retire ${descriptor.status} agents in "${descriptor.group}"`
    : 'Retire ungrouped agents';
  const wizardTitle = descriptor.kind === 'retire-group-preview'
    ? `Banish ${descriptor.status} familiars in "${descriptor.group}"`
    : 'Banish unbound familiars';

  const updateVisible = (checked) => {
    if (busy || locked) return;
    setSelected((current) => {
      const next = new Set(current);
      for (const candidate of visibleCandidates) {
        if (checked) next.add(candidate.conv_id);
        else next.delete(candidate.conv_id);
      }
      return next;
    });
  };
  const updateCandidate = (candidate, checked) => {
    if (busy || locked) return;
    setSelected((current) => {
      const next = new Set(current);
      if (checked) next.add(candidate.conv_id);
      else next.delete(candidate.conv_id);
      return next;
    });
  };
  const changeShutdown = (event) => {
    if (busy || locked) return;
    const checked = event.currentTarget.checked;
    setShutdown(checked);
    // Bulk retire deliberately differs from single retire: turning shutdown
    // off forces worktree deletion off, while turning it back on merely
    // re-enables the control and preserves that visible OFF choice.
    if (!checked) setDeleteWorktrees(false);
  };

  const finishResult = async () => {
    if (!result || busy) return;
    setBusy(true);
    try {
      await actions.finishBulkRetire({ kind: descriptor.kind, response: result });
    } catch (_) {
      // Roster refresh is advisory after an accepted mutation. The action
      // releases ownership in finally, so a refresh failure must not become an
      // unhandled rejection after this component has already unmounted.
    }
  };
  const close = () => {
    if (result) void finishResult();
    else actions.close();
  };
  const submit = async () => {
    if (busy) return;
    if (result) {
      await finishResult();
      return;
    }
    if (selectedCandidates.length === 0) return;
    const request = submittedRequest || (descriptor.kind === 'retire-group-preview'
      ? Object.freeze({
        group: descriptor.group,
        convs: Object.freeze(selectedCandidates.map((candidate) => candidate.conv_id)),
        shutdown,
        deleteWorktree: shutdown && deleteWorktrees,
      })
      : Object.freeze({
        agents: Object.freeze(selectedCandidates.map((candidate) => candidate.agent_id || candidate.conv_id)),
        shutdown,
        deleteWorktrees: shutdown && deleteWorktrees,
      }));
    if (!submittedRequest) setSubmittedRequest(request);
    setError('');
    setBusy(true);
    try {
      const response = descriptor.kind === 'retire-group-preview'
        ? await actions.retireGroupPreview(request)
        : await actions.retireUngroupedPreview(request);
      if (activeRef.current) setResult(response || {});
    } catch (cause) {
      if (activeRef.current) setError(cause?.message || String(cause));
    } finally {
      if (activeRef.current) setBusy(false);
    }
  };

  const retrying = !!submittedRequest && !result;
  const summary = result ? bulkRetireSummary(descriptor, result) : null;
  const warning = result && (result.warnings || []).length
    ? `⚠ ${result.warnings.join('  ⚠ ')}` : '';
  const hint = result
    ? html`<${Words}
      plain=${`Retire complete — ${summaryText(summary)}.`}
      wizard=${`Banishment complete — ${summaryText(summary)}.`}
    />`
    : descriptor.kind === 'retire-group-preview'
      ? html`<${Words}
        plain=${`These ${descriptor.status} agents in group "${descriptor.group}" will be demoted to plain, reinstatable conversations. Each ticked agent is removed from all its groups, including groups it owns, and its permission and sudo grants are revoked. Untick any you want to keep; only ticked agents are retired.`}
        wizard=${`These ${descriptor.status} familiars in party "${descriptor.group}" will return to restorable conversation scrolls. Each ticked familiar is removed from all its parties, including parties it owns, and its boons and sudo grants are revoked. Untick any you want to keep; only ticked familiars are banished.`}
      />`
      : html`<${Words}
        plain="These agents are not in any group. Each ticked agent will be demoted to a plain, reinstatable conversation and its grants revoked."
        wizard="These unbound familiars belong to no party. Each ticked familiar will return to a restorable conversation scroll and lose its boons."
      />`;

  return html`<${TransactionDialogFrame}
    id="retire-preview-modal"
    labelledby="retire-preview-title"
    title=${html`<${Words} plain=${regularTitle} wizard=${wizardTitle} />`}
    dialogClass="cleanup-modal"
    busy=${busy}
    dirty=${dirty}
    error=${result ? warning : error}
    errorID="retire-preview-error"
    primaryLabel=${result ? 'Done' : retrying ? 'Retry retire' : selectedCandidates.length === 1
      ? 'Retire 1 agent' : `Retire ${selectedCandidates.length} agents`}
    busyLabel=${html`<span class="btn-spinner" aria-hidden="true"></span>${result ? 'Refreshing…' : retrying ? 'Retrying…' : 'Retiring…'}`}
    primaryClass=${result ? 'primary' : 'primary danger'}
    submitDisabled=${!result && selectedCandidates.length === 0}
    hideCancel=${!!result}
    cancelID="retire-preview-cancel"
    submitID="retire-preview-submit"
    onClose=${close}
    onSubmit=${submit}
    confirmDiscard=${confirmDiscard}
  >
    <p class="cleanup-hint" id="retire-preview-hint">${hint}</p>
    ${result ? html`<${BulkRetireResult} descriptor=${descriptor} response=${result} />` : html`
      <div class="cleanup-toolbar">
        <button type="button" id="retire-preview-select-all" disabled=${busy || locked} onClick=${() => updateVisible(true)}>select all</button>
        <button type="button" id="retire-preview-select-none" disabled=${busy || locked} onClick=${() => updateVisible(false)}>select none</button>
        <input
          type="search"
          id="retire-preview-search"
          placeholder="filter title / id…"
          aria-label="Filter agents"
          value=${query}
          disabled=${busy || locked}
          onInput=${(event) => setQuery(event.currentTarget.value)}
        />
        <span class="cleanup-count" id="retire-preview-count">${selectedCandidates.length} of ${candidates.length} selected</span>
      </div>
      <div class="cleanup-list" id="retire-preview-list">
        ${visibleCandidates.length ? visibleCandidates.map((candidate) => html`
          <div class="cleanup-row" key=${candidate.conv_id}><label>
            <input
              type="checkbox"
              data-conv=${candidate.conv_id}
              checked=${selected.has(candidate.conv_id)}
              disabled=${busy || locked}
              onChange=${(event) => updateCandidate(candidate, event.currentTarget.checked)}
            />
            <span class="title">${candidate.title || '(untitled)'}</span>
            <span class="id">${candidate.conv_id.slice(0, 8)}</span>
            <span class="cleanup-badge">${candidate.status}</span>
            ${candidate.role ? html`<span class="cleanup-badge">${candidate.role}</span>` : null}
          </label></div>
        `) : html`<div class="cleanup-empty">no agents match the filter</div>`}
      </div>
      <label class="delete-agent-wt" id="retire-preview-shutdown-row">
        <input
          type="checkbox"
          id="retire-preview-shutdown"
          checked=${shutdown}
          disabled=${busy || locked}
          onChange=${changeShutdown}
        />
        <span><${Words} plain="Also shut down running sessions" wizard="Also slumber running familiars" />
          <span class="wt-note">soft-exits each tmux pane (/exit) — conversations are kept either way</span>
        </span>
      </label>
      <label class=${`delete-agent-wt${!shutdown ? ' disabled' : ''}`} id="retire-preview-wt-row">
        <input
          type="checkbox"
          id="retire-preview-wt"
          checked=${deleteWorktrees}
          disabled=${busy || locked || !shutdown}
          onChange=${(event) => {
            if (!busy && !locked && shutdown) setDeleteWorktrees(event.currentTarget.checked);
          }}
        />
        <span><${Words} plain="Also delete each agent’s git worktree + branch" wizard="Also dissolve each familiar’s git worktree + branch" />
          <span class="wt-note">main/shared/no-worktree agents are kept by the daemon</span>
        </span>
      </label>
    `}
  </${TransactionDialogFrame}>`;
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
      ...(worktree?.removable === true && deleteWorktree === true
        ? { expectedWorktree: worktree.path } : {}),
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
  if (current.descriptor.kind === 'retire-group-preview'
    || current.descriptor.kind === 'retire-ungrouped-preview') {
    return html`<${BulkRetireDialog}
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
