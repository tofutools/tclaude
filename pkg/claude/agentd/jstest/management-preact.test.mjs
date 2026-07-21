import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const catalog = [{ name: 'claude', display_name: 'Claude Code', models: ['sonnet'], effort_levels: ['low', 'high'], can_sandbox: true, sandbox_modes: ['inherit', 'on'], default_sandbox: 'inherit', can_approval: true, approval_modes: ['inherit', 'plan'], default_approval: 'inherit', approval_mode_help: { inherit: 'keep settings', plan: 'plan only' }, can_auto_review: false, can_ask_timeout: true, ask_timeout_modes: ['inherit', '60s'], default_ask_timeout: 'inherit', can_remote_control: true, can_auto_memory: true }, { name: 'codex', models: [], can_sandbox: true, sandbox_modes: ['workspace-write'], default_sandbox: 'workspace-write', can_approval: true, approval_modes: ['never', 'untrusted', 'on-failure', 'on-request'], default_approval: 'never', approval_mode_help: { never: 'never prompt', untrusted: 'ask for untrusted', 'on-failure': 'deprecated retry', 'on-request': 'ask when requested' }, can_auto_review: true, can_remote_control: false, can_auto_memory: false }];

function choose(select, value) {
  for (const option of select.options) {
    if (option.value === value) option.setAttribute('selected', '');
    else option.removeAttribute('selected');
  }
  Object.defineProperty(select, 'value', { configurable: true, writable: true, value });
}

function selectedValue(select) {
  return select.getAttribute('value')
    ?? Array.from(select.options).find((option) => option.selected)?.value
    ?? select.value
    ?? '';
}

test('management model preserves full-replace profile and role semantics', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/management-model.js');
  const original = { name: 'old', aliases: ['codex-reviewer'], harness: 'codex', approval: 'never', auto_review: true, model: 'gpt-5', disabled: false, disabled_reason: 'previous outage' };
  const draft = model.profileDraft(original, {}, catalog); draft.name = 'renamed'; draft.trust_dir = '1';
  assert.equal(draft.approval_reviewer, 'auto_review');
  draft.aliases_text += ', cold-reviewer';
  const payload = model.profilePayload(draft, original, catalog);
  assert.equal(payload.name, 'renamed'); assert.equal(payload.approval, 'never'); assert.equal(payload.auto_review, true); assert.equal(payload.trust_dir, true);
  assert.equal(payload.disabled, false); assert.equal(payload.disabled_reason, 'previous outage');
  draft.approval_reviewer = 'human'; assert.equal(model.profilePayload(draft, original, catalog).auto_review, false);
  assert.deepEqual(payload.aliases, ['codex-reviewer', 'cold-reviewer']);
  draft.harness = 'claude'; draft.approval = 'plan'; draft.sandbox = 'on';
  const switched = model.profilePayload(draft, original, catalog);
  assert.equal(switched.approval, 'plan'); assert.equal(switched.auto_review, undefined); assert.equal(switched.trust_dir, undefined);
  const role = model.roleDraft({ name: 'reviewer', permissions: ['read'] }, catalog);
  assert.deepEqual(model.rolePayload(role, catalog).permissions, ['read']);
  const defaults = model.profileDraft(null, {}, catalog); assert.equal(defaults.sandbox, 'inherit'); assert.equal(defaults.approval, 'inherit'); assert.equal(defaults.ask_user_question_timeout, 'inherit');
  const legacyCodex = model.profileDraft({ name: 'legacy', harness: 'codex', approval: '' }, {}, catalog);
  assert.equal(legacyCodex.approval, 'never', 'an empty legacy Codex profile renders the explicit daemon default');
  const legacyPayload = model.profilePayload(legacyCodex, { name: 'legacy', harness: 'codex', approval: '' }, catalog);
  assert.equal(legacyPayload.approval, 'never');
  assert.equal('auto_review' in legacyPayload, false, 'unset reviewer stays sparse for lower-tier resolution');
  assert.deepEqual(model.harnessDefaults({ sandbox_modes: ['on'], approval_modes: ['plan'], ask_timeout_modes: ['60s'] }), { sandbox: 'on', approval: 'plan', approval_reviewer: '', ask_user_question_timeout: '60s' });
});

