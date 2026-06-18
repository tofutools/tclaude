// modal-profiles.js — the spawn-profiles management overlay + editor (JOH-210
// inc3). Mirrors modal-templates.js: a manage overlay lists the saved profiles
// as cards (with Edit / Delete / + new), and an editor modal creates/edits one.
//
// A spawn profile is a reusable bundle of the spawn-agent dialog's fields —
// harness / model / effort / sandbox / trust-dir + name / role / descr /
// initial-message and the dialog toggles (NOT cwd / worktree). The data layer
// (profiles.js) owns the fetch + cache; this module is all DOM.
//
// The editor surfaces the same launch fields the spawn dialog does, driven off
// the snapshot's `harnesses` catalog exactly like that dialog. Two launch
// fields the spawn dialog also lacks — approval and auto_review, both Codex
// approval-subsystem features with no entry in the catalog — are NOT edited
// here; they remain reachable via the CLI / `/v1` API. Because the server's
// PATCH is a FULL replace, editing a profile that carries them would otherwise
// wipe them, so submit carries them forward from the original (guarded on an
// unchanged harness, since they're Codex-gated).

import { $, esc, bindSelectTitles } from './helpers.js';
import { lastSnapshot } from './dashboard.js';
import { confirmModal, toast, bindBackdropDiscard, bindManageOverlayDismiss } from './refresh.js';
import {
  loadProfiles, createProfile, updateProfile, deleteProfile, profileSummary,
} from './profiles.js';

// The original profile object while editing an existing one (the PATCH target
// + the source of carried-forward fields the editor doesn't surface); null
// while creating.
let profileEditorEditing = null;

// The last list the manage overlay fetched — the filter box re-paints from
// this without a re-fetch; a create/edit/delete reloads it.
let lastProfiles = [];

// ---- harness catalog (snapshot-driven, like the spawn dialog) -----------

function profileHarnessCatalog() {
  return (lastSnapshot && lastSnapshot.harnesses) || [];
}
function profileHarnessByName(name) {
  return profileHarnessCatalog().find(h => h.name === name) || null;
}

// profileActiveModelEl returns the Model control in play for the selected
// harness — the curated <select> for a harness with a model list, the
// free-text <input> for one without (Codex). Mirrors activeSpawnModelEl.
function profileActiveModelEl() {
  const h = profileHarnessByName($('#profile-editor-harness').value);
  const codexStyle = h && (!h.models || h.models.length === 0);
  return codexStyle ? $('#profile-editor-model-codex') : $('#profile-editor-model');
}

function populateProfileHarnessSelect() {
  const sel = $('#profile-editor-harness');
  const cat = profileHarnessCatalog();
  sel.innerHTML = cat
    .map(h => `<option value="${esc(h.name)}">${esc(h.display_name || h.name)}</option>`)
    .join('');
  if (cat.some(h => h.name === 'claude')) sel.value = 'claude';
  else if (cat.length) sel.value = cat[0].name;
}

// populateProfileEffortSelect rebuilds the Effort options from the harness's
// levels (data-driven off the catalog), preserving the current selection when
// the new list still offers it. Mirrors populateSpawnEffortSelect.
function populateProfileEffortSelect(h) {
  const levels = h && h.effort_levels;
  if (!levels || !levels.length) return; // keep the static fallback options
  const sel = $('#profile-editor-effort');
  const prev = sel.value;
  sel.innerHTML = `<option value="">Default (harness's own)</option>`
    + levels.map(l => `<option value="${esc(l)}">${esc(l)}</option>`).join('');
  if ([...sel.options].some(o => o.value === prev)) sel.value = prev;
}

