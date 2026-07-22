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

test('sandbox manager clones a full profile through the guarded create editor', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { createManagementActions }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/management-actions.js'),
    harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  const source = {
    id: 7,
    name: 'restricted',
    filesystem: [{ path: '/work', access: 'write' }],
    environment: [{ name: 'CACHE', value: '/cache' }],
    includes: ['base'],
    agent_directories: ['GOCACHE'],
    network_access: 'internet',
    read_baseline: 'minimal',
    read_baseline_exclusions: ['secrets.ssh'],
    break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'read' }],
    created_at: 'old',
    updated_at: 'old',
  };
  state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [
    { name: 'base' }, source, { name: 'restricted-copy' }, { name: 'restricted-copy-2' },
  ]);
  state.openManager('sandbox');
  let scribeCall = null;
  const actions = createManagementActions({
    state,
    confirm: async () => true,
    notify() {},
    summonSandboxScribe: async (...args) => { scribeCall = args; },
    sandboxAPI: {
      loadSandboxCommonRules: async () => ({ version: 1, categories: [], informational: [] }),
      inspectSandboxDirectories: async () => ({ missing: [], creatable: [] }),
    },
  });
  const cleanups = []; const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  mountManagementIsland({ host, state, actions, confirmDiscard: async () => true, openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); } });
  await harness.act(() => Promise.resolve());

  const card = [...host.querySelectorAll('.sandbox-profile-card')]
    .find((item) => item.querySelector('.tc-name').textContent === source.name);
  await harness.act(() => harness.fireEvent(card.querySelector('.sandbox-profile-clone'), 'click'));
  assert.equal(state.dialog.value.kind, 'sandbox-editor');
  assert.equal(state.dialog.value.options.editExisting, false, 'a clone must POST a new row, never PATCH its source');
  assert.equal(state.dialog.value.options.cloneSourceName, source.name);
  assert.equal(state.dialog.value.seed.name, 'restricted-copy-3', 'the suggested name skips existing copies');
  assert.deepEqual(state.dialog.value.seed.filesystem, source.filesystem);
  assert.deepEqual(state.dialog.value.seed.break_glass_filesystem, source.break_glass_filesystem);
  assert.match(host.querySelector('#sandbox-profile-editor-title').textContent, /Clone sandbox profile: restricted/);
  assert.equal(host.querySelector('#sandbox-profile-editor-modal input').value, 'restricted-copy-3');
  assert.ok(host.querySelector('#sandbox-profile-editor-break-glass-ack'), 'cloned authority still demands a fresh acknowledgement');
  await harness.act(() => harness.fireEvent(host.querySelector('#sandbox-profile-editor-scribe'), 'click'));
  assert.equal(scribeCall[1], '', 'the clone scribe handoff has no edit target');
  assert.deepEqual(scribeCall[3], { editExisting: false, cloneSourceName: 'restricted' }, 'the clone scribe handoff preserves create mode and its label');
  cleanups.reverse().forEach((fn) => fn());
});

