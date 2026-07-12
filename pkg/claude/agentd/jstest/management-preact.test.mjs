import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const catalog = [{ name: 'claude', display_name: 'Claude Code', models: ['sonnet'], effort_levels: ['low', 'high'], can_sandbox: true, sandbox_modes: ['inherit', 'on'], default_sandbox: 'inherit', can_approval: true, approval_modes: ['inherit', 'plan'], default_approval: 'inherit', can_ask_timeout: true, ask_timeout_modes: ['inherit', '60s'], default_ask_timeout: 'inherit', can_remote_control: true }, { name: 'codex', models: [], can_sandbox: true, sandbox_modes: ['workspace-write'], default_sandbox: 'workspace-write', can_approval: false, can_remote_control: false }];

test('management model preserves full-replace profile and role semantics', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/management-model.js');
  const original = { name: 'old', harness: 'codex', approval: 'never', auto_review: true, model: 'gpt-5' };
  const draft = model.profileDraft(original, {}, catalog); draft.name = 'renamed'; draft.trust_dir = '1';
  const payload = model.profilePayload(draft, original, catalog);
  assert.equal(payload.name, 'renamed'); assert.equal(payload.approval, 'never'); assert.equal(payload.auto_review, true); assert.equal(payload.trust_dir, true);
  draft.harness = 'claude'; draft.approval = 'plan'; draft.sandbox = 'on';
  const switched = model.profilePayload(draft, original, catalog);
  assert.equal(switched.approval, 'plan'); assert.equal(switched.auto_review, undefined); assert.equal(switched.trust_dir, undefined);
  const role = model.roleDraft({ name: 'reviewer', permissions: ['read'] }, catalog);
  assert.deepEqual(model.rolePayload(role, catalog).permissions, ['read']);
  const defaults = model.profileDraft(null, {}, catalog); assert.equal(defaults.sandbox, 'inherit'); assert.equal(defaults.approval, 'inherit'); assert.equal(defaults.ask_user_question_timeout, 'inherit');
  assert.deepEqual(model.harnessDefaults({ sandbox_modes: ['on'], approval_modes: ['plan'], ask_timeout_modes: ['60s'] }), { sandbox: 'on', approval: 'plan', ask_user_question_timeout: '60s' });
});

test('management actions reject stale loads and expose mutation failures', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { createManagementActions }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-actions.js'),
  ]);
  const state = createManagementState(); const pending = [];
  const actions = createManagementActions({ state, confirm: async () => true, notify() {}, profileAPI: { loadProfiles: () => new Promise((resolve) => pending.push(resolve)), createProfile: async () => { throw new Error('duplicate'); } } });
  const first = actions.load('profiles'); const second = actions.load('profiles'); pending[1]([{ name: 'new' }]); await second; pending[0]([{ name: 'old' }]); await first;
  assert.deepEqual(state.profiles.value, [{ name: 'new' }]);
  const ok = await actions.saveProfile({ draft: { name: 'x' }, original: null, options: {}, payload: { name: 'x' } });
  assert.equal(ok, false); assert.equal(state.error.value, 'duplicate');
});

test('management island renders keyed profile list and explicit editor state', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState(); state.profilesRequest.commitRequest(state.profilesRequest.beginRequest(), [{ name: 'one', model: 'sonnet' }]); state.openManager('profiles');
  const actions = { load() {}, openProfileEditor(seed = null, options = {}) { state.openDialog({ kind: 'profile-editor', seed, options, catalog }); }, openRoleEditor() {}, removeProfile() {}, removeRole() {}, openManager() {}, saveProfile() {} };
  const cleanups = []; const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  mountManagementIsland({ host, state, actions, confirmDiscard: async () => false, openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); } });
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelectorAll('.profile-card').length, 1);
  host.querySelector('.profile-card button').click(); await harness.act(() => Promise.resolve());
  const input = host.querySelector('#profile-editor-name'); assert.equal(input.value, 'one'); assert.match(input.placeholder, /profile name/);
  const model = host.querySelector('#profile-editor-model'); assert.equal(model.tagName, 'SELECT'); assert.ok([...model.options].some((option) => option.value === 'sonnet'));
  const askTimeout = host.querySelector('#profile-editor-ask-timeout'); assert.equal([...askTimeout.options].find((option) => option.value === 'inherit').textContent.includes('recommended'), true);
  assert.match([...host.querySelectorAll('.cron-create-row input')].find((field) => field.placeholder?.includes('names the spawned agent')).placeholder, /names the spawned agent/);
  input.value = 'changed'; input.dispatchEvent(new harness.window.Event('input', { bubbles: true })); await harness.act(() => Promise.resolve());
  const escape = new harness.window.Event('keydown', { bubbles: true }); Object.defineProperty(escape, 'key', { value: 'Escape' }); harness.document.dispatchEvent(escape); await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#profile-editor-modal'), 'discard rejection keeps the topmost editor open');
  assert.ok(host.querySelector('#profiles-manage-modal'), 'Escape does not also close the underlying manager');
  host.querySelector('#profile-editor-modal').dispatchEvent(new harness.window.Event('mousedown', { bubbles: true })); await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#profile-editor-modal'), 'discard rejection keeps dirty editor open');
  cleanups.reverse().forEach((fn) => fn()); assert.equal(host.childElementCount, 0);
});

