import { $, esc, bindModalSubmitHotkey } from './helpers.js';
import { lastSnapshot } from './dashboard.js';
import { confirmModal, toast, bindBackdropDiscard, bindManageOverlayDismiss } from './refresh.js';
import { openTermModal } from './modal-term.js';

const API = '/api/sandbox-profiles';
let profiles = [];
let editingName = '';
let editorOnCreate = null;
let editorSaving = false;
let spawnPreviewGeneration = 0;
let scribePollGeneration = 0;

const SANDBOX_SCRIBE_NAME = 'sandbox-scribe';
const SANDBOX_SCRIBE_SLUGS = ['sandbox-profiles.draft'];

async function api(path, opts = {}) {
  const r = await fetch(path, { credentials: 'same-origin', ...opts });
  if (!r.ok) {
    const raw = await r.text();
    try {
      const body = JSON.parse(raw);
      throw new Error(body.message || body.error || raw || `HTTP ${r.status}`);
    } catch (err) {
      if (err instanceof SyntaxError) throw new Error(raw || `HTTP ${r.status}`);
      throw err;
    }
  }
  if (r.status === 204) return null;
  return r.json().catch(() => ({}));
}

async function loadSandboxProfiles() {
  const list = await api(API);
  profiles = Array.isArray(list) ? list : [];
  return profiles;
}

function profileOptions(blankLabel = '— none —') {
  return `<option value="">${esc(blankLabel)}</option>`
    + profiles.map(p => `<option value="${esc(p.name)}">${esc(p.name)}</option>`).join('');
}

function profileSummary(p) {
  const fs = p.filesystem || [];
  const env = p.environment || [];
  const reads = fs.filter(g => g.access === 'read').length;
  const writes = fs.filter(g => g.access === 'write').length;
  const parts = [];
  if (reads) parts.push(`${reads} read`);
  if (writes) parts.push(`${writes} write`);
  if (env.length) parts.push(`${env.length} env key${env.length === 1 ? '' : 's'}`);
  return parts.join(' · ') || 'no additive capabilities';
}

function paintSandboxProfiles() {
  const q = ($('#filter-sandbox-profiles').value || '').trim().toLowerCase();
  const shown = profiles.filter(p => !q || p.name.toLowerCase().includes(q));
  $('#filter-sandbox-profiles-count').textContent = q ? `${shown.length} / ${profiles.length}` : `${profiles.length}`;
  $('#sandbox-profiles-list').innerHTML = shown.length ? shown.map(p => `
    <div class="template-card" data-sandbox-profile="${esc(p.name)}">
      <div class="tc-head"><span class="tc-name">${esc(p.name)}</span>
        <span class="tc-descr">${esc(profileSummary(p))}</span>
        <span class="tc-actions">
          <button class="tool" data-sandbox-profile-action="edit" data-name="${esc(p.name)}">edit</button>
          <button class="tool" data-sandbox-profile-action="delete" data-name="${esc(p.name)}">delete</button>
        </span>
      </div>
      <div class="modal-meta">filesystem: ${esc((p.filesystem || []).map(g => `${g.access} ${g.path}`).join(' · ') || 'none')}<br>environment keys: ${esc((p.environment || []).map(e => e.name).join(', ') || 'none')}</div>
    </div>`).join('') : '<div class="template-empty">No sandbox profiles match.</div>';
}

async function loadAssignments() {
  const global = await api('/api/sandbox-profile-default');
  const globalSel = $('#sandbox-profile-global');
  globalSel.innerHTML = profileOptions();
  globalSel.value = (global && global.name) || '';

  const groupSel = $('#sandbox-profile-group');
  const prev = groupSel.value;
  const groups = (lastSnapshot && lastSnapshot.groups) || [];
  groupSel.innerHTML = '<option value="">— choose group —</option>'
    + groups.map(g => `<option value="${esc(g.name)}">${esc(g.name)}</option>`).join('');
  if (groups.some(g => g.name === prev)) groupSel.value = prev;
  await loadGroupAssignment();
}

async function loadGroupAssignment() {
  const group = $('#sandbox-profile-group').value;
  const sel = $('#sandbox-profile-group-value');
  sel.innerHTML = profileOptions();
  sel.disabled = !group;
  if (!group) return;
  const current = await api(`/api/groups/${encodeURIComponent(group)}/sandbox-profile`);
  sel.value = (current && current.name) || '';
}

async function openManager() {
  $('#sandbox-profiles-manage-modal').classList.add('show');
  $('#sandbox-profiles-manage-error').textContent = '';
  try {
    await loadSandboxProfiles();
    paintSandboxProfiles();
    await loadAssignments();
  } catch (err) {
    $('#sandbox-profiles-manage-error').textContent = err.message || String(err);
  }
}