test('sandbox editor and its clone mode save on Ctrl/Cmd+Enter', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  for (const [label, options] of [['edit', {}], ['clone', { editExisting: false, cloneSourceName: 'restricted' }]]) {
    for (const modifier of ['ctrlKey', 'metaKey']) {
      const state = createManagementState();
      const saved = [];
      const actions = {
        load() {}, openSandboxEditor() {}, removeSandbox() {}, configureSandboxWithAgent() {},
        loadCommonRuleCatalog: async () => ({ version: 1, categories: [], informational: [] }),
        inspectDirectories: async () => ({ missing: [], creatable: [] }),
        saveSandbox: async (payload) => { saved.push(payload); },
      };
      state.openDialog({ kind: 'sandbox-editor', seed: { name: 'restricted', filesystem: [] }, options });
      const cleanups = []; const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
      mountManagementIsland({ host, state, actions, confirmDiscard: async () => true, openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); } });
      await harness.act(() => Promise.resolve());
      const name = host.querySelector('#sandbox-profile-editor-modal input');
      await harness.act(() => harness.fireEvent(name, 'keydown', { key: 'Enter', [modifier]: true, isComposing: true, keyCode: 229 }));
      assert.equal(saved.length, 0, `${label}: IME composition must not submit`);
      const plainEnter = harness.fireEvent(name, 'keydown', { key: 'Enter' });
      assert.equal(plainEnter.defaultPrevented, false, `${label}: plain Enter retains the field default`);
      assert.equal(saved.length, 0, `${label}: plain Enter must not submit`);
      let shortcut;
      await harness.act(() => { shortcut = harness.fireEvent(name, 'keydown', { key: 'Enter', [modifier]: true }); });
      assert.equal(shortcut.defaultPrevented, true, `${label}: ${modifier}+Enter is claimed by the editor`);
      assert.equal(saved.length, 1, `${label}: ${modifier}+Enter saves`);
      assert.equal(saved[0].draft.name, 'restricted');
      assert.equal(saved[0].options.cloneSourceName, options.cloneSourceName);
      cleanups.reverse().forEach((fn) => fn());
      host.remove();
    }
  }
});

// The break-glass guards are the reason the hotkey carries its own block
// condition rather than leaning on the overlay's `blocked` prop: an
// unacknowledged rule set, and a daemon refusal whose registry reload failed,
// must both refuse the keyboard exactly as they refuse the Save button.
test('sandbox editor Ctrl+Enter respects the break-glass acknowledgement and recovery block', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const baseActions = {
    load: async () => false, openSandboxEditor() {}, removeSandbox() {}, configureSandboxWithAgent() {},
    loadCommonRuleCatalog: async () => ({ version: 1, categories: [], informational: [] }),
    inspectDirectories: async () => ({ missing: [], creatable: [] }),
  };
  const mount = (state, actions) => {
    const cleanups = []; const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
    mountManagementIsland({ host, state, actions, confirmDiscard: async () => true, openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); } });
    return { host, dispose: () => { cleanups.reverse().forEach((fn) => fn()); host.remove(); } };
  };

  const unacknowledged = createManagementState();
  const refused = [];
  unacknowledged.openDialog({
    kind: 'sandbox-editor',
    seed: { name: 'restricted', filesystem: [], break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'read' }] },
    options: {},
  });
  const first = mount(unacknowledged, { ...baseActions, saveSandbox: async (payload) => { refused.push(payload); } });
  await harness.act(() => Promise.resolve());
  await harness.act(() => harness.fireEvent(first.host.querySelector('#sandbox-profile-editor-modal input'), 'keydown', { key: 'Enter', ctrlKey: true }));
  assert.equal(refused.length, 0, 'Ctrl+Enter cannot skip the break-glass acknowledgement');
  assert.match(unacknowledged.error.value, /acknowledgement/);
  first.dispose();

  const blocked = createManagementState();
  const attempts = [];
  blocked.openDialog({ kind: 'sandbox-editor', seed: { name: 'restricted', filesystem: [] }, options: {} });
  const second = mount(blocked, {
    ...baseActions,
    // The daemon refused the commit and its registry reload failed too, so the
    // editor cannot show the rules a fresh acknowledgement would cover.
    saveSandbox: async (payload) => { attempts.push(payload); return { breakGlassAckRequired: true, recovered: false }; },
  });
  await harness.act(() => Promise.resolve());
  const input = second.host.querySelector('#sandbox-profile-editor-modal input');
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter', ctrlKey: true }));
  assert.equal(attempts.length, 1);
  await harness.act(() => Promise.resolve());
  assert.equal(second.host.querySelector('#sandbox-profile-editor-submit').disabled, true, 'the failed recovery disables Save');
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter', ctrlKey: true }));
  assert.equal(attempts.length, 1, 'Ctrl+Enter cannot save past a blocked break-glass recovery');
  second.dispose();
});

