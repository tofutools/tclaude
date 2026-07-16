import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const groups = [{
  name: 'alpha',
  default_cwd: '/repo',
  default_context: 'shared',
  default_profile: 'group-default',
  remote_control_policy: 'inherit',
}, {
  name: 'beta',
  default_cwd: '/beta',
  remote_control_policy: 'optin',
}];

const harnesses = [{
  name: 'claude', display_name: 'Claude Code',
  models: ['sonnet', 'opus'], effort_levels: ['low', 'high'],
  can_sandbox: true, sandbox_modes: ['inherit', 'on', 'off'], default_sandbox: 'inherit',
  sandbox_mode_help: { inherit: 'keep settings', on: 'force on', off: 'force off' },
  can_approval: true, approval_modes: ['inherit', 'plan'], default_approval: 'inherit',
  approval_mode_help: { inherit: 'keep rules', plan: 'read only' },
  can_auto_review: false,
  can_ask_timeout: true, ask_timeout_modes: ['inherit', 'never'], default_ask_timeout: 'inherit',
  ask_timeout_mode_help: { inherit: 'keep settings', never: 'wait forever' },
  can_remote_control: true,
}, {
  name: 'codex', display_name: 'Codex CLI',
  models: [], effort_levels: ['medium', 'high', 'max'],
  can_sandbox: true,
  sandbox_modes: ['tclaude-agent', 'danger-full-access'],
  default_sandbox: 'tclaude-agent',
  sandbox_mode_help: { 'tclaude-agent': 'managed', 'danger-full-access': 'off' },
  can_approval: true, approval_modes: ['never', 'untrusted', 'on-failure', 'on-request'], default_approval: 'never',
  approval_mode_help: {
    never: 'never prompt', untrusted: 'ask for untrusted',
    'on-failure': 'deprecated retry', 'on-request': 'ask when requested',
  },
  can_auto_review: true,
  can_ask_timeout: false, ask_timeout_modes: [], default_ask_timeout: '',
  can_remote_control: false,
}];

const profiles = [{
  name: 'group-default', harness: 'claude', model: 'opus', effort: 'high',
  role: 'reviewer', initial_message: 'review this', remote_control: true,
  is_owner: true, permission_overrides: { 'groups.spawn': 'grant' },
}, {
  name: 'codex-profile', aliases: ['codex-fast'], harness: 'codex',
  model: 'gpt-5.6', sandbox: 'danger-full-access', approval: 'on-request',
  auto_review: true, trust_dir: false,
  remote_control: true,
}];

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

async function flush(harness, turns = 8) {
  await harness.act(async () => {
    for (let turn = 0; turn < turns; turn += 1) await Promise.resolve();
  });
}

async function settleWorktrees(harness) {
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  await flush(harness);
}

function setValue(element, value) {
  Object.defineProperty(element, 'value', { configurable: true, writable: true, value });
}

function selectedValue(select) {
  return select.getAttribute('value')
    ?? Array.from(select.options).find((option) => option.selected)?.getAttribute('value')
    ?? '';
}

