// Sandbox-profile compatibility and spawn-dialog integration. The management
// list/editor/import/export DOM is Preact-owned by management-island.js; this
// module retains the independent launch-policy preview and scribe boundary.
import { $, esc, syncSelectTitle } from './helpers.js';
import { toast } from './refresh.js';
import { openTermModal } from './terminals-tab.js';
import { createSandboxDraftQueue } from './sandbox-draft-queue.js';
import { managementController } from './management-controller.js';

const API = '/api/sandbox-profiles';
const SANDBOX_SCRIBE_NAME = 'sandbox-scribe';
const SANDBOX_SCRIBE_SLUGS = ['sandbox-profiles.draft'];
let profiles = [];
let spawnPreviewGeneration = 0;

async function api(path, options = {}) {
  const response = await fetch(path, { credentials: 'same-origin', ...options });
  if (!response.ok) {
    const raw = await response.text();
    try { const body = JSON.parse(raw); throw new Error(body.message || body.error || raw || `HTTP ${response.status}`); }
    catch (error) { if (error instanceof SyntaxError) throw new Error(raw || `HTTP ${response.status}`); throw error; }
  }
  if (response.status === 204) return null;
  return response.json().catch(() => ({}));
}

async function loadSandboxProfiles() {
  const list = await api(API); profiles = Array.isArray(list) ? list : []; return profiles;
}

function profileOptions(blankLabel = '— none —') {
  return `<option value="">${esc(blankLabel)}</option>` + profiles.map((profile) => `<option value="${esc(profile.name)}">${esc(profile.name)}</option>`).join('');
}

function flattenProfilePreview(profile, byName, state) {
  const filesystem = new Map(); const environment = new Map(); const owned = new Map();
  state.onPath.add(profile.name);
  for (const name of profile.includes || []) {
    if (state.onPath.has(name)) { state.problems.add(name); continue; }
    let flat = state.memo.get(name);
    if (!flat) { const included = byName[name]; if (!included) { state.problems.add(name); continue; } flat = flattenProfilePreview(included, byName, state); state.memo.set(name, flat); }
    for (const [path, access] of flat.filesystem) filesystem.set(path, access);
    for (const name of flat.environment.keys()) { owned.delete(name); environment.set(name, true); }
    for (const name of flat.owned.keys()) { environment.delete(name); owned.set(name, true); }
  }
  state.onPath.delete(profile.name);
  for (const grant of profile.filesystem || []) filesystem.set(grant.path, grant.access);
  for (const entry of profile.environment || []) { owned.delete(entry.name); environment.set(entry.name, true); }
  for (const name of profile.agent_directories || []) { environment.delete(name); owned.set(name, true); }
  return { filesystem, environment, owned };
}

function composePreview(applied, byName = {}) {
  const filesystem = new Map(); const environment = new Map(); const owned = new Map(); const state = { memo: new Map(), onPath: new Set(), problems: new Set() };
  for (const { scope, profile } of applied) {
    const flat = flattenProfilePreview(profile, byName, state);
    for (const [path, access] of flat.filesystem) {
      const previous = filesystem.get(path); const rank = { read: 0, write: 1, deny: 2 };
      if (!previous || rank[access] >= rank[previous.access]) filesystem.set(path, { access, scope });
    }
    for (const name of flat.environment.keys()) environment.set(name, scope);
    for (const name of flat.owned.keys()) owned.set(name, scope);
  }
  const scopes = applied.map((item) => `${item.scope}:${item.profile.name}`).join(' → ') || 'no profiles applied';
  const grants = [...filesystem].map(([path, value]) => `${value.access} ${path} (${value.scope})`).join(' · ');
  const keys = [...environment].map(([name, scope]) => `${name} (${scope})`).join(', ');
  const ownedKeys = [...owned].map(([name, scope]) => `${name} (${scope})`).join(', ');
  const problems = state.problems.size ? ` · ⚠ unresolved includes: ${[...state.problems].sort().join(', ')}` : '';
  return `${scopes}${grants ? ` · ${grants}` : ''}${keys ? ` · env: ${keys}` : ''}${ownedKeys ? ` · agent dirs: ${ownedKeys}` : ''}${problems}`;
}

async function refreshSpawnSandboxProfileUI(groupName = '') {
  const select = $('#agent-spawn-sandbox-profile'); const preview = $('#agent-spawn-sandbox-profile-preview');
  if (!select || !preview) return;
  const setPreview = (text) => {
    preview.textContent = text;
    const option = select.selectedOptions && select.selectedOptions[0];
    if (option) option.title = text;
    syncSelectTitle(select);
  };
  const generation = ++spawnPreviewGeneration; const selected = select.value;
  try {
    await loadSandboxProfiles(); if (generation !== spawnPreviewGeneration) return;
    select.innerHTML = profileOptions('— global + group defaults only —');
    if (profiles.some((profile) => profile.name === selected)) select.value = selected;
    const [global, group] = await Promise.all([api('/api/sandbox-profile-default'), groupName ? api(`/api/groups/${encodeURIComponent(groupName)}/sandbox-profile`) : Promise.resolve({ name: '' })]);
    if (generation !== spawnPreviewGeneration) return;
    const byName = Object.fromEntries(profiles.map((profile) => [profile.name, profile])); const applied = [];
    if (global.name && byName[global.name]) applied.push({ scope: 'global', profile: byName[global.name] });
    if (group.name && byName[group.name]) applied.push({ scope: 'group', profile: byName[group.name] });
    if (select.value && byName[select.value]) applied.push({ scope: 'explicit', profile: byName[select.value] });
    setPreview(composePreview(applied, byName));
  } catch (error) { if (generation === spawnPreviewGeneration) setPreview(`Could not preview sandbox policy: ${error.message || String(error)}`); }
}

