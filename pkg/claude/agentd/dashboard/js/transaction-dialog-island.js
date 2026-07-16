import { h, render } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  ManagementOverlay as Overlay,
  useGuardedOverlayClose,
} from './management-overlay.js';
import { relTime } from './helpers.js';
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
  const { requestClose, registerClose } = useGuardedOverlayClose();
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
      registerClose=${registerClose}
    >
      <h3 id=${labelledby}>${title}</h3>
      ${meta ? html`<div class="modal-meta" id=${metaID || undefined}>${meta}</div>` : null}
      ${children}
      <div class="cleanup-error" id=${errorID || undefined} role=${error ? 'alert' : undefined}>${error}</div>
      <div class="modal-buttons">
        ${hideCancel ? null : html`<button id=${cancelID || `${baseID}-cancel`} type="button" disabled=${busy} onClick=${() => { void requestClose(); }}>Cancel</button>`}
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

const NO_WINDOW_ROLE = '(no role)';
const NO_WINDOW_GROUP = '(no group)';

function windowBucketKeys(candidates, field, emptyKey) {
  const keys = [];
  for (const candidate of candidates) {
    const values = candidate[field]?.length ? candidate[field] : [emptyKey];
    for (const value of values) {
      if (!keys.includes(value)) keys.push(value);
    }
  }
  keys.sort((left, right) =>
    (left === emptyKey) - (right === emptyKey) || left.localeCompare(right));
  return keys;
}

