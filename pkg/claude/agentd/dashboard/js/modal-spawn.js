// modal-spawn.js — the spawn / clone / reincarnate agent modals.
//
// Extracted from dashboard.js in the Stage 2 module split. The spawn and
// clone modals embed the worktree picker from modal-link-wt.

import { $, $$, esc, shortId } from './helpers.js';
import { groupDefaultContext } from './modal-templates.js';
import {
  WT_NEW, wtToggleNew, wtLoad, bindWtPicker, wtResolve, wtResolveCwd,
} from './modal-link-wt.js';
// lastSnapshot lives in dashboard.js; refresh() / toast in refresh.js.
// Imported back — benign cycles (see render.js); TDZ-safe.
import { lastSnapshot } from './dashboard.js';
import { refresh, toast, bindBackdropDiscard } from './refresh.js';
import { slopJackpot } from './slop-fx.js';


// ---- Agent spawn modal --------------------------------------------------
//
// Opens with `{groupName}` pre-filled from a group header's
// "+ spawn agent" button — the group is fixed and the <select> stays
// hidden. (The form still supports an empty open, showing the group
// <select>, for any future caller.) On submit it POSTs to
// /api/groups/{name}/spawn, which forks `tclaude session new` and waits
// for the conv-id before returning.

// Tracks the cwd value the spawn form last auto-filled from a group
// default, so switching the group <select> can refresh the prefill
// without clobbering a path the user typed by hand.
let lastSpawnCwdPrefill = '';

// True once the human has typed in the "Worktree repo" field. Until
// then that field mirrors CWD; after, CWD changes leave it alone so
// a deliberately-pointed sub-repo path isn't clobbered.
let spawnWtRepoEdited = false;

// groupDefaultCwd looks up a group's default spawn dir from the
// latest snapshot. "" when the group is unknown or has no default.
function groupDefaultCwd(groupName) {
  const groups = (lastSnapshot && lastSnapshot.groups) || [];
  const g = groups.find(x => x.name === groupName);
  return (g && g.default_cwd) || '';
}

// spawnAutoFocusPref reads the persisted "auto focus" checkbox state
// for the spawn modal. Defaults to true: a freshly-spawned agent runs
// detached with no window, so the common case is wanting one opened.
function spawnAutoFocusPref() {
  try {
    const v = localStorage.getItem('tclaude.dash.spawn.autofocus');
    return v === null ? true : v === '1';
  } catch (_) { return true; }
}

// prefillSpawnCwd fills #agent-spawn-cwd with the group's default
// dir. With force=false it leaves a user-typed value alone — it
// only overwrites an empty field or a stale prior auto-prefill.
function prefillSpawnCwd(groupName, force) {
  const cwdEl = $('#agent-spawn-cwd');
  if (!force && cwdEl.value.trim() !== '' && cwdEl.value !== lastSpawnCwdPrefill) {
    return;
  }
  const dflt = groupDefaultCwd(groupName);
  cwdEl.value = dflt;
  lastSpawnCwdPrefill = dflt;
}

// updateSpawnGroupContextRow shows the "include group default
// context" checkbox only when the selected group actually has a
// startup context — there's nothing to opt into otherwise. The
// checkbox is (re)set to checked whenever the row becomes visible
// so switching groups always lands on the opt-in default.
function updateSpawnGroupContextRow(groupName) {
  const hasContext = groupDefaultContext(groupName).trim() !== '';
  $('#agent-spawn-group-context-row').style.display = hasContext ? '' : 'none';
  if (hasContext) $('#agent-spawn-group-context').checked = true;
}

// Label for the leading "no worktree" option in the spawn modal's
// worktree picker.
const SPAWN_WT_NONE = '(no worktree — use CWD above)';