function sandboxScribeToken() {
  if (globalThis.crypto?.randomUUID) return globalThis.crypto.randomUUID().replaceAll('-', '');
  return `${Date.now().toString(36)}${Math.random().toString(36).slice(2)}${Math.random().toString(36).slice(2)}`;
}

function sandboxScribeBrief(token, targetName, seed) {
  return [
    'You are a sandbox-profile scribe. Talk with the human to interactively design one filesystem/environment sandbox profile, including optional per-agent generated directories.',
    'Critical safety boundary: create a structured DRAFT only. Never create, edit, delete, assign, or apply a sandbox profile; never launch or relaunch an agent; never request sandbox-profiles.manage. Your purpose-specific permission is sandbox-profiles.draft.',
    'Environment values are ordinary non-secret configuration. Filesystem entries are absolute-path rules with access "read", "write", or "deny". agent_directories is an array of environment-variable names backed by isolated writable directories created at spawn. includes is an ordered array of other profiles composed first. The daemon remains authoritative for validation.',
    targetName ? `This is a proposed replacement for the existing profile named "${targetName}".` : 'This is a proposed new sandbox profile.',
    `Starting draft:\n${JSON.stringify(seed, null, 2)}`,
    'Discuss the desired paths, access levels, environment names/values, included profiles, agent-owned directory variables, and profile name. Wait until the human agrees that the proposal is ready.',
    `Then write the complete profile JSON to a file and run exactly this draft handoff:\n\`tclaude agent sandbox-profiles draft --token ${token} --file <path>\``,
    'That command validates and returns the draft to the dashboard; it does NOT save anything.',
  ].join('\n\n');
}

function openSandboxProfileEditor(seed = null, options = {}) { return managementController().openSandboxProfileEditor(seed, options); }
function openSandboxProfilesManageModal() { return managementController().openSandboxProfilesManageModal(); }

const sandboxDraftQueue = createSandboxDraftQueue({
  canDeliver: () => !document.querySelector('#sandbox-profile-editor-modal'),
  deliver: ({ draft, targetName, onCreate }) => {
    openSandboxProfileEditor(draft.profile, { targetName, onCreate, notice: 'Agent draft loaded. Review every field, then explicitly save.' });
    toast('sandbox scribe draft ready — review and explicitly save');
  },
});

async function pollSandboxScribeDraft(token, targetName, onCreate) {
  const deadline = Date.now() + 30 * 60 * 1000;
  while (Date.now() < deadline) {
    try {
      const response = await fetch(`/api/sandbox-profile-drafts/${encodeURIComponent(token)}`, { credentials: 'same-origin' });
      if (response.ok) { const draft = await response.json(); const opened = sandboxDraftQueue.enqueue({ draft, targetName, onCreate }); if (!opened) toast(`sandbox scribe draft ready — queued for review (${sandboxDraftQueue.pendingCount()} waiting)`); return; }
      if (response.status !== 404) throw new Error((await response.text()) || `HTTP ${response.status}`);
    } catch (error) { toast(`sandbox draft handoff failed: ${error.message || String(error)}`, true); return; }
    await new Promise((resolve) => setTimeout(resolve, 1500));
  }
}

async function summonSandboxScribe(seed, targetName = '', onCreate = null) {
  const token = sandboxScribeToken();
  try {
    const response = await fetch('/api/scribe', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: SANDBOX_SCRIBE_NAME, slugs: SANDBOX_SCRIBE_SLUGS, brief: sandboxScribeBrief(token, targetName, seed) }) });
    if (!response.ok) throw new Error((await response.text()) || `HTTP ${response.status}`);
    const result = await response.json().catch(() => ({})); const name = result.name || SANDBOX_SCRIBE_NAME;
    if (result.focus_mode === 'browser' && result.focus_ws) openTermModal({ wsPath: result.focus_ws, label: name, hideConv: result.conv_id || null });
    toast(`summoned ${name}${result.focus_mode === 'browser' ? ' — opened in-browser terminal' : ' — opening its terminal'}`); void pollSandboxScribeDraft(token, targetName, onCreate);
  } catch (error) { openSandboxProfileEditor(seed, { targetName, onCreate, notice: `Could not summon sandbox scribe: ${error.message || String(error)}` }); toast(error.message || String(error), true); }
}

function bindSandboxProfilesUI() {
  $('#sandbox-profiles-manage-open').addEventListener('click', openSandboxProfilesManageModal);
  $('#agent-spawn-sandbox-profile').addEventListener('change', () => refreshSpawnSandboxProfileUI($('#agent-spawn-group').value));
}

export { bindSandboxProfilesUI, refreshSpawnSandboxProfileUI, loadSandboxProfiles, openSandboxProfilesManageModal, openSandboxProfileEditor, summonSandboxScribe };
