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

import { $, esc, bindSelectTitles, bindModalSubmitHotkey, setModelSelectValue, syncCustomModelRow, MODEL_CUSTOM_VALUE } from './helpers.js';
import { lastSnapshot } from './dashboard.js';
import { confirmModal, toast, bindBackdropDiscard, bindManageOverlayDismiss } from './refresh.js';
import {
  loadProfiles, createProfile, updateProfile, deleteProfile, profileSummary,
  exportProfiles, inspectProfileImport, importProfiles,
} from './profiles.js';
import { openSpawnPermEditor } from './modal-message.js';
// wizWord swaps the profile vocabulary for 🧙 wizard mode: in wizard mode we
// Summon Familiars, so a saved spawn "profile" is re-lettered a "familiar
// pattern". Used here for the JS-rendered spots (the editor title + the manage
// overlay's empty-state); the static spots use the pure-CSS .profiles-word span
// swaps in dashboard.css.
import { wizWord } from './slop.js';

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

// LOCAL mode (template-local per-agent launch config): non-null while the
// editor edits an unnamed, unstored profile object owned by the caller — the
// template editor's "✎ custom…" per-agent launch config. Holds the caller's
// onSave(payload) callback; submit hands the built profile-shaped payload back
// instead of POST/PATCHing the registry. The name row, the identity rows
// (agent_name / role / descr / initial_message — those live on the template
// agent itself) and the spawn-dialog-only toggles (sync_worktree / auto_focus /
// include_group_default_context) are hidden: the server rejects them on a
// template-local profile. Reset on every open/close.
let profileEditorLocal = null;

// profileEditorLocalHiddenRows returns the field rows local mode hides.
function profileEditorLocalHiddenRows() {
  return [
    $('#profile-editor-name'), $('#profile-editor-agent-name'), $('#profile-editor-role'),
    $('#profile-editor-descr'), $('#profile-editor-init-msg'), $('#profile-editor-sync-worktree'),
    $('#profile-editor-auto-focus'), $('#profile-editor-group-context'),
  ].map(el => el && el.closest('.cron-create-row')).filter(Boolean);
}

// The last list the manage overlay fetched — the filter box re-paints from
// this without a re-fetch; a create/edit/delete reloads it.
let lastProfiles = [];

let profileImportEnvelope = null;
let profileImportPreview = null;

// ---- harness catalog (snapshot-driven, like the spawn dialog) -----------

function profileHarnessCatalog() {
  return (lastSnapshot && lastSnapshot.harnesses) || [];
}
function profileHarnessByName(name) {
  return profileHarnessCatalog().find(h => h.name === name) || null;
}