// applyProfileEditorHarness reshapes the editor's Model / Sandbox / Effort /
// trust-dir rows for the chosen harness — the curated model <select> vs the
// free-text input, the sandbox row (with its modes) for a harness that takes a
// launch sandbox, the Codex-only trust-dir row. Mirrors applySpawnHarness.
function applyProfileEditorHarness(harnessName) {
  const h = profileHarnessByName(harnessName);
  const hasModelList = !h || (h.models && h.models.length > 0);
  $('#profile-editor-model-claude-row').style.display = hasModelList ? '' : 'none';
  $('#profile-editor-model-codex-row').style.display = hasModelList ? 'none' : '';

  const canSandbox = !!(h && h.can_sandbox && h.sandbox_modes && h.sandbox_modes.length);
  $('#profile-editor-sandbox-row').style.display = canSandbox ? '' : 'none';
  if (canSandbox) {
    const sandSel = $('#profile-editor-sandbox');
    sandSel.innerHTML = h.sandbox_modes
      .map(m => `<option value="${esc(m)}">${esc(m)}${m === h.default_sandbox ? ' (recommended)' : ''}</option>`)
      .join('');
    sandSel.value = h.default_sandbox || h.sandbox_modes[0];
  }

  // trust-dir is Codex-only — it edits ~/.codex/config.toml, the same gating
  // the spawn dialog applies.
  const isCodex = !!(h && h.name === 'codex');
  $('#profile-editor-trust-dir-row').style.display = isCodex ? '' : 'none';

  populateProfileEffortSelect(h);
}

// ---- tri-state *bool toggles --------------------------------------------
//
// A profile's *bool fields are tri-state: unset (leave the dialog default),
// true, or false. The editor represents each as a <select> with "" (unset) /
// "1" (yes) / "0" (no), so a sparse profile round-trips faithfully instead of
// a plain checkbox collapsing unset into false.
function setTri(sel, v) { sel.value = (v == null) ? '' : (v ? '1' : '0'); }
function readTri(sel) { const v = sel.value; return v === '' ? null : (v === '1'); }

function setSelectIfPresent(sel, value) {
  if (value && [...sel.options].some(o => o.value === value)) sel.value = value;
}

// ---- Manage-profiles overlay --------------------------------------------

function openProfilesManageModal() {
  $('#profiles-manage-modal').classList.add('show');
  reloadProfilesList();
  setTimeout(() => $('#filter-profiles').focus(), 0);
}

function closeProfilesManageModal() { $('#profiles-manage-modal').classList.remove('show'); }

function filterProfiles(list, q) {
  if (!q) return list;
  const n = q.toLowerCase();
  return list.filter(p =>
    (p.name || '').toLowerCase().includes(n) ||
    (p.role || '').toLowerCase().includes(n) ||
    (p.model || '').toLowerCase().includes(n) ||
    (p.harness || '').toLowerCase().includes(n) ||
    (p.agent_name || '').toLowerCase().includes(n));
}

// reloadProfilesList force-fetches the profiles, then paints. Profiles are not
// part of the 2s snapshot (they live on their own endpoint), so the overlay is
// repainted explicitly here on open and after every mutation, rather than
// riding the snapshot tick the way the templates listing does.
async function reloadProfilesList() {
  const host = $('#profiles-list');
  try {
    lastProfiles = await loadProfiles(true);
  } catch (err) {
    host.innerHTML = `<div class="template-empty">Couldn't load profiles: ${esc((err && err.message) || String(err))}</div>`;
    return;
  }
  if (!$('#profiles-manage-modal').classList.contains('show')) return; // closed mid-fetch
  paintProfilesList();
}

function paintProfilesList() {
  const host = $('#profiles-list');
  const all = lastProfiles;
  const q = $('#filter-profiles').value;
  const list = filterProfiles(all, q);
  const countEl = $('#filter-profiles-count');
  if (countEl) countEl.textContent = q ? `${list.length} / ${all.length}` : `${all.length}`;
  if (!list.length) {
    host.innerHTML = `<div class="template-empty">${all.length
      ? 'No profiles match the filter.'
      : 'No spawn profiles yet — press <b>+ new profile</b> to define one. A profile pre-fills the spawn dialog and can be a group or dashboard default.'}</div>`;
    return;
  }
  host.innerHTML = list.map(profileCardHTML).join('');
}

