// modal-roles.js — the role-library management overlay + editor (JOH-240).
// Mirrors modal-profiles.js: a manage overlay lists the saved roles as cards
// (with edit / delete / + new), and an editor modal creates/edits one.
//
// A role is a named bundle of defaults a template roster agent references: a
// canonical role-brief (folded into the referencing agent's startup context as
// a "## Role" block), a default launch shape (harness / model / effort /
// sandbox / approval or a spawn-profile reference), and a default permission
// set. The data layer (roles.js) owns the fetch + cache; this module is DOM.
//
// The launch fields are driven off the snapshot's `harnesses` catalog exactly
// like the profile editor, so model/effort/sandbox/approval validate against
// the chosen harness. Because the server's PATCH is a FULL replace, the editor
// surfaces every stored field.

import { $, $$, esc, bindSelectTitles, populateModelSelect, setModelSelectValue, syncCustomModelRow, MODEL_CUSTOM_VALUE } from './helpers.js';
import { lastSnapshot } from './dashboard.js';
import { confirmModal, toast, bindBackdropDiscard, bindManageOverlayDismiss } from './refresh.js';
import { loadRoles, createRole, updateRole, deleteRole, roleSummary } from './roles.js';
import { loadProfiles } from './profiles.js';
// wizWord swaps the role vocabulary for 🧙 wizard mode: roles are the party's
// classes, so the JS-rendered spots re-letter "role" to "class". The static
// spots use the pure-CSS .roles-word span swaps in dashboard.css.
import { wizWord } from './slop.js';

// The original role object while editing an existing one (the PATCH target);
// null while creating.
let roleEditorEditing = null;

// The last list the manage overlay fetched — the filter box re-paints from this
// without a re-fetch; a create/edit/delete reloads it.
let lastRoles = [];

// ---- harness catalog (snapshot-driven, like the profile editor) ---------

function roleHarnessCatalog() {
  return (lastSnapshot && lastSnapshot.harnesses) || [];
}
function roleHarnessByName(name) {
  return roleHarnessCatalog().find(h => h.name === name) || null;
}

// roleActiveModelEl returns the Model control in play for the selected harness —
// the curated <select> for a harness with a model list, its revealed "Custom…"
// free-text input when the select sits on that sentinel, or the fallback free-text
// <input> for a harness without a model list. Mirrors profileActiveModelEl.
function roleActiveModelEl() {
  const h = roleHarnessByName($('#role-editor-harness').value);
  const freeTextStyle = h && (!h.models || h.models.length === 0);
  if (freeTextStyle) return $('#role-editor-model-codex');
  const sel = $('#role-editor-model');
  return sel.value === MODEL_CUSTOM_VALUE ? $('#role-editor-model-custom') : sel;
}

function populateRoleHarnessSelect() {
  const sel = $('#role-editor-harness');
  const cat = roleHarnessCatalog();
  sel.innerHTML = cat
    .map(h => `<option value="${esc(h.name)}">${esc(h.display_name || h.name)}</option>`)
    .join('');
  if (cat.some(h => h.name === 'claude')) sel.value = 'claude';
  else if (cat.length) sel.value = cat[0].name;
}

// populateRoleEffortSelect rebuilds the Effort options from the harness's levels
// (data-driven off the catalog), preserving the current selection when the new
// list still offers it. Mirrors populateProfileEffortSelect.
function populateRoleEffortSelect(h) {
  const levels = h && h.effort_levels;
  if (!levels || !levels.length) return; // keep the static fallback options
  const sel = $('#role-editor-effort');
  const prev = sel.value;
  sel.innerHTML = `<option value="">Default (harness's own)</option>`
    + levels.map(l => `<option value="${esc(l)}">${esc(l)}</option>`).join('');
  if ([...sel.options].some(o => o.value === prev)) sel.value = prev;
}

