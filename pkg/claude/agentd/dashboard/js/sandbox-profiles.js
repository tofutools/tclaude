import { $, $$, esc, bindModalSubmitHotkey, pickDirectory } from './helpers.js';
import { confirmModal, toast, bindBackdropDiscard, bindManageOverlayDismiss } from './refresh.js';
import { openTermModal } from './modal-term.js';
import { wizWord } from './slop.js';

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

// profileCapabilitiesHTML renders the profile's grants as a compact, scannable
// list — one line per filesystem grant and per environment key, each tagged
// (read / write / env) and monospaced. Long paths ellipsis-truncate with the
// full value in a title tooltip, so a card never sprawls into a wrapped wall of
// text the way the old single ` · `-joined paragraph did.
function profileCapabilitiesHTML(p) {
  const rows = [];
  for (const g of (p.filesystem || [])) {
    const acc = g.access === 'write' ? 'write' : 'read';
    rows.push(`<div class="sbx-cap"><span class="sbx-cap-tag sbx-cap-${acc}">${acc}</span><span class="sbx-cap-val" title="${esc(g.path)}">${esc(g.path)}</span></div>`);
  }
  for (const e of (p.environment || [])) {
    rows.push(`<div class="sbx-cap"><span class="sbx-cap-tag sbx-cap-env">env</span><span class="sbx-cap-val" title="${esc(e.name)}">${esc(e.name)}</span></div>`);
  }
  if (!rows.length) return `<div class="sbx-caps sbx-caps-empty">no additive capabilities</div>`;
  return `<div class="sbx-caps">${rows.join('')}</div>`;
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
      ${profileCapabilitiesHTML(p)}
    </div>`).join('') : `<div class="template-empty">${esc(wizWord('No sandbox profiles match.', 'No wards match.'))}</div>`;
}

async function openManager() {
  $('#sandbox-profiles-manage-modal').classList.add('show');
  $('#sandbox-profiles-manage-error').textContent = '';
  try {
    await loadSandboxProfiles();
    paintSandboxProfiles();
  } catch (err) {
    $('#sandbox-profiles-manage-error').textContent = err.message || String(err);
  }
}

function closeManager() { $('#sandbox-profiles-manage-modal').classList.remove('show'); }

// ---- Structured filesystem / environment editors ------------------------
//
// The two <textarea>s (#…-filesystem / #…-environment) remain the single source
// of truth the save/scribe paths read — see saveEditor. The tables below are a
// live *view* over that JSON: a row edit re-serializes its table straight back
// into the textarea (rowsToFilesystemJSON / rowsToEnvironmentJSON), and opening
// the profile or blurring the raw JSON re-renders the rows from it. So the two
// representations stay in lock-step without the save code having to know the
// tables exist. Blank rows are dropped on serialize, so an empty trailing row a
// human is about to fill never lands in the profile.

function filesystemRowHTML(grant = {}) {
  const path = esc(grant.path || '');
  const write = grant.access === 'write';
  return `<div class="sbx-row" data-sbx-fs-row>
    <input class="sbx-path" type="text" autocomplete="off" spellcheck="false" placeholder="~/path/to/dir or /absolute/path" value="${path}" aria-label="Directory path" />
    <button type="button" class="sbx-browse" data-sbx-browse title="Open a native directory picker on the daemon's desktop">Browse…</button>
    <select class="sbx-access" aria-label="Access level" title="read = the agent may read this tree; write = read and write">
      <option value="read"${write ? '' : ' selected'}>read</option>
      <option value="write"${write ? ' selected' : ''}>write</option>
    </select>
    <button type="button" class="sbx-del" data-sbx-del title="Remove this directory" aria-label="Remove directory">✕</button>
  </div>`;
}

function environmentRowHTML(entry = {}) {
  return `<div class="sbx-row sbx-row-env" data-sbx-env-row>
    <input class="sbx-env-name" type="text" autocomplete="off" spellcheck="false" placeholder="NAME" value="${esc(entry.name || '')}" aria-label="Variable name" />
    <input class="sbx-env-value" type="text" autocomplete="off" spellcheck="false" placeholder="value" value="${esc(entry.value || '')}" aria-label="Variable value" />
    <button type="button" class="sbx-del" data-sbx-del title="Remove this variable" aria-label="Remove variable">✕</button>
  </div>`;
}

// parseJSONArrayField returns the parsed array from a textarea, or null when the
// contents aren't a JSON array (so callers can leave the tables untouched rather
// than wiping them on a transient hand-edit typo).
function parseJSONArrayField(id) {
  try {
    const v = JSON.parse($(id).value || '[]');
    return Array.isArray(v) ? v : null;
  } catch (_) {
    return null;
  }
}

// renderFilesystemRows / renderEnvironmentRows repaint a table from its textarea
// and return whether the raw JSON was a parseable array. On invalid JSON they
// leave the current table untouched and return false, so callers can refuse to
// hide the raw editor behind stale tables.
function renderFilesystemRows() {
  const rows = parseJSONArrayField('#sandbox-profile-editor-filesystem');
  if (rows == null) return false; // invalid raw JSON — keep the current table
  $('#sandbox-profile-editor-fs-rows').innerHTML = rows.map(filesystemRowHTML).join('');
  return true;
}

function renderEnvironmentRows() {
  const rows = parseJSONArrayField('#sandbox-profile-editor-environment');
  if (rows == null) return false;
  $('#sandbox-profile-editor-env-rows').innerHTML = rows.map(environmentRowHTML).join('');
  return true;
}

// rowsToFilesystemJSON collects the table into the textarea. A path is trimmed
// and blank paths are dropped; the daemon still owns canonicalization, ~ and
// protected-root checks (so we deliberately don't pre-validate here).
function rowsToFilesystemJSON() {
  const out = [];
  for (const row of $$('#sandbox-profile-editor-fs-rows [data-sbx-fs-row]')) {
    const path = row.querySelector('.sbx-path').value.trim();
    if (!path) continue;
    out.push({ path, access: row.querySelector('.sbx-access').value });
  }
  $('#sandbox-profile-editor-filesystem').value = JSON.stringify(out, null, 2);
}

// rowsToEnvironmentJSON collects the env table. The name is trimmed; the value
// is kept verbatim (leading/trailing spaces can be meaningful). A row with an
// empty name is dropped even if it carries a value, matching the daemon's
// name-required rule.
function rowsToEnvironmentJSON() {
  const out = [];
  for (const row of $$('#sandbox-profile-editor-env-rows [data-sbx-env-row]')) {
    const name = row.querySelector('.sbx-env-name').value.trim();
    if (!name) continue;
    out.push({ name, value: row.querySelector('.sbx-env-value').value });
  }
  $('#sandbox-profile-editor-environment').value = JSON.stringify(out, null, 2);
}

async function browseFilesystemRow(pathInput, btn) {
  const errEl = $('#sandbox-profile-editor-error');
  errEl.textContent = '';
  const prevLabel = btn.textContent;
  btn.disabled = true;
  btn.textContent = 'Opening…';
  try {
    const res = await pickDirectory({ startDir: pathInput.value.trim(), title: 'Select a directory to grant the sandbox' });
    if (res.error) { errEl.textContent = res.error; return; }
    if (res.canceled) return; // dialog dismissed — leave the row as-is
    pathInput.value = res.path;
    pathInput.focus();
    rowsToFilesystemJSON();
  } finally {
    btn.disabled = false;
    btn.textContent = prevLabel;
  }
}

// setEditorAdvanced expands/collapses the raw-JSON panel. Expanding first pushes
// the current tables into the textareas (so the raw view is fresh); collapsing
// re-derives the tables from the textareas (so any hand-edit sticks). If a hand
// edit left either textarea unparseable, collapsing would hide the broken JSON
// behind stale tables that look fine — and Save (which reads the textareas) then
// fails with an error the user can't see. So a failed re-derive keeps the panel
// open and surfaces the reason instead. force skips the guard (used to reveal
// the panel on a save error, where the parse message is shown separately).
function setEditorAdvanced(open, { force = false } = {}) {
  if (open) {
    rowsToFilesystemJSON();
    rowsToEnvironmentJSON();
  } else {
    // Render both (side effects wanted) before deciding, so a table with valid
    // JSON still refreshes even when the other one blocks the collapse.
    const fsOK = renderFilesystemRows();
    const envOK = renderEnvironmentRows();
    if (!force && !(fsOK && envOK)) {
      $('#sandbox-profile-editor-error').textContent =
        `Fix the raw ${fsOK ? 'Environment' : 'Filesystem'} JSON before collapsing the advanced editor.`;
      open = true; // veto the collapse; leave the panel open on the bad JSON
    }
  }
  $('#sandbox-profile-editor-advanced').hidden = !open;
  const btn = $('#sandbox-profile-editor-advanced-toggle');
  btn.setAttribute('aria-expanded', open ? 'true' : 'false');
  btn.textContent = open ? '▾ Advanced — edit raw JSON' : '▸ Advanced — edit raw JSON';
}

function openEditor(p = null, { onCreate = null, targetName = null } = {}) {
  // An omitted target means the caller opened a saved profile directly, so
  // infer the update target from its name. Scribe handoffs always pass a
  // target explicitly: an existing name for edits, or '' for new drafts. Do
  // not replace that explicit empty target with the draft's proposed name —
  // doing so turns the subsequent create into a PATCH for a row that does not
  // exist yet.
  editingName = targetName === null ? (p ? p.name : '') : targetName;
  editorOnCreate = editingName ? null : onCreate;
  $('#sandbox-profile-editor-title').textContent = editingName
    ? wizWord(`Edit sandbox profile: ${editingName}`, `Edit ward: ${editingName}`)
    : wizWord('New sandbox profile', 'New ward');
  $('#sandbox-profile-editor-name').value = p ? p.name : '';
  $('#sandbox-profile-editor-filesystem').value = JSON.stringify((p && p.filesystem) || [], null, 2);
  $('#sandbox-profile-editor-environment').value = JSON.stringify((p && p.environment) || [], null, 2);
  setEditorAdvanced(false); // collapse the raw panel and render the tables from the JSON
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
    setEditorAdvanced(true, { force: true }); // reveal the raw JSON that failed to parse
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
    // A JSON.parse failure means a hand-edit broke a raw textarea — reveal the
    // advanced panel so the user can see and fix the JSON (it may be collapsed
    // and hidden behind stale-looking tables). Other errors (e.g. missing name)
    // are about visible fields, so leave the panel state alone.
    if (err instanceof SyntaxError) setEditorAdvanced(true, { force: true });
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

// ---- Export / import -----------------------------------------------------
//
// Export/import reuse the daemon's /api/sandbox-profiles/export|import surface
// (the loopback twins of the CLI path). Only the profiles themselves travel:
// global/group assignments live in the Groups tab, so this dialog neither
// exports (no include_assignments) nor applies (apply_assignments:false) them.
// The daemon has no import/inspect endpoint, so the preview is client-side —
// it parses the bundle and flags name clashes against the loaded list, then a
// single on-conflict policy (error/skip/overwrite) governs the whole import.

const EXPORT_FORMAT = 'tclaude-sandbox-profiles';
const EXPORT_VERSION = 1;

let importEnvelope = null;

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

function openExportModal() {
  $('#sandbox-profile-export-error').textContent = '';
  const list = $('#sandbox-profile-export-list');
  if (!profiles.length) {
    list.innerHTML = `<div class="template-empty">${esc(wizWord('No sandbox profiles to export.', 'No wards to inscribe.'))}</div>`;
  } else {
    list.innerHTML = profiles.map(p => `<label class="profile-transfer-row">
      <input type="checkbox" data-sandbox-export-name="${esc(p.name)}" checked />
      <span class="profile-transfer-main">
        <span class="profile-transfer-name">${esc(p.name)}</span>
        <span class="profile-transfer-summary">${esc(profileSummary(p))}</span>
      </span>
    </label>`).join('');
  }
  $('#sandbox-profile-export-submit').disabled = !profiles.length;
  $('#sandbox-profile-export-modal').classList.add('show');
}

function closeExportModal() { $('#sandbox-profile-export-modal').classList.remove('show'); }

function selectedExportNames() {
  return [...$('#sandbox-profile-export-list').querySelectorAll('[data-sandbox-export-name]')]
    .filter(el => el.checked)
    .map(el => el.dataset.sandboxExportName);
}

async function submitExport() {
  const errEl = $('#sandbox-profile-export-error');
  errEl.textContent = '';
  const names = selectedExportNames();
  if (!names.length) { errEl.textContent = 'select at least one sandbox profile'; return; }
  const btn = $('#sandbox-profile-export-submit');
  btn.disabled = true;
  try {
    const q = new URLSearchParams();
    names.forEach(n => q.append('name', n));
    const bundle = await api(`${API}/export?${q.toString()}`);
    downloadJSON('sandbox-profiles.json', bundle);
    closeExportModal();
    toast(`${names.length} sandbox profile${names.length === 1 ? '' : 's'} exported`);
  } catch (err) {
    errEl.textContent = err.message || String(err);
  } finally {
    btn.disabled = false;
  }
}

function resetImportPreview() {
  importEnvelope = null;
  const host = $('#sandbox-profile-import-preview');
  host.innerHTML = '';
  host.hidden = true;
  $('#sandbox-profile-import-conflict-row').style.display = 'none';
  $('#sandbox-profile-import-submit').disabled = true;
}

function openImportModal() {
  $('#sandbox-profile-import-file').value = '';
  $('#sandbox-profile-import-paste').value = '';
  $('#sandbox-profile-import-error').textContent = '';
  resetImportPreview();
  $('#sandbox-profile-import-modal').classList.add('show');
  setTimeout(() => $('#sandbox-profile-import-paste').focus(), 0);
}

function closeImportModal() { $('#sandbox-profile-import-modal').classList.remove('show'); }

async function readImportSource() {
  const fileInput = $('#sandbox-profile-import-file');
  const file = fileInput.files && fileInput.files[0];
  if (file) return (await file.text()).trim();
  return $('#sandbox-profile-import-paste').value.trim();
}

function renderImportPreview(incoming) {
  const existing = new Set(profiles.map(p => p.name));
  let conflicts = 0;
  const host = $('#sandbox-profile-import-preview');
  host.innerHTML = incoming.map(p => {
    const clash = existing.has(p.name);
    if (clash) conflicts++;
    return `<div class="profile-transfer-row${clash ? ' conflict' : ''}">
      <span class="profile-transfer-main">
        <span class="profile-transfer-name">${esc(p.name || '(unnamed)')}</span>
        <span class="profile-transfer-summary">${esc(profileSummary(p))}</span>
        ${clash ? '<span class="profile-transfer-note">already exists locally</span>' : ''}
      </span>
    </div>`;
  }).join('');
  host.hidden = false;
  const conflictRow = $('#sandbox-profile-import-conflict-row');
  if (conflicts) {
    conflictRow.style.display = '';
    // Default to a non-destructive policy so a bundle with clashes doesn't
    // hard-fail the whole import.
    $('#sandbox-profile-import-conflict').value = 'skip';
  } else {
    conflictRow.style.display = 'none';
  }
  $('#sandbox-profile-import-submit').disabled = false;
}

async function inspectImport() {
  const errEl = $('#sandbox-profile-import-error');
  errEl.textContent = '';
  resetImportPreview();
  const btn = $('#sandbox-profile-import-inspect');
  btn.disabled = true;
  try {
    const raw = await readImportSource();
    if (!raw) { errEl.textContent = 'pick a file or paste the sandbox-profile JSON'; return; }
    let env;
    try {
      env = JSON.parse(raw);
    } catch (e) {
      errEl.textContent = 'not valid JSON: ' + (e.message || String(e));
      return;
    }
    if (!env || env.format !== EXPORT_FORMAT || env.format_version !== EXPORT_VERSION) {
      errEl.textContent = `not a tclaude sandbox-profile export (format=${JSON.stringify(env && env.format)}, version=${env && env.format_version})`;
      return;
    }
    const incoming = Array.isArray(env.profiles) ? env.profiles : [];
    if (!incoming.length) {
      const host = $('#sandbox-profile-import-preview');
      host.innerHTML = `<div class="template-empty">${esc(wizWord('The bundle contains no sandbox profiles.', 'The scroll bears no wards.'))}</div>`;
      host.hidden = false;
      return;
    }
    importEnvelope = env;
    renderImportPreview(incoming);
  } catch (err) {
    errEl.textContent = err.message || String(err);
  } finally {
    btn.disabled = false;
  }
}

async function submitImport() {
  const errEl = $('#sandbox-profile-import-error');
  errEl.textContent = '';
  if (!importEnvelope) { errEl.textContent = 'preview the import first'; return; }
  const conflictRow = $('#sandbox-profile-import-conflict-row');
  const onConflict = conflictRow.style.display === 'none' ? 'error' : $('#sandbox-profile-import-conflict').value;
  const btn = $('#sandbox-profile-import-submit');
  btn.disabled = true;
  try {
    const res = await api(`${API}/import`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ...importEnvelope, on_conflict: onConflict, apply_assignments: false }),
    });
    closeImportModal();
    const imported = (res && res.imported) || [];
    const skipped = (res && res.skipped) || [];
    let msg = `${imported.length} sandbox profile${imported.length === 1 ? '' : 's'} imported`;
    if (skipped.length) msg += `, ${skipped.length} skipped`;
    toast(msg);
    await openManager();
    await refreshSpawnSandboxProfileUI($('#agent-spawn-group').value);
  } catch (err) {
    errEl.textContent = err.message || String(err);
  } finally {
    btn.disabled = false;
  }
}

function bindSandboxProfilesUI() {
  $('#sandbox-profiles-manage-open').addEventListener('click', openManager);
  $('#sandbox-profiles-manage-close').addEventListener('click', closeManager);
  bindManageOverlayDismiss('sandbox-profiles-manage-modal', closeManager);
  $('#filter-sandbox-profiles').addEventListener('input', paintSandboxProfiles);
  $('#sandbox-profile-create-open').addEventListener('click', () => openEditor());
  $('#sandbox-profile-scribe-open').addEventListener('click', () => summonSandboxScribe({ name: '', filesystem: [], environment: [] }));
  $('#sandbox-profile-export-open').addEventListener('click', openExportModal);
  $('#sandbox-profile-export-cancel').addEventListener('click', closeExportModal);
  $('#sandbox-profile-export-submit').addEventListener('click', submitExport);
  bindBackdropDiscard('sandbox-profile-export-modal', closeExportModal);
  $('#sandbox-profile-import-open').addEventListener('click', openImportModal);
  $('#sandbox-profile-import-cancel').addEventListener('click', closeImportModal);
  $('#sandbox-profile-import-inspect').addEventListener('click', inspectImport);
  $('#sandbox-profile-import-submit').addEventListener('click', submitImport);
  bindBackdropDiscard('sandbox-profile-import-modal', closeImportModal);
  $('#sandbox-profiles-list').addEventListener('click', e => {
    const btn = e.target.closest('[data-sandbox-profile-action]');
    if (!btn) return;
    const p = profiles.find(x => x.name === btn.dataset.name);
    if (btn.dataset.sandboxProfileAction === 'edit' && p) openEditor(p);
    if (btn.dataset.sandboxProfileAction === 'delete') void removeProfile(btn.dataset.name);
  });
  // Structured filesystem table: row edits re-serialize into the textarea; the
  // ✕ removes a row; Browse… opens the native picker for that row.
  const fsRows = $('#sandbox-profile-editor-fs-rows');
  fsRows.addEventListener('input', rowsToFilesystemJSON);
  fsRows.addEventListener('change', rowsToFilesystemJSON);
  fsRows.addEventListener('click', e => {
    const del = e.target.closest('[data-sbx-del]');
    if (del) { del.closest('.sbx-row').remove(); rowsToFilesystemJSON(); return; }
    const browse = e.target.closest('[data-sbx-browse]');
    if (browse) { void browseFilesystemRow(browse.closest('.sbx-row').querySelector('.sbx-path'), browse); }
  });
  $('#sandbox-profile-editor-fs-add').addEventListener('click', () => {
    fsRows.insertAdjacentHTML('beforeend', filesystemRowHTML());
    fsRows.lastElementChild.querySelector('.sbx-path').focus();
  });

  // Structured environment table: same wiring, minus the picker.
  const envRows = $('#sandbox-profile-editor-env-rows');
  envRows.addEventListener('input', rowsToEnvironmentJSON);
  envRows.addEventListener('change', rowsToEnvironmentJSON);
  envRows.addEventListener('click', e => {
    const del = e.target.closest('[data-sbx-del]');
    if (del) { del.closest('.sbx-row').remove(); rowsToEnvironmentJSON(); }
  });
  $('#sandbox-profile-editor-env-add').addEventListener('click', () => {
    envRows.insertAdjacentHTML('beforeend', environmentRowHTML());
    envRows.lastElementChild.querySelector('.sbx-env-name').focus();
  });

  // Advanced raw-JSON panel: the toggle folds the tables into JSON / back; a
  // hand-edit re-flows into the tables on blur.
  $('#sandbox-profile-editor-advanced-toggle').addEventListener('click', () => {
    setEditorAdvanced($('#sandbox-profile-editor-advanced').hidden);
  });
  $('#sandbox-profile-editor-filesystem').addEventListener('change', renderFilesystemRows);
  $('#sandbox-profile-editor-environment').addEventListener('change', renderEnvironmentRows);

  $('#sandbox-profile-editor-cancel').addEventListener('click', closeEditor);
  $('#sandbox-profile-editor-scribe').addEventListener('click', summonSandboxScribeFromEditor);
  $('#sandbox-profile-editor-submit').addEventListener('click', saveEditor);
  bindModalSubmitHotkey($('#sandbox-profile-editor-modal'), $('#sandbox-profile-editor-submit'));
  bindBackdropDiscard('sandbox-profile-editor-modal', closeEditor, () => !editorSaving);
  $('#agent-spawn-sandbox-profile').addEventListener('change', () => refreshSpawnSandboxProfileUI($('#agent-spawn-group').value));

}

export {
  bindSandboxProfilesUI, refreshSpawnSandboxProfileUI,
  loadSandboxProfiles, openEditor as openSandboxProfileEditor,
};