test('Codex profile permission modes populate, survive harness switches, save, and reopen', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'profile-editor', seed: { name: 'legacy', harness: 'codex', approval: '', auto_review: true }, options: {}, catalog });
  const saves = []; const cleanups = []; const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  mountManagementIsland({ host, state, actions: { async saveProfile(value) { saves.push(value); } }, confirmDiscard: async () => true, openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); } });
  await harness.act(() => Promise.resolve());

  const approval = harness.getByLabelText(host, /^Approval policy/);
  assert.deepEqual([...approval.options].map((option) => option.value), ['never', 'untrusted', 'on-failure', 'on-request']);
  assert.match(approval.options[0].textContent, /Never ask — no approval prompts/);
  assert.match(approval.options[0].textContent, /recommended/, 'empty legacy value displays an explicit effective default');
  // Mode help is collapsed behind the [?] disclosure, not printed under the
  // control: the hint id now belongs to the hidden description the button
  // reveals, and the select carries the same copy as its hover tooltip.
  assert.equal(host.querySelector('#profile-editor-approval-hint').textContent, 'never prompt');
  assert.match(host.querySelector('#profile-editor-approval-hint').getAttribute('class'), /spawn-field-description/);
  assert.equal(approval.getAttribute('title'), 'never prompt');
  assert.equal(host.querySelector('#profile-editor-approval-row .spawn-field-help-trigger').getAttribute('aria-expanded'), 'false');
  assert.equal(host.querySelector('#profile-editor-approval-caveat'), null, 'help with no ⚠ leaves nothing on screen');
  const initialReviewer = host.querySelector('#profile-editor-approval-reviewer');
  assert.deepEqual([...initialReviewer.options].map((option) => option.value), ['', 'human', 'auto_review']);
  assert.equal(selectedValue(initialReviewer), 'auto_review');
  await harness.act(() => harness.fireEvent(host.querySelector('#profile-editor-submit'), 'click'));
  assert.equal(saves[0].payload.approval, 'never');
  assert.equal(saves[0].payload.auto_review, true);

  const harnessSelect = host.querySelector('#profile-editor-harness');
  choose(harnessSelect, 'claude'); await harness.act(() => harness.fireEvent(harnessSelect, 'change'));
  assert.deepEqual([...harness.getByLabelText(host, /^Permission mode/).options].map((option) => option.value), ['inherit', 'plan']);
  assert.equal(host.querySelector('#profile-editor-approval-reviewer').closest('.cron-create-row').hidden, true);
  choose(harnessSelect, 'codex'); await harness.act(() => harness.fireEvent(harnessSelect, 'change'));
  const switchedApproval = harness.getByLabelText(host, /^Approval policy/);
  assert.deepEqual([...switchedApproval.options].map((option) => option.value), ['never', 'untrusted', 'on-failure', 'on-request']);
  assert.match(switchedApproval.options[0].textContent, /recommended/);

  choose(switchedApproval, 'untrusted'); await harness.act(() => harness.fireEvent(switchedApproval, 'change'));
  const switchedReviewer = host.querySelector('#profile-editor-approval-reviewer');
  choose(switchedReviewer, 'human'); await harness.act(() => harness.fireEvent(switchedReviewer, 'change'));
  await harness.act(() => harness.fireEvent(host.querySelector('#profile-editor-submit'), 'click'));
  assert.equal(saves.length, 2); assert.equal(saves[1].payload.approval, 'untrusted');
  assert.equal(saves[1].payload.auto_review, false);

  state.closeDialog();
  await harness.act(() => Promise.resolve());
  state.openDialog({ kind: 'profile-editor', seed: { name: 'legacy', harness: 'codex', approval: 'untrusted', auto_review: true }, options: {}, catalog });
  await harness.act(() => Promise.resolve());
  await harness.act(() => harness.fireEvent(host.querySelector('#profile-editor-submit'), 'click'));
  assert.equal(saves[2].payload.approval, 'untrusted', 'saved mode displays when reopened');
  assert.equal(saves[2].payload.auto_review, true, 'saved reviewer displays when reopened');
  cleanups.reverse().forEach((fn) => fn());
});