// applyRoleEditorHarness reshapes the editor's Model / Sandbox / Permission-mode
// / Effort rows for the chosen harness — the curated model <select> vs the
// free-text input, and the sandbox + permission-mode rows (with their modes) for
// a harness that exposes them. Mirrors applyProfileEditorHarness (roles carry no
// trust-dir / remote-control, so those rows are absent).
function applyRoleEditorHarness(harnessName) {
  const h = roleHarnessByName(harnessName);
  const hasModelList = !h || (h.models && h.models.length > 0);
  $('#role-editor-model-claude-row').style.display = hasModelList ? '' : 'none';
  $('#role-editor-model-codex-row').style.display = hasModelList ? 'none' : '';
  if (hasModelList && h) populateModelSelect($('#role-editor-model'), h.models);
  // The free-text "Custom…" row belongs to the curated <select>; reconcile it
  // with the selector, or hide it for a harness with no suggestions.
  if (hasModelList) syncCustomModelRow('role-editor-model');
  else $('#role-editor-model-custom-row').style.display = 'none';

  const canSandbox = !!(h && h.can_sandbox && h.sandbox_modes && h.sandbox_modes.length);
  $('#role-editor-sandbox-row').style.display = canSandbox ? '' : 'none';
  if (canSandbox) {
    const sandSel = $('#role-editor-sandbox');
    sandSel.innerHTML = h.sandbox_modes
      .map(m => `<option value="${esc(m)}">${esc(m)}${m === h.default_sandbox ? ' (recommended)' : ''}</option>`)
      .join('');
    sandSel.value = h.default_sandbox || h.sandbox_modes[0];
  }

  const canApproval = !!(h && h.can_approval && h.approval_modes && h.approval_modes.length);
  $('#role-editor-approval-row').style.display = canApproval ? '' : 'none';
  if (canApproval) {
    const apprSel = $('#role-editor-approval');
    apprSel.innerHTML = h.approval_modes
      .map(m => `<option value="${esc(m)}">${esc(m)}${m === h.default_approval ? ' (recommended)' : ''}</option>`)
      .join('');
    apprSel.value = h.default_approval || h.approval_modes[0];
  }

  populateRoleEffortSelect(h);
}

function setSelectIfPresent(sel, value) {
  if (value && [...sel.options].some(o => o.value === value)) sel.value = value;
}

// ---- Manage-roles overlay -----------------------------------------------

function openRolesManageModal() {
  $('#roles-manage-modal').classList.add('show');
  reloadRolesList();
  setTimeout(() => $('#filter-roles').focus(), 0);
}

function closeRolesManageModal() { $('#roles-manage-modal').classList.remove('show'); }

function filterRoles(list, q) {
  if (!q) return list;
  const n = q.toLowerCase();
  return list.filter(rl =>
    (rl.name || '').toLowerCase().includes(n) ||
    (rl.descr || '').toLowerCase().includes(n) ||
    (rl.model || '').toLowerCase().includes(n) ||
    (rl.harness || '').toLowerCase().includes(n));
}

// reloadRolesList force-fetches the roles, then paints. Roles are not part of
// the 2s snapshot (they live on their own endpoint), so the overlay is
// repainted explicitly on open and after every mutation.
async function reloadRolesList() {
  const host = $('#roles-list');
  try {
    lastRoles = await loadRoles(true);
  } catch (err) {
    host.innerHTML = `<div class="template-empty">Couldn't load roles: ${esc((err && err.message) || String(err))}</div>`;
    return;
  }
  if (!$('#roles-manage-modal').classList.contains('show')) return; // closed mid-fetch
  paintRolesList();
}

function paintRolesList() {
  const host = $('#roles-list');
  const all = lastRoles;
  const q = $('#filter-roles').value;
  const list = filterRoles(all, q);
  const countEl = $('#filter-roles-count');
  if (countEl) countEl.textContent = q ? `${list.length} / ${all.length}` : `${all.length}`;
  if (!list.length) {
    host.innerHTML = `<div class="template-empty">${all.length
      ? wizWord('No roles match the filter.', 'No classes match the filter.')
      : wizWord(
        'No roles yet — press <b>+ new role</b> to define one. A role bundles a canonical brief, a default launch shape and a default permission set that a template agent references.',
        'No classes yet — press <b>+ new class</b> to define one. A class bundles a canonical calling, a default launch shape and a default set of blessings that a familiar takes on.')}</div>`;
    return;
  }
  host.innerHTML = list.map(roleCardHTML).join('');
}

function roleCardHTML(rl) {
  const summary = roleSummary(rl);
  return `<div class="template-card role-card" data-role="${esc(rl.name)}">
    <div class="tc-head">
      <span class="tc-name">${esc(rl.name)}</span>
      ${summary ? `<span class="tc-descr">${esc(summary)}</span>` : ''}
      <span class="tc-actions">
        <button class="tool" data-ract="edit" data-role="${esc(rl.name)}">edit</button>
        <button class="tool" data-ract="delete" data-role="${esc(rl.name)}">delete</button>
      </span>
    </div>
    ${rl.descr ? `<div class="tc-sub">${esc(rl.descr)}</div>` : ''}
  </div>`;
}