test('agent-spawn model preserves precedence, sparse profiles, gates, and hidden-field clearing', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = {
    groups, harnesses, userDefaultModel: 'user-sonnet', normalizeNames: true,
  };
  const remembered = (name) => name === '' ? 'low' : name === 'opus' ? 'high' : '';
  let draft = model.createSpawnDraft({
    groups, harnesses, groupName: 'alpha', autoFocus: false, rememberedEffort: remembered,
  });
  assert.equal(draft.group, 'alpha');
  assert.equal(draft.fixedGroup, true);
  assert.equal(draft.cwd, '/repo');
  assert.equal(draft.wtRepo, '/repo');
  assert.equal(draft.harness, 'claude');
  assert.equal(draft.effort, 'low');
  assert.equal(draft.autoFocus, false);

  const pinnedMissing = model.createSpawnDraft({
    groups, harnesses, groupName: 'snapshot-only', defaultGroup: 'alpha',
  });
  assert.equal(pinnedMissing.group, 'snapshot-only', 'a pinned snapshot group cannot fall through');
  assert.equal(pinnedMissing.fixedGroup, true);
  assert.equal(pinnedMissing.cwd, '');

  const nonRepoPrepared = model.prepareSpawnDraft(
    { ...draft, name: 'worker' }, context, '', false,
  );
  assert.equal(nonRepoPrepared.worktree, '', 'sync cannot select a worktree before repo validation');
  const repoPrepared = model.prepareSpawnDraft(
    { ...draft, name: 'worker' }, context, '', true,
  );
  assert.equal(repoPrepared.worktree, model.WT_NEW);

  draft = model.applySpawnProfile({ ...draft, profile: 'group-default' }, profiles[0], context, remembered);
  assert.equal(draft.model, 'opus');
  assert.equal(draft.effort, 'high');
  assert.equal(draft.role, 'reviewer');
  assert.equal(draft.remoteControl, true);
  assert.equal(draft.owner, true);
  assert.deepEqual(draft.permissionOverrides, { 'groups.spawn': 'grant' });

  const sparse = model.applySpawnProfile(
    { ...draft, model: 'sonnet' }, { harness: 'claude', role: 'navigator' }, context, remembered,
  );
  assert.equal(sparse.model, 'sonnet', 'a sparse same-harness profile preserves the live model');
  assert.equal(sparse.role, 'navigator');

  draft = model.applySpawnProfile(draft, profiles[1], context, remembered);
  assert.equal(draft.harness, 'codex');
  assert.equal(draft.model, 'gpt-5.6');
  assert.equal(draft.sandbox, 'danger-full-access');
  assert.equal(draft.approval, 'on-request');
  assert.equal(draft.approvalReviewer, 'auto_review');
  assert.equal(model.spawnProfileSeed(draft, context).auto_review, true);
  assert.equal(draft.trustDirSpecified, true, 'profile false is explicit');
  assert.equal(draft.remoteControl, false, 'unsupported hidden remote state is cleared');
  assert.equal(model.spawnCapabilityView(draft, context).sandboxProfilesDisabled, true);

  const sparseCodex = model.applySpawnProfile(draft, {
    name: 'codex-default-reviewer', harness: 'codex',
  }, context);
  assert.equal(sparseCodex.approvalReviewer, '',
    'switching to a sparse profile clears the previous explicit reviewer');

  const customBlank = { ...model.selectSpawnHarness(draft, 'claude', context), customModel: true };
  assert.equal(model.modelSelectValue(customBlank, context), model.MODEL_CUSTOM_VALUE);

  draft = model.selectSpawnHarness(draft, 'claude', context, remembered);
  assert.equal(draft.model, '', 'a harness namespace change clears the incompatible model');
  assert.equal(draft.trustDirSpecified, false);
  assert.equal(draft.sandbox, 'inherit');
  assert.equal(draft.approvalReviewer, '');
  assert.equal(draft.remoteControl, false);

  draft = model.setSpawnCwd({
    ...draft, worktree: model.WT_NEW, worktreeBranch: 'old', worktreeBase: 'main',
  }, '/manual');
  assert.equal(draft.worktree, '');
  assert.equal(draft.worktreeBranch, '');
  assert.equal(draft.worktreeBase, '');
  draft = model.selectSpawnGroup(draft, 'beta', context);
  assert.equal(draft.cwd, '/manual', 'manual cwd survives a group source change');
  assert.equal(draft.remoteControl, true, 'the new group policy owns remote-control prefill');
  assert.equal(draft.includeGroupContext, true);

  const changedGroup = model.selectSpawnGroup({
    ...draft, worktree: model.WT_NEW, worktreeBranch: 'old', worktreeBase: 'main',
  }, 'alpha', context);
  assert.equal(changedGroup.worktree, '');
  assert.equal(changedGroup.worktreeBranch, '');
  assert.equal(changedGroup.worktreeBase, '');

  const changedRepo = model.setSpawnWorktreeRepo({
    ...draft, worktree: model.WT_NEW, worktreeBranch: 'old', worktreeBase: 'trunk',
  }, '/other');
  assert.equal(changedRepo.worktree, '');
  assert.equal(changedRepo.worktreeBranch, '');
  assert.equal(changedRepo.worktreeBase, '');

  const noSyncProfile = model.applySpawnProfile(
    draft, { sync_worktree: false }, context, remembered, true,
  );
  assert.equal(noSyncProfile.syncWorktree, false);

  const cleared = model.clearSpawnProfileFields({
    ...noSyncProfile, name: 'worker', role: 'lead', owner: true,
    permissionOverrides: { x: 'deny' }, profile: 'group-default',
  }, context, { autoFocus: true, rememberedEffort: remembered });
  assert.equal(cleared.name, '');
  assert.equal(cleared.role, '');
  assert.equal(cleared.owner, false);
  assert.equal(cleared.syncWorktree, true, 'Clear restores the blank-form sync default');
  assert.deepEqual(cleared.permissionOverrides, {});
  assert.equal(cleared.cwd, '/manual', 'Clear leaves location state alone');
});

