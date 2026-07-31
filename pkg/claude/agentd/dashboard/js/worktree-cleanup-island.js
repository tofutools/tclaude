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
  normalizePrunableRepos,
  normalizeWorktreeCandidates,
  prunableRecordCount,
  prunableRepoMatches,
  reconcilePrunableRepos,
  reconcileWorktreeCandidates,
  removableWorktrees,
  selectedWorktrees,
  selectedPrunableRepos,
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

function PrunableRepoRow({ repo, locked, onToggle }) {
  return html`<div class="cleanup-row cleanup-prune-row" data-prune-root=${repo.repo_root}>
    <label title="Bookkeeping only — stale-record pruning never removes a checkout directory or branch">
      <input type="checkbox" data-prune-root=${repo.repo_root} checked=${repo.checked}
        disabled=${locked}
        onChange=${(event) => onToggle(repo.repo_root, event.currentTarget.checked)} />
      <span class="branch">${repo.count} stale Git record${repo.count === 1 ? '' : 's'}</span>
      <span class="cleanup-badge cat-stale">bookkeeping only</span>
      <span class="path" title=${repo.repo_root}>${repo.repo_root}</span>
    </label>
    <div class="cleanup-prune-note">Safe to prune: removes broken <code>.git/worktrees</code>
      metadata only — no checkout directory or branch.</div>
    <details class="cleanup-prune-reasons">
      <summary>Why ${repo.count === 1 ? 'is this record' : `are these ${repo.count} records`} stale?</summary>
      <ul>${repo.reasons.map((entry) => html`<li key=${entry.reason}>
        <strong>${entry.count}</strong> — ${entry.reason}
      </li>`)}</ul>
      <p>If records remain after prune, the result says so. An active agent sandbox can hold
        bind mounts on these entries, causing Git to fail while still exiting 0.</p>
    </details>
  </div>`;
}

function CleanupOutcomeList({ response }) {
  const outcomes = response?.outcomes || [];
  const pruneOutcomes = response?.prune_outcomes || [];
  return html`<div class="cleanup-list" id="worktree-cleanup-list">
    ${pruneOutcomes.map((outcome) => html`
      <div class="cleanup-row cleanup-prune-row" key=${`prune:${outcome.repo_root}`}
        data-prune-root=${outcome.repo_root}>
        <span class=${`cleanup-badge ${outcome.result || ''}`}>${outcome.result || 'unknown'}</span>
        <span class="branch">${outcome.cleared} pruned / ${outcome.remaining} remain</span>
        <span class="path" title=${outcome.repo_root}>${outcome.repo_root}</span>
        <span class="meta">${outcome.detail || ''}</span>
      </div>
    `)}
    ${outcomes.length ? outcomes.map((outcome) => html`
      <div class="cleanup-row" key=${outcome.path} data-path=${outcome.path}>
        <span class=${`cleanup-badge ${outcome.result || ''}`}>${outcome.result || 'unknown'}</span>
        <span class="branch">${outcome.branch || '(detached)'}</span>
        <span class="path" title=${outcome.path}>${outcome.path}</span>
        <span class="meta">${outcome.detail || ''}</span>
      </div>
    `) : (pruneOutcomes.length ? null : html`<div class="cleanup-empty">no cleanup outcomes returned</div>`)}
  </div>`;
}