test('sandbox clone suggestions stay within the UTF-8 server limit across collisions', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { createManagementActions }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/management-actions.js'),
  ]);
  const state = createManagementState();
  const source = { name: 'é'.repeat(100), filesystem: [], environment: [] };
  state.sandboxProfiles.value = [source];
  const actions = createManagementActions({ state, confirm: async () => true, notify() {} });
  actions.openSandboxClone(source);
  const first = state.dialog.value.seed.name;
  assert.ok(new TextEncoder().encode(first).length <= 200);
  assert.equal(first.endsWith('-copy'), true);
  assert.equal(first.includes('\uFFFD'), false, 'truncation never splits a Unicode code point');

  state.sandboxProfiles.value = [source, { name: first }];
  actions.openSandboxClone(source);
  const second = state.dialog.value.seed.name;
  assert.ok(new TextEncoder().encode(second).length <= 200);
  assert.equal(second.endsWith('-copy-2'), true);
  assert.notEqual(second, first);
});

test('sandbox scribe return reopens clone drafts in explicit create mode', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/refresh.js', 'export function toast() {}');
  await harness.replaceDashboardModule('js/terminals-tab.js', 'export function openTermModal() {}');
  const [{ registerManagementController }, { summonSandboxScribe }, { createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-controller.js'),
    harness.importDashboardModule('js/sandbox-profiles.js'),
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/management-island.js'),
  ]);
  let opened = null;
  const unregister = registerManagementController({
    openSandboxProfileEditor(seed, options) { opened = { seed, options }; },
  });
  t.after(unregister);
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (path) => {
    if (path === '/api/scribe') return { ok: true, json: async () => ({ name: 'sandbox-scribe' }) };
    if (String(path).startsWith('/api/sandbox-profile-drafts/')) {
      return { ok: true, json: async () => ({ profile: { name: 'restricted-copy', filesystem: [] } }) };
    }
    throw new Error(`unexpected fetch: ${path}`);
  };
  t.after(() => { globalThis.fetch = originalFetch; });

  await summonSandboxScribe(
    { name: 'restricted-copy', filesystem: [] },
    '',
    null,
    { editExisting: false, cloneSourceName: 'restricted' },
  );
  await harness.act(() => Promise.resolve());
  assert.equal(opened.seed.name, 'restricted-copy');
  assert.equal(opened.options.targetName, '');
  assert.equal(opened.options.editExisting, false, 'the returned named draft remains a create');
  assert.equal(opened.options.cloneSourceName, 'restricted', 'the returned editor remains labeled as a clone');

  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-editor', seed: opened.seed, options: opened.options });
  const cleanups = []; const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  mountManagementIsland({
    host,
    state,
    actions: {
      loadCommonRuleCatalog: async () => ({ version: 1, categories: [], informational: [] }),
      inspectDirectories: async () => ({ missing: [], creatable: [] }),
      createDirectories: async () => {},
      saveSandbox: async () => {},
      configureSandboxWithAgent() {},
    },
    confirmDiscard: async () => true,
    openProfilePermissions() {},
    registerCleanup(fn) { cleanups.push(fn); },
  });
  await harness.act(() => Promise.resolve());
  assert.match(host.querySelector('#sandbox-profile-editor-title').textContent, /Clone sandbox profile: restricted/);
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
  // The save body always carries the full-replace shape, so break-glass rides
  // along explicitly even when untouched. The retired read_baseline fields are
  // gone from the wire entirely.
  const body = { ...draft, break_glass_filesystem: [] };
  const create = actions.saveSandbox({ draft, original: null }); await Promise.resolve();
  assert.deepEqual(state.sandboxDiff.value, { before: null, after: body }); state.cancelSandboxDiff(true);
  assert.equal(await create, true);
  assert.deepEqual(calls[0], ['preview', '', body]); assert.deepEqual(calls[1], ['save', '', body, 'r1']); assert.equal(refreshed, 1);
  const replacement = { ...draft, name: 'renamed' }; const replacementBody = { ...body, name: 'renamed' }; const update = actions.saveSandbox({ draft: replacement, original: replacement, options: { targetName: 'safe' } }); await Promise.resolve(); state.cancelSandboxDiff(true); await update;
  assert.deepEqual(calls[2], ['preview', 'safe', replacementBody]); assert.deepEqual(calls[3], ['save', 'safe', replacementBody, 'r1']);
  const copied = { ...draft, name: 'safe-copy' }; const copiedBody = { ...body, name: 'safe-copy' }; const clone = actions.saveSandbox({ draft: copied, original: draft, options: { editExisting: false } }); await Promise.resolve(); state.cancelSandboxDiff(true); await clone;
  assert.deepEqual(calls[4], ['preview', '', copiedBody]); assert.deepEqual(calls[5], ['save', '', copiedBody, 'r1']);
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

const COMMON_RULES = {
  version: 1,
  home: '/home/op',
  categories: [
    { id: 'secrets.ssh', label: 'Deny SSH credentials', description: 'SSH private keys and known hosts.', warning: 'Breaks git over SSH.', paths: ['/home/op/.ssh'] },
    { id: 'home.directory', label: 'Deny home directory', description: 'Everything under the home directory.', warning: 'You must reopen the harness, tclaude and toolchain directories (~/go, ~/.cargo, ~/.codex) or the agent cannot function.', paths: ['/home/op'] },
    { id: 'empty.here', label: 'Nothing on this platform', description: 'Resolves nowhere here.', paths: [] },
  ],
  informational: [{ id: 'agentd.control-plane', label: 'Control plane', description: 'Required socket access.' }],
  global_filesystem: [
    { path: '~/.claude/sessions', access: 'deny', harnesses: ['claude', 'codex'], origins: [
      { harness: 'claude', source: '~/.claude/settings.json', setting: 'sandbox.filesystem.denyRead + denyWrite', note: "Claude Code's global sandbox is enabled." },
      { harness: 'codex', source: '~/.codex/tclaude-agent.config.toml', setting: 'permissions.tclaude-agent.filesystem', note: "Applied by tclaude's managed Codex sandbox profile." },
    ] },
    { path: '~/.codex', access: 'deny-read', harnesses: ['claude'], origins: [
      { harness: 'claude', source: '~/.claude/settings.json', setting: 'sandbox.filesystem.denyRead', note: "Claude Code's global sandbox is enabled." },
    ] },
    { path: '~/.tclaude/api/agentd.sock', access: 'read', harnesses: ['claude', 'codex'], origins: [
      { harness: 'claude', source: '~/.claude/settings.json', setting: 'sandbox.filesystem.allowRead', note: "Claude Code's global sandbox is enabled." },
      { harness: 'codex', source: '~/.codex/tclaude-agent.config.toml', setting: 'permissions.tclaude-agent.filesystem', note: "Applied by tclaude's managed Codex sandbox profile." },
    ] },
  ],
  global_config_warnings: [],
};

function mountSandboxEditor(harness, mountManagementIsland, state, overrides = {}) {
  const cleanups = [];
  const host = harness.document.createElement('div');
  harness.document.body.appendChild(host);
  mountManagementIsland({
    host,
    state,
    actions: {
      async loadCommonRuleCatalog() { return COMMON_RULES; },
      async inspectDirectories() { return { missing: [], creatable: [] }; },
      async createDirectories() {},
      async saveSandbox() {},
      configureSandboxWithAgent() {},
      ...overrides,
    },
    confirmDiscard: async () => true,
    openProfilePermissions() {},
    registerCleanup(fn) { cleanups.push(fn); },
  });
  return { host, unmount: () => cleanups.reverse().forEach((fn) => fn()) };
}

test('global harness filesystem rows are visible, immutable, attributable, and never saved', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-editor', seed: { name: 'plain', filesystem: [{ path: '/work', access: 'write' }], environment: [], includes: [], agent_directories: [] }, options: {} });
  let saved = null;
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, { async saveSandbox(value) { saved = value; } });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));

  const toggle = host.querySelector('#sandbox-profile-editor-show-global-filesystem');
  assert.equal(toggle.hasAttribute('checked'), true, 'inherited context is visible by default');
  const inherited = [...host.querySelectorAll('.sbx-global-row')];
  assert.equal(inherited.length, COMMON_RULES.global_filesystem.length);
  assert.equal(inherited[0].querySelector('.sbx-path').hasAttribute('readonly'), true);
  assert.equal(inherited[0].querySelectorAll('button').length, 0, 'an inherited row has no browse or delete actions');
  assert.match(inherited[0].textContent, /Claude \+ Codex/);
  assert.match(inherited[0].getAttribute('title'), /settings\.json.*tclaude-agent\.config\.toml/s);
  assert.match(inherited[1].textContent, /deny read.*Claude/);

  host.querySelector('#sandbox-profile-editor-submit').click(); await harness.act(() => Promise.resolve());
  assert.deepEqual(saved.draft.filesystem, [{ path: '/work', access: 'write' }]);

  toggle.checked = false;
  toggle.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#sandbox-profile-editor-global-filesystem'), null, 'the checkbox folds inherited context without changing the draft');
  unmount();
});