test('profile choices expose aliases as distinct handles tied to one profile', async (t) => {
  const harness = await createPreactHarness(t);
  const profiles = await harness.importDashboardModule('js/profiles.js');
  const list = [{ name: 'gpt5.6-sol-high', aliases: ['codex-reviewer', 'cold-reviewer'] }, { name: 'paused', aliases: [], disabled: true, disabled_reason: 'provider outage' }];
  assert.deepEqual(profiles.profileChoices(list).map(({ value, label }) => ({ value, label })), [
    { value: 'gpt5.6-sol-high', label: 'gpt5.6-sol-high' },
    { value: 'codex-reviewer', label: 'codex-reviewer → gpt5.6-sol-high' },
    { value: 'cold-reviewer', label: 'cold-reviewer → gpt5.6-sol-high' },
    { value: 'paused', label: 'paused [🚫 disabled: provider outage]' },
  ]);
  assert.equal(profiles.findProfileByHandle(list, 'codex-reviewer').name, 'gpt5.6-sol-high');
  assert.deepEqual(profiles.profileDetailChips({
		disabled: false, disabled_reason: 'previous outage',
    harness: 'claude', model: 'sonnet', effort: 'high', sandbox: 'inherit', approval: 'plan',
    ask_user_question_timeout: '5m', auto_review: false, trust_dir: true, remote_control: false,
    auto_memory: true,
    agent_name: 'worker', role: 'reviewer', descr: 'cold\nreview', initial_message: 'check this',
    sync_worktree: true, auto_focus: false, include_group_default_context: true, is_owner: false,
    permission_overrides: { 'human.notify': 'grant', 'groups.spawn': 'deny' },
  }), [
		'last disable reason · previous outage',
    'harness claude', 'model sonnet', 'effort high', 'sandbox inherit', 'approval plan',
    'ask-timeout 5m', 'auto-review off', 'trust-dir on', 'remote-control off', 'auto-memory on',
    'name worker', 'role reviewer', 'descr cold review', 'initial message · 10 chars',
    'sync-wt on', 'focus off', 'group-ctx on', 'owner off',
    'perm groups.spawn deny', 'perm human.notify grant',
  ]);
});

test('profile clone payload leaves unique aliases with the source', async (t) => {
  const harness = await createPreactHarness(t);
  const clone = await harness.importDashboardModule('js/clone-payload.js');
  assert.deepEqual(clone.clonePayload({
    name: 'original', aliases: ['codex-reviewer'], model: 'sonnet',
    created_at: 'old', updated_at: 'old',
  }, 'copy'), { name: 'copy', model: 'sonnet' });
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
  const state = createManagementState(); state.profilesRequest.commitRequest(state.profilesRequest.beginRequest(), [{ name: 'one', aliases: ['reviewer'], model: 'sonnet', disabled: true, disabled_reason: 'provider outage' }]); state.openManager('profiles');
  const actions = { load() {}, openProfileEditor(seed = null, options = {}) { state.openDialog({ kind: 'profile-editor', seed, options, catalog }); }, openRoleEditor() {}, removeProfile() {}, removeRole() {}, openManager() {}, saveProfile() {} };
  const cleanups = []; const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  mountManagementIsland({ host, state, actions, confirmDiscard: async () => false, openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); } });
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelectorAll('.profile-card').length, 1);
  assert.equal(host.querySelector('.profile-card').classList.contains('profile-card-disabled'), true);
  assert.match(host.querySelector('.tc-disabled').textContent, /🚫 Disabled/);
  assert.match(host.querySelector('.tc-aliases').textContent, /aka reviewer/);
  host.querySelector('.profile-card button').click(); await harness.act(() => Promise.resolve());
  const input = host.querySelector('#profile-editor-name'); assert.equal(input.value, 'one'); assert.match(input.placeholder, /profile name/);
  assert.equal(host.querySelector('#profile-editor-aliases').value, 'reviewer');
  assert.equal(host.querySelector('#profile-editor-disabled').hasAttribute('checked'), true);
  assert.equal(host.querySelector('#profile-editor-disabled-reason').value, 'provider outage');
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

