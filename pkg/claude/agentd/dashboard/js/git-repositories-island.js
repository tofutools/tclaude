import { h, render } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { registerGitRepositoriesController } from './git-repositories-controller.js';
import { gitRepoRequests, matchingGitRepos } from './git-repositories-actions.js';

const html = htm.bind(h);
function Words({ plain, wizard = plain }) {
  return html`<span class="theme-copy-regular">${plain}</span><span class="theme-copy-wizard">${wizard}</span>`;
}

export function GitRepositoriesDialog({ current, state, actions }) {
  const [repos, setRepos] = useState([]);
  const [issues, setIssues] = useState([]);
  const [selected, setSelected] = useState(new Set());
  const [switchDefault, setSwitchDefault] = useState(true);
  const [discard, setDiscard] = useState(false);
  const [query, setQuery] = useState('');
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(false);
  const [error, setError] = useState('');
  const [outcomes, setOutcomes] = useState({});
  const [generation, setGeneration] = useState(0);
  const running = useRef(false);
  const filterRef = useRef(null);
  const sync = current.mode === 'sync';
  const verb = sync ? 'Sync' : 'Pull';
  const wizardVerb = sync ? 'Harmonize' : 'Summon';
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true); setError('');
    actions.scan(current.group, controller.signal).then((data) => {
      if (controller.signal.aborted) return;
      setRepos(data.repos); setIssues(data.issues);
      setSelected(new Set(data.repos.filter((repo) => !repo.error).map((repo) => repo.path)));
    }).catch((err) => { if (!controller.signal.aborted) setError(err.message); })
      .finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => controller.abort();
  }, [current.group, generation]);

  const visible = matchingGitRepos(repos, query);
  const selectable = visible.filter((repo) => !repo.error);
  const allShown = selectable.length > 0 && selectable.every((repo) => selected.has(repo.path));
  const results = Object.values(outcomes);
  const completed = results.filter((out) => out.status !== 'running' && out.status !== 'queued');
  const locked = loading || busy || done;
  const toggle = (path, value) => setSelected((before) => {
    const next = new Set(before); if (value) next.add(path); else next.delete(path); return next;
  });
  const submit = async () => {
    if (running.current || locked || !selected.size) return;
    running.current = true; setBusy(true); setError('');
    const requests = gitRepoRequests(repos, selected, { ...current, switchDefault, discard });
    setOutcomes(Object.fromEntries(requests.map((r) => [r.path, { path: r.path, status: 'queued', detail: '' }])));
    try {
      await actions.run(requests, (out) => setOutcomes((before) => ({ ...before, [out.path]: out })));
    } catch (err) { setError(err.message); }
    finally { running.current = false; setBusy(false); setDone(true); }
  };
  return html`<${Overlay} id="git-repositories-modal" dialogClass="cron-create-modal git-repositories-modal"
    labelledby="git-repositories-title" onClose=${state.close} blocked=${busy} onSubmitHotkey=${submit} initialFocusRef=${filterRef}>
    <div class="git-repos-heading">
      <h3 id="git-repositories-title"><${Words} plain=${`${verb} repositories`} wizard=${sync ? '✧ Harmonize repositories' : '↓ Summon latest code'} /></h3>
      <p role="status"><${Words} plain=${loading ? 'Discovering repositories…' : `${repos.length} repositories discovered in group home directories.`}
        wizard=${loading ? 'Seeking repositories…' : `${repos.length} repositories discovered in party home directories.`} /></p>
      <p class="git-repos-hint"><${Words} plain="Scope: " />${current.group || html`<${Words} plain="All groups" wizard="All parties" />`} · <${Words} plain="Shared directories appear once." /></p>
    </div>
    <div class="git-repos-options">
      <label><input type="checkbox" checked=${switchDefault} disabled=${locked} onChange=${(e) => setSwitchDefault(e.currentTarget.checked)} />
        <span><${Words} plain="Switch to default branch first" /><small><${Words} plain="Use each repository’s default branch, shown below." /></small></span></label>
      <label><input type="checkbox" checked=${discard} disabled=${locked} onChange=${(e) => setDiscard(e.currentTarget.checked)} />
        <span><${Words} plain="Discard uncommitted changes" /><small class="git-repos-warning"><${Words} plain="Deletes tracked edits and untracked files in selected repositories. Ignored files are kept." /></small></span></label>
    </div>
    <p class="git-repos-hint"><${Words} plain=${sync ? 'Fetch remote updates, prune stale remote references, then pull. Does not push.' : 'Fetch and pull the latest code. Does not push.'} /></p>
    <div class="git-repos-toolbar">
      <input ref=${filterRef} type="search" aria-label="Filter repositories or groups" placeholder="Filter repositories / groups…"
        value=${query} onInput=${(e) => setQuery(e.currentTarget.value)} />
      <button type="button" disabled=${locked || !selectable.length} onClick=${() => {
        setSelected((before) => { const next = new Set(before); selectable.forEach((r) => allShown ? next.delete(r.path) : next.add(r.path)); return next; });
      }}><${Words} plain=${`${allShown ? 'Deselect' : 'Select'} ${query.trim() ? 'shown' : 'all'}`} /></button>
    </div>
    <div class="git-repos-list" tabIndex="0" aria-label="Repositories" aria-busy=${loading}>
      ${visible.map((repo) => {
        const outcome = outcomes[repo.path];
        return html`<div key=${repo.path} class="git-repo-entry">
          <label class="git-repo-row">
            <input type="checkbox" aria-label=${`Select ${repo.name}: ${repo.path}`} checked=${selected.has(repo.path)} disabled=${locked || !!repo.error}
              onChange=${(e) => toggle(repo.path, e.currentTarget.checked)} />
            <span class="git-repo-name"><strong>${repo.name}</strong><small title=${repo.path}>${repo.path}</small></span>
            <span class="git-repo-branch" title=${`${repo.branch || 'Detached HEAD'} → ${switchDefault ? repo.default_branch || '?' : repo.branch || '?'}`}>${repo.branch || '(detached)'} → ${switchDefault ? repo.default_branch || '?' : repo.branch || '?'}</span>
            <span class=${`git-repo-badge ${outcome?.status || (repo.error ? 'failed' : repo.dirty ? 'dirty' : 'clean')}`}>
              ${outcome ? html`<${Words} plain=${outcome.status} wizard=${({ running: 'channeling', queued: 'waiting', updated: 'renewed' })[outcome.status] || outcome.status} />` : repo.error ? 'Unavailable' : repo.dirty ? 'Changed' : 'Clean'}</span>
            <span class="git-repo-groups" title=${repo.groups.join(', ')}>${repo.groups.join(', ')}</span>
          </label>
          ${repo.error || outcome?.detail ? html`<div class="git-repo-detail" role=${outcome?.status === 'failed' ? 'alert' : undefined}>${outcome?.detail || repo.error}</div>` : null}
        </div>`;
      })}
      ${!loading && !visible.length ? html`<p class="git-repos-empty">${repos.length ? 'No matching repositories.' : 'No repositories found in this scope.'}</p>` : null}
    </div>
    ${issues.length ? html`<details class="git-repos-issues"><summary><${Words} plain=${`${issues.length} groups without a usable repository`} wizard=${`${issues.length} parties without a usable repository`} /></summary>
      <ul>${issues.map((issue) => html`<li key=${issue.group}>${issue.group}: ${issue.detail}</li>`)}</ul></details>` : null}
    <p class="git-repos-hint"><${Words} plain="Fast-forward only. Dirty checkouts are skipped unless discard is selected; diverged or blocked branches are reported individually." /></p>
    ${error ? html`<p class="git-repos-error" role="alert">${error}</p>` : null}
    <div class="git-repos-footer">
      <span role="status" aria-live="polite">${busy || done ? `${completed.length} / ${selected.size} completed · ${completed.filter((o) => o.status === 'updated').length} updated · ${completed.filter((o) => o.status === 'skipped').length} skipped · ${completed.filter((o) => o.status === 'failed').length} failed` : `${selected.size} of ${repos.length} selected`}</span>
      <div>${!done ? html`<button type="button" disabled=${loading || busy} onClick=${() => setGeneration((n) => n + 1)}><${Words} plain="Rescan" wizard="Seek again" /></button>` : null}
        <button type="button" disabled=${busy} onClick=${state.close}><${Words} plain=${done ? 'Done' : 'Cancel'} /></button>
        ${!done ? html`<button type="button" class=${`primary${discard ? ' danger' : ''}`} disabled=${locked || !selected.size} onClick=${submit} aria-keyshortcuts="Control+Enter Meta+Enter" title="Submit selected repositories (Ctrl/Cmd+Enter)">
          <${Words} plain=${busy ? `${verb === 'Sync' ? 'Syncing' : 'Pulling'}…` : `${verb} ${selected.size} ${selected.size === 1 ? 'repository' : 'repositories'}`}
            wizard=${busy ? 'Channeling…' : `${wizardVerb} ${selected.size} ${selected.size === 1 ? 'repository' : 'repositories'}`} /></button>` : null}</div>
    </div>
  </${Overlay}>`;
}

function GitRepositoriesApp({ state, actions }) {
  const current = state.dialog.value;
  return current ? html`<${GitRepositoriesDialog} key=${current.key} current=${current} state=${state} actions=${actions} />` : null;
}
export function mountGitRepositoriesIsland({ host, state, actions, registerCleanup }) {
  render(html`<${GitRepositoriesApp} state=${state} actions=${actions} />`, host);
  const unregister = registerGitRepositoriesController(state);
  registerCleanup(() => { unregister(); state.dispose(); render(null, host); });
}