test('agent-spawn model normalizes names and builds exact launch bodies', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = { groups, harnesses, userDefaultModel: '', normalizeNames: true };
  assert.equal(model.normalizeSpawnName(' code  reviewer! '), 'code-reviewer');
  assert.equal(model.deriveSpawnNameFromMessage('🔥 fix the auth flow now'), 'fix-the-auth-flow');
  assert.match(model.spawnNameHint('bad name', true).text, /bad-name/);
  assert.match(model.validateSpawnDraft({
    ...model.createSpawnDraft({ groups, harnesses, groupName: 'alpha' }), name: '🔥',
  }, context), /name or an initial description/);

  let draft = model.createSpawnDraft({ groups, harnesses, groupName: 'alpha' });
  draft = {
    ...draft,
    name: 'worker', role: 'reviewer', descr: 'does review', task: 'https://linear.app/TCL-458',
    initialMessage: 'ship it', model: 'opus', effort: 'high', sandbox: 'on',
    approval: 'plan', askTimeout: 'never', sandboxProfile: 'strict',
    remoteControl: false, owner: true, permissionOverrides: { 'groups.spawn': 'grant' },
    cwd: '/mono', wtRepo: '/mono/sub', profile: 'group-default',
  };
  const request = model.buildSpawnRequest(
    draft, context, { path: '/tmp/wt', branch: 'worker' }, ['/tmp/a.png'],
  );
  assert.equal(request.url, '/api/groups/alpha/spawn');
  assert.deepEqual(request.body, {
    name: 'worker', role: 'reviewer', descr: 'does review', initial_message: 'ship it',
    auto_focus: true, include_group_context: true, profile: 'group-default',
    attachments: ['/tmp/a.png'], effort: 'high', model: 'opus',
    task_ref_url: 'https://linear.app/TCL-458', harness: 'claude', sandbox: 'on',
    sandbox_profile: 'strict', approval: 'plan', ask_user_question_timeout: 'never',
    remote_control: false, is_owner: true,
    permission_overrides: { 'groups.spawn': 'grant' },
    cwd: '/mono', worktree_path: '/tmp/wt', worktree_branch: 'worker',
  });

  const codex = model.selectSpawnHarness(draft, 'codex', context);
  const codexBody = model.buildSpawnRequest({
    ...codex, name: 'worker', sandbox: 'danger-full-access', sandboxProfile: 'stale',
    approval: 'on-request', approvalReviewer: 'auto_review',
    trustDir: false, trustDirSpecified: true,
  }, context, { path: '', branch: '' }).body;
  assert.equal(codexBody.trust_dir, false);
  assert.equal('sandbox_profile' in codexBody, false);
  assert.equal('remote_control' in codexBody, false);
  assert.equal(codexBody.approval, 'on-request');
  assert.equal(codexBody.auto_review, true);
  const humanBody = model.buildSpawnRequest({
    ...codex, name: 'worker', approvalReviewer: 'human',
  }, context, { path: '', branch: '' }).body;
  assert.equal(humanBody.auto_review, false, 'explicit human review overrides a profile');
});