test('profile editor saves with Ctrl/Cmd+Enter', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'profile-editor', seed: { name: 'reviewer', harness: 'claude' }, options: {}, catalog });
  const saves = [];
  const cleanups = []; const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  mountManagementIsland({ host, state, actions: { async saveProfile(value) { saves.push(value); } }, confirmDiscard: async () => true, openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); } });
  await harness.act(() => Promise.resolve());
  const dialog = host.querySelector('#profile-editor-modal [role="dialog"]');

  const plainEnter = harness.fireEvent(dialog, 'keydown', { key: 'Enter' });
  assert.equal(plainEnter.defaultPrevented, false, 'plain Enter retains the field default');
  assert.equal(saves.length, 0);

  for (const modifier of ['ctrlKey', 'metaKey']) {
    let shortcut;
    await harness.act(() => { shortcut = harness.fireEvent(dialog, 'keydown', { key: 'Enter', [modifier]: true }); });
    assert.equal(shortcut.defaultPrevented, true, `${modifier}+Enter is claimed by the editor`);
  }
  assert.equal(saves.length, 2, 'both platform shortcuts use the profile save path');
  assert.equal(saves[0].draft.name, 'reviewer');

  state.busy.value = 'profile-save'; await harness.act(() => Promise.resolve());
  harness.fireEvent(dialog, 'keydown', { key: 'Enter', ctrlKey: true });
  assert.equal(saves.length, 2, 'an in-flight save cannot be submitted twice');
  cleanups.reverse().forEach((fn) => fn());
});

test('sandbox manager renders included profiles and static environment bindings', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [{
    name: 'child', filesystem: [], environment: [{ name: 'GOCACHE', value: '/var/cache/go build' }], includes: ['base'], agent_directories: [],
  }]);
  state.openManager('sandbox');
  const actions = { load() {}, openSandboxEditor() {}, removeSandbox() {}, configureSandboxWithAgent() {} };
  const cleanups = []; const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  mountManagementIsland({ host, state, actions, confirmDiscard: async () => true, openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); } });
  await harness.act(() => Promise.resolve());
  const tag = host.querySelector('.sbx-cap-inc');
  assert.ok(tag, 'include tag uses the CSS-owned class');
  assert.equal(tag.textContent, 'include');
  assert.equal(tag.nextElementSibling.title, 'base');
  const env = host.querySelector('.sbx-cap-env');
  assert.equal(env.textContent, 'env');
  assert.equal(env.nextElementSibling.textContent, 'GOCACHE → /var/cache/go build');
  assert.equal(env.nextElementSibling.title, 'GOCACHE → /var/cache/go build');
  cleanups.reverse().forEach((fn) => fn());
});

