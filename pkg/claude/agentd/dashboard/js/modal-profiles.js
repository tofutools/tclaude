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
// the snapshot's `harnesses` catalog exactly like that dialog — including the
// Permission-mode (approval) dropdown for a harness that surfaces approval
// modes (Claude Code). Two fields stay un-surfaced: Codex's approval policy
// (its harness exposes no dialog modes yet — CLI/`/v1`-only) and auto_review
// (the experimental guardian opt-in, Codex-only). Because the server's PATCH is
// a FULL replace, editing a profile that carries an un-surfaced value would
// wipe it, so submit carries those forward from the original (guarded on an
// unchanged harness).

import { $, esc, bindSelectTitles } from './helpers.js';
import { lastSnapshot } from './dashboard.js';
import { confirmModal, toast, bindBackdropDiscard, bindManageOverlayDismiss } from './refresh.js';
import {
  loadProfiles, createProfile, updateProfile, deleteProfile, profileSummary,
} from './profiles.js';
import { openSpawnPermEditor } from './modal-message.js';
import { isWizardActive } from './slop.js';

// wizWord swaps the profile vocabulary for 🧙 wizard mode: in wizard mode we
// Summon Familiars, so a saved spawn "profile" is re-lettered a "familiar
// pattern". Mirrors the pure-CSS .profiles-word span swaps in dashboard.css
// for the spots that are JS-rendered here (the editor title + the manage
// overlay's empty-state).
function wizWord(regular, wizard) { return isWizardActive() ? wizard : regular; }

// Buffered per-slug permission overrides for the profile being edited.
// slug → 'grant' | 'deny'; reset per openProfileEditor, written by the stacked
// Permissions… editor, and folded into the save payload.
let profilePermOverrides = {};

// The original profile object while editing an existing one (the PATCH target
// + the source of carried-forward fields the editor doesn't surface); null
// while creating (incl. a pre-filled "save as" / "new profile" create).
let profileEditorEditing = null;

// Optional callback invoked with the saved profile's name after a successful
// submit — lets a caller that opened the editor (the spawn dialog's "Save as
// profile", the default-profile pickers' "new profile") react, e.g. select
// the new profile as a default. Reset on every open/close.
let profileEditorOnSaved = null;

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

// applyProfileEditorHarness reshapes the editor's Model / Sandbox / Permission-
// mode / Effort / trust-dir rows for the chosen harness — the curated model
// <select> vs the free-text input, the sandbox + permission-mode rows (with
// their modes) for a harness that exposes them, the Codex-only trust-dir row.
// Mirrors applySpawnHarness.
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

  // Permission mode (Claude Code) — same gating as the spawn dialog: shown only
  // for a harness that surfaces approval modes (Codex has none, so its approval
  // stays carried-forward, not edited here — see buildProfilePayload).
  const canApproval = !!(h && h.can_approval && h.approval_modes && h.approval_modes.length);
  $('#profile-editor-approval-row').style.display = canApproval ? '' : 'none';
  if (canApproval) {
    const apprSel = $('#profile-editor-approval');
    apprSel.innerHTML = h.approval_modes
      .map(m => `<option value="${esc(m)}">${esc(m)}${m === h.default_approval ? ' (recommended)' : ''}</option>`)
      .join('');
    apprSel.value = h.default_approval || h.approval_modes[0];
  }

  // trust-dir is Codex-only — it edits ~/.codex/config.toml, the same gating
  // the spawn dialog applies.
  const isCodex = !!(h && h.name === 'codex');
  $('#profile-editor-trust-dir-row').style.display = isCodex ? '' : 'none';

  // remote-control is the inverse: a Claude-Code feature, gated on the harness
  // having built-in Remote Access (can_remote_control) — shown for Claude,
  // hidden for a harness without it (Codex). Fail-open to shown when the
  // catalog hasn't loaded yet, matching the spawn dialog's applySpawnHarness.
  const canRemoteControl = h ? !!h.can_remote_control : true;
  $('#profile-editor-remote-control-row').style.display = canRemoteControl ? '' : 'none';

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

// updateProfilePermsIndicator mirrors the buffered override count next to the
// profile editor's Permissions… button (e.g. "1 grant · 2 denies").
function updateProfilePermsIndicator() {
  const el = $('#profile-editor-perms-indicator');
  if (!el) return;
  const slugs = Object.keys(profilePermOverrides);
  if (!slugs.length) {
    el.hidden = true;
    el.textContent = '';
    return;
  }
  let grants = 0, denies = 0;
  slugs.forEach(s => { if (profilePermOverrides[s] === 'deny') denies++; else grants++; });
  const parts = [];
  if (grants) parts.push(`${grants} grant${grants === 1 ? '' : 's'}`);
  if (denies) parts.push(`${denies} den${denies === 1 ? 'y' : 'ies'}`);
  el.textContent = parts.join(' · ');
  el.hidden = false;
}

