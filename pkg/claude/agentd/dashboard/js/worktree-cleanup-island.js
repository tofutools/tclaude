import { h, render } from 'preact';
import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { registerWorktreeCleanupController } from './worktree-cleanup-controller.js';
import {
  WORKTREE_CATEGORIES,
  categoryWorktrees,
  dirtyWorktrees,
  freezeWorktreeCleanupRequest,
  normalizeWorktreeCandidates,
  reconcileWorktreeCandidates,
  removableWorktrees,
  selectedWorktrees,
  visibleWorktrees,
  worktreeCleanupSummary,
  worktreeMatches,
} from './worktree-cleanup-model.js';

const html = htm.bind(h);

function Words({ plain, wizard }) {
  return html`<span class="theme-copy-regular">${plain}</span><span class="theme-copy-wizard">${wizard}</span>`;
}

function agentLabel(agents, wizard = false) {
  if (!agents?.length) return '';
  const names = agents.map((agent) => agent.title || agent.conv_id.slice(0, 8));
  return `${wizard ? 'familiar' : 'agent'}: ${names.join(', ')}`;
}

function wizardReason(reason) {
  return String(reason || '')
    .replace('in use by a running agent', 'bound to a channeling familiar')
    .replace('retired agent', 'banished familiar')
    .replace('belongs to agent', 'belongs to familiar')
    .replace('reinstate-resume loses this dir', 'restoration loses this directory')
    .replace('never removed', 'never pruned')
    .replace('safe to remove', 'safe to prune')
    .replace('before deleting', 'before pruning')
    .replace('deleting breaks its resume', 'pruning breaks its return path');
}

function CleanupCandidateRow({ candidate, locked, wizard, onToggle }) {
  const disabled = candidate.is_main;
  const wizardCategory = ({
    retired: 'banished', agent: 'familiar', live: 'channeling',
  })[candidate.category] || candidate.category;
  return html`<div
    class=${`cleanup-row${disabled ? ' disabled' : ''}`}
    title=${wizard ? wizardReason(candidate.reason) : candidate.reason}
    data-path=${candidate.path}
  ><label>
    <input
      type="checkbox"
      data-path=${candidate.path}
      checked=${candidate.checked}
      disabled=${disabled || locked}
      onChange=${(event) => onToggle(candidate.path, event.currentTarget.checked)}
    />
    <span class="branch">${candidate.branch || '(detached)'}</span>
    <span class=${`cleanup-badge cat-${candidate.category}`}>
      <${Words} plain=${candidate.category} wizard=${wizardCategory} />
    </span>
    ${candidate.dirty ? html`<span class="cleanup-badge dirty">uncommitted</span>` : null}
    ${candidate.agents.length ? html`<span class="cleanup-badge">
      <${Words}
        plain=${agentLabel(candidate.agents)}
        wizard=${agentLabel(candidate.agents, true)}
      />
    </span>` : null}
    <span class="path" title=${candidate.path}>${candidate.path}</span>
  </label></div>`;
}

function CleanupOutcomeList({ response }) {
  const outcomes = response?.outcomes || [];
  return html`<div class="cleanup-list" id="worktree-cleanup-list">
    ${outcomes.length ? outcomes.map((outcome) => html`
      <div class="cleanup-row" key=${outcome.path} data-path=${outcome.path}>
        <span class=${`cleanup-badge ${outcome.result || ''}`}>${outcome.result || 'unknown'}</span>
        <span class="branch">${outcome.branch || '(detached)'}</span>
        <span class="path" title=${outcome.path}>${outcome.path}</span>
        <span class="meta">${outcome.detail || ''}</span>
      </div>
    `) : html`<div class="cleanup-empty">no worktree outcomes returned</div>`}
  </div>`;
}