test('profile editor Escape follows the visual stack over a later spawn dialog', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }, { isTopmostOverlay }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'), harness.importDashboardModule('js/overlay-stack.js'),
  ]);
  const state = createManagementState();
  const cleanups = []; const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  const spawn = harness.document.createElement('div'); spawn.id = 'agent-spawn-modal'; spawn.className = 'modal-overlay show'; spawn.style.zIndex = '100'; harness.document.body.appendChild(spawn);
  let spawnCloses = 0;
  const dismissSpawn = (event) => {
    if (event.key !== 'Escape' || !spawn.classList.contains('show') || !isTopmostOverlay(spawn)) return;
    event.preventDefault(); event.stopImmediatePropagation(); spawn.classList.remove('show'); spawnCloses += 1;
  };
  harness.document.addEventListener('keydown', dismissSpawn);
  let discard = false; let confirms = 0;
  mountManagementIsland({ host, state, actions: { saveProfile() {} }, confirmDiscard: async () => { confirms += 1; return discard; }, openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); } });

  const openEditor = async () => {
    state.openDialog({ kind: 'profile-editor', seed: { name: '', harness: 'claude' }, options: { editExisting: false }, catalog });
    await harness.act(() => Promise.resolve());
    host.querySelector('#profile-editor-modal').style.zIndex = '150';
  };
  const pressEscape = async () => {
    const event = new harness.window.Event('keydown', { bubbles: true }); Object.defineProperty(event, 'key', { value: 'Escape' });
    harness.document.dispatchEvent(event); await harness.act(() => Promise.resolve());
  };

  await openEditor(); await pressEscape();
  assert.equal(host.querySelector('#profile-editor-modal'), null, 'a clean editor closes even though the spawn overlay is later in the DOM');
  assert.equal(confirms, 0, 'a clean editor needs no discard confirmation');
  assert.equal(spawn.classList.contains('show'), true, 'closing the editor leaves the underlying spawn dialog open');
  assert.equal(spawnCloses, 0);

  await openEditor();
  const name = host.querySelector('#profile-editor-name'); name.value = 'new-pattern'; name.dispatchEvent(new harness.window.Event('input', { bubbles: true })); await harness.act(() => Promise.resolve());
  await pressEscape();
  assert.ok(host.querySelector('#profile-editor-modal'), 'rejecting discard keeps the dirty editor open');
  assert.equal(confirms, 1, 'a dirty editor offers discard confirmation');
  discard = true; await pressEscape();
  assert.equal(host.querySelector('#profile-editor-modal'), null, 'accepting discard closes the dirty editor');
  assert.equal(spawn.classList.contains('show'), true, 'discarding the editor still leaves the underlying spawn dialog open');
  assert.equal(spawnCloses, 0);

  harness.document.removeEventListener('keydown', dismissSpawn); cleanups.reverse().forEach((fn) => fn()); spawn.remove();
});

test('local profile editor skips its hidden autofocus field', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'profile-editor', seed: { name: 'local', harness: 'claude' }, options: { local: true }, catalog });
  const cleanups = []; const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  mountManagementIsland({ host, state, actions: { saveProfile() {} }, confirmDiscard: async () => true, openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); } });
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#profile-editor-name').closest('[hidden]') !== null, true);
  assert.equal(harness.document.activeElement, host.querySelector('#profile-editor-harness'), 'hidden autofocus fields cannot retain focus behind the modal');
  cleanups.reverse().forEach((fn) => fn());
});

