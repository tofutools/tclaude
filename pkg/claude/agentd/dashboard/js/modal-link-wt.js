// modal-link-wt.js — shared worktree-picker utilities used by spawn + clone.
//
// The former inter-group Links controllers moved to the exclusive Preact
// feature. Keep this historical filename until the remaining worktree-picker
// importers move at a clean contained rewrite point.

import { $, esc, shortCwd, syncSelectTitle } from './helpers.js';

// ---- Worktree picker (shared by spawn + clone modals) -------------------
//
// A worktree picker is a <select> plus two reveal-on-demand rows,
// identified by an element-id prefix (`agent-spawn` or `clone-agent`):
//
//   #<prefix>-worktree      <select> — none / existing worktrees / "+ create"
//   #<prefix>-wt-new-row    new-branch row, shown only for "+ create"
//   #<prefix>-wt-base-row   base-branch row, same visibility
//   #<prefix>-wt-branch     new branch name (text input)
//   #<prefix>-wt-base       base branch (<select>)
//
// wtRefresh repopulates the <select> from GET /api/worktrees; on
// submit, wtResolveCwd turns the picker state into the directory the
// spawn/clone should run in — POSTing /api/worktrees first when the
// human chose "+ create". The whole feature is opt-in: the default
// "" option leaves spawn/clone behaviour exactly as before.

const WT_NEW = '__new__';

// wtToggleNew shows/hides the new-branch + base-branch rows. In a repo
// with no commits yet (dataset.hasCommits === '0') the base-branch
// dropdown is meaningless — "+ create" cuts an orphan branch — so it's
// swapped for an explanatory hint.
function wtToggleNew(prefix, show) {
  const empty = $(`#${prefix}-worktree`).dataset.hasCommits === '0';
  $(`#${prefix}-wt-new-row`).style.display = show ? '' : 'none';
  $(`#${prefix}-wt-base-row`).style.display = (show && !empty) ? '' : 'none';
  const hint = document.getElementById(`${prefix}-wt-orphan-hint`);
  if (hint) hint.style.display = (show && empty) ? '' : 'none';
}

// wtRefresh repopulates the worktree <select> for `repo`. A missing
// or non-repo `repo` disables the picker (spawn/clone then just uses
// the directory as-is). noneLabel is the text of the leading "no
// worktree" option — phrased per modal.
async function wtRefresh(prefix, repo, noneLabel) {
  const select = $(`#${prefix}-worktree`);
  wtToggleNew(prefix, false);
  repo = (repo || '').trim();
  select.dataset.repoRoot = '';
  select.dataset.hasCommits = '1'; // assume commits until told otherwise
  if (!repo) {
    select.innerHTML = '<option value="">(enter a CWD to enable worktrees)</option>';
    select.disabled = true;
    return;
  }
  select.disabled = true;
  select.innerHTML = '<option value="">loading…</option>';
  let data = {};
  try {
    const r = await fetch(`/api/worktrees?repo=${encodeURIComponent(repo)}`, { credentials: 'same-origin' });
    data = await r.json();
  } catch (_) { data = {}; }
  // Stale-guard: if the picker was reloaded for a different repo
  // while this fetch was in flight, drop the result.
  if ((select.dataset.pendingRepo || '') !== repo) return;
  if (!data || !data.is_repo) {
    // Not a git repo. If the daemon found nested repos under it
    // (a "virtual monorepo" launch dir), point the human at the
    // Worktree-repo field — wtFillSubRepos populates its datalist.
    const hasSubs = data && Array.isArray(data.sub_repos) && data.sub_repos.length;
    select.innerHTML = hasSubs
      ? '<option value="">(not a git repo — pick a sub-repo in "Worktree repo" above)</option>'
      : '<option value="">(not a git repo — worktrees unavailable)</option>';
    select.disabled = true;
    wtFillSubRepos(prefix, data && data.sub_repos);
    return;
  }
  select.dataset.repoRoot = data.repo_root || '';
  // A freshly-init'd repo (unborn HEAD) has no commit to base on — the
  // picker hides the base dropdown and "+ create" cuts an orphan branch.
  select.dataset.hasCommits = data.has_commits === false ? '0' : '1';
  const opts = [`<option value="">${esc(noneLabel || '(no worktree)')}</option>`];
  (data.worktrees || []).forEach(wt => {
    const br = wt.branch || '(detached)';
    const tag = wt.is_main ? ' [main]' : '';
    // The visible label shortens the path (shortCwd), but the option's
    // title carries the full branch + untruncated path so hovering the
    // closed <select> reveals the whole thing — see syncSelectTitle.
    const full = `${br}${tag} — ${wt.path}`;
    opts.push(`<option value="wt:${esc(wt.path)}" data-branch="${esc(wt.branch || '')}" title="${esc(full)}">${esc(br)}${tag} — ${esc(shortCwd(wt.path))}</option>`);
  });
  opts.push(`<option value="${WT_NEW}">+ create new worktree…</option>`);
  select.innerHTML = opts.join('');
  select.disabled = false;
  // Repopulation doesn't fire `change`, so refresh the closed-box tooltip
  // to match the now-selected option (the leading "no worktree" on load).
  syncSelectTitle(select);
  const base = $(`#${prefix}-wt-base`);
  base.innerHTML = (data.branches || []).map(b => `<option value="${esc(b)}">${esc(b)}</option>`).join('');
  if (data.default_branch) base.value = data.default_branch;
}