// The presets are row inserters, nothing more: what they add is an ordinary,
// visible, editable deny row, and the entry's warning must be on screen at the
// moment the rows appear. Nothing about the preset is retained afterwards.
test('the common-rule menu inserts plain editable deny rows and warns at insertion', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({
    kind: 'sandbox-editor',
    seed: { name: 'hardened', filesystem: [{ path: '/home/op/.ssh', access: 'deny' }], environment: [], includes: [], agent_directories: [] },
    options: {},
  });
  let saved = null;
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, { async saveSandbox(value) { saved = value; } });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));

  // The menu ships folded and lives on the filesystem table, not in a section
  // of its own — there is only one filesystem mechanism now.
  const menu = host.querySelector('#sandbox-profile-editor-common-rules');
  assert.equal(menu.hasAttribute('open'), false, 'the preset menu ships folded');
  assert.equal(menu.closest('fieldset').querySelector('legend').textContent, 'Filesystem');
  menu.open = true; menu.dispatchEvent(new harness.window.Event('toggle'));
  await harness.act(() => Promise.resolve());

  const entries = [...host.querySelectorAll('.sbx-common-rule-entry')];
  assert.deepEqual(entries.map((entry) => entry.getAttribute('data-rule')), ['secrets.ssh', 'home.directory', 'empty.here']);
  // Warning and exact paths are readable before the click, not only after it.
  assert.match(entries[1].querySelector('.sbx-common-rule-warn').textContent, /~\/go, ~\/\.cargo, ~\/\.codex/);
  assert.equal(entries[1].querySelector('.sbx-common-rule-paths').textContent, '/home/op');
  // An entry with no paths here is inert but stays focusable, so the reason it
  // is inert is still announced with it.
  const inertAdd = entries[2].querySelector('.sbx-common-rule-add');
  assert.equal(inertAdd.getAttribute('aria-disabled'), 'true');
  assert.notEqual(inertAdd.disabled, true, 'the inert entry keeps its place in the tab order');
  const rowsBefore = host.querySelectorAll('.sbx-section .sbx-path').length;
  inertAdd.click();
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelectorAll('.sbx-section .sbx-path').length, rowsBefore, 'an entry with no paths here cannot be inserted');
  assert.equal(host.querySelector('#sandbox-profile-editor-common-rule-notice'), null);

  entries[1].querySelector('.sbx-common-rule-add').click();
  await harness.act(() => Promise.resolve());
  const rows = [...host.querySelectorAll('.sbx-section .sbx-row')].filter((row) => row.querySelector('.sbx-path'));
  const inserted = rows[rows.length - 1];
  assert.equal(inserted.querySelector('.sbx-path').value, '/home/op');
  assert.equal(inserted.querySelector('.sbx-access').getAttribute('value'), 'deny');
  assert.notEqual(inserted.querySelector('.sbx-path').disabled, true, 'inserted rows stay ordinary editable rows');
  const notice = host.querySelector('#sandbox-profile-editor-common-rule-notice');
  assert.match(notice.textContent, /Added 1 deny row from “Deny home directory”: \/home\/op/);
  assert.match(notice.querySelector('.sbx-common-rule-warn').textContent, /reopen the harness, tclaude and toolchain directories/);

  // A path already in the table is left exactly as authored rather than
  // silently re-denied or duplicated, and the notice says so.
  entries[0].querySelector('.sbx-common-rule-add').click();
  await harness.act(() => Promise.resolve());
  assert.match(host.querySelector('#sandbox-profile-editor-common-rule-notice').textContent, /added no rows.*1 path was already in the table/);

  // The inserted row is editable like any other, and the saved draft carries
  // rows only — no preset ID, no hidden state.
  const edited = [...host.querySelectorAll('.sbx-path')].find((input) => input.value === '/home/op');
  edited.value = '/home/op/private';
  edited.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(saved.draft.filesystem, [
    { path: '/home/op/.ssh', access: 'deny' },
    { path: '/home/op/private', access: 'deny' },
  ]);
  assert.equal(saved.draft.read_baseline, undefined);
  assert.equal(saved.draft.read_baseline_exclusions, undefined);
  unmount();
});