test('sandbox actions preserve dry-run, canonical commit, delete, and import boundaries', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { createManagementActions }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-actions.js'),
  ]);
  const state = createManagementState(); const calls = []; let refreshed = 0; let genericConfirms = 0;
  const sandboxAPI = {
    loadSandboxProfiles: async () => [{ name: 'safe' }],
    previewSandboxProfile: async (name, body) => { calls.push(['preview', name, body]); return { before: null, after: body, revision: 'r1' }; },
    saveSandboxProfile: async (...args) => { calls.push(['save', ...args]); }, deleteSandboxProfile: async (name) => calls.push(['delete', name]),
    inspectSandboxImport: async (value) => ({ profiles: value.profiles }), importSandboxProfiles: async (...args) => { calls.push(['import', ...args]); return {}; },
  };
  const actions = createManagementActions({ state, confirm: async () => { genericConfirms += 1; return true; }, notify() {}, refreshSandboxSpawn: async () => { refreshed += 1; }, sandboxAPI });
  const draft = { name: 'safe', filesystem: [{ path: '/tmp', access: 'write' }], environment: [], includes: ['base'], agent_directories: ['GOCACHE'], network_access: 'internet' };
  // The save body always carries the full-replace shape, so the TCL-609
  // fields ride along explicitly even when untouched.
  const body = { ...draft, read_baseline: '', read_baseline_exclusions: [], break_glass_filesystem: [] };
  const create = actions.saveSandbox({ draft, original: null }); await Promise.resolve();
  assert.deepEqual(state.sandboxDiff.value, { before: null, after: body }); state.cancelSandboxDiff(true);
  assert.equal(await create, true);
  assert.deepEqual(calls[0], ['preview', '', body]); assert.deepEqual(calls[1], ['save', '', body, 'r1']); assert.equal(refreshed, 1);
  const replacement = { ...draft, name: 'renamed' }; const replacementBody = { ...body, name: 'renamed' }; const update = actions.saveSandbox({ draft: replacement, original: replacement, options: { targetName: 'safe' } }); await Promise.resolve(); state.cancelSandboxDiff(true); await update;
  assert.deepEqual(calls[2], ['preview', 'safe', replacementBody]); assert.deepEqual(calls[3], ['save', 'safe', replacementBody, 'r1']);
  assert.equal(genericConfirms, 0, 'sandbox saves use the dedicated diff instead of the generic JSON confirmation blob');
  await actions.removeSandbox('safe'); assert.deepEqual(calls.find((call) => call[0] === 'delete'), ['delete', 'safe']);
  assert.equal(genericConfirms, 1, 'ordinary destructive confirmations still use the shared prompt');
  await actions.importSandboxBundle({ profiles: [draft] }, 'skip'); assert.equal(calls.find((call) => call[0] === 'import')[2], 'skip');
});

test('sandbox import accepts the current v2 export envelope', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState(); state.openDialog({ kind: 'sandbox-import' });
  let inspected = null;
  const actions = {
    async inspectSandboxBundle(value) { inspected = value; return { profiles: value.profiles, warnings: [] }; },
    async importSandboxBundle() {},
  };
  const cleanups = []; const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  mountManagementIsland({ host, state, actions, confirmDiscard: async () => true, openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); } }); await harness.act(() => Promise.resolve());
  const envelope = { format: 'tclaude-sandbox-profiles', format_version: 2, profiles: [{ name: 'offline', network_access: 'none' }] };
  const raw = host.querySelector('#sandbox-profile-import-modal textarea'); raw.value = JSON.stringify(envelope); raw.dispatchEvent(new harness.window.Event('input', { bubbles: true })); await harness.act(() => Promise.resolve());
  const preview = [...host.querySelectorAll('#sandbox-profile-import-modal button')].find((button) => button.textContent === 'Preview'); preview.click(); await harness.act(() => Promise.resolve());
  assert.deepEqual(inspected, envelope); assert.ok(host.querySelector('#sandbox-profile-import-modal .profile-transfer-list'));
  cleanups.reverse().forEach((fn) => fn());
});

test('sandbox save preview renders a focused line diff and restores the editor on cancel', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-editor', seed: { name: 'dev', filesystem: [], environment: [], includes: [], agent_directories: [] }, options: {} });
  const actions = { async inspectDirectories() { return { missing: [], creatable: [] }; }, async createDirectories() {}, saveSandbox() {}, configureSandboxWithAgent() {} };
  const cleanups = []; const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  mountManagementIsland({ host, state, actions, confirmDiscard: async () => false, openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); } }); await harness.act(() => Promise.resolve());
  const before = { name: 'dev', filesystem: [{ path: '/cache', access: 'read' }], environment: [] };
  const after = { name: 'dev', filesystem: [{ path: '/cache', access: 'write' }], environment: [] };
  const submit = host.querySelector('#sandbox-profile-editor-submit'); submit.focus(); state.busy.value = 'sandbox-save'; await harness.act(() => Promise.resolve());
  const harnessFocus = submit.focus; Object.defineProperty(submit, 'focus', { configurable: true, value() { if (!this.disabled && !this.closest('[inert]')) harnessFocus.call(this); } });
  const decision = state.confirmSandboxDiff(before, after); await harness.act(() => Promise.resolve());
  const modal = host.querySelector('#sandbox-profile-diff-modal');
  assert.ok(modal); assert.equal(modal.querySelectorAll('.dl.add').length, 1); assert.equal(modal.querySelectorAll('.dl.del').length, 1); assert.ok(modal.querySelectorAll('.dl.ctx').length > 0);
  assert.match(modal.querySelector('#sandbox-profile-diff-sub').textContent, /1 line\(s\) added, 1 removed/);
  assert.equal(harness.document.activeElement.id, 'sandbox-profile-diff-confirm');
  const editor = host.querySelector('#sandbox-profile-editor-modal'); assert.equal(editor.inert, true); assert.equal(editor.getAttribute('aria-hidden'), 'true');
  modal.querySelector('#sandbox-profile-diff-cancel').click(); state.busy.value = ''; await harness.act(() => Promise.resolve());
  assert.equal(await decision, false); assert.equal(host.querySelector('#sandbox-profile-diff-modal'), null); assert.equal(editor.inert, false); assert.equal(editor.hasAttribute('aria-hidden'), false); assert.ok(host.querySelector('#sandbox-profile-editor-modal')); assert.equal(harness.document.activeElement, submit, 'focus returns after the editor is interactive again');
  cleanups.reverse().forEach((fn) => fn());
});