// applyWtSync reflects the "Sync worktree branch with name"
// checkbox into the spawn modal's worktree picker. Call it after
// the picker (re)loads, after the name changes, and whenever the
// checkbox itself is toggled.
//
// The sync only works when the picker landed on a usable git repo —
// wtRefresh leaves the <select> disabled in every other state ((no
// CWD), (not a repo), still loading) — so the checkbox is disabled
// to match. When checked with a non-empty name it forces the
// picker into "+ create new worktree" and mirrors the name into
// the new-branch field; clearing the name drops it back to "no
// worktree".
function applyWtSync() {
  const syncEl = $('#agent-spawn-wt-sync');
  const select = $('#agent-spawn-worktree');
  const usable = !select.disabled;
  syncEl.disabled = !usable;
  $('#agent-spawn-wt-sync-row').classList.toggle('disabled', !usable);
  if (!usable || !syncEl.checked) return;
  const name = $('#agent-spawn-name').value.trim();
  if (name) {
    if (select.value !== WT_NEW) select.value = WT_NEW;
    wtToggleNew('agent-spawn', true);
    $('#agent-spawn-wt-branch').value = name;
  } else if (select.value === WT_NEW) {
    // Name cleared while syncing — fall back to "no worktree".
    select.value = '';
    wtToggleNew('agent-spawn', false);
    $('#agent-spawn-wt-branch').value = '';
  }
}

// spawnWtLoad reloads the spawn worktree picker for `cwd`, then
// re-applies the name-sync checkbox once the list settles (the
// checkbox's usable state depends on whether `cwd` is a git repo).
function spawnWtLoad(cwd) {
  return wtLoad('agent-spawn', cwd, SPAWN_WT_NONE).then(applyWtSync);
}

function openAgentSpawnModal(opts) {
  const groupName = (opts && opts.groupName) || '';
  const groupRow = $('#agent-spawn-group-row');
  const select = $('#agent-spawn-group');
  // Populate the <select> from the latest snapshot. The select stays
  // hidden when groupName is fixed; we still set the value so submit
  // can read it from one place.
  const groups = (lastSnapshot && lastSnapshot.groups) || [];
  select.innerHTML = groups.map(g => `<option value="${esc(g.name)}">${esc(g.name)}</option>`).join('');
  if (groupName) {
    // Pre-pinned: append/select the target group even if it isn't in
    // the snapshot yet (paranoid — the user just clicked its header
    // so it must be there, but defend anyway).
    if (![...select.options].some(o => o.value === groupName)) {
      const opt = document.createElement('option');
      opt.value = groupName;
      opt.textContent = groupName;
      select.appendChild(opt);
    }
    select.value = groupName;
    groupRow.style.display = 'none';
  } else {
    groupRow.style.display = '';
    if (!select.value && groups.length) select.value = groups[0].name;
  }
  $('#agent-spawn-name').value = '';
  $('#agent-spawn-role').value = '';
  $('#agent-spawn-descr').value = '';
  $('#agent-spawn-init-msg').value = '';
  $('#agent-spawn-cwd').value = '';
  // Restore the auto-focus checkbox from the human's last choice
  // (defaults on — see spawnAutoFocusPref).
  $('#agent-spawn-focus').checked = spawnAutoFocusPref();
  // Prefill the cwd from the selected group's default spawn dir.
  // force=true: the modal just opened fresh, so there's no
  // user-typed value to protect.
  prefillSpawnCwd(select.value, true);
  // Show the "include group default context" checkbox iff the
  // selected group carries a startup context.
  updateSpawnGroupContextRow(select.value);
  $('#agent-spawn-wt-branch').value = '';
  // The worktree picker targets a separate "Worktree repo" field.
  // It mirrors CWD until the human edits it; for a monorepo CWD the
  // field's datalist offers the nested repos to drill into.
  spawnWtRepoEdited = false;
  $('#agent-spawn-subrepo-list').innerHTML = '';
  $('#agent-spawn-wt-repo').value = $('#agent-spawn-cwd').value;
  // Restore the name→branch sync to its default-on state.
  $('#agent-spawn-wt-sync').checked = true;
  // Load the worktree picker against the Worktree-repo field, then
  // apply the name-sync checkbox once it settles.
  spawnWtLoad($('#agent-spawn-wt-repo').value.trim());
  $('#agent-spawn-error').textContent = '';
  const meta = $('#agent-spawn-meta');
  if (groupName) {
    meta.textContent = `joining group: ${groupName}`;
    meta.style.display = '';
  } else {
    meta.style.display = 'none';
  }
  $('#agent-spawn-modal').classList.add('show');
  setTimeout(() => {
    if (groupName) $('#agent-spawn-name').focus();
    else select.focus();
  }, 0);
}