// A profile written before TCL-623 may still carry the retired fields. The
// editor must simply not render them — never error, and never imply they are
// still enforced.
test('a profile carrying retired baseline fields loads with no baseline UI at all', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [{
    name: 'base', filesystem: [], environment: [], includes: [], agent_directories: [],
    read_baseline_exclusions: ['future.inherited-store'],
  }]);
  state.openDialog({
    kind: 'sandbox-editor',
    seed: {
      name: 'legacy', filesystem: [{ path: '/work', access: 'write' }], environment: [], includes: ['base'], agent_directories: [],
      read_baseline: 'minimal', read_baseline_exclusions: ['future.secret-store'],
    },
    options: {},
  });
  let saved = null;
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, { async saveSandbox(value) { saved = value; } });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  assert.equal(host.querySelector('#sandbox-profile-editor-read-baseline'), null);
  assert.equal(host.querySelector('.sbx-read-exclusions'), null);
  assert.equal(host.querySelector('#sandbox-profile-editor-modal').textContent.includes('future.secret-store'), false);
  assert.equal(host.querySelector('.cron-create-error').textContent, '', 'an old profile loads without an error');
  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(saved.draft.filesystem, [{ path: '/work', access: 'write' }]);
  assert.equal('read_baseline' in saved.draft, false, 'the retired fields are dropped, not round-tripped');
  assert.equal('read_baseline_exclusions' in saved.draft, false);
  unmount();
});