// profileActiveModelEl returns the Model control in play for the selected
// harness — the curated <select> for a harness with a model list, its revealed
// "Custom…" free-text input when the select sits on that sentinel, or the Codex
// free-text <input> for a harness without a model list. Mirrors activeSpawnModelEl.
function profileActiveModelEl() {
  const h = profileHarnessByName($('#profile-editor-harness').value);
  const codexStyle = h && (!h.models || h.models.length === 0);
  if (codexStyle) return $('#profile-editor-model-codex');
  const sel = $('#profile-editor-model');
  return sel.value === MODEL_CUSTOM_VALUE ? $('#profile-editor-model-custom') : sel;
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
  // The free-text "Custom…" row belongs to the curated <select>; reconcile it
  // with the select for Claude, hide it for a free-text harness (Codex).
  if (hasModelList) syncCustomModelRow('profile-editor-model');
  else $('#profile-editor-model-custom-row').style.display = 'none';

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

  // AskUserQuestion timeout — surfaced only for a harness with the dialog
  // (Claude Code), mirroring the sandbox/approval rows.
  const canAskTimeout = !!(h && h.can_ask_timeout && h.ask_timeout_modes && h.ask_timeout_modes.length);
  $('#profile-editor-ask-timeout-row').style.display = canAskTimeout ? '' : 'none';
  if (canAskTimeout) {
    const atSel = $('#profile-editor-ask-timeout');
    atSel.innerHTML = h.ask_timeout_modes
      .map(m => `<option value="${esc(m)}">${esc(m)}${m === h.default_ask_timeout ? ' (recommended)' : ''}</option>`)
      .join('');
    atSel.value = h.default_ask_timeout || h.ask_timeout_modes[0];
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

// ---- Export / import -----------------------------------------------------

function openProfileExportModal() {
  $('#profile-export-error').textContent = '';
  const list = $('#profile-export-list');
  if (!lastProfiles.length) {
    list.innerHTML = `<div class="template-empty">${wizWord('No profiles to export.', 'No familiar patterns to inscribe.')}</div>`;
  } else {
    list.innerHTML = lastProfiles.map(p => `<label class="profile-transfer-row">
      <input type="checkbox" data-profile-export-name="${esc(p.name)}" checked />
      <span class="profile-transfer-main">
        <span class="profile-transfer-name">${esc(p.name)}</span>
        ${profileSummary(p) ? `<span class="profile-transfer-summary">${esc(profileSummary(p))}</span>` : ''}
      </span>
    </label>`).join('');
  }
  $('#profile-export-submit').disabled = !lastProfiles.length;
  $('#profile-export-modal').classList.add('show');
}

function closeProfileExportModal() { $('#profile-export-modal').classList.remove('show'); }

function selectedExportProfileNames() {
  return [...$('#profile-export-list').querySelectorAll('[data-profile-export-name]')]
    .filter(el => el.checked)
    .map(el => el.dataset.profileExportName);
}

function downloadJSON(name, value) {
  const blob = new Blob([JSON.stringify(value, null, 2) + '\n'], { type: 'application/json' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}

async function submitProfileExport() {
  const errEl = $('#profile-export-error');
  errEl.textContent = '';
  const names = selectedExportProfileNames();
  if (!names.length) { errEl.textContent = 'select at least one profile'; return; }
  const btn = $('#profile-export-submit');
  btn.disabled = true;
  try {
    const bundle = await exportProfiles(names);
    downloadJSON('spawn-profiles.json', bundle);
    closeProfileExportModal();
    toast(`${names.length} profile${names.length === 1 ? '' : 's'} exported`);
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    btn.disabled = false;
  }
}

function resetProfileImportPreview() {
  profileImportEnvelope = null;
  profileImportPreview = null;
  $('#profile-import-preview').innerHTML = '';
  $('#profile-import-preview').hidden = true;
  $('#profile-import-submit').disabled = true;
}

function openProfileImportModal() {
  $('#profile-import-file').value = '';
  $('#profile-import-paste').value = '';
  $('#profile-import-error').textContent = '';
  resetProfileImportPreview();
  $('#profile-import-modal').classList.add('show');
  setTimeout(() => $('#profile-import-paste').focus(), 0);
}

function closeProfileImportModal() { $('#profile-import-modal').classList.remove('show'); }

async function readProfileImportSource() {
  const fileInput = $('#profile-import-file');
  const file = fileInput.files && fileInput.files[0];
  if (file) return (await file.text()).trim();
  return $('#profile-import-paste').value.trim();
}

function importProfileByName(name) {
  return (profileImportEnvelope && profileImportEnvelope.profiles || []).find(p => p.name === name) || null;
}

function profileImportRowHTML(prev) {
  const source = importProfileByName(prev.name);
  const summary = source ? profileSummary(source) : '';
  const disabled = prev.valid ? '' : ' disabled';
  const checked = prev.valid ? ' checked' : '';
  const conflictControls = prev.exists && prev.valid ? `<span class="profile-import-conflict">
      <select data-profile-import-action="${esc(prev.name)}" title="How to handle this existing local profile">
        <option value="rename" selected>Rename</option>
        <option value="overwrite">Overwrite</option>
      </select>
      <input type="text" data-profile-import-as="${esc(prev.name)}" value="${esc(prev.default_name || (prev.name + '-copy'))}" autocomplete="off" spellcheck="false" />
    </span>` : '';
  return `<div class="profile-transfer-row profile-import-row${prev.exists ? ' conflict' : ''}${prev.valid ? '' : ' invalid'}">
    <input type="checkbox" data-profile-import-include="${esc(prev.name)}"${checked}${disabled} />
    <span class="profile-transfer-main">
      <span class="profile-transfer-name">${esc(prev.name)}</span>
      ${summary ? `<span class="profile-transfer-summary">${esc(summary)}</span>` : ''}
      ${prev.exists ? '<span class="profile-transfer-note">already exists locally</span>' : ''}
      ${prev.error ? `<span class="profile-transfer-error">${esc(prev.error)}</span>` : ''}
    </span>
    ${conflictControls}
  </div>`;
}

function renderProfileImportPreview() {
  const host = $('#profile-import-preview');
  const rows = (profileImportPreview && profileImportPreview.profiles) || [];
  if (!rows.length) {
    host.innerHTML = '<div class="template-empty">The export contains no profiles.</div>';
    host.hidden = false;
    $('#profile-import-submit').disabled = true;
    return;
  }
  host.innerHTML = rows.map(profileImportRowHTML).join('');
  host.hidden = false;
  $('#profile-import-submit').disabled = rows.every(r => !r.valid);
}

async function inspectProfileImportSource() {
  const errEl = $('#profile-import-error');
  errEl.textContent = '';
  resetProfileImportPreview();
  const btn = $('#profile-import-inspect');
  btn.disabled = true;
  try {
    const raw = await readProfileImportSource();
    if (!raw) { errEl.textContent = 'pick a file or paste the profile JSON'; return; }
    try {
      profileImportEnvelope = JSON.parse(raw);
    } catch (e) {
      errEl.textContent = 'not valid JSON: ' + ((e && e.message) || String(e));
      return;
    }
    profileImportPreview = await inspectProfileImport(profileImportEnvelope);
    renderProfileImportPreview();
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    btn.disabled = false;
  }
}

function profileImportDecisions() {
  return [...$('#profile-import-preview').querySelectorAll('[data-profile-import-include]')].map(cb => {
    const name = cb.dataset.profileImportInclude;
    const actionEl = $(`[data-profile-import-action="${CSS.escape(name)}"]`);
    const asEl = $(`[data-profile-import-as="${CSS.escape(name)}"]`);
    const action = cb.checked ? (actionEl ? actionEl.value : 'create') : 'skip';
    const out = {
      name,
      include: cb.checked,
      action,
    };
    if (action === 'rename' && asEl) out.as = asEl.value.trim();
    return out;
  });
}

async function submitProfileImport() {
  const errEl = $('#profile-import-error');
  errEl.textContent = '';
  if (!profileImportEnvelope || !profileImportPreview) {
    errEl.textContent = 'preview the import first';
    return;
  }
  const btn = $('#profile-import-submit');
  btn.disabled = true;
  try {
    const res = await importProfiles(profileImportEnvelope, profileImportDecisions());
    closeProfileImportModal();
    const imported = (res && res.imported) || [];
    const skipped = (res && res.skipped) || [];
    const updated = imported.filter(p => p.updated).length;
    let msg = `${imported.length} profile${imported.length === 1 ? '' : 's'} imported`;
    if (updated) msg += ` (${updated} overwritten)`;
    if (skipped.length) msg += `, ${skipped.length} skipped`;
    toast(msg);
    reloadProfilesList();
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    btn.disabled = false;
  }
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
//   - local: {onSave(payload)} — LOCAL mode (see profileEditorLocal): edit an
//     unnamed profile object for the caller instead of the registry. `seed`
//     pre-fills the form (the current inline config, or a registry profile to
//     fork from); submit builds the payload and hands it to onSave — no REST.
// The manage modal's existing callers pass one argument, so they keep the
// edit-existing behaviour (openProfileEditor(null) = blank create,
// openProfileEditor(p) = edit p).
function openProfileEditor(seed, { editExisting = true, onSaved = null, local = null } = {}) {
  profileEditorEditing = (!local && editExisting && seed) ? seed : null;
  profileEditorOnSaved = onSaved;
  // Remember the seed in local mode so buildProfilePayload's carry-forward of
  // un-surfaced fields (Codex approval, auto_review) works for a re-edit of an
  // existing inline config too, not only for registry edits.
  profileEditorLocal = local ? { ...local, seed: seed || null } : null;
  for (const row of profileEditorLocalHiddenRows()) row.hidden = !!local;
  $('#profile-editor-title').textContent = local
    ? wizWord('Custom launch — this agent only', 'Bespoke summons — this familiar only')
    : profileEditorEditing
      ? wizWord(`Edit profile: ${seed.name}`, `Edit pattern: ${seed.name}`)
      : wizWord('New spawn profile', 'New familiar pattern');
  $('#profile-editor-submit').textContent = local ? 'Apply' : 'Save profile';
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
  // selects. setModelSelectValue clears both controls (dropping any option a
  // prior open injected) and, for the curated <select>, keeps an out-of-catalog
  // seed model (a full id like "claude-opus-4-8[1m]") selectable rather than
  // silently dropping it — so capturing a live agent running a non-preset model
  // into a profile round-trips its exact model.
  setModelSelectValue($('#profile-editor-model'), '');
  $('#profile-editor-model-codex').value = '';
  if (seed && seed.model) setModelSelectValue(profileActiveModelEl(), seed.model);
  // Reconcile the "Custom…" free-text row: the reset above left the select on a
  // concrete value (Default or the seeded model), so the row hides — a seed
  // never lands the editor on a half-typed custom entry.
  syncCustomModelRow('profile-editor-model');
  setSelectIfPresent($('#profile-editor-effort'), seed ? seed.effort : '');
  setSelectIfPresent($('#profile-editor-sandbox'), seed ? seed.sandbox : '');
  setSelectIfPresent($('#profile-editor-approval'), seed ? seed.approval : '');
  setSelectIfPresent($('#profile-editor-ask-timeout'), seed ? seed.ask_user_question_timeout : '');

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
  // Local mode hides the name row, so land the focus on the first visible field.
  setTimeout(() => $(local ? '#profile-editor-harness' : '#profile-editor-name').focus(), 0);
}

function closeProfileEditor() {
  $('#profile-editor-modal').classList.remove('show');
  profileEditorOnSaved = null;
  profileEditorLocal = null;
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
  // AskUserQuestion timeout: Claude-Code-only, editable only for a harness that
  // surfaces the dialog. Its dropdown is the source of truth. Never carried
  // forward on a harness switch (the backend rejects it on a non-Claude
  // profile), so a switch to Codex correctly drops it.
  const surfacesAskTimeout = !!(hEntry && hEntry.can_ask_timeout
    && hEntry.ask_timeout_modes && hEntry.ask_timeout_modes.length);
  if (surfacesAskTimeout && $('#profile-editor-ask-timeout').value) {
    body.ask_user_question_timeout = $('#profile-editor-ask-timeout').value;
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
  const carrySrc = profileEditorEditing || (profileEditorLocal && profileEditorLocal.seed) || null;
  if (carrySrc && norm(carrySrc.harness) === norm(harness)) {
    if (!surfacesApproval && carrySrc.approval) body.approval = carrySrc.approval;
    if (carrySrc.auto_review != null) body.auto_review = carrySrc.auto_review;
  }
  return body;
}

async function submitProfileEditor() {
  const errEl = $('#profile-editor-error');
  errEl.textContent = '';
  // LOCAL mode: build the profile-shaped payload, strip everything a
  // template-local profile can't carry (the hidden rows + the name), and hand
  // it back to the caller — the template save is where the server validates it.
  if (profileEditorLocal) {
    const p = buildProfilePayload('');
    delete p.name;
    delete p.agent_name; delete p.role; delete p.descr; delete p.initial_message;
    delete p.sync_worktree; delete p.auto_focus; delete p.include_group_default_context;
    const onSave = profileEditorLocal.onSave;
    closeProfileEditor();
    if (onSave) onSave(p);
    return;
  }
  const name = $('#profile-editor-name').value.trim();
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
  $('#profile-export-open').addEventListener('click', openProfileExportModal);
  $('#profile-import-open').addEventListener('click', openProfileImportModal);

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
  // Ctrl/Cmd+Enter saves from anywhere in the editor — the shared modal
  // submit-hotkey convention (spawn / clone / reincarnate / export dialogs).
  bindModalSubmitHotkey($('#profile-editor-modal'), $('#profile-editor-submit'));
  // Permissions… opens the stacked per-slug editor on the profile's buffer
  // — same sibling-overlay pattern as the spawn dialog's button.
  $('#profile-editor-perms').addEventListener('click', openProfilePermsEditor);
  $('#profile-editor-harness').addEventListener('change', (e) => {
    applyProfileEditorHarness(e.target.value);
  });
  // Picking "Custom model id…" reveals the free-text row and focuses it.
  $('#profile-editor-model').addEventListener('change', () => {
    syncCustomModelRow('profile-editor-model', { focus: true });
  });
  bindBackdropDiscard('profile-editor-modal', closeProfileEditor);

  // Export / import modals.
  $('#profile-export-cancel').addEventListener('click', closeProfileExportModal);
  $('#profile-export-submit').addEventListener('click', submitProfileExport);
  $('#profile-export-list').addEventListener('change', () => {
    $('#profile-export-submit').disabled = !selectedExportProfileNames().length;
  });
  bindBackdropDiscard('profile-export-modal', closeProfileExportModal);

  $('#profile-import-cancel').addEventListener('click', closeProfileImportModal);
  $('#profile-import-inspect').addEventListener('click', inspectProfileImportSource);
  $('#profile-import-submit').addEventListener('click', submitProfileImport);
  $('#profile-import-file').addEventListener('change', inspectProfileImportSource);
  $('#profile-import-paste').addEventListener('input', resetProfileImportPreview);
  $('#profile-import-preview').addEventListener('change', e => {
    const action = e.target.closest('[data-profile-import-action]');
    if (!action) return;
    const name = action.dataset.profileImportAction;
    const asEl = $(`[data-profile-import-as="${CSS.escape(name)}"]`);
    if (asEl) asEl.disabled = action.value !== 'rename';
  });
  bindBackdropDiscard('profile-import-modal', closeProfileImportModal);
}

// removeProfile is exported so the palette dock's card ⚙ → Delete menu item can
// reuse the exact confirm + delete + toast the manager's delete button uses (the
// dock adds a dashboard refresh after, since reloadProfilesList only repaints the
// closed manager overlay).
export { bindProfilesUI, openProfileEditor, openProfilesManageModal, removeProfile };