function profileCardHTML(p) {
  const summary = profileSummary(p);
  return `<div class="template-card profile-card" data-profile="${esc(p.name)}">
    <div class="tc-head">
      <span class="tc-name">${esc(p.name)}</span>
      ${summary ? `<span class="tc-descr">${esc(summary)}</span>` : ''}
      <span class="tc-actions">
        <button class="tool" data-pact="edit" data-profile="${esc(p.name)}">edit</button>
        <button class="tool" data-pact="delete" data-profile="${esc(p.name)}">delete</button>
      </span>
    </div>
  </div>`;
}

function profilesByName() {
  const m = {};
  for (const p of lastProfiles) m[p.name] = p;
  return m;
}

// ---- Profile editor modal -----------------------------------------------

function openProfileEditor(profile) {
  profileEditorEditing = profile || null;
  $('#profile-editor-title').textContent =
    profile ? `Edit profile: ${profile.name}` : 'New spawn profile';
  $('#profile-editor-error').textContent = '';
  $('#profile-editor-name').value = profile ? (profile.name || '') : '';

  // Harness first — populate the selector, set it to the profile's harness (or
  // default), then reshape the launch rows before filling per-field controls.
  populateProfileHarnessSelect();
  const hSel = $('#profile-editor-harness');
  if (profile && profile.harness && [...hSel.options].some(o => o.value === profile.harness)) {
    hSel.value = profile.harness;
  }
  applyProfileEditorHarness(hSel.value);

  // Model into the now-active control; effort + sandbox into their (reshaped)
  // selects.
  $('#profile-editor-model').value = '';
  $('#profile-editor-model-codex').value = '';
  if (profile && profile.model) profileActiveModelEl().value = profile.model;
  setSelectIfPresent($('#profile-editor-effort'), profile ? profile.effort : '');
  setSelectIfPresent($('#profile-editor-sandbox'), profile ? profile.sandbox : '');

  setTri($('#profile-editor-trust-dir'), profile ? profile.trust_dir : null);
  setTri($('#profile-editor-sync-worktree'), profile ? profile.sync_worktree : null);
  setTri($('#profile-editor-auto-focus'), profile ? profile.auto_focus : null);
  setTri($('#profile-editor-group-context'), profile ? profile.include_group_default_context : null);

  $('#profile-editor-agent-name').value = profile ? (profile.agent_name || '') : '';
  $('#profile-editor-role').value = profile ? (profile.role || '') : '';
  $('#profile-editor-descr').value = profile ? (profile.descr || '') : '';
  $('#profile-editor-init-msg').value = profile ? (profile.initial_message || '') : '';

  $('#profile-editor-modal').classList.add('show');
  bindSelectTitles($('#profile-editor-modal'));
  setTimeout(() => $('#profile-editor-name').focus(), 0);
}

function closeProfileEditor() { $('#profile-editor-modal').classList.remove('show'); }

// buildProfilePayload assembles the full desired state from the editor. The
// server's PATCH is a full replace, so every field the profile should keep
// must be present. Launch fields are gated on the chosen harness's
// capabilities so we never post a value it would reject (a sandbox / trust-dir
// on Claude). Approval + auto_review aren't edited here; on an edit that keeps
// the same harness they're carried forward from the original so the
// full-replace doesn't drop a CLI-set value.
function buildProfilePayload(name) {
  const harness = $('#profile-editor-harness').value;
  const hEntry = profileHarnessByName(harness);
  const body = {
    name,
    harness,
    model: profileActiveModelEl().value.trim(),
    effort: $('#profile-editor-effort').value,
    agent_name: $('#profile-editor-agent-name').value.trim(),
    role: $('#profile-editor-role').value.trim(),
    descr: $('#profile-editor-descr').value.trim(),
    initial_message: $('#profile-editor-init-msg').value,
  };
  // Sandbox: only for a harness that takes a launch sandbox (Codex); its
  // select always carries a value (the default is pre-selected).
  if (hEntry && hEntry.can_sandbox && $('#profile-editor-sandbox').value) {
    body.sandbox = $('#profile-editor-sandbox').value;
  }
  // trust-dir: Codex-only (the backend rejects a true on any other harness).
  const trustDir = (harness === 'codex') ? readTri($('#profile-editor-trust-dir')) : null;
  if (trustDir != null) body.trust_dir = trustDir;

  const syncWt = readTri($('#profile-editor-sync-worktree'));
  if (syncWt != null) body.sync_worktree = syncWt;
  const autoFocus = readTri($('#profile-editor-auto-focus'));
  if (autoFocus != null) body.auto_focus = autoFocus;
  const groupCtx = readTri($('#profile-editor-group-context'));
  if (groupCtx != null) body.include_group_default_context = groupCtx;

  // Carry forward the un-surfaced Codex approval fields on a same-harness edit
  // (treat "" and "claude" as the same default harness).
  const norm = (h) => h || 'claude';
  if (profileEditorEditing && norm(profileEditorEditing.harness) === norm(harness)) {
    if (profileEditorEditing.approval) body.approval = profileEditorEditing.approval;
    if (profileEditorEditing.auto_review != null) body.auto_review = profileEditorEditing.auto_review;
  }
  return body;
}