test('agent-spawn state snapshots opens and invalidates every async generation', async (t) => {
  const harness = await createPreactHarness(t);
  const { createAgentSpawnState } = await harness.importDashboardModule('js/agent-spawn-state.js');
  let snapshot = { groups, harnesses, user_default_model: 'sonnet', spawn_name_normalize: false };
  const state = createAgentSpawnState({ getSnapshot: () => snapshot });
  state.open({ groupName: 'alpha', role: 'reviewer' });
  const first = state.dialog.value;
  assert.equal(first.groups.length, 2);
  assert.equal(first.normalizeNames, false);
  assert.equal(state.isCurrent(first.generation), true);
  snapshot = { groups: [], harnesses: [] };
  assert.equal(first.groups.length, 2, 'poll replacement cannot retarget an open draft');
  state.refreshSandboxPolicy();
  assert.equal(state.dialog.value.generation, first.generation);
  assert.equal(state.dialog.value.sandboxRevision, 1);
  state.close();
  assert.equal(state.isCurrent(first.generation), false);
  state.open();
  assert.equal(state.dialog.value.groups.length, 0);
});

test('agent-spawn actions preserve effort memory, HTTP errors, upload retry inputs, and completion', async (t) => {
  const harness = await createPreactHarness(t);
  const { createAgentSpawnActions } = await harness.importDashboardModule('js/agent-spawn-actions.js');
  const calls = [];
  const store = new Map();
  let response = { ok: false, status: 403, text: async () => 'permission denied' };
  const actions = createAgentSpawnActions({
    fetchImpl: async (url, options) => { calls.push(['fetch', url, options]); return response; },
    prefs: { getItem: (key) => store.get(key) ?? null, setItem: (key, value) => store.set(key, value) },
    loadProfiles: async () => profiles,
    loadSandboxProfiles: async () => [],
    getDashboardDefaultProfile: () => 'dash-default',
    pickDirectory: async () => ({ canceled: true }),
    openProfileEditor: (...args) => calls.push(['profile', ...args]),
    openPermissions: (...args) => calls.push(['permissions', ...args]),
    confirm: async () => true,
    notify: (...args) => calls.push(['notify', ...args]),
    refresh: () => calls.push(['refresh']),
    openTerminal: (...args) => calls.push(['terminal', ...args]),
    celebrateSlop: () => calls.push(['slop']),
    celebrateWizard: () => calls.push(['wizard']),
    recordInteraction: (...args) => calls.push(['interaction', ...args]),
    shortID: (value) => value.slice(0, 8),
  });
  actions.rememberLaunchPreferences({ autoFocus: false, model: 'opus', effort: 'high' });
  assert.equal(actions.autoFocusDefault(), false);
  assert.equal(actions.rememberedEffort('opus'), 'high');
  await assert.rejects(() => actions.spawn({ url: '/spawn', body: {} }), /permission denied/);

  response = {
    ok: true, status: 200,
    json: async () => ({ conv_id: '1234567890', focus_mode: 'browser', focus_ws: '/ws' }),
  };
  const payload = await actions.spawn({ url: '/spawn', body: { name: 'worker' } });
  actions.complete(payload, { name: 'worker', group: 'alpha', autoFocus: true });
  assert.ok(calls.some(([kind]) => kind === 'terminal'));
  assert.ok(calls.some(([kind, message]) => kind === 'notify' && /opened in-browser/.test(message)));
  assert.ok(calls.some(([kind]) => kind === 'refresh'));

  const worktreeDraft = {
    worktree: '__new__', wtRepo: '/next', worktreeBranch: 'worker', worktreeBase: 'main',
  };
  const beforeWorktreeCalls = calls.length;
  await assert.rejects(() => actions.resolveWorktree(worktreeDraft, {
    phase: 'loading', repo: '/next', repoRoot: '/old', worktrees: [],
  }), /finish loading/);
  await assert.rejects(() => actions.resolveWorktree(worktreeDraft, {
    phase: 'ready', repo: '/old', repoRoot: '/old', worktrees: [],
  }), /finish loading/);
  assert.equal(calls.length, beforeWorktreeCalls, 'stale worktree metadata cannot issue a POST');
});