function closeManager() { $('#sandbox-profiles-manage-modal').classList.remove('show'); }

function openEditor(p = null, { onCreate = null, targetName = '' } = {}) {
  editingName = targetName || (p ? p.name : '');
  editorOnCreate = editingName ? null : onCreate;
  $('#sandbox-profile-editor-title').textContent = editingName ? `Edit sandbox profile: ${editingName}` : 'New sandbox profile';
  $('#sandbox-profile-editor-name').value = p ? p.name : '';
  $('#sandbox-profile-editor-filesystem').value = JSON.stringify((p && p.filesystem) || [], null, 2);
  $('#sandbox-profile-editor-environment').value = JSON.stringify((p && p.environment) || [], null, 2);
  $('#sandbox-profile-editor-error').textContent = '';
  $('#sandbox-profile-editor-modal').classList.add('show');
  setTimeout(() => $('#sandbox-profile-editor-name').focus(), 0);
}

function closeEditor() {
  if (editorSaving) return;
  $('#sandbox-profile-editor-modal').classList.remove('show');
  editorOnCreate = null;
}

function setEditorSaving(saving) {
  editorSaving = saving;
  $('#sandbox-profile-editor-submit').disabled = saving;
  $('#sandbox-profile-editor-cancel').disabled = saving;
}

function sandboxScribeToken() {
  if (globalThis.crypto && typeof globalThis.crypto.randomUUID === 'function') {
    return globalThis.crypto.randomUUID().replaceAll('-', '');
  }
  return `${Date.now().toString(36)}${Math.random().toString(36).slice(2)}${Math.random().toString(36).slice(2)}`;
}

function sandboxScribeBrief(token, targetName, seed) {
  const target = targetName
    ? `This is a proposed replacement for the existing profile named "${targetName}".`
    : 'This is a proposed new sandbox profile.';
  return [
    'You are a sandbox-profile scribe. Talk with the human to interactively design one additive filesystem/environment sandbox profile.',
    'Critical safety boundary: create a structured DRAFT only. Never create, edit, delete, assign, or apply a sandbox profile; never launch or relaunch an agent; never request sandbox-profiles.manage. You only hold sandbox-profiles.draft.',
    'Environment values are ordinary non-secret configuration. Filesystem entries are absolute-path grants with access "read" or "write". The daemon remains authoritative for canonicalization, protected paths, reserved environment variables, duplicate handling, and all other validation.',
    target,
    `Starting draft:\n${JSON.stringify(seed, null, 2)}`,
    'Discuss the desired paths, access levels, environment names/values, and profile name. Wait until the human agrees that the proposal is ready.',
    `Then write the complete profile JSON to a file and run exactly this draft handoff (add no assignment or launch commands):\n\`tclaude agent sandbox-profiles draft --token ${token} --file <path>\``,
    'That command validates and returns the draft to the dashboard; it does NOT save anything. Remind the human to preview it there and explicitly press Save.',
  ].join('\n\n');
}

async function pollSandboxScribeDraft(token, generation, targetName, onCreate) {
  const deadline = Date.now() + 30 * 60 * 1000;
  while (generation === scribePollGeneration && Date.now() < deadline) {
    try {
      const r = await fetch(`/api/sandbox-profile-drafts/${encodeURIComponent(token)}`, { credentials: 'same-origin' });
      if (r.ok) {
        const draft = await r.json();
        if (generation !== scribePollGeneration) return;
        openEditor(draft.profile, { targetName, onCreate });
        $('#sandbox-profile-editor-error').textContent = 'Agent draft loaded. Review every field, then explicitly Save sandbox profile to apply it to the library. No assignments will be changed.';
        toast('sandbox scribe draft ready — review and explicitly save');
        return;
      }
      if (r.status !== 404) throw new Error((await r.text()) || `HTTP ${r.status}`);
    } catch (err) {
      if (generation === scribePollGeneration) toast(`sandbox draft handoff failed: ${err.message || String(err)}`, true);
      return;
    }
    await new Promise(resolve => setTimeout(resolve, 1500));
  }
}