// The catalog is a convenience, not a dependency: a feed that fails must never
// block editing the table by hand.
test('a failing common-rule feed leaves the filesystem table usable', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-editor', seed: { name: 'plain', filesystem: [], environment: [], includes: [], agent_directories: [] }, options: {} });
  let feedOffline = true;
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    async loadCommonRuleCatalog() { if (feedOffline) throw new Error('feed offline'); return COMMON_RULES; },
  });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  // The failure belongs to the menu it came from, not to the editor's shared
  // error line, and it offers a way back.
  const feedError = host.querySelector('#sandbox-profile-editor-common-rule-feed-error');
  assert.match(feedError.textContent, /Could not load the common-rule catalog: feed offline/);
  assert.equal(feedError.getAttribute('role'), 'alert');
  assert.equal(host.querySelector('.cron-create-error').textContent, '', 'an optional feed never writes to the shared error signal');
  assert.equal(host.querySelectorAll('.sbx-common-rule-entry').length, 0);
  // Nothing inside a closed <details> is visible or announced, so the summary
  // itself has to say the presets are unavailable.
  assert.match(host.querySelector('.sbx-common-rule-summary').textContent, /unavailable/);
  host.querySelector('.sbx-section .sbx-add-row').click();
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelectorAll('.sbx-section .sbx-path').length, 1, 'rows can still be added by hand');
  // Retry recovers the menu without a reopen.
  feedOffline = false;
  await harness.act(() => { feedError.querySelector('button').click(); return new Promise((resolve) => setTimeout(resolve, 50)); });
  assert.equal(host.querySelector('#sandbox-profile-editor-common-rule-feed-error'), null);
  assert.equal(host.querySelectorAll('.sbx-common-rule-entry').length, COMMON_RULES.categories.length);
  assert.equal(host.querySelector('.sbx-common-rule-summary').textContent.includes('unavailable'), false);
  unmount();
});