async function mountSpawn(t, overrides = {}) {
  const harness = await createPreactHarness(t);
  const [{ AgentSpawnApp }, { createAgentSpawnState }] = await Promise.all([
    harness.importDashboardModule('js/agent-spawn-island.js'),
    harness.importDashboardModule('js/agent-spawn-state.js'),
  ]);
  const state = createAgentSpawnState({
    getSnapshot: () => ({ groups, harnesses, user_default_model: 'sonnet' }),
  });
  const calls = [];
  const actions = {
    autoFocusDefault: () => true,
    rememberedEffort: () => '',
    rememberLaunchPreferences: (...args) => calls.push(['prefs', ...args]),
    dashboardDefaultProfile: () => '',
    loadProfiles: async () => profiles,
    loadWorktrees: async (repo) => ({
      repo, isRepo: true, empty: false, hasCommits: true, repoRoot: repo,
      worktrees: [], branches: ['main'], defaultBranch: 'main', subRepos: [],
    }),
    loadSandboxPolicy: async (_group, selected) => ({ profiles: [], selected, preview: 'no profiles applied' }),
    resolveWorktree: async () => ({ path: '', branch: '' }),
    uploadAttachments: async () => [],
    spawn: async () => ({ conv_id: 'abcdef1234' }),
    complete: (...args) => calls.push(['complete', ...args]),
    pickDirectory: async () => ({ canceled: true }),
    openProfileEditor: (...args) => calls.push(['profile', ...args]),
    openPermissions: (...args) => calls.push(['permissions', ...args]),
    confirmAutoName: async () => true,
    ...overrides,
  };
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`<${AgentSpawnApp}
    state=${state} actions=${actions} confirmDiscard=${async () => true}
  />`, host);
  return { harness, host, state, actions, calls, cleanup: mounted.unmount };
}

test('Preact agent-spawn owner renders profile/custom/capability states without remounting on refresh', async (t) => {
  const mounted = await mountSpawn(t);
  const { harness, host, state } = mounted;
  state.open({ groupName: 'alpha' });
  await flush(harness);
  assert.ok(host.querySelector('#agent-spawn-modal'));
  assert.equal(host.querySelector('#agent-spawn-group-row').hidden, true);
  assert.equal(host.querySelector('#agent-spawn-cwd').value, '/repo');
  assert.equal(harness.document.activeElement.id, 'agent-spawn-name');

  await flush(harness);
  assert.equal(selectedValue(host.querySelector('#agent-spawn-load-profile')), 'group-default');
  assert.equal(selectedValue(host.querySelector('#agent-spawn-model')), 'opus');
  assert.equal(host.querySelector('#agent-spawn-role').value, 'reviewer');
  const name = host.querySelector('#agent-spawn-name');
  setValue(name, 'my worker');
  await harness.act(() => harness.fireEvent(name, 'input'));
  assert.match(host.querySelector('#agent-spawn-name-hint').textContent, /my-worker/);
  const sameNameNode = host.querySelector('#agent-spawn-name');
  state.refreshSandboxPolicy();
  await flush(harness);
  assert.equal(host.querySelector('#agent-spawn-name'), sameNameNode, 'source refresh preserves the keyed draft DOM');
  assert.equal(host.querySelector('#agent-spawn-name').value, 'my worker');

  const harnessSelect = host.querySelector('#agent-spawn-harness');
  setValue(harnessSelect, 'codex');
  await harness.act(() => harness.fireEvent(harnessSelect, 'change'));
  assert.equal(host.querySelector('#agent-spawn-model-codex-row').hidden, false);
  assert.equal(host.querySelector('#agent-spawn-approval-row').hidden, false);
  assert.equal(host.querySelector('#agent-spawn-approval-reviewer-row').hidden, false);
  assert.match(host.querySelector('#agent-spawn-approval').textContent, /Never ask — no approval prompts/);
  const reviewer = host.querySelector('#agent-spawn-approval-reviewer');
  setValue(reviewer, 'auto_review');
  await harness.act(() => harness.fireEvent(reviewer, 'change'));
  assert.match(host.querySelector('#agent-spawn-approval-reviewer-hint').textContent, /No effect with/);
  assert.equal(host.querySelector('#agent-spawn-remote-control-row').hidden, true);
  assert.equal(host.querySelector('#agent-spawn-trust-dir-row').hidden, false);
  mounted.cleanup();
});