async function submitProfileEditor() {
  const name = $('#profile-editor-name').value.trim();
  const errEl = $('#profile-editor-error');
  errEl.textContent = '';
  if (!name) { errEl.textContent = 'profile name is required'; return; }
  const payload = buildProfilePayload(name);
  const editing = profileEditorEditing ? profileEditorEditing.name : null;
  const btn = $('#profile-editor-submit');
  btn.disabled = true;
  try {
    if (editing) await updateProfile(editing, payload);
    else await createProfile(payload);
    closeProfileEditor();
    toast(editing ? `profile updated: ${name}` : `profile created: ${name}`);
    reloadProfilesList();
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    btn.disabled = false;
  }
}

async function removeProfile(name) {
  const ok = await confirmModal({
    title: 'Delete profile?',
    body: `Delete the spawn profile "${name}"? Any group or the dashboard that names it as a default will fall back to blank spawn fields until re-pointed. Agents already spawned are untouched.`,
    meta: name,
    okLabel: 'Delete profile',
  });
  if (!ok) return;
  try {
    await deleteProfile(name);
    toast(`profile deleted: ${name}`);
    reloadProfilesList();
  } catch (err) {
    toast((err && err.message) || String(err), true);
  }
}

// ---- wiring -------------------------------------------------------------

function bindProfilesUI() {
  // Entry point: the Groups cog's "⧉ profiles…" management overlay.
  $('#profiles-manage-open').addEventListener('click', openProfilesManageModal);
  $('#profiles-manage-close').addEventListener('click', closeProfilesManageModal);
  bindManageOverlayDismiss('profiles-manage-modal', closeProfilesManageModal);
  $('#profile-create-open').addEventListener('click', () => openProfileEditor(null));

  // Filter box — re-paints from the already-fetched list (no re-fetch per
  // keystroke). The profiles overlay isn't snapshot-backed, so this is wired
  // directly rather than through bindFilter.
  $('#filter-profiles').addEventListener('input', paintProfilesList);
  $('#filter-profiles-clear').addEventListener('click', () => {
    $('#filter-profiles').value = '';
    paintProfilesList();
    $('#filter-profiles').focus();
  });

  // Profile-card actions (delegated — the list re-renders on every mutation).
  // data-pact keeps these off the global row-action bus.
  $('#profiles-list').addEventListener('click', e => {
    const btn = e.target.closest('[data-pact]');
    if (!btn) return;
    const name = btn.dataset.profile;
    if (btn.dataset.pact === 'edit') {
      const p = profilesByName()[name];
      if (p) openProfileEditor(p);
    } else if (btn.dataset.pact === 'delete') {
      removeProfile(name);
    }
  });

  // Editor modal.
  $('#profile-editor-cancel').addEventListener('click', closeProfileEditor);
  $('#profile-editor-submit').addEventListener('click', submitProfileEditor);
  $('#profile-editor-harness').addEventListener('change', (e) => {
    applyProfileEditorHarness(e.target.value);
  });
  bindBackdropDiscard('profile-editor-modal', closeProfileEditor);
}

export { bindProfilesUI };