// A feed that never settles must not strand the operator: retry stays live so a
// second attempt can supersede a hung one, and a synchronous throw is a failure
// like any other rather than a stuck "retrying…".
test('a hung or synchronously throwing common-rule feed can still be retried', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-editor', seed: { name: 'plain', filesystem: [], environment: [], includes: [], agent_directories: [] }, options: {} });
  let mode = 'throw-sync';
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    loadCommonRuleCatalog() {
      if (mode === 'throw-sync') throw new Error('feed exploded');
      if (mode === 'hang') return new Promise(() => {});
      return Promise.resolve(COMMON_RULES);
    },
  });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  const feedError = () => host.querySelector('#sandbox-profile-editor-common-rule-feed-error');
  assert.match(feedError().textContent, /feed exploded/, 'a synchronous throw surfaces as a feed failure');
  assert.notEqual(feedError().querySelector('button').disabled, true);

  mode = 'hang';
  await harness.act(() => { feedError().querySelector('button').click(); return new Promise((resolve) => setTimeout(resolve, 50)); });
  assert.notEqual(feedError().querySelector('button').disabled, true, 'a hung load never disables its own way out');

  mode = 'ok';
  await harness.act(() => { feedError().querySelector('button').click(); return new Promise((resolve) => setTimeout(resolve, 50)); });
  assert.equal(feedError(), null);
  assert.equal(host.querySelectorAll('.sbx-common-rule-entry').length, COMMON_RULES.categories.length);
  unmount();
});

// state.error carries save, validation and break-glass refusals. A catalog
// rejection landing after one of those must not replace the reason the save
// was refused with an explanation of an optional convenience.
test('a late common-rule feed rejection does not overwrite a refused save', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({
    kind: 'sandbox-editor',
    seed: { name: 'restricted', filesystem: [], environment: [], includes: [], agent_directories: [], break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'read' }] },
    options: {},
  });
  let rejectFeed = null;
  let saved = null;
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    loadCommonRuleCatalog() { return new Promise((_, reject) => { rejectFeed = reject; }); },
    async saveSandbox(value) { saved = value; },
  });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));

  // The save is refused locally: break-glass rules need an acknowledgement.
  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.equal(saved, null);
  assert.match(host.querySelector('.cron-create-error').textContent, /acknowledgement/);

  // Only now does the feed give up.
  rejectFeed(new Error('feed offline'));
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 50)));
  assert.match(host.querySelector('.cron-create-error').textContent, /acknowledgement/, 'the refusal reason survives the late rejection');
  assert.match(host.querySelector('#sandbox-profile-editor-common-rule-feed-error').textContent, /feed offline/);
  unmount();
});

// The daemon canonicalizes paths and folds deny over write, so a trailing
// separator is not a different location: appending a deny for an alias of an
// authored `write` row would silently override it while the notice claims the
// path was left as authored.
test('common-rule insertion treats separator aliases as the same authored path', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({
    kind: 'sandbox-editor',
    // Aliases of the catalog's `/home/op` and `/home/op/.ssh`, authored by hand.
    // `..` is folded because the daemon Cleans before it resolves symlinks.
    seed: { name: 'aliased', filesystem: [{ path: '/home/op/', access: 'write' }, { path: '/home/op/tmp/../.ssh', access: 'write' }], environment: [], includes: [], agent_directories: [] },
    options: {},
  });
  let saved = null;
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, { async saveSandbox(value) { saved = value; } });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  const entries = [...host.querySelectorAll('.sbx-common-rule-entry')];
  entries[1].querySelector('.sbx-common-rule-add').click();
  await harness.act(() => Promise.resolve());
  assert.match(host.querySelector('#sandbox-profile-editor-common-rule-notice').textContent, /added no rows.*1 path was already in the table and left as authored/);
  entries[0].querySelector('.sbx-common-rule-add').click();
  await harness.act(() => Promise.resolve());
  assert.match(host.querySelector('#sandbox-profile-editor-common-rule-notice').textContent, /added no rows.*1 path was already in the table and left as authored/);
  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(saved.draft.filesystem, [
    { path: '/home/op/', access: 'write' },
    { path: '/home/op/tmp/../.ssh', access: 'write' },
  ], 'the authored rows are untouched and no aliased deny was appended');
  unmount();
});