async function summonSandboxScribe(seed, targetName = '', onCreate = null) {
  const token = sandboxScribeToken();
  const generation = ++scribePollGeneration;
  closeEditor();
  try {
    const r = await fetch('/api/scribe', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        name: SANDBOX_SCRIBE_NAME,
        slugs: SANDBOX_SCRIBE_SLUGS,
        exclusive: true,
        brief: sandboxScribeBrief(token, targetName, seed),
      }),
    });
    if (!r.ok) throw new Error((await r.text()) || `HTTP ${r.status}`);
    const resp = await r.json().catch(() => ({}));
    const verb = resp.reused ? 'resumed' : 'summoned';
    if (resp.focus_mode === 'browser' && resp.focus_ws) {
      openTermModal({ wsPath: resp.focus_ws, label: SANDBOX_SCRIBE_NAME, hideConv: resp.conv_id || null });
      toast(`${verb} ${SANDBOX_SCRIBE_NAME} — opened in-browser terminal`);
    } else {
      toast(`${verb} ${SANDBOX_SCRIBE_NAME} — opening its terminal`);
    }
    void pollSandboxScribeDraft(token, generation, targetName, onCreate);
  } catch (err) {
    const message = err.message || String(err);
    openEditor(seed, { targetName, onCreate });
    $('#sandbox-profile-editor-error').textContent = `Could not summon sandbox scribe: ${message}`;
    toast(message, true);
  }
}

function summonSandboxScribeFromEditor() {
  const errEl = $('#sandbox-profile-editor-error');
  errEl.textContent = '';
  try {
    const seed = {
      name: $('#sandbox-profile-editor-name').value.trim(),
      filesystem: JSON.parse($('#sandbox-profile-editor-filesystem').value || '[]'),
      environment: JSON.parse($('#sandbox-profile-editor-environment').value || '[]'),
    };
    void summonSandboxScribe(seed, editingName, editorOnCreate);
  } catch (err) {
    errEl.textContent = `Fix the JSON before handing it to the agent: ${err.message || String(err)}`;
  }
}

async function saveEditor() {
  if (editorSaving) return;
  const errEl = $('#sandbox-profile-editor-error');
  errEl.textContent = '';
  let body;
  try {
    body = {
      name: $('#sandbox-profile-editor-name').value.trim(),
      filesystem: JSON.parse($('#sandbox-profile-editor-filesystem').value || '[]'),
      environment: JSON.parse($('#sandbox-profile-editor-environment').value || '[]'),
    };
    if (!body.name) throw new Error('name is required');
  } catch (err) {
    errEl.textContent = err.message || String(err);
    return;
  }

  // Capture the launching selector's target before the request begins. Cancel,
  // backdrop dismissal and duplicate submit stay locked until this POST/PATCH
  // settles, so another editor invocation cannot steal the callback.
  const onCreate = editorOnCreate;
  setEditorSaving(true);
  try {
    await api(editingName ? `${API}/${encodeURIComponent(editingName)}` : API, {
      method: editingName ? 'PATCH' : 'POST',
      headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
    });
  } catch (err) {
    errEl.textContent = err.message || String(err);
    return;
  } finally {
    setEditorSaving(false);
  }

  closeEditor();
  toast(`sandbox profile saved: ${body.name}`);
  if (onCreate) {
    // Only a successful create reaches this handoff; cancel or validation
    // failure leaves the assignment untouched. The launching chip picker owns
    // assignment and repaint after the handoff.
    await onCreate(body.name);
    return;
  }
  await openManager();
  await refreshSpawnSandboxProfileUI($('#agent-spawn-group').value);
}

async function removeProfile(name) {
  const ok = await confirmModal({
    title: 'Delete sandbox profile?',
    body: `Delete “${name}”? Global and group assignments to it are cleared. Running agents keep their frozen launch snapshot.`,
    meta: name, okLabel: 'Delete sandbox profile',
  });
  if (!ok) return;
  try {
    await api(`${API}/${encodeURIComponent(name)}`, { method: 'DELETE' });
    toast(`sandbox profile deleted: ${name}`);
    await openManager();
    await refreshSpawnSandboxProfileUI($('#agent-spawn-group').value);
  } catch (err) { toast(err.message || String(err), true); }
}

function composePreview(applied) {
  const fs = new Map();
  const env = new Map();
  for (const { scope, profile } of applied) {
    for (const grant of (profile.filesystem || [])) {
      const prev = fs.get(grant.path);
      fs.set(grant.path, { access: prev && prev.access === 'write' ? 'write' : grant.access, scope });
    }
    for (const entry of (profile.environment || [])) env.set(entry.name, scope);
  }
  const scopes = applied.map(x => `${x.scope}:${x.profile.name}`).join(' → ') || 'no profiles applied';
  const grants = [...fs.entries()].map(([path, v]) => `${v.access} ${path} (${v.scope})`).join(' · ');
  const keys = [...env.entries()].map(([name, scope]) => `${name} (${scope})`).join(', ');
  return `${scopes}${grants ? ` · ${grants}` : ''}${keys ? ` · env: ${keys}` : ''}`;
}