export function WorktreeCleanupDialog({ current, state, actions }) {
  const { descriptor } = current;
  const allGroups = !descriptor.group;
  const [candidates, setCandidates] = useState([]);
  const [repoRoots, setRepoRoots] = useState([]);
  const [query, setQuery] = useState('');
  const [deleteBranches, setDeleteBranches] = useState(true);
  const [busyAction, setBusyAction] = useState('scan');
  const [error, setError] = useState('');
  const [submittedRequest, setSubmittedRequest] = useState(null);
  const [result, setResult] = useState(null);
  const [wizard, setWizard] = useState(() => document.body.classList.contains('wizard'));
  const touchedChoices = useRef(new Map());
  const generation = useRef(0);
  const active = useRef(true);
  const submitLock = useRef(false);
  const doneRef = useRef(null);

  useEffect(() => () => {
    active.current = false;
    generation.current += 1;
  }, []);
  useEffect(() => {
    const updateWizard = (event) => setWizard(
      event?.detail?.active === true || document.body.classList.contains('wizard'),
    );
    document.addEventListener('tclaude:wizard', updateWizard);
    return () => document.removeEventListener('tclaude:wizard', updateWizard);
  }, []);

  const load = useCallback(async (rescan) => {
    const requestGeneration = ++generation.current;
    setError('');
    setBusyAction(rescan ? 'rescan' : 'scan');
    try {
      const response = await actions.scan(descriptor.group);
      if (!active.current || requestGeneration !== generation.current) return;
      setRepoRoots(response.repoRoots);
      setCandidates(rescan
        ? reconcileWorktreeCandidates(response.worktrees, touchedChoices.current)
        : normalizeWorktreeCandidates(response.worktrees));
    } catch (cause) {
      if (active.current && requestGeneration === generation.current) {
        setError(`scan failed: ${cause?.message || String(cause)}`);
      }
    } finally {
      if (active.current && requestGeneration === generation.current) setBusyAction('');
    }
  }, [actions, descriptor.group]);

  useEffect(() => {
    void load(false);
  }, [load]);
  useEffect(() => {
    if (result) doneRef.current?.focus();
  }, [result]);

  const selected = selectedWorktrees(candidates);
  const removable = removableWorktrees(candidates);
  const visible = visibleWorktrees(candidates, query);
  const locked = !!submittedRequest || !!result;
  const busy = !!busyAction;
  const closeBlocked = busyAction === 'submit';

  const replaceChoices = (rows, checked) => {
    if (busy || locked) return;
    const paths = new Set(rows.filter((candidate) => !candidate.is_main).map((candidate) => candidate.path));
    if (!paths.size) return;
    for (const path of paths) touchedChoices.current.set(path, checked);
    setCandidates((currentCandidates) => Object.freeze(currentCandidates.map((candidate) =>
      paths.has(candidate.path) && !candidate.is_main
        ? Object.freeze({ ...candidate, checked }) : candidate)));
  };

  const toggleCandidate = (path, checked) => {
    if (busy || locked) return;
    const candidate = candidates.find((entry) => entry.path === path);
    if (!candidate || candidate.is_main) return;
    touchedChoices.current.set(path, checked);
    setCandidates((currentCandidates) => Object.freeze(currentCandidates.map((entry) =>
      entry.path === path ? Object.freeze({ ...entry, checked }) : entry)));
  };

  const toggleBucket = (rows) => {
    if (!rows.length) return;
    replaceChoices(rows, !rows.every((candidate) => candidate.checked));
  };

  const submit = async () => {
    if (busy || result || selected.length === 0 || submitLock.current) return;
    submitLock.current = true;
    const request = submittedRequest || freezeWorktreeCleanupRequest(candidates, deleteBranches);
    if (!request.paths.length) {
      submitLock.current = false;
      return;
    }
    if (!submittedRequest) setSubmittedRequest(request);
    setError('');
    setBusyAction('submit');
    try {
      const response = await actions.cleanup(request);
      if (active.current) setResult(response);
    } catch (cause) {
      submitLock.current = false;
      if (active.current) setError(cause?.message || String(cause));
    } finally {
      if (active.current) setBusyAction('');
    }
  };

  const close = () => {
    if (!closeBlocked) state.finish(result ? { response: result } : null);
  };

  const regularWhere = repoRoots.length
    ? (allGroups && repoRoots.length > 1 ? `${repoRoots.length} group repos` : repoRoots.join(', '))
    : (allGroups ? 'repos used by any group' : "this group's repo");
  const wizardWhere = repoRoots.length
    ? (allGroups && repoRoots.length > 1 ? `${repoRoots.length} party groves` : repoRoots.join(', '))
    : (allGroups ? 'groves used by any party' : "this party's repo");
  const regularHint = removable.length === 0
    ? `No removable worktrees found in ${regularWhere}.`
    : `${removable.length} removable worktree${removable.length === 1 ? '' : 's'} in ${regularWhere}. Orphans (no agent) and retired-agent leftovers are pre-ticked; worktrees a still-enrolled agent uses (resume-bound) and ones with uncommitted changes are left unticked for you to review. Only ticked worktrees are removed.`;
  const wizardHint = removable.length === 0
    ? `No removable worktrees found in ${wizardWhere}.`
    : `${removable.length} removable worktree${removable.length === 1 ? '' : 's'} in ${wizardWhere}. Orphans (no familiar) and banished-familiar leftovers are pre-ticked; worktrees a still-bound familiar uses and ones with uncommitted changes are left unticked for review. Only ticked worktrees are removed.`;
  const retrying = !!submittedRequest;

  return html`<${Overlay}
    id="worktree-cleanup-modal"
    dialogClass="cleanup-modal"
    labelledby="worktree-cleanup-title"
    onClose=${close}
    blocked=${closeBlocked}
  >
    <h3 id="worktree-cleanup-title"><${Words}
      plain=${allGroups
        ? 'Clean up worktrees across all groups'
        : `Clean up worktrees in group "${descriptor.group}"`}
      wizard=${allGroups
        ? 'Prune stray branches across all parties'
        : `Clean up worktrees in party "${descriptor.group}"`}
    /></h3>
    ${result ? html`
      <p class="cleanup-hint" id="worktree-cleanup-hint">${worktreeCleanupSummary(result)}</p>
      <${CleanupOutcomeList} response=${result} />
      <div class="cleanup-error" id="worktree-cleanup-error"></div>
      <div class="modal-buttons">
        <button ref=${doneRef} id="worktree-cleanup-submit" class="primary" type="button" onClick=${close}>Done</button>
      </div>
    ` : html`
      <p class="cleanup-hint" id="worktree-cleanup-hint"><${Words}
        plain=${regularHint} wizard=${wizardHint}
      /></p>
      <div class="cleanup-toolbar">
        <button type="button" id="worktree-cleanup-select-all" disabled=${busy || locked}
          onClick=${() => replaceChoices(
            removable.filter((candidate) => worktreeMatches(candidate, query)), true,
          )}>select all</button>
        <button type="button" id="worktree-cleanup-select-none" disabled=${busy || locked}
          onClick=${() => replaceChoices(
            removable.filter((candidate) => worktreeMatches(candidate, query)), false,
          )}>select none</button>
        <button type="button" id="worktree-cleanup-rescan" disabled=${busy || locked}
          title="Re-scan the repo for worktrees right now (live state can shift as agents come and go)"
          onClick=${() => void load(true)}>${busyAction === 'rescan' ? 'scanning…' : '⟳ rescan'}</button>
        <input type="search" id="worktree-cleanup-search" placeholder="filter path / branch…"
          aria-label="Filter worktrees" value=${query} disabled=${busy || locked}
          onInput=${(event) => setQuery(event.currentTarget.value)} />
        <span class="spacer"></span>
        <span class="cleanup-count" id="worktree-cleanup-count">
          ${selected.length} of ${removable.length} selected
        </span>
      </div>
      <div class="cleanup-toolbar cleanup-categories" id="worktree-cleanup-categories">
        ${WORKTREE_CATEGORIES.map((definition) => {
          const rows = categoryWorktrees(candidates, definition.key);
          if (!rows.length) return null;
          const on = rows.filter((candidate) => candidate.checked).length;
          return html`<button
            type="button" key=${definition.key} data-cat=${definition.key}
            class=${on === rows.length ? 'active' : ''}
            disabled=${busy || locked}
            title=${`Toggle all ${rows.length} ${definition.label} worktrees`}
            onClick=${() => toggleBucket(rows)}
          ><${Words} plain=${definition.label} wizard=${definition.wizardLabel} /> ${on}/${rows.length}</button>`;
        })}
        ${(() => {
          const rows = dirtyWorktrees(candidates);
          if (!rows.length) return null;
          const on = rows.filter((candidate) => candidate.checked).length;
          return html`<button type="button" data-dirty="1"
            class=${on === rows.length ? 'active' : ''} disabled=${busy || locked}
            title=${`Toggle all ${rows.length} worktrees with uncommitted changes`}
            onClick=${() => toggleBucket(rows)}>uncommitted ${on}/${rows.length}</button>`;
        })()}
      </div>
      <div class="cleanup-list" id="worktree-cleanup-list">
        ${busyAction === 'scan' && candidates.length === 0
          ? html`<div class="cleanup-empty">scanning…</div>`
          : visible.length
            ? visible.map((candidate) => html`<${CleanupCandidateRow}
              key=${candidate.path} candidate=${candidate} locked=${busy || locked}
              wizard=${wizard} onToggle=${toggleCandidate}
            />`)
            : html`<div class="cleanup-empty">
              ${error && candidates.length === 0 ? 'scan failed' : 'no worktrees match the filter'}
            </div>`}
      </div>
      <label class="delete-agent-wt" id="worktree-cleanup-branches-row">
        <input type="checkbox" id="worktree-cleanup-branches" checked=${deleteBranches}
          disabled=${busy || locked}
          onChange=${(event) => setDeleteBranches(event.currentTarget.checked)} />
        <span>Also delete the feature branch
          <span class="wt-note">force-deletes each removed worktree's local branch — <code>main</code>/<code>master</code> are always kept</span>
        </span>
      </label>
      <div class="cleanup-error" id="worktree-cleanup-error" role=${error ? 'alert' : undefined}>${error}</div>
      <div class="modal-buttons">
        <button id="worktree-cleanup-cancel" type="button" disabled=${closeBlocked} onClick=${close}>
          <${Words} plain="Cancel" wizard="Dispel" />
        </button>
        <button id="worktree-cleanup-submit" class="primary danger" type="button"
          disabled=${busy || selected.length === 0} aria-busy=${busyAction === 'submit' ? 'true' : undefined}
          onClick=${() => void submit()}>
          ${busyAction === 'submit'
            ? html`<span class="btn-spinner" aria-hidden="true"></span><${Words}
              plain=${retrying ? 'Retrying…' : 'Removing…'} wizard=${retrying ? 'Retrying…' : 'Pruning…'} />`
            : html`<${Words}
              plain=${`${retrying ? 'Retry ' : ''}Remove ${selected.length} worktree${selected.length === 1 ? '' : 's'}`}
              wizard=${`${retrying ? 'Retry ' : ''}Prune ${selected.length} worktree${selected.length === 1 ? '' : 's'}`}
            />`}
        </button>
      </div>
    `}
  </${Overlay}>`;
}

export function WorktreeCleanupApp({ state, actions }) {
  const current = state.dialog.value;
  if (!current) return null;
  return html`<${WorktreeCleanupDialog}
    key=${current.key} current=${current} state=${state} actions=${actions}
  />`;
}

export function mountWorktreeCleanupIsland({ host, state, actions, registerCleanup }) {
  render(html`<${WorktreeCleanupApp} state=${state} actions=${actions} />`, host);
  const unregister = registerWorktreeCleanupController(state);
  registerCleanup(() => {
    unregister();
    state.dispose();
    render(null, host);
  });
}