test('sandbox actions preserve dry-run, canonical commit, delete, and import boundaries', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { createManagementActions }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-actions.js'),
  ]);
  const state = createManagementState(); const calls = []; let refreshed = 0;
  const sandboxAPI = {
    loadSandboxProfiles: async () => [{ name: 'safe' }],
    previewSandboxProfile: async (name, body) => { calls.push(['preview', name, body]); return { before: null, after: body, revision: 'r1' }; },
    saveSandboxProfile: async (...args) => { calls.push(['save', ...args]); }, deleteSandboxProfile: async (name) => calls.push(['delete', name]),
    inspectSandboxImport: async (value) => ({ profiles: value.profiles }), importSandboxProfiles: async (...args) => { calls.push(['import', ...args]); return {}; },
  };
  const actions = createManagementActions({ state, confirm: async () => true, notify() {}, refreshSandboxSpawn: async () => { refreshed += 1; }, sandboxAPI });
  const draft = { name: 'safe', filesystem: [{ path: '/tmp', access: 'write' }], environment: [], includes: ['base'], agent_directories: ['GOCACHE'] };
  assert.equal(await actions.saveSandbox({ draft, original: null }), true);
  assert.deepEqual(calls[0], ['preview', '', draft]); assert.deepEqual(calls[1], ['save', '', draft, 'r1']); assert.equal(refreshed, 1);
  const replacement = { ...draft, name: 'renamed' }; await actions.saveSandbox({ draft: replacement, original: replacement, options: { targetName: 'safe' } });
  assert.deepEqual(calls[2], ['preview', 'safe', replacement]); assert.deepEqual(calls[3], ['save', 'safe', replacement, 'r1']);
  await actions.removeSandbox('safe'); assert.deepEqual(calls.find((call) => call[0] === 'delete'), ['delete', 'safe']);
  await actions.importSandboxBundle({ profiles: [draft] }, 'skip'); assert.equal(calls.find((call) => call[0] === 'import')[2], 'skip');
});