function rolesByName() {
  const m = {};
  for (const rl of lastRoles) m[rl.name] = rl;
  return m;
}

// ---- Role editor modal --------------------------------------------------

// populateRoleProfileSelect fills the spawn-profile <select> with the loaded
// profiles (blank = none), preserving the seed's reference even if the profile
// list hasn't loaded (a "⚠ missing" placeholder keeps a dangling ref visible).
async function populateRoleProfileSelect(current) {
  const sel = $('#role-editor-profile');
  let names = [];
  try {
    names = (await loadProfiles()).map(p => p.name);
  } catch { /* leave the list empty; the seed placeholder still shows */ }
  const opts = [`<option value="">(none)</option>`]
    .concat(names.map(n => `<option value="${esc(n)}">${esc(n)}</option>`));
  if (current && !names.includes(current)) {
    opts.push(`<option value="${esc(current)}">⚠ ${esc(current)} (missing)</option>`);
  }
  sel.innerHTML = opts.join('');
  sel.value = current || '';
}

// renderRolePerms paints the permission checklist from the snapshot's slug
// registry, checking the ones the role carries. A role's permissions are a
// plain grant LIST (not grant/deny overrides), so a simple checklist fits.
function renderRolePerms(selected) {
  const slugs = (lastSnapshot && lastSnapshot.slugs) || [];
  const set = new Set(selected || []);
  $('#role-editor-perms-list').innerHTML = slugs.map(s =>
    `<label title="${esc(s.description || '')}"><input type="checkbox" class="role-perm" data-slug="${esc(s.slug)}"${set.has(s.slug) ? ' checked' : ''} /> ${esc(s.slug)}</label>`
  ).join('');
  updateRolePermsCount();
}

function updateRolePermsCount() {
  const n = $$('#role-editor-perms-list .role-perm').filter(c => c.checked).length;
  const el = $('#role-editor-perms-count');
  if (el) el.textContent = n;
}

// openRoleEditor opens the editor, populated from `seed` (a role-shaped object,
// or null for a blank form).
function openRoleEditor(seed) {
  roleEditorEditing = seed || null;
  $('#role-editor-title').textContent = roleEditorEditing
    ? wizWord(`Edit role: ${seed.name}`, `Edit class: ${seed.name}`)
    : wizWord('New role', 'New class');
  $('#role-editor-error').textContent = '';
  $('#role-editor-name').value = seed ? (seed.name || '') : '';
  $('#role-editor-descr').value = seed ? (seed.descr || '') : '';
  $('#role-editor-brief').value = seed ? (seed.brief || '') : '';

  // Harness first — populate the selector, set it to the seed's harness (or
  // default), reshape the launch rows, then fill per-field controls.
  populateRoleHarnessSelect();
  const hSel = $('#role-editor-harness');
  if (seed && seed.harness && [...hSel.options].some(o => o.value === seed.harness)) {
    hSel.value = seed.harness;
  }
  applyRoleEditorHarness(hSel.value);

  // setModelSelectValue clears the curated <select> (dropping any injected
  // option) and keeps an out-of-catalog seed model (a full id like
  // "claude-opus-4-8[1m]") selectable rather than silently dropping it onto the
  // <select>'s prior pick — the same fix the spawn/profile editors carry.
  setModelSelectValue($('#role-editor-model'), '');
  $('#role-editor-model-codex').value = '';
  if (seed && seed.model) setModelSelectValue(roleActiveModelEl(), seed.model);
  // Reconcile the "Custom…" free-text row: the reset above left the select on a
  // concrete value, so the row hides — a seed never lands on a half-typed entry.
  syncCustomModelRow('role-editor-model');
  setSelectIfPresent($('#role-editor-effort'), seed ? seed.effort : '');
  setSelectIfPresent($('#role-editor-sandbox'), seed ? seed.sandbox : '');
  setSelectIfPresent($('#role-editor-approval'), seed ? seed.approval : '');

  populateRoleProfileSelect(seed ? seed.spawn_profile : '');
  renderRolePerms(seed ? seed.permissions : []);

  $('#role-editor-modal').classList.add('show');
  bindSelectTitles($('#role-editor-modal'));
  setTimeout(() => $('#role-editor-name').focus(), 0);
}

function closeRoleEditor() { $('#role-editor-modal').classList.remove('show'); }