function closeAgentSpawnModal() {
  $('#agent-spawn-modal').classList.remove('show');
}

async function submitAgentSpawn() {
  const group = $('#agent-spawn-group').value;
  const name = $('#agent-spawn-name').value.trim();
  const role = $('#agent-spawn-role').value.trim();
  const descr = $('#agent-spawn-descr').value.trim();
  // The initial message is delivered to the new agent's inbox (an
  // agent_messages row), not typed into its pane — so newlines are
  // preserved. Send the textarea verbatim; the daemon trims it.
  const initMsg = $('#agent-spawn-init-msg').value;
  const cwd = $('#agent-spawn-cwd').value.trim();
  const wtRepo = $('#agent-spawn-wt-repo').value.trim();
  const autoFocus = $('#agent-spawn-focus').checked;
  const includeGroupContext = $('#agent-spawn-group-context').checked;
  const errEl = $('#agent-spawn-error');
  errEl.textContent = '';
  if (!group) {
    errEl.textContent = 'group is required';
    return;
  }
  // Persist the checkbox so the human's choice sticks across spawns.
  try { localStorage.setItem('tclaude.dash.spawn.autofocus', autoFocus ? '1' : '0'); } catch (_) {}
  const submitBtn = $('#agent-spawn-submit');
  submitBtn.disabled = true;
  submitBtn.textContent = 'Spawning…';
  // Slop-mode lever yank — the body.slop CSS swaps the button text
  // for "🎰 PULL!" via ::before and listens for this class to play
  // the yank-down animation. Self-removes after the animation so a
  // failed submit retry yanks again on the next click. Harmless in
  // non-slop mode (the class has no styling there).
  submitBtn.classList.add('slop-pull-active');
  setTimeout(() => submitBtn.classList.remove('slop-pull-active'), 700);
  try {
    // Resolve the worktree picker (it targets the "Worktree repo"
    // field, which may differ from CWD). Two outcomes:
    //   • Worktree repo == CWD → the worktree becomes the spawn cwd
    //     (the long-standing single-directory behaviour).
    //   • Worktree repo is a sub-repo of a monorepo CWD → the agent
    //     still launches in CWD; the worktree path + branch ride
    //     along so the daemon's welcome points the agent at it.
    const sel = await wtResolve('agent-spawn', wtRepo);
    const body = { name, role, descr, initial_message: initMsg, auto_focus: autoFocus, include_group_context: includeGroupContext };
    if (sel.path && wtRepo && wtRepo !== cwd) {
      body.cwd = cwd;
      body.worktree_path = sel.path;
      body.worktree_branch = sel.branch;
    } else if (sel.path) {
      body.cwd = sel.path;
    } else {
      body.cwd = cwd;
    }
    const r = await fetch(`/api/groups/${encodeURIComponent(group)}/spawn`, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (!r.ok) {
      errEl.textContent = (await r.text()) || `HTTP ${r.status}`;
      return;
    }
    let payload = {};
    try { payload = await r.json(); } catch (_) {}
    closeAgentSpawnModal();
    const label = name || (payload.conv_id ? shortId(payload.conv_id) : 'agent');
    toast(`spawned ${label} → ${group}${autoFocus ? ' — opening terminal' : ''}`);
    // Vegas-themed celebration when slop is on; silent no-op otherwise.
    slopJackpot();
    // Keep the destination group expanded so the new member is visible.
    try { localStorage.setItem('tclaude.dash.group.' + group, '1'); } catch (_) {}
    refresh();
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    submitBtn.disabled = false;
    submitBtn.textContent = 'Spawn';
  }
}

function bindAgentSpawnModal() {
  // The spawn modal is opened per-group from each group's
  // "+ spawn agent" button (data-act="spawn-agent"); it has no
  // global open button. Switching the group <select> re-prefills
  // the cwd from the newly-chosen group's default, mirrors it into
  // Worktree-repo (unless the human pinned that), and reloads the
  // picker.
  $('#agent-spawn-group').addEventListener('change', (e) => {
    prefillSpawnCwd(e.target.value, false);
    updateSpawnGroupContextRow(e.target.value);
    if (!spawnWtRepoEdited) $('#agent-spawn-wt-repo').value = $('#agent-spawn-cwd').value;
    spawnWtLoad($('#agent-spawn-wt-repo').value.trim());
  });
  $('#agent-spawn-cancel').addEventListener('click', closeAgentSpawnModal);
  $('#agent-spawn-submit').addEventListener('click', submitAgentSpawn);
  bindWtPicker('agent-spawn');
  // Name-sync wiring: typing in the name mirrors into the
  // worktree branch; toggling the checkbox re-applies the sync;
  // hand-editing the branch or picking a worktree by hand turns the
  // sync off so it stops fighting the human.
  $('#agent-spawn-name').addEventListener('input', applyWtSync);
  $('#agent-spawn-wt-sync').addEventListener('change', applyWtSync);
  $('#agent-spawn-wt-branch').addEventListener('input', () => {
    $('#agent-spawn-wt-sync').checked = false;
  });
  $('#agent-spawn-worktree').addEventListener('change', (e) => {
    if (e.target.value !== WT_NEW) $('#agent-spawn-wt-sync').checked = false;
  });
  // Re-list worktrees when the CWD field settles (debounced). CWD
  // mirrors into Worktree-repo until the human edits the latter.
  let spawnCwdTimer;
  $('#agent-spawn-cwd').addEventListener('input', () => {
    clearTimeout(spawnCwdTimer);
    spawnCwdTimer = setTimeout(() => {
      if (!spawnWtRepoEdited) $('#agent-spawn-wt-repo').value = $('#agent-spawn-cwd').value;
      spawnWtLoad($('#agent-spawn-wt-repo').value.trim());
    }, 350);
  });
  // Editing "Worktree repo" detaches it from CWD and reloads the
  // picker against the typed/picked repo (e.g. a monorepo sub-repo).
  let spawnWtRepoTimer;
  $('#agent-spawn-wt-repo').addEventListener('input', () => {
    spawnWtRepoEdited = true;
    clearTimeout(spawnWtRepoTimer);
    spawnWtRepoTimer = setTimeout(() => {
      spawnWtLoad($('#agent-spawn-wt-repo').value.trim());
    }, 350);
  });
  bindBackdropDiscard('agent-spawn-modal', closeAgentSpawnModal);
}

// ---- Clone agent modal --------------------------------------------------
//
// Submit POSTs to /api/agents/{conv}/clone with `{follow_up, no_copy_conv}`.
// Follow-up is optional; newlines are stripped client-side because the
// server rejects them (tmux send-keys would split them into multiple
// submits).

function openCloneAgentModal(conv, label, cwd) {
  cwd = cwd || '';
  const meta = $('#clone-agent-meta');
  const src = label || shortId(conv);
  meta.textContent = cwd ? `source: ${src}  ·  ${cwd}` : `source: ${src}`;
  $('#clone-agent-followup').value = '';
  $('#clone-agent-copy-conv').checked = true;
  $('#clone-agent-wt-branch').value = '';
  $('#clone-agent-error').textContent = '';
  $('#clone-agent-modal').dataset.conv = conv;
  $('#clone-agent-modal').dataset.label = label || '';
  $('#clone-agent-modal').dataset.cwd = cwd;
  // The picker lists worktrees of the source agent's repo; "+ create"
  // forks a new one and the clone spawns there.
  wtLoad('clone-agent', cwd, '(no worktree — same directory as source)');
  $('#clone-agent-modal').classList.add('show');
  setTimeout(() => $('#clone-agent-followup').focus(), 0);
}

function closeCloneAgentModal() {
  $('#clone-agent-modal').classList.remove('show');
}

// normaliseFollowUp collapses newlines/tabs/runs-of-whitespace to a
// single space and trims. Server rejects newlines outright; this
// keeps the textarea ergonomic while staying safe.
function normaliseFollowUp(s) {
  return String(s || '').replace(/[\r\n\t]+/g, ' ').replace(/\s+/g, ' ').trim();
}

// Server's spawn poll is 30 s (reincarnateSpawnTimeout in clone.go).
// Give a small grace window before the UI surfaces a timeout so a
// just-barely-late response is treated as success, not error.
const CLONE_FETCH_TIMEOUT_MS = 35_000;

async function submitCloneAgent() {
  const modal = $('#clone-agent-modal');
  const conv = modal.dataset.conv;
  const label = modal.dataset.label || shortId(conv);
  const followUp = normaliseFollowUp($('#clone-agent-followup').value);
  const copyConv = $('#clone-agent-copy-conv').checked;
  const errEl = $('#clone-agent-error');
  errEl.textContent = '';
  const submitBtn = $('#clone-agent-submit');
  submitBtn.disabled = true;
  submitBtn.textContent = 'Cloning…';
  // AbortController gives us a clean "the server is hung" path instead
  // of leaving the modal in 'Cloning…' until the browser's default
  // network timeout (which can be minutes). The window is generous
  // because the server itself polls up to 30 s for the new tmux
  // session to register.
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), CLONE_FETCH_TIMEOUT_MS);
  try {
    // Resolve the worktree picker → optional cwd override. An empty
    // result means "inherit the source's cwd" (historical behaviour).
    const cwd = await wtResolveCwd('clone-agent', modal.dataset.cwd || '', '');
    const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/clone`, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ follow_up: followUp, no_copy_conv: !copyConv, cwd }),
      signal: ctrl.signal,
    });
    if (!r.ok) {
      errEl.textContent = (await r.text()) || `HTTP ${r.status}`;
      return;
    }
    let payload = {};
    try { payload = await r.json(); } catch (_) {}
    closeCloneAgentModal();
    const dst = payload.new_conv ? ' → ' + shortId(payload.new_conv) : '';
    if (payload.warning) {
      // Server returned 200 but flagged a partial-success — keep the
      // user informed instead of silently celebrating; isErr=true
      // styles the toast as a warning.
      toast(`cloned ${label}${dst} (warning: ${payload.warning})`, true);
    } else {
      toast(`cloned ${label}${dst}`);
    }
    refresh();
  } catch (err) {
    if (err && err.name === 'AbortError') {
      errEl.textContent = `clone timed out after ${CLONE_FETCH_TIMEOUT_MS / 1000}s — the new agent may still come online; check ~/.tclaude/output.log and refresh in a moment.`;
    } else {
      errEl.textContent = (err && err.message) || String(err);
    }
  } finally {
    clearTimeout(timer);
    submitBtn.disabled = false;
    submitBtn.textContent = 'Clone';
  }
}

function bindCloneAgentModal() {
  $('#clone-agent-cancel').addEventListener('click', closeCloneAgentModal);
  $('#clone-agent-submit').addEventListener('click', submitCloneAgent);
  bindWtPicker('clone-agent');
  bindBackdropDiscard('clone-agent-modal', closeCloneAgentModal);
}

// ---- Reincarnate agent modal --------------------------------------------
//
// Two modes, chosen by the radiogroup; both POST to
// /api/agents/{conv}/reincarnate:
//   - "self" (the DEFAULT): POST {mode:'self', focus_hint?} — the
//     daemon messages the agent to reincarnate itself. focus_hint is
//     OPTIONAL, so Submit is always enabled.
//   - "force": POST {mode:'force', follow_up} — the immediate
//     daemon-driven reincarnation. follow_up is REQUIRED, so Submit
//     is disabled until the follow-up textarea has content.

function reincarnateMode() {
  const checked = $('input[name=reincarnate-mode]:checked');
  return (checked && checked.value) || 'self';
}

// updateReincarnateMode shows the fields for the selected mode,
// relabels Submit, and recomputes its disabled state. Self-mode's
// Submit is always enabled (the focus hint is optional); force-mode's
// is gated on a non-empty follow-up.
function updateReincarnateMode() {
  const isForce = reincarnateMode() === 'force';
  $('#reincarnate-self-fields').hidden = isForce;
  $('#reincarnate-force-fields').hidden = !isForce;
  const submitBtn = $('#reincarnate-agent-submit');
  submitBtn.textContent = isForce ? 'Force reincarnate' : 'Ask agent';
  submitBtn.disabled = isForce && !normaliseFollowUp($('#reincarnate-agent-followup').value);
}

function openReincarnateAgentModal(conv, label) {
  const meta = $('#reincarnate-agent-meta');
  meta.textContent = label ? `target: ${label}` : `target: ${shortId(conv)}`;
  $('#reincarnate-agent-followup').value = '';
  $('#reincarnate-agent-focus').value = '';
  $('#reincarnate-agent-error').textContent = '';
  // Every open resets to the self-reincarnate default.
  const selfRadio = $('input[name=reincarnate-mode][value=self]');
  if (selfRadio) selfRadio.checked = true;
  updateReincarnateMode();
  $('#reincarnate-agent-modal').dataset.conv = conv;
  $('#reincarnate-agent-modal').dataset.label = label || '';
  $('#reincarnate-agent-modal').classList.add('show');
  setTimeout(() => $('#reincarnate-agent-focus').focus(), 0);
}

function closeReincarnateAgentModal() {
  $('#reincarnate-agent-modal').classList.remove('show');
}

async function submitReincarnateAgent() {
  const modal = $('#reincarnate-agent-modal');
  const conv = modal.dataset.conv;
  const label = modal.dataset.label || shortId(conv);
  const errEl = $('#reincarnate-agent-error');
  errEl.textContent = '';
  const mode = reincarnateMode();
  let body;
  if (mode === 'force') {
    const followUp = normaliseFollowUp($('#reincarnate-agent-followup').value);
    if (!followUp) {
      errEl.textContent = 'follow-up is required for force reincarnate';
      return;
    }
    body = { mode: 'force', follow_up: followUp };
  } else {
    // Focus hint is optional — send it trimmed, or omit when blank.
    const hint = $('#reincarnate-agent-focus').value.trim();
    body = { mode: 'self' };
    if (hint) body.focus_hint = hint;
  }
  const submitBtn = $('#reincarnate-agent-submit');
  submitBtn.disabled = true;
  submitBtn.textContent = mode === 'force' ? 'Reincarnating…' : 'Asking…';
  try {
    const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/reincarnate`, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (!r.ok) {
      errEl.textContent = (await r.text()) || `HTTP ${r.status}`;
      return;
    }
    let payload = {};
    try { payload = await r.json(); } catch (_) {}
    closeReincarnateAgentModal();
    if (mode === 'force') {
      const suffix = payload.new_title ? ' → ' + payload.new_title : (payload.new_conv ? ' → ' + shortId(payload.new_conv) : '');
      toast(`reincarnated ${label}${suffix}`);
    } else {
      toast(`asked ${label} to reincarnate itself`);
    }
    refresh();
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    // Recompute label + disabled state for whatever mode is selected
    // (relevant only on the error path — success closed the modal).
    updateReincarnateMode();
  }
}

function bindReincarnateAgentModal() {
  $('#reincarnate-agent-cancel').addEventListener('click', closeReincarnateAgentModal);
  $('#reincarnate-agent-submit').addEventListener('click', submitReincarnateAgent);
  $('#reincarnate-agent-followup').addEventListener('input', updateReincarnateMode);
  $$('input[name=reincarnate-mode]').forEach(rdo => {
    rdo.addEventListener('change', () => {
      updateReincarnateMode();
      const focusEl = reincarnateMode() === 'force'
        ? $('#reincarnate-agent-followup') : $('#reincarnate-agent-focus');
      setTimeout(() => focusEl.focus(), 0);
    });
  });
  bindBackdropDiscard('reincarnate-agent-modal', closeReincarnateAgentModal);
}

// Renaming an agent is no longer a modal of its own — it folded into
// the per-agent edit panel (editMemberModal, refresh.js) and the
// click-to-edit name cell (the rename-name handler, row-actions.js).
// Both POST /api/agents/{conv}/rename, same as this modal once did.

export {
  openAgentSpawnModal, bindAgentSpawnModal,
  openCloneAgentModal, bindCloneAgentModal,
  openReincarnateAgentModal, bindReincarnateAgentModal,
};