test('Preact agent-spawn Clear wins a race with the initial default-profile load', async (t) => {
  const pending = deferred();
  const mounted = await mountSpawn(t, {
    dashboardDefaultProfile: () => 'group-default',
    loadProfiles: () => pending.promise,
  });
  const { harness, host, state } = mounted;
  try {
    state.open({ groupName: 'alpha' });
    await flush(harness);
    host.querySelector('#agent-spawn-clear').click();
    await flush(harness);
    pending.resolve(profiles);
    await flush(harness);
    assert.equal(selectedValue(host.querySelector('#agent-spawn-load-profile')), '');
    assert.equal(selectedValue(host.querySelector('#agent-spawn-model')), '');
    assert.equal(host.querySelector('#agent-spawn-role').value, '');
    assert.equal(host.querySelector('#agent-spawn-owner').hasAttribute('checked'), false);
  } finally {
    mounted.cleanup();
  }
});

test('Preact agent-spawn does not apply a stale group default after an early group switch', async (t) => {
  const pending = deferred();
  const mounted = await mountSpawn(t, { loadProfiles: () => pending.promise });
  const { harness, host, state } = mounted;
  try {
    state.open({ defaultGroup: 'alpha' });
    await flush(harness);
    const group = host.querySelector('#agent-spawn-group');
    setValue(group, 'beta');
    await harness.act(() => harness.fireEvent(group, 'change'));
    pending.resolve(profiles);
    await flush(harness);
    assert.equal(host.querySelector('#agent-spawn-cwd').value, '/beta');
    assert.equal(selectedValue(host.querySelector('#agent-spawn-load-profile')), '');
    assert.equal(selectedValue(host.querySelector('#agent-spawn-model')), '');
    assert.equal(host.querySelector('#agent-spawn-role').value, '');
  } finally {
    mounted.cleanup();
  }
});

test('Preact agent-spawn explicit role wins profile defaults and profile-load failure', async (t) => {
  const loaded = await mountSpawn(t);
  try {
    loaded.state.open({ groupName: 'alpha', role: 'operator' });
    await flush(loaded.harness);
    assert.equal(selectedValue(loaded.host.querySelector('#agent-spawn-load-profile')), 'group-default');
    assert.equal(loaded.host.querySelector('#agent-spawn-role').value, 'operator');
  } finally {
    loaded.cleanup();
  }

  const rejected = await mountSpawn(t, { loadProfiles: async () => { throw new Error('offline'); } });
  try {
    rejected.state.open({ groupName: 'alpha', role: 'operator' });
    await flush(rejected.harness);
    assert.equal(rejected.host.querySelector('#agent-spawn-role').value, 'operator');
  } finally {
    rejected.cleanup();
  }
});

test('Preact agent-spawn preserves direct worktree edits across a delayed profile load', async (t) => {
  const pending = deferred();
  const mounted = await mountSpawn(t, { loadProfiles: () => pending.promise });
  const { harness, host, state } = mounted;
  try {
    state.open({ groupName: 'alpha' });
    await flush(harness);
    await settleWorktrees(harness);
    const worktree = host.querySelector('#agent-spawn-worktree');
    assert.match(worktree.textContent, /create new worktree/);
    setValue(worktree, '__new__');
    await harness.act(() => harness.fireEvent(worktree, 'change'));
    assert.equal(host.querySelector('#agent-spawn-wt-new-row').hidden, false);
    const branch = host.querySelector('#agent-spawn-wt-branch');
    setValue(branch, 'feature/manual');
    await harness.act(() => harness.fireEvent(branch, 'input'));
    assert.equal(host.querySelector('#agent-spawn-wt-new-row').hidden, false);
    pending.resolve(profiles);
    await flush(harness);
    assert.equal(host.querySelector('#agent-spawn-wt-new-row').hidden, false);
    assert.equal(host.querySelector('#agent-spawn-wt-branch').value, 'feature/manual');
    assert.equal(host.querySelector('#agent-spawn-wt-sync').hasAttribute('checked'), false);
  } finally {
    mounted.cleanup();
  }
});