// buildRolePayload assembles the full desired state from the editor. The
// server's PATCH is a full replace, so every field the role should keep must be
// present. Launch fields are gated on the chosen harness so we never post a
// value it would reject.
function buildRolePayload(name) {
  const harness = $('#role-editor-harness').value;
  const hEntry = roleHarnessByName(harness);
  const body = {
    name,
    descr: $('#role-editor-descr').value.trim(),
    brief: $('#role-editor-brief').value,
    harness,
    model: roleActiveModelEl().value.trim(),
    effort: $('#role-editor-effort').value,
    spawn_profile: $('#role-editor-profile').value.trim(),
    permissions: $$('#role-editor-perms-list .role-perm').filter(c => c.checked).map(c => c.dataset.slug),
  };
  if (hEntry && hEntry.can_sandbox && $('#role-editor-sandbox').value) {
    body.sandbox = $('#role-editor-sandbox').value;
  }
  const surfacesApproval = !!(hEntry && hEntry.can_approval
    && hEntry.approval_modes && hEntry.approval_modes.length);
  if (surfacesApproval && $('#role-editor-approval').value) {
    body.approval = $('#role-editor-approval').value;
  }
  return body;
}

async function submitRoleEditor() {
  const name = $('#role-editor-name').value.trim();
  const errEl = $('#role-editor-error');
  errEl.textContent = '';
  if (!name) { errEl.textContent = 'role name is required'; return; }
  const payload = buildRolePayload(name);
  const editing = roleEditorEditing ? roleEditorEditing.name : null;
  const btn = $('#role-editor-submit');
  btn.disabled = true;
  try {
    if (editing) await updateRole(editing, payload);
    else await createRole(payload);
    closeRoleEditor();
    toast(editing ? `role updated: ${name}` : `role created: ${name}`);
    reloadRolesList();
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    btn.disabled = false;
  }
}

async function removeRole(name) {
  const ok = await confirmModal({
    title: 'Delete role?',
    body: `Delete the role "${name}"? Roles resolve at deploy time, so this is refused while any template still references it — edit those templates to drop or repoint the reference first. A deleted canonical seed role reappears the next time the daemon opens its database.`,
    meta: name,
    okLabel: 'Delete role',
  });
  if (!ok) return;
  try {
    await deleteRole(name);
    toast(`role deleted: ${name}`);
    reloadRolesList();
  } catch (err) {
    // A 409 role_in_use carries the referencing-template list — surface it as a
    // sticky error toast so the human can go fix those templates (JOH-351).
    toast((err && err.message) || String(err), true);
  }
}

// ---- wiring -------------------------------------------------------------

function bindRolesUI() {
  $('#roles-manage-open').addEventListener('click', openRolesManageModal);
  $('#roles-manage-close').addEventListener('click', closeRolesManageModal);
  bindManageOverlayDismiss('roles-manage-modal', closeRolesManageModal);
  $('#role-create-open').addEventListener('click', () => openRoleEditor(null));

  $('#filter-roles').addEventListener('input', paintRolesList);
  $('#filter-roles-clear').addEventListener('click', () => {
    $('#filter-roles').value = '';
    paintRolesList();
    $('#filter-roles').focus();
  });

  // Role-card actions (delegated — the list re-renders on every mutation).
  $('#roles-list').addEventListener('click', e => {
    const btn = e.target.closest('[data-ract]');
    if (!btn) return;
    const name = btn.dataset.role;
    if (btn.dataset.ract === 'edit') {
      const rl = rolesByName()[name];
      if (rl) openRoleEditor(rl);
    } else if (btn.dataset.ract === 'delete') {
      removeRole(name);
    }
  });

  // Editor modal.
  $('#role-editor-cancel').addEventListener('click', closeRoleEditor);
  $('#role-editor-submit').addEventListener('click', submitRoleEditor);
  $('#role-editor-harness').addEventListener('change', (e) => {
    applyRoleEditorHarness(e.target.value);
  });
  // Picking "Custom model id…" reveals the free-text row and focuses it.
  $('#role-editor-model').addEventListener('change', () => {
    syncCustomModelRow('role-editor-model', { focus: true });
  });
  $('#role-editor-perms-list').addEventListener('change', updateRolePermsCount);
  bindBackdropDiscard('role-editor-modal', closeRoleEditor);
}

// removeRole is exported so the palette dock's card ⚙ → Delete menu item can
// reuse the manager's confirm + delete + toast (incl. the 409 role_in_use
// surfacing); the dock adds a dashboard refresh after.
export { bindRolesUI, openRoleEditor, openRolesManageModal, removeRole };