test('sandbox editor owns nested rows, raw validation, dirty discard, and save-in-flight state', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState(); state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [{ name: 'base' }, { name: 'dev' }]); state.openDialog({ kind: 'sandbox-editor', seed: { name: 'dev', filesystem: [], environment: [], includes: [], agent_directories: [], network_access: 'internet' }, options: {} });
  let saved = null; let scribe = null; const actions = { saveSandbox(value) { saved = value; }, configureSandboxWithAgent(value, options) { scribe = { value, options }; }, async inspectDirectories() { return { missing: ['/cache'], creatable: ['/cache'] }; }, async createDirectories() { return { created: ['/cache'] }; } };
  const cleanups = []; const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  mountManagementIsland({ host, state, actions, confirmDiscard: async () => false, openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); } }); await harness.act(() => Promise.resolve());
  assert.match(host.querySelector('#sandbox-profile-editor-modal .cron-create-row input').placeholder, /shared-build-caches/);
  const network = host.querySelector('#sandbox-profile-editor-network'); assert.ok(network.querySelector('option[value="internet"]')); network.querySelector('option[value="none"]').selected = true; network.dispatchEvent(new harness.window.Event('change', { bubbles: true })); await harness.act(() => Promise.resolve());
  host.querySelector('.sbx-section .sbx-add-row').click(); await harness.act(() => Promise.resolve());
  const path = host.querySelector('.sbx-path'); path.value = '/cache'; path.dispatchEvent(new harness.window.Event('input', { bubbles: true })); await harness.act(() => Promise.resolve());
  assert.equal(harness.document.activeElement === path || path.value === '/cache', true);
  host.querySelector('.sbx-include-add').click(); host.querySelector('.sbx-agent-add').click(); await harness.act(() => Promise.resolve());
  const access = host.querySelector('.sbx-access'); const include = host.querySelector('.sbx-inc-name'); assert.ok(access); assert.ok(include); assert.notEqual(access, include, 'access and included-profile selects have distinct layout contracts'); include.querySelector('option[value="base"]').selected = true; include.dispatchEvent(new harness.window.Event('change', { bubbles: true })); const agentDir = host.querySelector('.sbx-agent-name'); agentDir.value = 'GOCACHE'; agentDir.dispatchEvent(new harness.window.Event('input', { bubbles: true })); await harness.act(() => Promise.resolve());
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
  assert.equal(scribe.value.filesystem[0].path, '/raw'); assert.equal(scribe.value.network_access, 'none'); assert.equal(scribe.options.targetName, 'dev'); assert.equal(host.querySelector('#sandbox-profile-editor-modal'), null, 'scribe handoff closes the editor so its returned draft can be delivered');
  cleanups.reverse().forEach((fn) => fn());
});