export function WorktreeCleanupDialog({ current, state, actions }) {
  const { descriptor } = current;
  const allGroups = !descriptor.group;
  const [candidates, setCandidates] = useState([]);
  const [prunableRepos, setPrunableRepos] = useState([]);
  const [pruneScanErrors, setPruneScanErrors] = useState([]);
  const [repoRoots, setRepoRoots] = useState([]);
  const [query, setQuery] = useState('');
  const [deleteBranches, setDeleteBranches] = useState(true);
  const [busyAction, setBusyAction] = useState('scan');
  const [error, setError] = useState('');
  const [submittedRequest, setSubmittedRequest] = useState(null);
  const [result, setResult] = useState(null);
  const [wizard, setWizard] = useState(() => document.body.classList.contains('wizard'));
  const touchedChoices = useRef(new Map());
  const touchedPruneChoices = useRef(new Map());
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
      setPrunableRepos(rescan
        ? reconcilePrunableRepos(response.prunableRepos, touchedPruneChoices.current)
        : normalizePrunableRepos(response.prunableRepos));
      setPruneScanErrors(response.pruneScanErrors || []);
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
  const selectedPruneRepos = selectedPrunableRepos(prunableRepos);
  const removable = removableWorktrees(candidates);
  const visible = visibleWorktrees(candidates, query);
  const visiblePruneRepos = prunableRepos.filter((repo) => prunableRepoMatches(repo, query));
  const staleRecords = prunableRecordCount(prunableRepos);
  const selectedStaleRecords = prunableRecordCount(selectedPruneRepos);
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

  const replacePruneChoices = (rows, checked) => {
    if (busy || locked || !rows.length) return;
    const roots = new Set(rows.map((repo) => repo.repo_root));
    for (const root of roots) touchedPruneChoices.current.set(root, checked);
    setPrunableRepos((currentRepos) => Object.freeze(currentRepos.map((repo) =>
      roots.has(repo.repo_root) ? Object.freeze({ ...repo, checked }) : repo)));
  };

  const togglePrunableRepo = (root, checked) => {
    if (busy || locked) return;
    const repo = prunableRepos.find((entry) => entry.repo_root === root);
    if (!repo) return;
    touchedPruneChoices.current.set(root, checked);
    setPrunableRepos((currentRepos) => Object.freeze(currentRepos.map((entry) =>
      entry.repo_root === root ? Object.freeze({ ...entry, checked }) : entry)));
  };

  const toggleBucket = (rows) => {
    if (!rows.length) return;
    replaceChoices(rows, !rows.every((candidate) => candidate.checked));
  };

  const submit = async () => {
    if (busy || result || (selected.length === 0 && selectedPruneRepos.length === 0) || submitLock.current) return;
    submitLock.current = true;
    const request = submittedRequest || freezeWorktreeCleanupRequest(
      candidates, prunableRepos, deleteBranches,
    );
    if (!request.paths.length && !request.pruneRoots.length) {
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
  const scanFrame = pruneScanErrors.length
    ? `Stale-record scan incomplete for ${pruneScanErrors.length} repo${pruneScanErrors.length === 1 ? '' : 's'}; counts cover successful live scans only.`
    : 'Counts reflect this live scan.';
  const regularHint = removable.length === 0 && staleRecords === 0
    ? `No removable worktrees or stale Git records found in ${regularWhere}. ${scanFrame}`
    : `${removable.length} removable worktree${removable.length === 1 ? '' : 's'} and ${staleRecords} stale Git record${staleRecords === 1 ? '' : 's'} found in ${regularWhere}. ${scanFrame} Safe orphans, retired-agent leftovers, and bookkeeping-only stale records are pre-ticked; agent-bound or dirty worktrees remain unticked for review.`;
  const wizardHint = removable.length === 0 && staleRecords === 0
    ? `No removable worktrees or stale Git records found in ${wizardWhere}. ${scanFrame}`
    : `${removable.length} removable worktree${removable.length === 1 ? '' : 's'} and ${staleRecords} stale Git record${staleRecords === 1 ? '' : 's'} found in ${wizardWhere}. ${scanFrame} Safe orphans, banished-familiar leftovers, and bookkeeping-only stale records are pre-ticked; familiar-bound or dirty worktrees remain unticked for review.`;
  const retrying = !!submittedRequest;
  const selectedCountCopy = `${selected.length} of ${removable.length} worktrees + ${selectedStaleRecords} of ${staleRecords} stale records selected`;
  const removeCopy = `${selected.length} worktree${selected.length === 1 ? '' : 's'}`;
  const pruneCopy = `${selectedStaleRecords} stale record${selectedStaleRecords === 1 ? '' : 's'}`;
  let actionCore = 'Nothing selected';
  if (selected.length && selectedStaleRecords) actionCore = `Remove ${removeCopy} + prune ${pruneCopy}`;
  else if (selected.length) actionCore = `Remove ${removeCopy}`;
  else if (selectedStaleRecords) actionCore = `Prune ${pruneCopy}`;
  const actionCopy = `${retrying ? 'Retry ' : ''}${actionCore}`;

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
          onClick=${() => {
            replaceChoices(removable.filter((candidate) => worktreeMatches(candidate, query)), true);
            replacePruneChoices(visiblePruneRepos, true);
          }}>select all</button>
        <button type="button" id="worktree-cleanup-select-none" disabled=${busy || locked}
          onClick=${() => {
            replaceChoices(removable.filter((candidate) => worktreeMatches(candidate, query)), false);
            replacePruneChoices(visiblePruneRepos, false);
          }}>select none</button>
        <button type="button" id="worktree-cleanup-rescan" disabled=${busy || locked}
          title="Re-scan the repo for worktrees right now (live state can shift as agents come and go)"
          onClick=${() => void load(true)}>${busyAction === 'rescan' ? 'scanning…' : '⟳ rescan'}</button>
        <input type="search" id="worktree-cleanup-search" placeholder="filter path / branch / repo…"
          aria-label="Filter worktrees" value=${query} disabled=${busy || locked}
          onInput=${(event) => setQuery(event.currentTarget.value)} />
        <span class="spacer"></span>
        <span class="cleanup-count" id="worktree-cleanup-count">
          ${selectedCountCopy}
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
        ${staleRecords ? html`<button type="button" data-cat="stale"
          class=${selectedStaleRecords === staleRecords ? 'active' : ''} disabled=${busy || locked}
          title="Toggle bookkeeping-only stale Git records"
          onClick=${() => replacePruneChoices(prunableRepos, selectedStaleRecords !== staleRecords)}>
          stale records ${selectedStaleRecords}/${staleRecords}</button>` : null}
      </div>
      <div class="cleanup-list" id="worktree-cleanup-list">
        ${busyAction === 'scan' && candidates.length === 0 && prunableRepos.length === 0
          ? html`<div class="cleanup-empty">scanning…</div>`
          : visible.length || visiblePruneRepos.length
            ? html`${visiblePruneRepos.map((repo) => html`<${PrunableRepoRow}
              key=${repo.repo_root} repo=${repo} locked=${busy || locked}
              onToggle=${togglePrunableRepo}
            />`)}${visible.map((candidate) => html`<${CleanupCandidateRow}
              key=${candidate.path} candidate=${candidate} locked=${busy || locked}
              wizard=${wizard} onToggle=${toggleCandidate}
            />`)}`
            : html`<div class="cleanup-empty">
              ${error && candidates.length === 0 ? 'scan failed' : 'no cleanup rows match the filter'}
            </div>`}
      </div>
      <label class="delete-agent-wt" id="worktree-cleanup-branches-row">
        <input type="checkbox" id="worktree-cleanup-branches" checked=${deleteBranches}
          disabled=${busy || locked}
          onChange=${(event) => setDeleteBranches(event.currentTarget.checked)} />
        <span>Also delete the feature branch
          <span class="wt-note">force-deletes each removed worktree's local branch — <code>main</code>/<code>master</code> are always kept; stale-record pruning never deletes branches</span>
        </span>
      </label>
      <div class="cleanup-error" id="worktree-cleanup-error"
        role=${error || pruneScanErrors.length ? 'alert' : undefined}>${error || (pruneScanErrors.length
          ? `Stale-record scan incomplete: ${pruneScanErrors.map((entry) => `${entry.repo_root}: ${entry.detail}`).join('; ')}`
          : '')}</div>
      <div class="modal-buttons">
        <button id="worktree-cleanup-cancel" type="button" disabled=${closeBlocked} onClick=${close}>
          <${Words} plain="Cancel" wizard="Dispel" />
        </button>
        <button id="worktree-cleanup-submit" class="primary danger" type="button"
          disabled=${busy || (selected.length === 0 && selectedPruneRepos.length === 0)} aria-busy=${busyAction === 'submit' ? 'true' : undefined}
          onClick=${() => void submit()}>
          ${busyAction === 'submit'
            ? html`<span class="btn-spinner" aria-hidden="true"></span><${Words}
              plain="Removing…" wizard="Pruning…" />`
            : html`<${Words}
              plain=${actionCopy}
              wizard=${actionCopy}
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
