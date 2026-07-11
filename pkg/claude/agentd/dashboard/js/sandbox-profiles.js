import { $, esc, bindModalSubmitHotkey } from './helpers.js';
import { lastSnapshot } from './dashboard.js';
import { confirmModal, toast, bindBackdropDiscard, bindManageOverlayDismiss } from './refresh.js';

const API = '/api/sandbox-profiles';
let profiles = [];
let editingName = '';
let spawnPreviewGeneration = 0;

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

function openEditor(p = null) {
  editingName = p ? p.name : '';
  $('#sandbox-profile-editor-title').textContent = p ? `Edit sandbox profile: ${p.name}` : 'New sandbox profile';
  $('#sandbox-profile-editor-name').value = p ? p.name : '';
  $('#sandbox-profile-editor-filesystem').value = JSON.stringify((p && p.filesystem) || [], null, 2);
  $('#sandbox-profile-editor-environment').value = JSON.stringify((p && p.environment) || [], null, 2);
  $('#sandbox-profile-editor-error').textContent = '';
  $('#sandbox-profile-editor-modal').classList.add('show');
  setTimeout(() => $('#sandbox-profile-editor-name').focus(), 0);
}

function closeEditor() { $('#sandbox-profile-editor-modal').classList.remove('show'); }

async function saveEditor() {
  const errEl = $('#sandbox-profile-editor-error');
  errEl.textContent = '';
  try {
    const body = {
      name: $('#sandbox-profile-editor-name').value.trim(),
      filesystem: JSON.parse($('#sandbox-profile-editor-filesystem').value || '[]'),
      environment: JSON.parse($('#sandbox-profile-editor-environment').value || '[]'),
    };
    if (!body.name) throw new Error('name is required');
    await api(editingName ? `${API}/${encodeURIComponent(editingName)}` : API, {
      method: editingName ? 'PATCH' : 'POST',
      headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
    });
    closeEditor();
    toast(`sandbox profile saved: ${body.name}`);
    await openManager();
    await refreshSpawnSandboxProfileUI($('#agent-spawn-group').value);
  } catch (err) {
    errEl.textContent = err.message || String(err);
  }
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
  $('#sandbox-profiles-list').addEventListener('click', e => {
    const btn = e.target.closest('[data-sandbox-profile-action]');
    if (!btn) return;
    const p = profiles.find(x => x.name === btn.dataset.name);
    if (btn.dataset.sandboxProfileAction === 'edit' && p) openEditor(p);
    if (btn.dataset.sandboxProfileAction === 'delete') void removeProfile(btn.dataset.name);
  });
  $('#sandbox-profile-editor-cancel').addEventListener('click', closeEditor);
  $('#sandbox-profile-editor-submit').addEventListener('click', saveEditor);
  bindModalSubmitHotkey($('#sandbox-profile-editor-modal'), $('#sandbox-profile-editor-submit'));
  bindBackdropDiscard('sandbox-profile-editor-modal', closeEditor);
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

  // Assignment belongs next to the scope it affects. The global selector is
  // static; group selectors are rebuilt by the snapshot renderer, so one
  // delegated listener covers every current and future group row.
  $('#dashboard-default-sandbox-profile').addEventListener('change', e => {
    void setQuickAssignment(e.target, '', e.target.value);
  });
  $('#groups-list').addEventListener('change', e => {
    const select = e.target.closest('[data-sandbox-profile-quick-group]');
    if (!select) return;
    void setQuickAssignment(select, select.dataset.sandboxProfileQuickGroup, select.value);
  });
}

async function setQuickAssignment(select, group, name) {
  if (select.dataset.sandboxProfileQuickPending === 'true') return;
  const previous = select.dataset.current || '';
  select.dataset.sandboxProfileQuickPending = 'true';
  select.disabled = true;
  try {
    const path = group
      ? `/api/groups/${encodeURIComponent(group)}/sandbox-profile`
      : '/api/sandbox-profile-default';
    await api(path, {
      method: name ? 'PUT' : 'DELETE',
      headers: { 'Content-Type': 'application/json' },
      body: name ? JSON.stringify({ name }) : undefined,
    });
    select.dataset.current = name;
    toast(group
      ? (name ? `${group} sandbox profile: ${name}` : `${group} sandbox profile cleared`)
      : (name ? `global sandbox profile: ${name}` : 'global sandbox profile cleared'));
    // Release the refresh guard before asking refresh() to repaint the accepted
    // assignment. While the request was in flight the guard prevented the 2s
    // poll from replacing this disabled control with a writable stale copy.
    delete select.dataset.sandboxProfileQuickPending;
    select.disabled = false;
    await refresh();
    await refreshSpawnSandboxProfileUI($('#agent-spawn-group').value);
  } catch (err) {
    select.value = previous;
    toast(err.message || String(err), true);
  } finally {
    // The global control is a static DOM node (unlike group controls, which a
    // successful refresh replaces), so it must be re-enabled explicitly.
    // Re-enabling a detached group node is harmless.
    delete select.dataset.sandboxProfileQuickPending;
    select.disabled = false;
  }
}

export { bindSandboxProfilesUI, refreshSpawnSandboxProfileUI };