test('sandbox editor owns nested rows, raw validation, dirty discard, and save-in-flight state', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState(); state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [{ name: 'base' }, { name: 'dev' }]); state.openDialog({ kind: 'sandbox-editor', seed: { name: 'dev', filesystem: [], environment: [], includes: [], agent_directories: [] }, options: {} });
  let saved = null; let scribe = null; const actions = { saveSandbox(value) { saved = value; }, configureSandboxWithAgent(value, options) { scribe = { value, options }; }, async inspectDirectories() { return { missing: ['/cache'], creatable: ['/cache'] }; }, async createDirectories() { return { created: ['/cache'] }; } };
  const cleanups = []; const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  mountManagementIsland({ host, state, actions, confirmDiscard: async () => false, openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); } }); await harness.act(() => Promise.resolve());
  assert.match(host.querySelector('#sandbox-profile-editor-modal .cron-create-row input').placeholder, /shared-build-caches/);
  host.querySelector('.sbx-section .sbx-add-row').click(); await harness.act(() => Promise.resolve());
  const path = host.querySelector('.sbx-path'); path.value = '/cache'; path.dispatchEvent(new harness.window.Event('input', { bubbles: true })); await harness.act(() => Promise.resolve());
  assert.equal(harness.document.activeElement === path || path.value === '/cache', true);
  host.querySelector('.sbx-include-add').click(); host.querySelector('.sbx-agent-add').click(); await harness.act(() => Promise.resolve());
  const include = [...host.querySelectorAll('.sbx-section select')].at(-1); include.querySelector('option[value="base"]').selected = true; include.dispatchEvent(new harness.window.Event('change', { bubbles: true })); const agentDir = host.querySelector('.sbx-agent-name'); agentDir.value = 'GOCACHE'; agentDir.dispatchEvent(new harness.window.Event('input', { bubbles: true })); await harness.act(() => Promise.resolve());
  state.busy.value = 'sandbox-save'; await harness.act(() => Promise.resolve()); assert.equal(host.querySelector('#sandbox-profile-editor-modal .modal-buttons button').disabled, true);
  state.busy.value = ''; await harness.act(() => Promise.resolve());
  host.querySelector('.sbx-advanced-toggle').click(); await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('.sbx-section').hidden, true, 'structured fields are unavailable while raw JSON is authoritative');
  assert.equal(host.querySelector('#sandbox-profile-editor-includes').value.includes('base'), true); assert.equal(host.querySelector('#sandbox-profile-editor-agent-directories').value.includes('GOCACHE'), true);
  const raw = host.querySelector('.sbx-advanced-body textarea'); raw.value = '{'; raw.dispatchEvent(new harness.window.Event('input', { bubbles: true })); await harness.act(() => Promise.resolve());
  host.querySelector('#sandbox-profile-editor-modal .primary').click(); await harness.act(() => Promise.resolve());
  assert.match(host.querySelector('[role="alert"]').textContent, /JSON|position|property/i); assert.equal(saved, null);
  host.querySelector('#sandbox-profile-editor-scribe').click(); await harness.act(() => Promise.resolve()); assert.equal(scribe, null); assert.ok(host.querySelector('#sandbox-profile-editor-modal'), 'invalid raw JSON blocks scribe handoff');
  raw.value = '[{"path":"/raw","access":"read"}]'; raw.dispatchEvent(new harness.window.Event('input', { bubbles: true })); await harness.act(() => Promise.resolve()); host.querySelector('#sandbox-profile-editor-scribe').click(); await harness.act(() => Promise.resolve());
  assert.equal(scribe.value.filesystem[0].path, '/raw'); assert.equal(scribe.options.targetName, 'dev'); assert.equal(host.querySelector('#sandbox-profile-editor-modal'), null, 'scribe handoff closes the editor so its returned draft can be delivered');
  cleanups.reverse().forEach((fn) => fn());
});

test('role editor preserves missing profile references and nested permission focus', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState(); state.openDialog({ kind: 'role-editor', seed: { name: 'reviewer', spawn_profile: 'removed-profile', permissions: ['read'] }, catalog, slugs: [{ slug: 'read' }, { slug: 'write', description: 'write things' }] });
  let saved = null; const actions = { async saveRole(value) { saved = value; state.closeDialog(); } }; const cleanups = []; const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  mountManagementIsland({ host, state, actions, confirmDiscard: async () => true, openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); } }); await harness.act(() => Promise.resolve());
  const profile = [...host.querySelectorAll('option')].find((option) => option.value === 'removed-profile'); assert.match(profile.textContent, /missing/);
  assert.match(host.querySelector('#role-editor-name').placeholder, /reviewer/); assert.equal(host.querySelector('#role-editor-model').tagName, 'SELECT'); assert.ok([...host.querySelector('#role-editor-harness').options].some((option) => option.value === 'claude'));
  const write = [...host.querySelectorAll('.ta-perms-list input')][1]; write.focus(); write.checked = true; write.dispatchEvent(new harness.window.Event('change', { bubbles: true })); await harness.act(() => Promise.resolve());
  assert.equal(harness.document.activeElement, write); assert.match(host.querySelector('.cron-create-label').parentElement.parentElement.textContent, /reviewer|Role/i);
  host.querySelector('#role-editor-modal .primary').click(); await harness.act(() => Promise.resolve()); assert.ok(saved); assert.deepEqual(saved.payload.permissions, ['read', 'write']);
  cleanups.reverse().forEach((fn) => fn());
});