test('sandbox editor renders semantic restrictions, preserves unknown IDs, and locks leaves covered by Home', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), []);
  state.openDialog({
    kind: 'sandbox-editor',
    seed: {
      name: 'hardened', filesystem: [], environment: [], includes: [], agent_directories: [],
      read_baseline_exclusions: ['future.secret-store'],
    },
    options: {},
  });
  let saved = null;
  const actions = {
    async loadReadExclusionCatalog() {
      return {
        version: 1,
        categories: [
          { id: 'secrets.ssh', label: 'Deny SSH', description: 'SSH credentials.', tier: 'portable', paths: ['/home/op/.ssh'] },
          { id: 'home.directory', label: 'Deny Home', description: 'Home baseline.', tier: 'home', paths: ['/home/op'] },
        ],
        informational: [{ id: 'agentd.control-plane', label: 'Control plane', description: 'Required socket access.' }],
      };
    },
    async inspectDirectories() { return { missing: [], creatable: [] }; },
    async createDirectories() {},
    async saveSandbox(value) { saved = value; },
    configureSandboxWithAgent() {},
  };
  const cleanups = []; const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  mountManagementIsland({ host, state, actions, confirmDiscard: async () => true, openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); } });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  assert.match(host.querySelector('.sbx-read-exclusions').textContent, /Unknown restriction: future\.secret-store/);
  let choices = host.querySelectorAll('.sbx-exclusion-list input');
  assert.equal(choices.length, 2); assert.notEqual(choices[0].disabled, true); assert.notEqual(choices[1].checked, true);
  choices[1].checked = true; choices[1].dispatchEvent(new harness.window.Event('change', { bubbles: true })); await harness.act(() => Promise.resolve());
  choices = host.querySelectorAll('.sbx-exclusion-list input');
  assert.equal(choices[0].hasAttribute('checked'), true); assert.equal(choices[0].hasAttribute('disabled'), true);
  assert.match(choices[0].parentElement.textContent, /covered by Home directory exclusion/);
  host.querySelector('#sandbox-profile-editor-submit').click(); await harness.act(() => Promise.resolve());
  assert.deepEqual(saved.draft.read_baseline_exclusions, ['future.secret-store', 'home.directory']);
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

// Auto memory is a tri-state on the profile, and its unset case is NOT
// "inherit whatever the harness does": tclaude resolves unset to OFF and
// injects CLAUDE_CODE_DISABLE_AUTO_MEMORY, because agents sharing a repo
// otherwise cross-pollute one Claude Code project memory store. These pin the
// draft/payload round-trip and the harness gate.
test('profile auto memory round-trips as a tri-state and is gated on the harness', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/management-model.js');

  // Unset stays unset on the wire, so the server-side default (off) applies
  // rather than the dialog pinning an explicit value.
  const unset = model.profileDraft({ name: 'p', harness: 'claude' }, {}, catalog);
  assert.equal(unset.auto_memory, '');
  assert.equal(model.profilePayload(unset, null, catalog).auto_memory, undefined);

  // Both explicit values survive.
  const on = model.profileDraft({ name: 'p', harness: 'claude', auto_memory: true }, {}, catalog);
  assert.equal(on.auto_memory, '1');
  assert.equal(model.profilePayload(on, null, catalog).auto_memory, true);

  const off = model.profileDraft({ name: 'p', harness: 'claude', auto_memory: false }, {}, catalog);
  assert.equal(off.auto_memory, '0');
  assert.equal(model.profilePayload(off, null, catalog).auto_memory, false);

  // Codex has no auto-memory system, so the field is dropped rather than sent
  // as a value the server would reject.
  const codex = model.profileDraft({ name: 'p', harness: 'codex', auto_memory: true }, {}, catalog);
  codex.harness = 'codex';
  assert.equal(model.profilePayload(codex, null, catalog).auto_memory, undefined);

  // The tri-state labels name the real default so the operator isn't misled
  // into reading "Default" as "leave Claude Code alone".
  assert.match(model.AUTO_MEMORY_TRI_OPTIONS[0][1], /off/i);
});