test('Preact agent-spawn waits for worktree metadata before applying name sync', async (t) => {
  const pending = deferred();
  let spawnCalls = 0;
  const mounted = await mountSpawn(t, {
    loadWorktrees: () => pending.promise,
    spawn: async () => { spawnCalls += 1; return { conv_id: '1234567890' }; },
  });
  const { harness, host, state } = mounted;
  try {
    state.open({ groupName: 'alpha' });
    await flush(harness);
    const name = host.querySelector('#agent-spawn-name');
    setValue(name, 'worker');
    await harness.act(() => harness.fireEvent(name, 'input'));
    host.querySelector('#agent-spawn-submit').click();
    await flush(harness);
    assert.equal(spawnCalls, 0);
    assert.match(host.querySelector('#agent-spawn-error').textContent, /finish loading/);

    pending.resolve({
      repo: '/repo', isRepo: true, empty: false, hasCommits: true, repoRoot: '/repo',
      worktrees: [], branches: ['main'], defaultBranch: 'main', subRepos: [],
    });
    await settleWorktrees(harness);
    assert.equal(selectedValue(host.querySelector('#agent-spawn-worktree')), '__new__');
    host.querySelector('#agent-spawn-submit').click();
    await flush(harness);
    assert.equal(spawnCalls, 1);
    assert.equal(state.dialog.value, null);
  } finally {
    mounted.cleanup();
  }
});

test('Preact agent-spawn claims duplicate submit synchronously and retries failed spawn without re-upload', async (t) => {
  const pending = deferred();
  let uploadCalls = 0;
  let spawnCalls = 0;
  const mounted = await mountSpawn(t, {
    uploadAttachments: async () => { uploadCalls += 1; return ['/tmp/a']; },
    spawn: async () => { spawnCalls += 1; return pending.promise; },
  });
  const { harness, host, state, calls } = mounted;
  state.open({ groupName: 'alpha' });
  await flush(harness);
  await settleWorktrees(harness);
  const name = host.querySelector('#agent-spawn-name');
  setValue(name, 'worker');
  await harness.act(() => harness.fireEvent(name, 'input'));
  const button = host.querySelector('#agent-spawn-submit');
  const lateFile = new Blob(['late'], { type: 'text/plain' });
  Object.defineProperty(lateFile, 'name', { value: 'late.txt' });
  const input = host.querySelector('#agent-spawn-attach-input');
  Object.defineProperty(input, 'files', { configurable: true, value: [lateFile] });
  let drop;
  await harness.act(() => {
    button.click();
    button.click();
    harness.fireEvent(input, 'change');
    drop = harness.fireEvent(host.querySelector('#agent-spawn-modal'), 'drop', {
      dataTransfer: { types: ['Files'], files: [lateFile], dropEffect: '' },
    });
  });
  await flush(harness);
  assert.equal(spawnCalls, 1, host.querySelector('#agent-spawn-error')?.textContent || JSON.stringify(calls));
  assert.equal(uploadCalls, 0, 'an empty attachment set skips the upload endpoint');
  assert.equal(host.querySelectorAll('#agent-spawn-attachments-list li').length, 0);
  assert.equal(drop.defaultPrevented, true, 'busy file drops still suppress browser navigation');
  assert.equal(button.disabled, true);
  assert.equal(host.querySelector('#agent-spawn-sandbox').disabled, true);
  assert.equal(host.querySelector('#agent-spawn-sandbox-profile').disabled, true);
  pending.resolve({ conv_id: '1234567890' });
  await flush(harness);
  assert.equal(state.dialog.value, null);
  assert.equal(calls.filter(([kind]) => kind === 'complete').length, 1);
  mounted.cleanup();
});