// openProfilePermsEditor opens the stacked per-slug editor seeded from the
// profile's buffered overrides. The owner select gates the "via owner" preview;
// the group is unknown for a reusable profile, so a placeholder names it.
function openProfilePermsEditor() {
  openSpawnPermEditor({
    overrides: profilePermOverrides,
    ownsGroup: readTri($('#profile-editor-owner')) === true,
    group: 'the spawn group',
    label: $('#profile-editor-agent-name').value.trim(),
    onSave: (kept) => {
      profilePermOverrides = kept;
      updateProfilePermsIndicator();
    },
  });
}

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
      ? wizWord('No profiles match the filter.', 'No patterns match the filter.')
      : wizWord(
        'No spawn profiles yet — press <b>+ new profile</b> to define one. A profile pre-fills the spawn dialog and can be a group or dashboard default.',
        'No familiar patterns yet — press <b>+ new pattern</b> to weave one. A pattern pre-fills the Summon dialog and can be a group or dashboard default.')}</div>`;
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

// openProfileEditor opens the editor, populated from `seed` (a profile-shaped
// object, or null for a blank form). Options:
//   - editExisting (default true): edit `seed` in place — submit PATCHes
//     seed.name and the name field starts at seed.name. When false, `seed` is
//     only a pre-fill (the spawn dialog's "Save as profile", a picker's "new
//     profile"): submit CREATES a fresh profile and the name field starts
//     blank so the human names it.
//   - onSaved(name): fired after a successful submit (see profileEditorOnSaved).
// The manage modal's existing callers pass one argument, so they keep the
// edit-existing behaviour (openProfileEditor(null) = blank create,
// openProfileEditor(p) = edit p).
function openProfileEditor(seed, { editExisting = true, onSaved = null } = {}) {
  profileEditorEditing = (editExisting && seed) ? seed : null;
  profileEditorOnSaved = onSaved;
  $('#profile-editor-title').textContent = profileEditorEditing
    ? wizWord(`Edit profile: ${seed.name}`, `Edit pattern: ${seed.name}`)
    : wizWord('New spawn profile', 'New familiar pattern');
  $('#profile-editor-error').textContent = '';
  // Name field carries the existing name only when editing in place; a
  // pre-filled create starts blank so the human gives the new profile a name.
  $('#profile-editor-name').value = profileEditorEditing ? (seed.name || '') : '';

  // Harness first — populate the selector, set it to the seed's harness (or
  // default), then reshape the launch rows before filling per-field controls.
  populateProfileHarnessSelect();
  const hSel = $('#profile-editor-harness');
  if (seed && seed.harness && [...hSel.options].some(o => o.value === seed.harness)) {
    hSel.value = seed.harness;
  }
  applyProfileEditorHarness(hSel.value);

  // Model into the now-active control; effort + sandbox into their (reshaped)
  // selects.
  $('#profile-editor-model').value = '';
  $('#profile-editor-model-codex').value = '';
  if (seed && seed.model) profileActiveModelEl().value = seed.model;
  setSelectIfPresent($('#profile-editor-effort'), seed ? seed.effort : '');
  setSelectIfPresent($('#profile-editor-sandbox'), seed ? seed.sandbox : '');
  setSelectIfPresent($('#profile-editor-approval'), seed ? seed.approval : '');

  setTri($('#profile-editor-trust-dir'), seed ? seed.trust_dir : null);
  setTri($('#profile-editor-remote-control'), seed ? seed.remote_control : null);
  setTri($('#profile-editor-sync-worktree'), seed ? seed.sync_worktree : null);
  setTri($('#profile-editor-auto-focus'), seed ? seed.auto_focus : null);
  setTri($('#profile-editor-group-context'), seed ? seed.include_group_default_context : null);
  setTri($('#profile-editor-owner'), seed ? seed.is_owner : null);

  $('#profile-editor-agent-name').value = seed ? (seed.agent_name || '') : '';
  $('#profile-editor-role').value = seed ? (seed.role || '') : '';
  $('#profile-editor-descr').value = seed ? (seed.descr || '') : '';
  $('#profile-editor-init-msg').value = seed ? (seed.initial_message || '') : '';

  // Birth-time permission overrides: seed the buffer from the profile (a shallow
  // copy so editing doesn't mutate the seed) and refresh the indicator.
  profilePermOverrides = (seed && seed.permission_overrides) ? { ...seed.permission_overrides } : {};
  updateProfilePermsIndicator();

  $('#profile-editor-modal').classList.add('show');
  bindSelectTitles($('#profile-editor-modal'));
  setTimeout(() => $('#profile-editor-name').focus(), 0);
}

function closeProfileEditor() {
  $('#profile-editor-modal').classList.remove('show');
  profileEditorOnSaved = null;
}

// buildProfilePayload assembles the full desired state from the editor. The
// server's PATCH is a full replace, so every field the profile should keep
// must be present. Launch fields are gated on the chosen harness's
// capabilities so we never post a value it would reject (a trust-dir on Claude,
// remote-control on Codex). The permission mode (approval) is editable for a
// harness that surfaces it (Claude Code); un-surfaced approval-subsystem fields
// (Codex approval, auto_review) are carried forward from the original on a
// same-harness edit so the full-replace doesn't drop a CLI-set value.
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
  // Sandbox: only for a harness that takes a launch sandbox; its select always
  // carries a value (the default is pre-selected).
  if (hEntry && hEntry.can_sandbox && $('#profile-editor-sandbox').value) {
    body.sandbox = $('#profile-editor-sandbox').value;
  }
  // Permission mode: editable here only for a harness that surfaces approval
  // modes (Claude Code) — its dropdown is the source of truth. A modeless
  // harness (Codex) keeps its approval CLI-only, carried forward below.
  const surfacesApproval = !!(hEntry && hEntry.can_approval
    && hEntry.approval_modes && hEntry.approval_modes.length);
  if (surfacesApproval && $('#profile-editor-approval').value) {
    body.approval = $('#profile-editor-approval').value;
  }
  // trust-dir: Codex-only (the backend rejects a true on any other harness).
  const trustDir = (harness === 'codex') ? readTri($('#profile-editor-trust-dir')) : null;
  if (trustDir != null) body.trust_dir = trustDir;

  // remote-control: the inverse — a Claude-Code feature the backend rejects
  // (remote_control=true) on a harness with no built-in Remote Access (Codex),
  // the same gate the spawn dialog applies. Gated on the harness capability so
  // we never post a value it would 400.
  const canRemoteControl = hEntry ? !!hEntry.can_remote_control : true;
  const remoteControl = canRemoteControl ? readTri($('#profile-editor-remote-control')) : null;
  if (remoteControl != null) body.remote_control = remoteControl;

  const syncWt = readTri($('#profile-editor-sync-worktree'));
  if (syncWt != null) body.sync_worktree = syncWt;
  const autoFocus = readTri($('#profile-editor-auto-focus'));
  if (autoFocus != null) body.auto_focus = autoFocus;
  const groupCtx = readTri($('#profile-editor-group-context'));
  if (groupCtx != null) body.include_group_default_context = groupCtx;

  // Birth-time access controls: the tri-state owner pre-fill and the
  // buffered per-slug overrides. Sent only when set so a profile without them
  // round-trips unchanged.
  const owner = readTri($('#profile-editor-owner'));
  if (owner != null) body.is_owner = owner;
  if (Object.keys(profilePermOverrides).length) body.permission_overrides = profilePermOverrides;

  // Carry forward the un-surfaced approval-subsystem fields on a same-harness
  // edit (treat "" and "claude" as the same default harness), so the
  // full-replace PATCH doesn't drop a CLI-set value the editor can't show:
  //   - approval: only when the harness does NOT surface it as a dropdown
  //     (Codex). When it does (Claude Code), the dropdown above is authoritative
  //     — carrying forward would clobber a just-cleared/changed choice.
  //   - auto_review: never surfaced anywhere, so always carried forward.
  const norm = (h) => h || 'claude';
  if (profileEditorEditing && norm(profileEditorEditing.harness) === norm(harness)) {
    if (!surfacesApproval && profileEditorEditing.approval) body.approval = profileEditorEditing.approval;
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
    // Capture the callback before closeProfileEditor() clears it.
    const onSaved = profileEditorOnSaved;
    closeProfileEditor();
    toast(editing ? `profile updated: ${name}` : `profile created: ${name}`);
    reloadProfilesList();
    if (onSaved) onSaved(name);
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
  // Permissions… opens the stacked per-slug editor on the profile's buffer
  // — same sibling-overlay pattern as the spawn dialog's button.
  $('#profile-editor-perms').addEventListener('click', openProfilePermsEditor);
  $('#profile-editor-harness').addEventListener('change', (e) => {
    applyProfileEditorHarness(e.target.value);
  });
  bindBackdropDiscard('profile-editor-modal', closeProfileEditor);
}

export { bindProfilesUI, openProfileEditor };