// The catalog carries the daemon home so the browser can expand `~` before it
// cleans, matching the daemon's order. Presets must leave both the bare home and
// a descendant written with `~/` exactly as authored.
test('common-rule insertion treats ~ aliases as the same authored path', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({
    kind: 'sandbox-editor',
    seed: { name: 'tilde', filesystem: [{ path: '~', access: 'write' }, { path: '~/.ssh/', access: 'write' }], environment: [], includes: [], agent_directories: [] },
    options: {},
  });
  let saved = null;
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, { async saveSandbox(value) { saved = value; } });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  host.querySelector('.sbx-common-rule-entry[data-rule="secrets.ssh"] .sbx-common-rule-add').click();
  await harness.act(() => Promise.resolve());
  assert.match(host.querySelector('#sandbox-profile-editor-common-rule-notice').textContent, /added no rows.*1 path was already in the table and left as authored/);
  host.querySelector('.sbx-common-rule-entry[data-rule="home.directory"] .sbx-common-rule-add').click();
  await harness.act(() => Promise.resolve());
  assert.match(host.querySelector('#sandbox-profile-editor-common-rule-notice').textContent, /added no rows.*1 path was already in the table and left as authored/);
  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(saved.draft.filesystem, [
    { path: '~', access: 'write' },
    { path: '~/.ssh/', access: 'write' },
  ], 'the authored ~ rows are untouched and no aliased deny was appended');
  unmount();
});

test('common-rule insertion leaves ~otheruser paths literal', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({
    kind: 'sandbox-editor',
    seed: { name: 'other-home', filesystem: [{ path: '~otheruser/.ssh', access: 'write' }], environment: [], includes: [], agent_directories: [] },
    options: {},
  });
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state);
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  host.querySelector('.sbx-common-rule-entry[data-rule="secrets.ssh"] .sbx-common-rule-add').click();
  await harness.act(() => Promise.resolve());
  assert.match(host.querySelector('#sandbox-profile-editor-common-rule-notice').textContent, /Added 1 deny row/);
  assert.deepEqual([...host.querySelectorAll('.sbx-path')].map((input) => input.value), ['~otheruser/.ssh', '/home/op/.ssh']);
  unmount();
});

// The button applies the rule; the warning explaining what that costs must be
// announced with it, and the notice must be dismissable by name.
test('common-rule controls are described and named for assistive technology', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-editor', seed: { name: 'plain', filesystem: [], environment: [], includes: [], agent_directories: [] }, options: {} });
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state);
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));

  const home = host.querySelector('.sbx-common-rule-entry[data-rule="home.directory"]');
  const described = home.querySelector('.sbx-common-rule-add').getAttribute('aria-describedby').split(/\s+/);
  assert.equal(described.length, 3, 'description, warning and paths are all announced with the button');
  const texts = described.map((id) => host.querySelector(`#${id}`)?.textContent);
  assert.ok(texts.every((text) => typeof text === 'string' && text.length), 'every described-by id resolves to real text');
  assert.match(texts.join(' '), /reopen the harness, tclaude and toolchain directories/);
  assert.match(texts.join(' '), /\/home\/op/);
  // An entry without a warning describes only what it has.
  assert.equal(host.querySelector('.sbx-common-rule-entry[data-rule="empty.here"] .sbx-common-rule-add').getAttribute('aria-describedby').split(/\s+/).length, 2);

  home.querySelector('.sbx-common-rule-add').click();
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('.sbx-common-rule-dismiss').getAttribute('aria-label'), 'Dismiss common-rule notice');
  unmount();
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