// wtLoad sets the pending-repo guard then refreshes. Call this rather
// than wtRefresh directly so overlapping loads resolve correctly.
function wtLoad(prefix, repo, noneLabel) {
  $(`#${prefix}-worktree`).dataset.pendingRepo = (repo || '').trim();
  return wtRefresh(prefix, repo, noneLabel);
}

// bindWtPicker wires the <select>'s change event once at startup.
function bindWtPicker(prefix) {
  $(`#${prefix}-worktree`).addEventListener('change', (e) => {
    wtToggleNew(prefix, e.target.value === WT_NEW);
  });
}

// wtFillSubRepos populates a modal's sub-repo <datalist> from a
// /api/worktrees response's `sub_repos`. Only the spawn modal has
// the datalist (`<prefix>-subrepo-list`); for the clone picker this
// is a no-op. A null/empty list is left alone rather than cleared —
// once the human drills from the monorepo into one sub-repo the
// suggestions stay available so they can switch to a sibling.
function wtFillSubRepos(prefix, subRepos) {
  const list = document.getElementById(`${prefix}-subrepo-list`);
  if (!list) return;
  if (Array.isArray(subRepos) && subRepos.length) {
    list.innerHTML = subRepos
      .map(s => `<option value="${esc(s.path)}">${esc(s.rel)}</option>`)
      .join('');
  }
}

// wtResolve turns the current worktree-picker state into a
// {path, branch} selection. An empty path means "no worktree
// chosen" — the caller applies its own fallback. `repo` is the
// directory the picker loaded against. Throws on a creation
// failure so the caller can surface it.
async function wtResolve(prefix, repo) {
  const select = $(`#${prefix}-worktree`);
  const val = select.value || '';
  if (!val) return { path: '', branch: '' };
  if (val.startsWith('wt:')) {
    const opt = select.selectedOptions[0];
    return { path: val.slice(3), branch: (opt && opt.dataset.branch) || '' };
  }
  if (val === WT_NEW) {
    const branch = $(`#${prefix}-wt-branch`).value.trim();
    if (!branch) throw new Error('enter a branch name for the new worktree');
    const root = select.dataset.repoRoot || repo;
    const r = await fetch('/api/worktrees', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ repo: root, branch, from_branch: $(`#${prefix}-wt-base`).value || '' }),
    });
    if (!r.ok) throw new Error((await r.text()) || `HTTP ${r.status}`);
    const j = await r.json();
    return { path: j.path || '', branch: j.branch || branch };
  }
  return { path: '', branch: '' };
}

// wtResolveCwd is the cwd-only view of wtResolve, kept for the clone
// modal: it returns the worktree path or `fallback` when none is
// selected. Throws on a worktree-creation failure.
async function wtResolveCwd(prefix, repo, fallback) {
  const sel = await wtResolve(prefix, repo);
  return sel.path || fallback;
}

export {
  WT_NEW, wtToggleNew, wtLoad, bindWtPicker, wtResolve, wtResolveCwd,
};