test('Preact agent-spawn owns attachment input, retry caching, removal, and object URL cleanup', async (t) => {
  const originalCreate = URL.createObjectURL;
  const originalRevoke = URL.revokeObjectURL;
  const revoked = [];
  URL.createObjectURL = () => 'blob:spawn-preview';
  URL.revokeObjectURL = (value) => revoked.push(value);
  t.after(() => {
    URL.createObjectURL = originalCreate;
    URL.revokeObjectURL = originalRevoke;
  });

  let uploadCalls = 0;
  let attempts = 0;
  const mounted = await mountSpawn(t, {
    uploadAttachments: async (attachments) => {
      uploadCalls += 1;
      assert.deepEqual(attachments.map((attachment) => attachment.name), ['shot.png']);
      return ['/tmp/shot.png'];
    },
    spawn: async (request) => {
      attempts += 1;
      assert.deepEqual(request.body.attachments, ['/tmp/shot.png']);
      if (attempts === 1) throw new Error('temporary spawn failure');
      return { conv_id: '1234567890' };
    },
  });
  const { harness, host, state } = mounted;
  state.open({ groupName: 'alpha' });
  await flush(harness);
  await settleWorktrees(harness);
  const name = host.querySelector('#agent-spawn-name');
  setValue(name, 'worker');
  await harness.act(() => harness.fireEvent(name, 'input'));
  const image = new Blob(['png'], { type: 'image/png' });
  Object.defineProperty(image, 'name', { value: 'shot.png' });
  const textFile = new Blob(['notes'], { type: 'text/plain' });
  Object.defineProperty(textFile, 'name', { value: 'notes.txt' });
  const input = host.querySelector('#agent-spawn-attach-input');
  Object.defineProperty(input, 'files', { configurable: true, value: [image, textFile] });
  await harness.act(() => harness.fireEvent(input, 'change'));
  assert.equal(
    host.querySelector('#agent-spawn-attachments-list img').getAttribute('src'),
    'blob:spawn-preview',
  );
  const removeButtons = host.querySelectorAll('#agent-spawn-attachments-list .att-remove');
  assert.equal(removeButtons.length, 2);
  await harness.act(() => removeButtons[1].click());
  assert.equal(host.querySelectorAll('#agent-spawn-attachments-list li').length, 1);

  host.querySelector('#agent-spawn-submit').click();
  await flush(harness);
  assert.match(host.querySelector('#agent-spawn-error').textContent, /temporary spawn failure/);
  host.querySelector('#agent-spawn-submit').click();
  await flush(harness);
  assert.equal(uploadCalls, 1, 'a spawn-only retry reuses uploaded paths');
  assert.equal(attempts, 2);
  assert.deepEqual(revoked, ['blob:spawn-preview'], 'closing the dialog revokes live previews');
  mounted.cleanup();
});

test('Preact agent-spawn preserves failed drafts, permission handoff, IME-safe hotkey, and busy dismissal', async (t) => {
  let attempts = 0;
  const mounted = await mountSpawn(t, {
    spawn: async () => {
      attempts += 1;
      if (attempts === 1) throw new Error('permission denied');
      return { conv_id: '1234567890' };
    },
  });
  const { harness, host, state, calls } = mounted;
  state.open({ groupName: 'alpha' });
  await flush(harness);
  await settleWorktrees(harness);
  const name = host.querySelector('#agent-spawn-name');
  setValue(name, 'worker');
  await harness.act(() => harness.fireEvent(name, 'input'));
  await harness.act(() => harness.fireEvent(host.querySelector('#agent-spawn-perms'), 'click'));
  assert.ok(calls.filter(([kind]) => kind === 'permissions').length >= 1, JSON.stringify(calls));
  const permissions = calls.find(([kind]) => kind === 'permissions')[1];
  permissions.onSave({ 'groups.spawn': 'deny' });
  await flush(harness);
  assert.match(host.querySelector('#agent-spawn-perms-indicator').textContent, /1 deny/);

  const modal = host.querySelector('#agent-spawn-modal .cron-create-modal');
  const composing = new harness.window.Event('keydown', { bubbles: true, cancelable: true });
  Object.assign(composing, { key: 'Enter', ctrlKey: true, isComposing: true, keyCode: 229 });
  modal.dispatchEvent(composing);
  await flush(harness);
  assert.equal(attempts, 0, 'IME composition cannot submit');
  host.querySelector('#agent-spawn-submit').click();
  await flush(harness);
  assert.equal(attempts, 1);
  assert.match(host.querySelector('#agent-spawn-error').textContent, /permission denied/);
  assert.equal(host.querySelector('#agent-spawn-name').value, 'worker');
  host.querySelector('#agent-spawn-submit').click();
  await flush(harness);
  assert.equal(attempts, 2);
  assert.equal(state.dialog.value, null);
  mounted.cleanup();
});