function WindowSelectionDialog({ descriptor, actions, confirmDiscard }) {
  const candidates = descriptor.candidates || [];
  const [direction, setDirection] = useState('focus');
  const [query, setQuery] = useState('');
  const [selected, setSelected] = useState(
    () => new Set(candidates.map((candidate) => candidate.conv_id)),
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [submittedRequest, setSubmittedRequest] = useState(null);
  const activeRef = useRef(true);
  useEffect(() => () => { activeRef.current = false; }, []);

  const normalizedQuery = query.trim().toLowerCase();
  const visibleCandidates = candidates.filter((candidate) => !normalizedQuery
    || candidate.title.toLowerCase().includes(normalizedQuery)
    || candidate.conv_id.toLowerCase().includes(normalizedQuery));
  const selectedCandidates = candidates.filter((candidate) => selected.has(candidate.conv_id));
  const locked = !!submittedRequest;
  const groupKeys = windowBucketKeys(candidates, 'groups', NO_WINDOW_GROUP);
  const roleKeys = windowBucketKeys(candidates, 'roles', NO_WINDOW_ROLE);
  const where = descriptor.scope === 'group'
    ? `group "${descriptor.group}"` : 'the dashboard';
  const wizardWhere = descriptor.scope === 'group'
    ? `party "${descriptor.group}"` : 'the tower';

  const bucketCandidates = (field, key, emptyKey) => candidates.filter((candidate) => {
    const values = candidate[field]?.length ? candidate[field] : [emptyKey];
    return values.includes(key);
  });
  const updateBucket = (field, key, emptyKey) => {
    if (busy || locked) return;
    const members = bucketCandidates(field, key, emptyKey);
    const next = new Set(selected);
    const allOn = members.every((candidate) => next.has(candidate.conv_id));
    for (const candidate of members) {
      if (allOn) next.delete(candidate.conv_id);
      else next.add(candidate.conv_id);
    }
    setSelected(next);
  };
  const updateAll = (checked) => {
    if (busy || locked) return;
    setSelected(checked
      ? new Set(candidates.map((candidate) => candidate.conv_id))
      : new Set());
  };
  const updateCandidate = (candidate, checked) => {
    if (busy || locked) return;
    const next = new Set(selected);
    if (checked) next.add(candidate.conv_id);
    else next.delete(candidate.conv_id);
    setSelected(next);
  };
  const submit = async () => {
    if (busy || selectedCandidates.length === 0) return;
    const request = submittedRequest || Object.freeze({
      direction,
      scope: descriptor.scope,
      ...(descriptor.scope === 'group' ? { group: descriptor.group } : {}),
      convs: Object.freeze(selectedCandidates.map(
        (candidate) => candidate.agent_id || candidate.conv_id,
      )),
      webTerminal: descriptor.webTerminal === true,
      targets: Object.freeze(selectedCandidates.map((candidate) => Object.freeze({
        selector: candidate.agent_id || candidate.conv_id,
        label: candidate.title || candidate.conv_id.slice(0, 8),
      }))),
    });
    if (!submittedRequest) setSubmittedRequest(request);
    setError('');
    setBusy(true);
    try {
      await actions.selectAgentWindows(request);
    } catch (cause) {
      if (activeRef.current) setError(cause?.message || String(cause));
    } finally {
      if (activeRef.current) setBusy(false);
    }
  };

  const retrying = !!submittedRequest;
  const plainHint = direction === 'focus'
    ? descriptor.webTerminal
      ? `Open or focus a web terminal pane for each selected running agent in ${where}.`
      : `Open or raise a terminal window for each selected running agent in ${where}.`
    : descriptor.webTerminal
      ? `Detach the web terminal panes of the selected running agents in ${where}. The agents keep running — only the terminal views are dismissed.`
      : `Detach the terminal windows of the selected running agents in ${where} so the desktop is decluttered. The agents keep running — only the windows are dismissed.`;
  const wizardHint = direction === 'focus'
    ? `Conjure a scrying portal for each chosen channeling familiar in ${wizardWhere}.`
    : `Draw the veil over the chosen familiars' scrying portals in ${wizardWhere} so the desktop is decluttered. The familiars keep channeling — only the portals are dismissed.`;
  const plainVerb = direction === 'focus' ? 'Focus' : 'Unfocus';
  const wizardVerb = direction === 'focus' ? 'Reveal' : 'Veil';
  const count = selectedCandidates.length;
  return html`<${TransactionDialogFrame}
    id="window-modal"
    labelledby="window-title"
    title=${html`<span class="window-title-regular">Agent windows</span><span class="window-title-wizard">Familiars' windows</span>`}
    dialogClass="cleanup-modal"
    busy=${busy}
    error=${error}
    errorID="window-error"
    primaryLabel=${html`<${Words}
      plain=${`${retrying ? 'Retry ' : ''}${plainVerb} ${count} agent${count === 1 ? '' : 's'}`}
      wizard=${`${retrying ? 'Retry ' : ''}${wizardVerb} ${count} familiar${count === 1 ? '' : 's'}`}
    />`}
    busyLabel=${html`<span class="btn-spinner" aria-hidden="true"></span><${Words}
      plain=${retrying ? 'Retrying…' : `${plainVerb}ing…`}
      wizard=${retrying ? 'Retrying…' : `${wizardVerb}ing…`}
    />`}
    primaryClass="primary"
    submitDisabled=${count === 0}
    cancelID="window-cancel"
    submitID="window-submit"
    onClose=${actions.close}
    onSubmit=${submit}
    confirmDiscard=${confirmDiscard}
  >
    <p class="cleanup-hint" id="window-hint"><${Words} plain=${plainHint} wizard=${wizardHint} /></p>
    <div class="window-direction" id="window-direction" role="radiogroup" aria-label="Window action">
      <label><input
        type="radio" name="window-direction" value="focus"
        checked=${direction === 'focus'} disabled=${busy || locked}
        onChange=${() => setDirection('focus')}
      />
        <span class="window-dir-label-regular">Focus</span><span class="window-dir-label-wizard">👁 Reveal</span>
        <span class="opt-note"><span class="window-dir-note-regular">— open or focus a terminal for each selected agent</span><span class="window-dir-note-wizard">— conjure a scrying portal for each chosen familiar</span></span>
      </label>
      <label><input
        type="radio" name="window-direction" value="unfocus"
        checked=${direction === 'unfocus'} disabled=${busy || locked}
        onChange=${() => setDirection('unfocus')}
      />
        <span class="window-dir-label-regular">Unfocus</span><span class="window-dir-label-wizard">🌫 Veil</span>
        <span class="opt-note"><span class="window-dir-note-regular">— detach the terminal views so the dashboard or desktop is decluttered. Detach-only: a window or tab tclaude opened closes itself, other tabs are untouched. The agents keep running — never the agent process.</span><span class="window-dir-note-wizard">— draw the veil over the chosen familiars' scrying portals so the desktop is decluttered. Veil-only: a window or tab tclaude opened closes itself, other tabs are untouched. The familiars keep channeling — never the familiar itself.</span></span>
      </label>
    </div>
    <div class="cleanup-toolbar">
      <button type="button" id="window-select-all" disabled=${busy || locked} onClick=${() => updateAll(true)}>select all</button>
      <button type="button" id="window-select-none" disabled=${busy || locked} onClick=${() => updateAll(false)}>select none</button>
      <input
        type="search" id="window-search" placeholder="filter title / id…"
        aria-label="Filter agents" value=${query} disabled=${busy || locked}
        onInput=${(event) => setQuery(event.currentTarget.value)}
      />
      <span class="spacer"></span>
      <span class="cleanup-count" id="window-count">${count} of ${candidates.length} selected</span>
    </div>
    <div class="window-groups" id="window-groups">
      <span class="roles-label"><${Words} plain="groups" wizard="parties" /></span>
      ${groupKeys.map((key) => {
        const members = bucketCandidates('groups', key, NO_WINDOW_GROUP);
        const on = members.filter((candidate) => selected.has(candidate.conv_id)).length;
        const stateClass = on === 0 ? '' : on === members.length ? ' on' : ' partial';
        return html`<button
          type="button" key=${key} class=${`window-role-chip${stateClass}`}
          data-group-chip=${key} disabled=${busy || locked}
          onClick=${() => updateBucket('groups', key, NO_WINDOW_GROUP)}
        >${key} (${on}/${members.length})</button>`;
      })}
    </div>
    <div class="window-roles" id="window-roles">
      <span class="roles-label"><${Words} plain="roles" wizard="classes" /></span>
      ${roleKeys.map((key) => {
        const members = bucketCandidates('roles', key, NO_WINDOW_ROLE);
        const on = members.filter((candidate) => selected.has(candidate.conv_id)).length;
        const stateClass = on === 0 ? '' : on === members.length ? ' on' : ' partial';
        return html`<button
          type="button" key=${key} class=${`window-role-chip${stateClass}`}
          data-role=${key} disabled=${busy || locked}
          onClick=${() => updateBucket('roles', key, NO_WINDOW_ROLE)}
        >${key} (${on}/${members.length})</button>`;
      })}
    </div>
    <div class="cleanup-list" id="window-list">
      ${visibleCandidates.length ? visibleCandidates.map((candidate) => html`
        <div class="cleanup-row" key=${candidate.conv_id}><label>
          <input
            type="checkbox" data-conv=${candidate.conv_id}
            checked=${selected.has(candidate.conv_id)} disabled=${busy || locked}
            onChange=${(event) => updateCandidate(candidate, event.currentTarget.checked)}
          />
          <span class="title">${candidate.title || '(untitled)'}</span>
          <span class="id">${candidate.conv_id.slice(0, 8)}</span>
          ${candidate.roles.map((role) => html`<span class="cleanup-badge" key=${role}>${role}</span>`)}
        </label></div>
      `) : html`<div class="cleanup-empty"><${Words} plain="no agents match the filter" wizard="no familiars match the filter" /></div>`}
    </div>
  </${TransactionDialogFrame}>`;
}

const CLEANUP_CATEGORY_ORDER = ['agent', 'retired', 'conversation'];
const CLEANUP_CATEGORY_LABEL = {
  agent: 'Active agents', retired: 'Retired agents', conversation: 'Conversations',
};

function cleanupTierCategories(tier) {
  if (tier === 'delete') return CLEANUP_CATEGORY_ORDER;
  if (tier === 'reinstate') return ['retired'];
  return ['agent'];
}

function cleanupInactivityHours(candidate) {
  if (!candidate.lastActivity) return Infinity;
  const parsed = Date.parse(candidate.lastActivity);
  if (Number.isNaN(parsed)) return Infinity;
  return (Date.now() - parsed) / 3600000;
}

function cleanupActivityLabel(candidate) {
  if (!candidate.lastActivity) return 'no recent activity';
  const relative = relTime(candidate.lastActivity);
  if (candidate.category === 'retired') return `retired ${relative}`;
  if (candidate.category === 'conversation') return `last activity ${relative}`;
  return `last seen ${relative}`;
}

function CleanupResult({ response }) {
  const outcomes = response?.outcomes || [];
  return html`<div class="cleanup-list" id="cleanup-list">
    ${outcomes.length ? outcomes.map((outcome) => html`
      <div class="cleanup-row" key=${outcome.conv_id || outcome.agent_id}>
        <span class=${`cleanup-badge ${outcome.result || ''}`}>${outcome.result || 'unknown'}</span>
        <span class="title">${outcome.title || String(outcome.conv_id || '').slice(0, 8)}</span>
        <span class="id">${String(outcome.conv_id || '').slice(0, 8)}</span>
        <span class="meta">${outcome.detail || ''}</span>
      </div>
    `) : html`<div class="cleanup-empty">Nothing to do.</div>`}
  </div>`;
}

function CleanupDialog({ descriptor, actions, confirmDiscard }) {
  const candidates = descriptor.candidates || [];
  const groupMode = descriptor.mode === 'group';
  const multiCategory = !groupMode;
  const [tier, setTier] = useState(groupMode ? 'unjoin' : descriptor.tier || 'delete');
  const [categoryOn, setCategoryOn] = useState(() => Object.fromEntries(
    CLEANUP_CATEGORY_ORDER.map((category) => [
      category, (descriptor.categories || CLEANUP_CATEGORY_ORDER).includes(category),
    ]),
  ));
  const [includeOnline, setIncludeOnline] = useState(false);
  const [includeOwners, setIncludeOwners] = useState(false);
  const [deleteWorktrees, setDeleteWorktrees] = useState(true);
  const [shutdown, setShutdown] = useState(true);
  const [query, setQuery] = useState('');
  const [age, setAge] = useState('0');
  const [selected, setSelected] = useState(() => new Set(
    groupMode
      ? candidates.filter((candidate) => !candidate.owner).map((candidate) => candidate.conv_id)
      : [],
  ));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [submittedRequest, setSubmittedRequest] = useState(null);
  const [result, setResult] = useState(null);
  const activeRef = useRef(true);
  useEffect(() => () => { activeRef.current = false; }, []);

  const effectiveTier = groupMode ? 'unjoin' : tier;
  const normalizedQuery = query.trim().toLowerCase();
  const rowVisible = (candidate) => {
    if (normalizedQuery && !candidate.title.toLowerCase().includes(normalizedQuery)
      && !candidate.conv_id.toLowerCase().includes(normalizedQuery)) return false;
    if (!multiCategory) return true;
    if (!categoryOn[candidate.category]) return false;
    if (!cleanupTierCategories(effectiveTier).includes(candidate.category)) return false;
    if (candidate.online && !includeOnline && effectiveTier !== 'reinstate') return false;
    return true;
  };
  const rowEnabled = (candidate) => !groupMode || !candidate.owner || includeOwners;
  const visibleCandidates = candidates.filter(rowVisible);
  const selectedCandidates = candidates.filter(
    (candidate) => rowVisible(candidate) && rowEnabled(candidate)
      && selected.has(candidate.conv_id),
  );
  const locked = !!submittedRequest;
  const controlsDisabled = busy || locked;

  const updateCandidate = (candidate, checked) => {
    if (controlsDisabled || !rowEnabled(candidate)) return;
    const next = new Set(selected);
    if (checked) next.add(candidate.conv_id);
    else next.delete(candidate.conv_id);
    setSelected(next);
  };
  const selectAll = () => {
    if (controlsDisabled) return;
    const next = new Set(selected);
    for (const candidate of visibleCandidates) {
      if (rowEnabled(candidate)) next.add(candidate.conv_id);
    }
    setSelected(next);
  };
  const selectNone = () => {
    if (!controlsDisabled) setSelected(new Set());
  };
  const applyAge = (value) => {
    if (controlsDisabled) return;
    const hours = Math.max(0, Number.parseFloat(value) || 0);
    const next = new Set(selected);
    for (const candidate of visibleCandidates) {
      if (!rowEnabled(candidate)) continue;
      if (cleanupInactivityHours(candidate) >= hours) next.add(candidate.conv_id);
      else next.delete(candidate.conv_id);
    }
    setSelected(next);
  };
  const changeOwners = (checked) => {
    if (controlsDisabled) return;
    setIncludeOwners(checked);
    if (groupMode) {
      const next = new Set(selected);
      for (const candidate of candidates) {
        if (!candidate.owner) continue;
        if (checked) next.add(candidate.conv_id);
        else next.delete(candidate.conv_id);
      }
      setSelected(next);
    }
  };

  const submit = async () => {
    if (busy || selectedCandidates.length === 0) return;
    const request = submittedRequest || Object.freeze({
      mode: descriptor.mode,
      ...(groupMode ? { group: descriptor.group } : {}),
      tier: effectiveTier,
      targets: Object.freeze(selectedCandidates.map(
        (candidate) => candidate.agent_id || candidate.conv_id,
      )),
      includeOwners: groupMode ? includeOwners
        : effectiveTier === 'unjoin' && includeOwners,
      includeOnline: !groupMode && effectiveTier !== 'reinstate' && includeOnline,
      deleteWorktrees: !groupMode && effectiveTier === 'delete' && deleteWorktrees,
      shutdown: !groupMode && effectiveTier === 'retire' && shutdown,
    });
    if (!submittedRequest) setSubmittedRequest(request);
    setError('');
    setBusy(true);
    try {
      const response = await actions.cleanup(request);
      if (activeRef.current) setResult(response || {});
    } catch (cause) {
      if (activeRef.current) setError(cause?.message || String(cause));
    } finally {
      if (activeRef.current) setBusy(false);
    }
  };

  const close = () => {
    if (result) actions.finishCleanup(result);
    else actions.close();
  };
  const count = selectedCandidates.length;
  const resultBits = result ? [
    ['removed', 'removed'], ['retired', 'retired'], ['reinstated', 'reinstated'],
    ['deleted', 'deleted'], ['skipped', 'skipped'], ['failed', 'failed'],
  ].filter(([field]) => result[field]).map(([field, label]) => `${result[field]} ${label}`) : [];

  let title;
  let hint;
  let primaryLabel;
  if (groupMode) {
    title = html`<${Words}
      plain=${`🧹 Clean up group: ${descriptor.group}`}
      wizard=${`🧹 Tidy party: ${descriptor.group}`}
    />`;
    hint = html`<${Words}
      plain="Removes the selected confirmed-offline members from this group. Their conversations keep running and stay on disk — only the membership is dropped. Owners are excluded unless you opt in below."
      wizard="Dismisses the selected confirmed-slumbering familiars from this party. Their conversation scrolls remain on disk — only the party bond is broken. Party owners are excluded unless you opt in below."
    />`;
    primaryLabel = html`<${Words}
      plain=${count ? `Remove ${count} from ${descriptor.group}` : 'Remove from group'}
      wizard=${count ? `Dismiss ${count} from ${descriptor.group}` : 'Dismiss from party'}
    />`;
  } else {
    title = '🧹 Clean up agents and conversations';
    if (effectiveTier === 'delete') {
      hint = 'Permanently deletes the selected conversations — wipes the history from disk and drops every group / owner / permission row. Works on active agents, retired agents and plain conversations alike. Cannot be undone.';
      primaryLabel = count
        ? `Delete ${count} conversation${count === 1 ? '' : 's'} permanently`
        : 'Delete conversations';
    } else if (effectiveTier === 'retire') {
      hint = 'Retires the selected agents: revokes their group memberships and permission grants so they stop being agents — the conversations stay on disk and can be reinstated later. The non-destructive soft-delete. Running sessions are also soft-stopped unless you untick the option below.';
      primaryLabel = count ? `Retire ${count} agent${count === 1 ? '' : 's'}` : 'Retire agents';
    } else if (effectiveTier === 'reinstate') {
      hint = 'Reinstates the selected retired agents — returns them to the active roster. Their former groups and permissions are not restored; they start fresh.';
      primaryLabel = count ? `Reinstate ${count} agent${count === 1 ? '' : 's'}` : 'Reinstate agents';
    } else {
      hint = 'Removes the selected agents from every group they belong to. They stay agents (and stay on disk) — only the group memberships are dropped.';
      primaryLabel = count
        ? `Remove ${count} agent${count === 1 ? '' : 's'} from all groups`
        : 'Remove from groups';
    }
  }

  if (result) {
    return html`<${TransactionDialogFrame}
      id="cleanup-modal"
      labelledby="cleanup-title"
      title=${title}
      dialogClass="cleanup-modal"
      primaryLabel="Done"
      primaryClass="primary"
      hideCancel=${true}
      submitID="cleanup-submit"
      onClose=${close}
      onSubmit=${close}
      confirmDiscard=${confirmDiscard}
    >
      <p class="cleanup-hint" id="cleanup-hint">Cleanup complete — ${resultBits.join(' · ') || 'nothing to do'}.</p>
      <${CleanupResult} response=${result} />
      ${(result.warnings || []).length ? html`<div class="cleanup-warn" id="cleanup-warn">
        ⚠ ${(result.warnings || []).join('\n⚠ ')}
      </div>` : null}
    </${TransactionDialogFrame}>`;
  }

  const tierOption = (value, label, note) => html`<label key=${value}>
    <input
      type="radio" name="cleanup-tier" value=${value}
      checked=${effectiveTier === value} disabled=${controlsDisabled}
      onChange=${() => setTier(value)}
    /> ${label} <span class="opt-note">— ${note}</span>
  </label>`;
  return html`<${TransactionDialogFrame}
    id="cleanup-modal"
    labelledby="cleanup-title"
    title=${title}
    dialogClass="cleanup-modal"
    busy=${busy}
    error=${error}
    errorID="cleanup-error"
    primaryLabel=${primaryLabel}
    busyLabel=${html`<span class="btn-spinner" aria-hidden="true"></span><${Words}
      plain="Cleaning up…" wizard="Tidying…" />`}
    primaryClass=${effectiveTier === 'delete' ? 'primary danger' : 'primary'}
    submitDisabled=${count === 0}
    cancelID="cleanup-cancel"
    submitID="cleanup-submit"
    onClose=${actions.close}
    onSubmit=${submit}
    confirmDiscard=${confirmDiscard}
  >
    <p class=${`cleanup-hint${effectiveTier === 'delete' ? ' danger' : ''}`} id="cleanup-hint">${hint}</p>
    <div class="cleanup-toolbar" id="cleanup-toolbar">
      <button type="button" id="cleanup-select-all" disabled=${controlsDisabled} onClick=${selectAll}>select all</button>
      <button type="button" id="cleanup-select-none" disabled=${controlsDisabled} onClick=${selectNone}>select none</button>
      <span title="Tick every visible row whose last activity is at least this many hours ago. 0 selects all.">
        inactive ≥ <input
          type="number" id="cleanup-age" min="0" step="1" value=${age}
          disabled=${controlsDisabled}
          onInput=${(event) => { setAge(event.currentTarget.value); applyAge(event.currentTarget.value); }}
        /> h
      </span>
      ${multiCategory ? html`<input
        type="search" id="cleanup-search" placeholder="filter title / id…"
        value=${query} disabled=${controlsDisabled}
        onInput=${(event) => setQuery(event.currentTarget.value)}
      />` : null}
      <span class="spacer"></span>
      <span class="cleanup-count" id="cleanup-count">${count} selected</span>
    </div>
    ${multiCategory ? html`<div class="cleanup-cats" id="cleanup-cats">
      <span class="cleanup-cats-label">categories:</span>
      ${CLEANUP_CATEGORY_ORDER.map((category) => html`<label class="cleanup-cat-toggle" key=${category}>
        <input
          type="checkbox" data-cat=${category} checked=${categoryOn[category]}
          disabled=${controlsDisabled}
          onChange=${(event) => setCategoryOn({
            ...categoryOn, [category]: event.currentTarget.checked,
          })}
        />
        ${CLEANUP_CATEGORY_LABEL[category]}
        <span class="muted"> (${candidates.filter((candidate) => candidate.category === category).length})</span>
      </label>`)}
    </div>` : null}
    <div class="cleanup-list" id="cleanup-list">
      ${visibleCandidates.length ? CLEANUP_CATEGORY_ORDER.map((category) => {
        const rows = visibleCandidates.filter((candidate) => candidate.category === category);
        if (!rows.length || (!multiCategory && category !== 'agent')) return null;
        return html`<div class="cleanup-category" key=${category}>
          ${multiCategory ? html`<div class="cleanup-cat-head">${CLEANUP_CATEGORY_LABEL[category]} <span>(${rows.length})</span></div>` : null}
          ${rows.map((candidate) => {
            const enabled = rowEnabled(candidate);
            return html`<div class=${`cleanup-row${enabled ? '' : ' disabled'}`} key=${candidate.conv_id}>
              <label><input
                type="checkbox" data-conv=${candidate.conv_id}
                checked=${enabled && selected.has(candidate.conv_id)}
                disabled=${controlsDisabled || !enabled}
                onChange=${(event) => updateCandidate(candidate, event.currentTarget.checked)}
              />
                <span class="title">${candidate.title || candidate.conv_id.slice(0, 8)}</span>
                <span class="id">${candidate.conv_id.slice(0, 8)}</span>
                ${candidate.owner ? html`<span class="cleanup-badge owner">owner</span>` : null}
                ${candidate.online ? html`<span class="cleanup-badge online">online</span>` : null}
                <span class="meta">${candidate.groups.length ? `in: ${candidate.groups.join(', ')}` : ''}</span>
                <span class="seen">${cleanupActivityLabel(candidate)}</span>
              </label>
            </div>`;
          })}
        </div>`;
      }) : html`<div class="cleanup-empty">${candidates.length
        ? 'No conversations match the current filters.' : 'Nothing to clean up.'}</div>`}
    </div>
    <div class="cleanup-options" id="cleanup-options">
      ${groupMode ? html`<label><input
        type="checkbox" id="cleanup-opt-owners" checked=${includeOwners}
        disabled=${controlsDisabled}
        onChange=${(event) => changeOwners(event.currentTarget.checked)}
      /> <${Words} plain="Include offline owners" wizard="Include slumbering party owners" />
        <span class="opt-note"><${Words}
          plain="— also strips their owner status"
          wizard="— also revokes their party-owner mark"
        /></span>
      </label>` : html`
        <div class="cleanup-tier">
          ${tierOption('unjoin', 'Unjoin from groups', 'stays an agent — only its group memberships are dropped')}
          ${tierOption('retire', 'Retire (soft-delete)', 'demote to a plain conversation: revokes groups + permissions, keeps the .jsonl, reinstatable')}
          ${tierOption('delete', 'Delete permanently', 'wipes the conversation from disk and every agent row — cannot be undone')}
          ${tierOption('reinstate', 'Reinstate', 'return a retired agent to the active roster — groups and permissions are not restored')}
        </div>
        <label id="cleanup-opt-owners-row" class=${effectiveTier === 'unjoin' ? '' : 'disabled'}><input
          type="checkbox" id="cleanup-opt-owners" checked=${includeOwners}
          disabled=${controlsDisabled || effectiveTier !== 'unjoin'}
          onChange=${(event) => changeOwners(event.currentTarget.checked)}
        /> Include offline owners <span class="opt-note">— unjoin tier only; retire and delete drop owner rows anyway</span></label>
        <label id="cleanup-opt-online-row" class=${effectiveTier === 'reinstate' ? 'disabled' : ''}><input
          type="checkbox" id="cleanup-opt-online" checked=${includeOnline}
          disabled=${controlsDisabled || effectiveTier === 'reinstate'}
          onChange=${(event) => setIncludeOnline(event.currentTarget.checked)}
        /> Include online sessions <span class="opt-note">— also act on conversations whose tmux session is still running. Delete force-stops them first; retire / unjoin leave the process running. Reinstate ignores liveness either way.</span></label>
        <label id="cleanup-opt-shutdown-row" class=${effectiveTier === 'retire' ? '' : 'disabled'}><input
          type="checkbox" id="cleanup-opt-shutdown" checked=${shutdown}
          disabled=${controlsDisabled || effectiveTier !== 'retire'}
          onChange=${(event) => setShutdown(event.currentTarget.checked)}
        /> Also shut down running sessions <span class="opt-note">— retire tier only; soft-exits (/exit) the tmux pane of each retired agent that is still running. The conversation is kept and reinstatable either way.</span></label>
        <label id="cleanup-opt-wt-row" class=${effectiveTier === 'delete' ? '' : 'disabled'}><input
          type="checkbox" id="cleanup-opt-wt" checked=${deleteWorktrees}
          disabled=${controlsDisabled || effectiveTier !== 'delete'}
          onChange=${(event) => setDeleteWorktrees(event.currentTarget.checked)}
        /> Delete associated git worktrees <span class="opt-note">— removes the worktree directory; the branch and its commits are kept. The main repo and worktrees shared with another agent are always skipped.</span></label>
        <div class="cleanup-related"><button
          type="button" id="cleanup-worktrees-all" disabled=${controlsDisabled}
          onClick=${() => actions.handoffCleanupWorktrees({ group: '' })}
        ><${Words}
          plain="🧹 Review worktrees across all groups…"
          wizard="🍂 Prune stray branches across all parties…"
        /></button><span class="opt-note"><${Words}
          plain="— opens a safe preview of stale worktrees in every group repo"
          wizard="— opens a safe preview of withered branches in every party grove"
        /></span></div>
      `}
    </div>
  </${TransactionDialogFrame}>`;
}

function DeleteGroupDialog({ descriptor, actions, confirmDiscard }) {
  const members = descriptor.members || [];
  const [retireEnabled, setRetireEnabled] = useState(true);
  const [selected, setSelected] = useState(
    () => Object.fromEntries(members.map((member) => [member.selector, member.defaultRetire])),
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);
  const [submittedRequest, setSubmittedRequest] = useState(null);
  const activeRef = useRef(true);
  useEffect(() => () => { activeRef.current = false; }, []);

  const retireTargets = retireEnabled
    ? members.filter((member) => selected[member.selector] === true) : [];
  const detachTargets = members.filter((member) => !retireTargets.includes(member));
  const locked = !!submittedRequest;
  const updateMember = (member, checked) => {
    if (busy || locked || !retireEnabled) return;
    setSelected((current) => ({ ...current, [member.selector]: checked }));
  };
  const changeRetire = (event) => {
    if (!busy && !locked) setRetireEnabled(event.currentTarget.checked);
  };
  const formatError = (cause) => {
    if (cause?.memberErrors) {
      const count = cause.memberErrors;
      return {
        plain: `retire failed for ${count} ${count === 1 ? 'agent' : 'agents'}; group was not deleted`,
        wizard: `banish failed for ${count} ${count === 1 ? 'familiar' : 'familiars'}; party was not disbanded`,
      };
    }
    const message = cause?.message || String(cause);
    if (!cause?.network) return { plain: message, wizard: message };
    return cause.phase === 'retire'
      ? { plain: `retire failed: ${message}`, wizard: `banish failed: ${message}` }
      : { plain: `delete failed: ${message}`, wizard: `disband failed: ${message}` };
  };
  const submit = async () => {
    if (busy) return;
    const request = submittedRequest || Object.freeze({
      group: descriptor.group,
      memberCount: members.length,
      retireMembers: Object.freeze(retireTargets.map((member) => Object.freeze({
        selector: member.selector,
        agent_id: member.agent_id,
        conv_id: member.conv_id,
      }))),
    });
    if (!submittedRequest) setSubmittedRequest(request);
    setError(null);
    setBusy(true);
    try {
      const result = await actions.deleteGroupPlan(request);
      const plainBits = [`deleted group "${descriptor.group}"`];
      const wizardBits = [`disbanded party "${descriptor.group}"`];
      if (result.retired) {
        plainBits.push(`retired ${result.retired}`);
        wizardBits.push(`banished ${result.retired}`);
      }
      if (result.detached) {
        plainBits.push(`detached ${result.detached}`);
        wizardBits.push(`detached ${result.detached}`);
      }
      await actions.finishDeleteGroup(result, {
        plain: plainBits.join(' · '),
        wizard: wizardBits.join(' · '),
      });
    } catch (cause) {
      if (activeRef.current) setError(formatError(cause));
    } finally {
      if (activeRef.current) setBusy(false);
    }
  };

  const plainBits = [];
  const wizardBits = [];
  if (retireTargets.length) {
    plainBits.push(`${retireTargets.length} retired`);
    wizardBits.push(`${retireTargets.length} banished`);
  }
  if (detachTargets.length) {
    plainBits.push(`${detachTargets.length} detached`);
    wizardBits.push(`${detachTargets.length} detached`);
  }
  const plainHint = `Deleting "${descriptor.group}" drops the group, owner rows, memberships, and group message history. Conversations are kept. ${plainBits.length ? `Preview: ${plainBits.join(', ')}.` : 'The group has no agents.'}`;
  const wizardHint = `Disbanding "${descriptor.group}" erases the party, owner marks, memberships, and party message history. Conversation scrolls are kept. ${wizardBits.length ? `Preview: ${wizardBits.join(', ')}.` : 'The party has no familiars.'}`;

  return html`<${TransactionDialogFrame}
    id="delete-group-modal"
    labelledby="delete-group-title"
    title=${html`<${Words} plain="Delete group" wizard="Disband this party?" />`}
    dialogClass="cleanup-modal"
    busy=${busy}
    error=${error ? html`<${Words} plain=${error.plain} wizard=${error.wizard} />` : ''}
    errorID="delete-group-error"
    primaryLabel=${html`<${Words} plain="Delete group" wizard="Disband this party?" />`}
    busyLabel=${html`<span class="btn-spinner" aria-hidden="true"></span><${Words}
      plain="Deleting…" wizard="Disbanding…"
    />`}
    primaryClass="primary danger"
    cancelID="delete-group-cancel"
    submitID="delete-group-submit"
    onClose=${actions.close}
    onSubmit=${submit}
    confirmDiscard=${confirmDiscard}
  >
    <p class="cleanup-hint danger" id="delete-group-hint"><${Words}
      plain=${plainHint} wizard=${wizardHint}
    /></p>
    <div class="cleanup-toolbar">
      <span class="cleanup-count" id="delete-group-count"><${Words}
        plain=${`${members.length} ${members.length === 1 ? 'agent' : 'agents'}: ${retireTargets.length} to retire, ${detachTargets.length} detach`}
        wizard=${`${members.length} ${members.length === 1 ? 'familiar' : 'familiars'}: ${retireTargets.length} to banish, ${detachTargets.length} detach`}
      /></span>
    </div>
    <div class="cleanup-list" id="delete-group-list">
      ${members.length ? members.map((member) => {
        const willRetire = retireEnabled && selected[member.selector] === true;
        const otherNames = member.otherGroups.map((entry) => entry.name);
        const plainWhy = willRetire
          ? (otherNames.length ? `also in ${otherNames.join(', ')} — explicitly included` : 'only member of this group')
          : otherNames.length
            ? `also in ${otherNames.join(', ')} — not auto-retired`
            : retireEnabled ? 'exempted from retirement' : 'retire option off';
        const wizardWhy = willRetire
          ? (otherNames.length ? `also in ${otherNames.join(', ')} — explicitly included` : 'only member of this party')
          : otherNames.length
            ? `also in ${otherNames.join(', ')} — not auto-banished`
            : retireEnabled ? 'exempted from banishment' : 'banish option off';
        return html`<div class="cleanup-row" key=${member.selector}><label>
          <input
            type="checkbox" data-agent=${member.selector}
            checked=${willRetire} disabled=${busy || locked || !retireEnabled}
            onChange=${(event) => updateMember(member, event.currentTarget.checked)}
          />
          <span class="title">${member.title || '(untitled)'}</span>
          <span class="id">${member.conv_id.slice(0, 8)}</span>
          <span class="cleanup-badge">${member.status}</span>
          ${member.role ? html`<span class="cleanup-badge">${member.role}</span>` : null}
          <span class="cleanup-badge"><${Words}
            plain=${willRetire ? 'retire + stop' : 'detach only'}
            wizard=${willRetire ? 'banish familiar + stop' : 'detach only'}
          /></span>
          <span class="muted"> <${Words} plain=${plainWhy} wizard=${wizardWhy} /></span>
        </label></div>`;
      }) : html`<div class="cleanup-empty"><${Words}
        plain="no agents in this group" wizard="no familiars in this party"
      /></div>`}
    </div>
    <label class="delete-agent-wt" id="delete-group-retire-row">
      <input
        type="checkbox" id="delete-group-retire" checked=${retireEnabled}
        disabled=${busy || locked} onChange=${changeRetire}
      />
      <span>
        <span class="delete-group-copy-regular">Retire checked agents before deleting the group</span>
        <span class="delete-group-copy-wizard">Banish checked familiars before disbanding the party</span>
        <span class="wt-note">
          <span class="delete-group-copy-regular">single-group agents are checked by default; agents also in other groups are unchecked by default and only detached unless you tick them</span>
          <span class="delete-group-copy-wizard">single-party familiars are checked by default; familiars also in other parties are unchecked by default and only detached unless you tick them</span>
        </span>
      </span>
    </label>
  </${TransactionDialogFrame}>`;
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
        plain="These agents are not in any group. Each ticked agent will be demoted to a plain, reinstatable conversation and its grants revoked. Untick any you want to keep; only the ticked agents are retired."
        wizard="These unbound familiars belong to no party. Each ticked familiar will return to a restorable conversation scroll and lose its boons. Untick any you want to keep; only the ticked familiars are banished."
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
          <span class="wt-note">The main worktree is never removed; a worktree shared with a surviving agent is kept. Removal happens only after its agent exits and requires shutdown; deleting a linked worktree also deletes its branch.</span>
        </span>
      </label>
    `}
  </${TransactionDialogFrame}>`;
}

function deleteRetiredAgeDays(candidate) {
  if (!candidate.retired_at) return Infinity;
  const retiredAt = Date.parse(candidate.retired_at);
  if (Number.isNaN(retiredAt)) return Infinity;
  return (Date.now() - retiredAt) / 86400000;
}

function DeleteRetiredResult({ response }) {
  const outcomes = response?.outcomes || [];
  return html`<div class="cleanup-list" id="delete-retired-list">
    ${outcomes.length ? outcomes.map((outcome) => html`
      <div class="cleanup-row" key=${outcome.conv_id}>
        <span class=${`cleanup-badge ${outcome.result || ''}`}>${outcome.result || 'unknown'}</span>
        <span class="title">${outcome.title || String(outcome.conv_id || '').slice(0, 8)}</span>
        <span class="id">${String(outcome.conv_id || '').slice(0, 8)}</span>
        <span class="meta">${outcome.detail || ''}</span>
      </div>
    `) : html`<div class="cleanup-empty">Nothing to do.</div>`}
  </div>`;
}

function deleteRetiredSummary(response) {
  const parts = [];
  if (response?.deleted) parts.push(`${response.deleted} deleted`);
  if (response?.skipped) parts.push(`${response.skipped} skipped`);
  if (response?.failed) parts.push(`${response.failed} failed`);
  return parts.join(' · ') || 'nothing to do';
}

function DeleteRetiredDialog({ descriptor, actions, confirmDiscard }) {
  const candidates = descriptor.candidates || [];
  const [query, setQuery] = useState('');
  const [minAge, setMinAge] = useState('0');
  const [selected, setSelected] = useState(
    () => new Set(candidates.map((candidate) => candidate.conv_id)),
  );
  const [deleteWorktrees, setDeleteWorktrees] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [failedAttempt, setFailedAttempt] = useState(false);
  const [result, setResult] = useState(null);
  const activeRef = useRef(true);
  useEffect(() => () => { activeRef.current = false; }, []);

  const normalizedQuery = query.trim().toLowerCase();
  const minAgeDays = Math.max(0, Number.parseFloat(minAge) || 0);
  const matchesFilter = (candidate) => {
    if (normalizedQuery
      && !candidate.title.toLowerCase().includes(normalizedQuery)
      && !candidate.conv_id.toLowerCase().includes(normalizedQuery)) return false;
    // Missing and invalid timestamps are deliberately infinitely old. At the
    // show-all value (0), future timestamps also stay visible despite their
    // negative computed age.
    return minAgeDays <= 0 || deleteRetiredAgeDays(candidate) >= minAgeDays;
  };
  const visibleCandidates = candidates.filter(matchesFilter);
  const visibleSelected = visibleCandidates.filter(
    (candidate) => selected.has(candidate.conv_id),
  );
  const dirty = !result && (
    query !== '' || minAge !== '0' || selected.size !== candidates.length
    || deleteWorktrees || failedAttempt
  );

  const updateVisible = (checked) => {
    if (busy || result) return;
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
    if (busy || result) return;
    setSelected((current) => {
      const next = new Set(current);
      if (checked) next.add(candidate.conv_id);
      else next.delete(candidate.conv_id);
      return next;
    });
  };

  const finishResult = async () => {
    if (!result || busy) return;
    setBusy(true);
    try {
      await actions.finishDeleteRetired({ kind: descriptor.kind, response: result });
    } catch (_) {
      // Accepted mutation plus advisory refresh: the action always releases
      // transaction ownership, so no error remains to paint after unmount.
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
    if (visibleSelected.length === 0) return;
    // Freeze this click's visible-and-checked stable selectors. A failed
    // attempt returns to an editable phase, where the human may change filters,
    // selection, or worktree choice before creating a new frozen attempt.
    const request = Object.freeze({
      agents: Object.freeze(visibleSelected.map(
        (candidate) => candidate.agent_id || candidate.conv_id,
      )),
      deleteWorktrees,
    });
    setError('');
    setBusy(true);
    try {
      const response = await actions.deleteRetiredPreview(request);
      if (activeRef.current) setResult(response || {});
    } catch (cause) {
      if (activeRef.current) {
        setError(cause?.message || String(cause));
        setFailedAttempt(true);
      }
    } finally {
      if (activeRef.current) setBusy(false);
    }
  };

  const warning = result && (result.warnings || []).length
    ? `⚠ ${result.warnings.join('  ⚠ ')}` : '';
  const hint = result
    ? `Delete complete — ${deleteRetiredSummary(result)}.`
    : 'Permanently deletes the ticked retired agents — wipes each conversation from disk '
      + 'and drops every agent / group / permission row. Only agents that are both ticked '
      + 'AND visible under the current filters are deleted. This cannot be undone.';
  return html`<${TransactionDialogFrame}
    id="delete-retired-modal"
    labelledby="delete-retired-title"
    title="Delete retired agents"
    dialogClass="cleanup-modal"
    busy=${busy}
    dirty=${dirty}
    error=${result ? warning : error}
    errorID="delete-retired-error"
    primaryLabel=${result ? 'Done' : failedAttempt ? 'Retry delete'
      : visibleSelected.length === 1 ? 'Delete 1 agent' : `Delete ${visibleSelected.length} agents`}
    busyLabel=${html`<span class="btn-spinner" aria-hidden="true"></span>${result ? 'Refreshing…' : failedAttempt ? 'Retrying…' : 'Deleting…'}`}
    primaryClass=${result ? 'primary' : 'primary danger'}
    submitDisabled=${!result && visibleSelected.length === 0}
    hideCancel=${!!result}
    cancelID="delete-retired-cancel"
    submitID="delete-retired-submit"
    onClose=${close}
    onSubmit=${submit}
    confirmDiscard=${confirmDiscard}
  >
    <p class=${`cleanup-hint${result ? '' : ' danger'}`} id="delete-retired-hint">${hint}</p>
    ${result ? html`<${DeleteRetiredResult} response=${result} />` : html`
      <div class="cleanup-toolbar">
        <button type="button" id="delete-retired-select-all" disabled=${busy} onClick=${() => updateVisible(true)}>select all</button>
        <button type="button" id="delete-retired-select-none" disabled=${busy} onClick=${() => updateVisible(false)}>select none</button>
        <span title="Hide retired agents younger than this — only those retired at least this many days ago stay in the list (and so can be deleted). 0 shows them all.">
          retired ≥ <input
            type="number"
            id="delete-retired-age"
            min="0"
            step="1"
            value=${minAge}
            disabled=${busy}
            onInput=${(event) => setMinAge(event.currentTarget.value)}
          /> d
        </span>
        <input
          type="search"
          id="delete-retired-search"
          placeholder="filter title / id…"
          aria-label="Filter retired agents"
          value=${query}
          disabled=${busy}
          onInput=${(event) => setQuery(event.currentTarget.value)}
        />
        <span class="spacer"></span>
        <span class="cleanup-count" id="delete-retired-count">${visibleSelected.length} of ${candidates.length} selected</span>
      </div>
      <div class="cleanup-list" id="delete-retired-list">
        ${visibleCandidates.length ? visibleCandidates.map((candidate) => {
          const age = candidate.retired_at
            ? `retired ${relTime(candidate.retired_at)}` : 'retired (unknown)';
          return html`<div class="cleanup-row" key=${candidate.conv_id}><label>
            <input
              type="checkbox"
              data-conv=${candidate.conv_id}
              checked=${selected.has(candidate.conv_id)}
              disabled=${busy}
              onChange=${(event) => updateCandidate(candidate, event.currentTarget.checked)}
            />
            <span class="title">${candidate.title || '(untitled)'}</span>
            <span class="id">${candidate.conv_id.slice(0, 8)}</span>
            <span class="seen">${age}</span>
            ${candidate.online ? html`<span class="cleanup-badge online">online — will skip</span>` : null}
            ${candidate.retired_by ? html`<span class="cleanup-badge">by ${candidate.retired_by}</span>` : null}
          </label></div>`;
        }) : html`<div class="cleanup-empty">no retired agents match the filter</div>`}
      </div>
      <label class="delete-agent-wt" id="delete-retired-wt-row">
        <input
          type="checkbox"
          id="delete-retired-wt"
          checked=${deleteWorktrees}
          disabled=${busy}
          onChange=${(event) => setDeleteWorktrees(event.currentTarget.checked)}
        />
        <span>Also delete each agent's git worktree + branch
          <span class="wt-note">removes the worktree directory and force-deletes its branch — the main repo and worktrees shared with a surviving agent are always kept</span>
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
    const deleting = !!worktree?.removable && shutdown && deleteWorktree;
    // Freeze the whole probed identity alongside the choice — path AND branch,
    // exactly what this row showed the operator. Retire force-deletes the
    // branch too, and an agent can `git switch` in place without ever leaving
    // the confirmed path, so a path-only precondition would still let a retry
    // destroy a branch nobody reviewed. An empty branch (detached HEAD) is a
    // real frozen value, not an absent one.
    const choice = submittedChoice || Object.freeze({
      shutdown,
      deleteWorktree: deleting,
      ...(deleting ? {
        expectedWorktree: worktree.path,
        expectedBranch: worktree.branch || '',
      } : {}),
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
  if (current.descriptor.kind === 'window-selection') {
    return html`<${WindowSelectionDialog}
      key=${current.key}
      descriptor=${current.descriptor}
      actions=${actions}
      confirmDiscard=${confirmDiscard}
    />`;
  }
  if (current.descriptor.kind === 'delete-group') {
    return html`<${DeleteGroupDialog}
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
  if (current.descriptor.kind === 'delete-retired-preview') {
    return html`<${DeleteRetiredDialog}
      key=${current.key}
      descriptor=${current.descriptor}
      actions=${actions}
      confirmDiscard=${confirmDiscard}
    />`;
  }
  if (current.descriptor.kind === 'cleanup') {
    return html`<${CleanupDialog}
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
