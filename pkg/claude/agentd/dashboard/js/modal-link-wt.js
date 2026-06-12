// modal-link-wt.js — the inter-group link modal and the worktree picker.
//
// The link create/edit modal, and the shared worktree picker (used by
// this modal and the spawn modals). Extracted from dashboard.js in the
// Stage 2 module split.

import { $, esc, shortCwd } from './helpers.js';
// lastSnapshot lives in dashboard.js; refresh() / toast in refresh.js.
// Imported back — benign cycles (see render.js); TDZ-safe.
import { lastSnapshot } from './dashboard.js';
import { refresh, toast, bindBackdropDiscard } from './refresh.js';


// ---- Link modal (create + edit) ----------------------------------------
//
// One modal, two states:
//   openLinkModal({ mode: 'create', preset: {from?, to?, linkMode?} })
//   openLinkModal({ mode: 'edit', linkID, preset: {from, to, linkMode} })
//
// Create posts to /api/groups/{from}/links with body {to, mode, bidir?};
// edit patches /api/groups/{from-or-to}/links/{linkID} with {mode}.
// Edit hides from/to fields (those are immutable on a link) and the
// bidir checkbox (no meaning when editing a single existing row).

let linkModalState = { mode: 'create', linkID: null };

function openLinkModal(opts) {
  opts = opts || {};
  const state = opts.mode === 'edit' ? 'edit' : 'create';
  const preset = opts.preset || {};
  linkModalState = { mode: state, linkID: opts.linkID || null, fromForEdit: preset.from || '' };
  const groups = ((lastSnapshot && lastSnapshot.groups) || []).map(g => g.name);
  const fromSel = $('#link-modal-from');
  const toSel = $('#link-modal-to');
  fromSel.innerHTML = groups.map(n => `<option value="${esc(n)}">${esc(n)}</option>`).join('');
  toSel.innerHTML = groups.map(n => `<option value="${esc(n)}">${esc(n)}</option>`).join('');
  // Defensive: paste in preset values that aren't in the snapshot yet.
  for (const [sel, val] of [[fromSel, preset.from], [toSel, preset.to]]) {
    if (val && ![...sel.options].some(o => o.value === val)) {
      const opt = document.createElement('option');
      opt.value = val;
      opt.textContent = val;
      sel.appendChild(opt);
    }
  }
  if (preset.from) fromSel.value = preset.from;
  else if (groups.length) fromSel.value = groups[0];
  if (preset.to) toSel.value = preset.to;
  else if (groups.length > 1) toSel.value = groups[1];

  $('#link-modal-mode').value = preset.linkMode || 'members->members';
  $('#link-modal-bidir').checked = false;
  $('#link-modal-error').textContent = '';

  const title = $('#link-modal-title');
  const meta = $('#link-modal-meta');
  const submit = $('#link-modal-submit');
  const bidirRow = $('#link-modal-bidir-row');
  if (state === 'edit') {
    title.textContent = 'Edit link mode';
    meta.textContent = `#${opts.linkID} · ${preset.from} → ${preset.to}`;
    meta.style.display = '';
    submit.textContent = 'Save changes';
    // From/To are immutable when editing — show as disabled.
    fromSel.disabled = true;
    toSel.disabled = true;
    bidirRow.style.display = 'none';
  } else {
    title.textContent = 'Add inter-group link';
    meta.style.display = 'none';
    submit.textContent = 'Create link';
    fromSel.disabled = !!preset.from; // fix FROM when invoked from a group card
    toSel.disabled = false;
    bidirRow.style.display = '';
  }

  $('#link-modal').classList.add('show');
  setTimeout(() => {
    if (state === 'edit') $('#link-modal-mode').focus();
    else if (preset.from && !preset.to) toSel.focus();
    else fromSel.focus();
  }, 0);
}

function closeLinkModal() {
  $('#link-modal').classList.remove('show');
}

async function submitLinkModal() {
  const state = linkModalState.mode;
  const errEl = $('#link-modal-error');
  errEl.textContent = '';
  const submit = $('#link-modal-submit');
  const mode = $('#link-modal-mode').value;
  submit.disabled = true;
  try {
    if (state === 'edit') {
      const id = linkModalState.linkID;
      const scope = linkModalState.fromForEdit || $('#link-modal-from').value;
      const r = await fetch(`/api/groups/${encodeURIComponent(scope)}/links/${encodeURIComponent(id)}`, {
        method: 'PATCH', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ mode }),
      });
      if (!r.ok) {
        errEl.textContent = (await r.text()) || `HTTP ${r.status}`;
        return;
      }
      closeLinkModal();
      toast(`link #${id} mode → ${mode}`);
    } else {
      const from = $('#link-modal-from').value;
      const to = $('#link-modal-to').value;
      if (!from || !to) {
        errEl.textContent = 'from and to are required';
        return;
      }
      if (from === to) {
        errEl.textContent = 'from and to must differ — use group membership for intra-group comm';
        return;
      }
      const bidir = $('#link-modal-bidir').checked;
      const r = await fetch(`/api/groups/${encodeURIComponent(from)}/links`, {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ to, mode, bidir }),
      });
      if (!r.ok) {
        errEl.textContent = (await r.text()) || `HTTP ${r.status}`;
        return;
      }
      closeLinkModal();
      toast(`linked: ${from} → ${to}${bidir ? ' (+reverse)' : ''}`);
    }
    refresh();
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    submit.disabled = false;
  }
}

function bindLinkModal() {
  $('#link-new-open').addEventListener('click', () => openLinkModal({ mode: 'create' }));
  $('#link-modal-cancel').addEventListener('click', closeLinkModal);
  $('#link-modal-submit').addEventListener('click', submitLinkModal);
  bindBackdropDiscard('link-modal', closeLinkModal);
}

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
    opts.push(`<option value="wt:${esc(wt.path)}" data-branch="${esc(wt.branch || '')}">${esc(br)}${tag} — ${esc(shortCwd(wt.path))}</option>`);
  });
  opts.push(`<option value="${WT_NEW}">+ create new worktree…</option>`);
  select.innerHTML = opts.join('');
  select.disabled = false;
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
  openLinkModal, bindLinkModal,
  WT_NEW, wtToggleNew, wtLoad, bindWtPicker, wtResolve, wtResolveCwd,
};