// refreshSpawnSandboxProfileUI fills the explicit human-controlled selector
// and renders a redacted global→group→explicit preview. Values are never shown;
// only environment names and filesystem grants/provenance appear.
async function refreshSpawnSandboxProfileUI(groupName = '') {
  const sel = $('#agent-spawn-sandbox-profile');
  const preview = $('#agent-spawn-sandbox-profile-preview');
  if (!sel || !preview) return;
  const generation = ++spawnPreviewGeneration;
  const selected = sel.value;
  try {
    await loadSandboxProfiles();
    if (generation !== spawnPreviewGeneration) return;
    sel.innerHTML = profileOptions('— global + group defaults only —');
    if (profiles.some(p => p.name === selected)) sel.value = selected;
    const [global, group] = await Promise.all([
      api('/api/sandbox-profile-default'),
      groupName ? api(`/api/groups/${encodeURIComponent(groupName)}/sandbox-profile`) : Promise.resolve({ name: '' }),
    ]);
    if (generation !== spawnPreviewGeneration) return;
    const byName = Object.fromEntries(profiles.map(p => [p.name, p]));
    const applied = [];
    if (global.name && byName[global.name]) applied.push({ scope: 'global', profile: byName[global.name] });
    if (group.name && byName[group.name]) applied.push({ scope: 'group', profile: byName[group.name] });
    if (sel.value && byName[sel.value]) applied.push({ scope: 'explicit', profile: byName[sel.value] });
    preview.textContent = composePreview(applied);
  } catch (err) {
    if (generation !== spawnPreviewGeneration) return;
    preview.textContent = `Could not preview sandbox policy: ${err.message || String(err)}`;
  }
}

function bindSandboxProfilesUI() {
  $('#sandbox-profiles-manage-open').addEventListener('click', openManager);
  $('#sandbox-profiles-manage-close').addEventListener('click', closeManager);
  bindManageOverlayDismiss('sandbox-profiles-manage-modal', closeManager);
  $('#filter-sandbox-profiles').addEventListener('input', paintSandboxProfiles);
  $('#sandbox-profile-create-open').addEventListener('click', () => openEditor());
  $('#sandbox-profile-scribe-open').addEventListener('click', () => summonSandboxScribe({ name: '', filesystem: [], environment: [] }));
  $('#sandbox-profiles-list').addEventListener('click', e => {
    const btn = e.target.closest('[data-sandbox-profile-action]');
    if (!btn) return;
    const p = profiles.find(x => x.name === btn.dataset.name);
    if (btn.dataset.sandboxProfileAction === 'edit' && p) openEditor(p);
    if (btn.dataset.sandboxProfileAction === 'delete') void removeProfile(btn.dataset.name);
  });
  $('#sandbox-profile-editor-cancel').addEventListener('click', closeEditor);
  $('#sandbox-profile-editor-scribe').addEventListener('click', summonSandboxScribeFromEditor);
  $('#sandbox-profile-editor-submit').addEventListener('click', saveEditor);
  bindModalSubmitHotkey($('#sandbox-profile-editor-modal'), $('#sandbox-profile-editor-submit'));
  bindBackdropDiscard('sandbox-profile-editor-modal', closeEditor, () => !editorSaving);
  $('#sandbox-profile-global').addEventListener('change', async e => {
    try {
      await api('/api/sandbox-profile-default', {
        method: e.target.value ? 'PUT' : 'DELETE',
        headers: { 'Content-Type': 'application/json' },
        body: e.target.value ? JSON.stringify({ name: e.target.value }) : undefined,
      });
      toast(e.target.value ? `global sandbox profile: ${e.target.value}` : 'global sandbox profile cleared');
      await refreshSpawnSandboxProfileUI($('#agent-spawn-group').value);
    } catch (err) { toast(err.message || String(err), true); }
  });
  $('#sandbox-profile-group').addEventListener('change', loadGroupAssignment);
  $('#sandbox-profile-group-value').addEventListener('change', async e => {
    const group = $('#sandbox-profile-group').value;
    if (!group) return;
    try {
      await api(`/api/groups/${encodeURIComponent(group)}/sandbox-profile`, {
        method: e.target.value ? 'PUT' : 'DELETE',
        headers: { 'Content-Type': 'application/json' },
        body: e.target.value ? JSON.stringify({ name: e.target.value }) : undefined,
      });
      toast(e.target.value ? `${group} sandbox profile: ${e.target.value}` : `${group} sandbox profile cleared`);
      await refreshSpawnSandboxProfileUI($('#agent-spawn-group').value);
    } catch (err) { toast(err.message || String(err), true); }
  });
  $('#agent-spawn-sandbox-profile').addEventListener('change', () => refreshSpawnSandboxProfileUI($('#agent-spawn-group').value));

}

export {
  bindSandboxProfilesUI, refreshSpawnSandboxProfileUI,
  loadSandboxProfiles, openEditor as openSandboxProfileEditor,
};
