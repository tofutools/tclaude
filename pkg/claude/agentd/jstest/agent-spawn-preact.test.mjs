import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent, assertSameNode } from './assertions.mjs';
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
  can_builtin_os_sandbox: true,
  can_sandbox: true, sandbox_modes: ['inherit', 'on', 'off'], default_sandbox: 'inherit',
  sandbox_mode_help: { inherit: 'keep settings', on: 'force on', off: 'force off' },
  can_approval: true, approval_modes: ['inherit', 'plan'], default_approval: 'inherit',
  approval_mode_help: { inherit: 'keep rules', plan: 'read only' },
  can_tools: false, tools_modes: [], default_tools: '', tools_mode_help: {},
  can_auto_review: false,
  can_ask_timeout: true, ask_timeout_modes: ['inherit', 'never'], default_ask_timeout: 'inherit',
  ask_timeout_mode_help: { inherit: 'keep settings', never: 'wait forever' },
  can_remote_control: true, can_auto_memory: true,
  can_dir_trust: true, dir_trust_store: '~/.claude.json',
  can_context_features: true,
  context_features: [
    { slug: 'bundled-skills', label: 'Bundled skills', descr: 'shipped skills', heavy: true },
    { slug: 'artifact', label: 'Artifacts', descr: 'artifact tool', heavy: true },
    { slug: 'claude-mds', label: 'CLAUDE.md', descr: 'memory files', caution: 'drops project instructions' },
  ],
}, {
  name: 'codex', display_name: 'Codex CLI',
  models: [], effort_levels: ['medium', 'high', 'max'],
  can_builtin_os_sandbox: true,
  can_sandbox: true,
  sandbox_modes: ['tclaude-agent', 'danger-full-access'],
  default_sandbox: 'tclaude-agent',
  sandbox_mode_help: { 'tclaude-agent': 'managed', 'danger-full-access': 'off' },
  can_approval: true, approval_modes: ['never', 'untrusted', 'on-failure', 'on-request'], default_approval: 'never',
  approval_mode_help: {
    never: 'never prompt', untrusted: 'ask for untrusted',
    'on-failure': 'deprecated retry', 'on-request': 'ask when requested',
  },
  can_tools: false, tools_modes: [], default_tools: '', tools_mode_help: {},
  can_auto_review: true,
  can_ask_timeout: false, ask_timeout_modes: [], default_ask_timeout: '',
  can_remote_control: false, can_auto_memory: false, can_ssh_workaround: true,
	can_codex_app_server: true,
  can_fast_mode: true,
  can_dir_trust: true, dir_trust_store: '~/.codex/config.toml',
  can_context_features: false, context_features: [],
}, {
  name: 'opencode', display_name: 'OpenCode',
  models: [], effort_levels: [],
  can_builtin_os_sandbox: false,
  can_sandbox: true,
  sandbox_modes: ['off'],
  default_sandbox: 'off',
  sandbox_mode_help: { off: '⚠ No tclaude OS containment' },
  can_approval: false, approval_modes: [], default_approval: '',
  approval_mode_help: {},
  can_tools: true, tools_modes: ['allow', 'ask', 'deny'], default_tools: 'allow',
  tools_mode_help: { allow: 'allow tools', ask: 'ask for tools', deny: 'deny tools' },
  can_auto_review: false,
  can_ask_timeout: false, ask_timeout_modes: [], default_ask_timeout: '',
  can_remote_control: false, can_auto_memory: false,
  // OpenCode has no trust-folder dialog, so no store to seed either.
  can_dir_trust: false, dir_trust_store: '',
}];

const profiles = [{
  name: 'group-default', harness: 'claude', model: 'opus', effort: 'high',
  role: 'reviewer', initial_message: 'review this', remote_control: true,
  is_owner: true, permission_overrides: { 'groups.members.spawn': 'grant' },
}, {
  name: 'codex-profile', aliases: ['codex-fast'], harness: 'codex',
  model: 'gpt-5.6', sandbox: 'danger-full-access', approval: 'on-request',
  auto_review: true, trust_dir: false,
  remote_control: true,
}];

const roles = [{
  name: 'reviewer', descr: 'Cold review', brief: 'Review carefully.',
  permissions: ['human.notify'],
}, {
  name: 'go-maintainer', descr: 'Own Go quality', brief: 'Keep Go healthy.',
  permissions: [{ slug: 'groups.members.spawn', scope: { group: ['alpha'] } }],
}];

const sandboxImpl = {
  options: [
    { value: 'harness-builtin', label: '{harness} built-in' },
    { value: 'tclaude-layer', label: 'tclaude built-in OS sandbox (experimental)' },
    { value: 'stacked', label: 'Stacked: tclaude + {harness} (experimental)' },
    { value: 'off', label: 'Off' },
  ],
  default: 'harness-builtin',
  host_available: true,
  server_host_available: true,
  stacked: {
    claude: { available: true },
    codex: { available: true },
  },
};

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
  assert.deepEqual(draft.permissionOverrides, { 'groups.members.spawn': 'grant' });

  const sparse = model.applySpawnProfile(
    { ...draft, model: 'sonnet' }, { harness: 'claude', role: 'navigator' }, context, remembered,
  );
  assert.equal(sparse.model, 'sonnet', 'a sparse same-harness profile preserves the live model');
  assert.equal(sparse.role, 'navigator');

  draft = model.applySpawnProfile(draft, profiles[1], context, remembered);
  assert.equal(draft.harness, 'codex');
  assert.equal(draft.model, 'gpt-5.6');
  assert.equal(draft.sandbox, 'danger-full-access');
  assert.equal(draft.sandboxImpl, '',
    'a legacy native-off profile preserves its independently resolved implementation');
  assert.equal(draft.approval, 'on-request');
  assert.equal(draft.approvalReviewer, 'auto_review');
  assert.equal(model.spawnProfileSeed(draft, context).auto_review, true);
  assert.equal(model.spawnProfileSeed(draft, context).sandbox_implementation, undefined);
  assert.match(
    model.sandboxImplHintFor(draft, model.spawnCapabilityView(draft, context)).text,
    /Legacy Codex CLI sandbox mode Off is preserved/,
  );
  assert.equal(draft.trustDirSpecified, true, 'profile false is explicit');
  assert.equal(draft.remoteControl, false, 'unsupported hidden remote state is cleared');

  // A profile that TURNED TRUST ON must not leak into the next profile applied.
  // Note the leak is only reachable through a profile with NO harness field: a
  // profile that names one goes through selectSpawnHarness, whose
  // harnessDefaults already resets both fields. A harness-less sparse profile
  // (role/initial_message only) skips that path entirely, so the trust branch
  // is the sole thing standing between a stale opt-in and an unasked-for
  // pre-trust — plus, because trustDirSpecified pins an explicit value, a
  // suppressed group/global default. Same rule the reviewer field follows.
  const trusting = model.applySpawnProfile(
    draft, { ...profiles[1], trust_dir: true }, context, remembered,
  );
  assert.equal(trusting.trustDir, true, 'an explicit profile opt-in is honoured');
  assert.equal(trusting.trustDirSpecified, true);
  const thenSparse = model.applySpawnProfile(
    trusting, { name: 'sparse-no-harness', role: 'navigator' }, context, remembered,
  );
  assert.equal(thenSparse.harness, 'codex', 'a harness-less profile leaves the harness alone');
  assert.equal(thenSparse.trustDir, false, 'a sparse profile clears a prior trust-dir opt-in');
  assert.equal(thenSparse.trustDirSpecified, false, 'and stops pinning it, so the tier stack decides');
  assert.equal('trust_dir' in model.buildSpawnRequest(
    { ...thenSparse, name: 'w' }, context, { path: '', branch: '' },
  ).body, false, 'so the request omits trust_dir entirely');
  assert.equal(model.spawnCapabilityView(draft, context).sandboxProfilesDisabled, true);
  assert.equal(model.approvalControlsVisibleFor({
    harness: 'codex', sandbox: 'tclaude-agent', sandboxImpl: '',
  }, 'harness-builtin'), true, 'resolved Codex built-in sandbox exposes approval controls');
  assert.equal(model.approvalControlsVisibleFor({
    harness: 'codex', sandbox: 'tclaude-agent', sandboxImpl: 'tclaude-layer',
  }, 'harness-builtin'), false, 'an explicit tclaude wall wins over the resolved default');
  assert.equal(model.approvalControlsVisibleFor({
    harness: 'codex', sandbox: 'danger-full-access', sandboxImpl: 'harness-builtin',
  }), false, 'Codex native sandbox Off cannot produce a boundary approval');
  assert.equal(model.approvalControlsVisibleFor({
    harness: 'codex', sandbox: 'tclaude-agent', sandboxImpl: 'stacked',
  }), true, 'stacked still runs Codex inside its own sandbox');
  assert.equal(model.approvalControlsVisibleFor({
    harness: 'claude', sandbox: 'off', sandboxImpl: 'off',
  }), true, 'Claude Code permission controls are independent of sandboxing');

  const openCode = model.selectSpawnHarness(draft, 'opencode', context);
  assert.equal(openCode.sandbox, 'off');
  assert.equal(openCode.tools, 'allow');
  assert.equal(model.spawnCapabilityView(openCode, context).sandboxProfilesDisabled, false,
    'legacy native Off remains independent from the inherited implementation');

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
    remoteControl: false, owner: true, permissionOverrides: { 'groups.members.spawn': 'grant' },
    cwd: '/mono', wtRepo: '/mono/sub', profile: 'group-default',
  };
  const request = model.buildSpawnRequest(
    draft, context, { path: '/tmp/wt', branch: 'worker' }, ['/tmp/a.png'],
  );
  assert.equal(request.url, '/api/groups/alpha/spawn');
  assert.deepEqual(request.body, {
    name: 'worker', role: 'reviewer', role_refs: [], descr: 'does review', initial_message: 'ship it',
    auto_focus: true, include_group_context: true, profile: 'group-default',
    attachments: ['/tmp/a.png'], effort: 'high', model: 'opus',
    task_ref_url: 'https://linear.app/TCL-458', harness: 'claude', sandbox: 'on',
    sandbox_profile: 'strict', approval: 'plan', ask_user_question_timeout: 'never',
    remote_control: false, auto_memory: false, is_owner: true,
    permission_overrides: { 'groups.members.spawn': 'grant' },
    // Always present for a trim-capable harness, empty when nothing is trimmed:
    // the form is the authoritative statement of what the agent loads, so an
    // omitted field would let the daemon's profile tier stack re-apply a
    // profile's trims the operator had cleared (TCL-597).
    context_features: {},
    cwd: '/mono', worktree_path: '/tmp/wt', worktree_branch: 'worker',
  });

  const webRequest = model.buildSpawnRequest(
    draft, { ...context, defaultTerminal: 'web' }, { path: '', branch: '' },
  );
  assert.equal(webRequest.body.auto_focus, true);
  assert.equal(webRequest.body.auto_focus_web, true,
    'web-terminal auto-focus must tell the daemon not to open a native window');

  const codex = model.selectSpawnHarness(draft, 'codex', context);
  const codexBody = model.buildSpawnRequest({
    ...codex, name: 'worker', sandbox: 'danger-full-access', sandboxProfile: 'stale',
    approval: 'on-request', approvalReviewer: 'auto_review',
    trustDir: false, trustDirSpecified: true,
  }, context, { path: '', branch: '' }).body;
  assert.equal(codexBody.trust_dir, false);
  assert.equal('sandbox_profile' in codexBody, false);
  assert.equal(codexBody.omit_sandbox_profiles, true);
  assert.equal('remote_control' in codexBody, false);
  assert.equal(codexBody.approval, 'on-request');
  assert.equal(codexBody.auto_review, true);
	assert.equal(codexBody.codex_app_server, false,
	  'the dashboard posts an authoritative default-off Codex drive choice');
  assert.equal(codexBody.fast_mode, 'inherit', 'dialog default explicitly selects Codex config.toml');
  const fastBody = model.buildSpawnRequest({ ...codex, name: 'fast', fastMode: '1' }, context, null, []).body;
  assert.equal(fastBody.fast_mode, 'on');
  const standardBody = model.buildSpawnRequest({ ...codex, name: 'standard', fastMode: '0' }, context, null, []).body;
  assert.equal(standardBody.fast_mode, 'off');
  const omittedBody = model.buildSpawnRequest({
    ...draft, name: 'worker', sandboxProfile: model.SANDBOX_PROFILE_NONE,
  }, context, { path: '', branch: '' }).body;
  assert.equal(omittedBody.omit_sandbox_profiles, true);
  assert.equal('sandbox_profile' in omittedBody, false);
  const openCode = model.selectSpawnHarness(draft, 'opencode', context);
  const openCodeBody = model.buildSpawnRequest({
    ...openCode, name: 'worker', sandboxProfile: 'stale', tools: 'deny',
  }, context, { path: '', branch: '' }).body;
  assert.equal(openCodeBody.sandbox, 'off');
  assert.equal(openCodeBody.sandbox_profile, 'stale');
  assert.equal(openCodeBody.tools, 'deny');
  const humanBody = model.buildSpawnRequest({
    ...codex, name: 'worker', approvalReviewer: 'human',
  }, context, { path: '', branch: '' }).body;
  assert.equal(humanBody.auto_review, false, 'explicit human review overrides a profile');
});

test('Codex app-server stays capability-gated and profile opt-in never leaks', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = { groups, harnesses, userDefaultModel: '', normalizeNames: true };
  const initial = model.createSpawnDraft({ groups, harnesses, groupName: 'alpha' });
  assert.equal(initial.codexAppServer, false, 'a fresh dialog is default-off');
  assert.equal(model.spawnCapabilityView(initial, context).showCodexAppServer, false,
	'non-Codex surfaces do not offer the drive');

  const codex = model.selectSpawnHarness(initial, 'codex', context);
  assert.equal(model.spawnCapabilityView(codex, context).showCodexAppServer, true);
  const optedIn = model.applySpawnProfile(
	codex, { name: 'api', harness: 'codex', codex_app_server: true }, context,
  );
  assert.equal(optedIn.codexAppServer, true);
  assert.equal(model.buildSpawnRequest({ ...optedIn, name: 'api' }, context, null, [])
	.body.codex_app_server, true);

  const sparse = model.applySpawnProfile(
	optedIn, { name: 'ordinary', harness: 'codex' }, context,
  );
  assert.equal(sparse.codexAppServer, false,
	'a sparse profile returns to default-off instead of inheriting a prior opt-in');
  assert.equal(model.buildSpawnRequest({ ...sparse, name: 'ordinary' }, context, null, [])
	.body.codex_app_server, false);

  const claude = model.selectSpawnHarness(optedIn, 'claude', context);
  assert.equal(claude.codexAppServer, false, 'switching harness clears the opt-in');
  assert.equal('codex_app_server' in model.buildSpawnRequest(
	{ ...claude, name: 'claude-worker' }, context, null, [],
  ).body, false, 'unsupported surfaces omit the field entirely');
});

test('native Codex registry warning is scoped to app-server builtin sandbox', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const unavailable = {
    codex_native_registry_ready: false,
    codex_native_registry_reason: 'managed target mode must be 0700; see '
      + model.CODEX_NATIVE_REGISTRY_SETUP_DOC,
  };
  const draft = {
    harness: 'codex', codexAppServer: true, sandbox: 'tclaude-agent', sandboxImpl: '',
  };
  assert.equal(model.nativeCodexRegistryWarningFor(draft, unavailable, 'harness-builtin'),
    'managed target mode must be 0700');
  assert.equal(model.nativeCodexRegistryWarningFor(draft, unavailable, 'tclaude-layer'), '',
    'the outer tclaude sandbox has no native-registry setup dependency');
  assert.equal(model.nativeCodexRegistryWarningFor({ ...draft, codexAppServer: false }, unavailable), '');
  assert.equal(model.nativeCodexRegistryWarningFor(draft, { codex_native_registry_ready: true }), '');
});

test('agent-spawn state snapshots opens and invalidates every async generation', async (t) => {
  const harness = await createPreactHarness(t);
  const { createAgentSpawnState } = await harness.importDashboardModule('js/agent-spawn-state.js');
  let snapshot = {
    groups, harnesses, user_default_model: 'sonnet', spawn_name_normalize: false,
    default_terminal: 'web',
  };
  const state = createAgentSpawnState({ getSnapshot: () => snapshot });
  state.open({ groupName: 'alpha', role: 'reviewer' });
  const first = state.dialog.value;
  assert.equal(first.groups.length, 2);
  assert.equal(first.normalizeNames, false);
  assert.equal(first.defaultTerminal, 'web');
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
  let paneResult = Promise.resolve({ key: 'window:worker' });
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
    openTerminalPane: (...args) => { calls.push(['pane', ...args]); return paneResult; },
    celebrateSlop: () => calls.push(['slop']),
    celebrateWizard: () => calls.push(['wizard']),
    recordInteraction: (...args) => calls.push(['interaction', ...args]),
    shortID: (value) => value.slice(0, 8),
  });
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const omittedPolicy = await actions.loadSandboxPolicy('alpha', model.SANDBOX_PROFILE_NONE);
  assert.equal(omittedPolicy.selected, model.SANDBOX_PROFILE_NONE);
  assert.match(omittedPolicy.preview, /No tclaude sandbox-profile values/);
  assert.equal(calls.filter(([, url]) => url?.includes('sandbox-profile-default')).length, 0,
    'an explicit omission does not fetch or preview ambient assignments');
  actions.rememberLaunchPreferences({ autoFocus: false, model: 'opus', effort: 'high' });
  assert.equal(actions.autoFocusDefault(), false);
  assert.equal(actions.rememberedEffort('opus'), 'high');
  await assert.rejects(() => actions.spawn({ url: '/spawn', body: {} }), /permission denied/);

  response = {
    ok: true, status: 200,
    json: async () => ({ conv_id: '1234567890', focus_mode: 'browser', focus_ws: '/ws' }),
  };
  const payload = await actions.spawn({ url: '/spawn', body: { name: 'worker' } });
  await actions.complete(payload, { name: 'worker', group: 'alpha', autoFocus: true });
  assert.ok(calls.some(([kind]) => kind === 'pane'));
  assert.ok(calls.some(([kind, message]) => kind === 'notify' && /opened in Terminals tab/.test(message)));
  assert.ok(calls.some(([kind]) => kind === 'refresh'));

  const beforeWebCompletion = calls.length;
  await actions.complete(
    {
      agent_id: 'agt_webworker', conv_id: 'abcdef1234', label: 'web-worker', focus_mode: 'browser',
      focus_ws: '/api/spawn-focus-ws/web-worker',
    },
    { name: 'web-worker', group: 'alpha', autoFocus: true, harness: 'copilot' },
  );
  const webCompletionCalls = calls.slice(beforeWebCompletion);
  assert.deepEqual(
    webCompletionCalls.find(([kind]) => kind === 'pane')?.[1],
    {
      ws: '/api/spawn-focus-ws/web-worker', label: 'web-worker', key: 'window:agt_webworker',
      hideConv: 'abcdef1234', agent: 'agt_webworker', harness: 'copilot',
    },
    'web-terminal preference opens a label-keyed pane in the Terminals tab',
  );
  assert.ok(webCompletionCalls.some(([kind, message]) => kind === 'notify'
    && /opened in Terminals tab/.test(message)));

  paneResult = Promise.resolve(null);
  const beforeMissingPane = calls.length;
  await actions.complete(payload, { name: 'missing-pane', group: 'alpha', autoFocus: true });
  const missingPaneCalls = calls.slice(beforeMissingPane);
  assert.equal(missingPaneCalls.some(([kind, message]) => kind === 'notify'
    && /opened in Terminals tab/.test(message)), false,
    'a null pane result must not claim success');
  assert.ok(missingPaneCalls.some(([kind, message, isError]) => kind === 'notify'
    && /terminal pane did not open/.test(message) && isError === true));

  paneResult = Promise.reject(new Error('runtime unavailable'));
  const beforeRejectedPane = calls.length;
  await actions.complete(payload, { name: 'rejected-pane', group: 'alpha', autoFocus: true });
  const rejectedPaneCalls = calls.slice(beforeRejectedPane);
  assert.equal(rejectedPaneCalls.some(([kind, message]) => kind === 'notify'
    && /opened in Terminals tab/.test(message)), false,
    'a rejected pane open must not claim success');
  assert.ok(rejectedPaneCalls.some(([kind, message, isError]) => kind === 'notify'
    && /terminal pane failed: runtime unavailable/.test(message) && isError === true));

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

test('spawn sandbox-policy preview surfaces daemon-owned access warnings before confirmation', async (t) => {
  const harness = await createPreactHarness(t);
  const { createAgentSpawnActions } = await harness.importDashboardModule('js/agent-spawn-actions.js');
  const requested = [];
  const json = (body) => ({ ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) });
  const actions = createAgentSpawnActions({
    fetchImpl: async (url, options = {}) => {
      requested.push([url, options]);
      if (url === '/api/sandbox-profile-default') return json({ name: 'net' });
      if (url.includes('/api/groups/')) return json({ name: '' });
      if (url === '/api/sandbox-profile-enforcement') return json({
        targets: [{ axes: {
          network: { outcome: 'not_enforced', detail: 'resolver says network list is not enforced' },
          unix_sockets: { outcome: 'enforced', detail: 'socket policy enforced' },
        } }],
        contexts: [{ notices: [{ class: 'composition', detail: 'global and group lists have an empty intersection' }] }],
      });
      throw new Error(`unexpected URL ${url}`);
    },
    prefs: { getItem: () => null, setItem() {} },
    loadProfiles: async () => [],
    loadSandboxProfiles: async () => [{
      id: 7, name: 'net', filesystem: [], environment: [], includes: [], agent_directories: [],
      network: { mode: 'list', allow: [{ domain: 'example.com' }] },
      unix_sockets: { mode: 'closed' },
    }],
    getDashboardDefaultProfile: () => '',
    pickDirectory: async () => ({ canceled: true }),
    openProfileEditor() {}, openPermissions() {}, confirm: async () => true,
    notify() {}, refresh() {}, openTerminal() {}, celebrateSlop() {}, celebrateWizard() {},
    recordInteraction() {}, shortID: (value) => value,
  });
  const preview = await actions.loadSandboxPolicy('crew', '');
  assert.match(preview.preview, /empty intersection/);
  assert.match(preview.preview, /resolver says network list is not enforced/);
  const prediction = requested.find(([url]) => url === '/api/sandbox-profile-enforcement');
  assert.equal(prediction[1].method, 'POST');
  assert.equal(JSON.parse(prediction[1].body).draft.id, 7);
});

test('worktree creation reports backend-owned config-lock retry progress', async (t) => {
  const harness = await createPreactHarness(t);
  const { createAgentSpawnActions } = await harness.importDashboardModule('js/agent-spawn-actions.js');
  const progress = [];
  let finishCreate;
  let postedProgressID = '';
  let polledProgressID = '';
  const json = (body) => ({
    ok: true, status: 200,
    json: async () => body,
    text: async () => JSON.stringify(body),
  });
  const actions = createAgentSpawnActions({
    fetchImpl: async (url, options = {}) => {
      if (url === '/api/worktrees') {
        postedProgressID = JSON.parse(options.body).progress_id;
        return new Promise((resolve) => { finishCreate = () => resolve(json({
          path: '/repo-worker', branch: 'worker', tracking_retries: 2, tracking_fallback: false,
        })); });
      }
      if (url.startsWith('/api/worktrees/progress?id=')) {
        polledProgressID = decodeURIComponent(url.split('=')[1]);
        setTimeout(() => finishCreate(), 10);
        return json({ retrying: true, attempt: 2, max: 10 });
      }
      throw new Error(`unexpected URL ${url}`);
    },
  });

  const selected = await actions.resolveWorktree({
    worktree: '__new__', wtRepo: '/repo', worktreeBranch: 'worker', worktreeBase: 'main',
  }, {
    phase: 'ready', repo: '/repo', repoRoot: '/repo', worktrees: [],
  }, (message) => progress.push(message));

  assert.deepEqual(selected, { path: '/repo-worker', branch: 'worker' });
  assert.equal(polledProgressID, postedProgressID, 'the poll observes the same server operation');
  assert.ok(progress.some((message) => /retrying upstream setup \(2\/10\)/.test(message)));
});

async function mountSpawn(t, overrides = {}) {
  const harness = await createPreactHarness(t);
  const [{ AgentSpawnApp }, { createAgentSpawnState }] = await Promise.all([
    harness.importDashboardModule('js/agent-spawn-island.js'),
    harness.importDashboardModule('js/agent-spawn-state.js'),
  ]);
  const state = createAgentSpawnState({
    getSnapshot: () => ({
      groups, harnesses, roles, sandbox_impl: sandboxImpl, user_default_model: 'sonnet',
    }),
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
    loadUnsandboxedAutonomy: async () => ({ info: [], warnings: [], sandboxState: '', sandboxSource: '' }),
    loadLaunchDefaults: async (group, profileHandle, harnessName) => {
      calls.push(['launch-defaults', group, profileHandle, harnessName]);
      // Deliberately NOT the harness default: a regression that dropped the
      // request and mapped the local sandboxImplDefault would render
      // "…built-in" and has to be distinguishable from reading this answer.
      return {
        harness: harnessName || 'claude', sandbox: '',
        implementation: 'tclaude-layer', resolved_by: 'group default profile "crew"',
      };
    },
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

test('spawn role chips compose multiple roles, inspect details, and feed the permission preview', async (t) => {
  const mounted = await mountSpawn(t);
  const { harness, host, state, calls } = mounted;
  state.open({ groupName: 'alpha' });
  await flush(harness);

  assertAbsent(host.querySelector('.spawn-form-section'));
  assertAbsent(host.querySelector('#agent-spawn-roles-hint'));
  assert.equal(host.querySelector('.agent-spawn-roles-row').nextElementSibling.classList.contains('spawn-role-row'), true,
    'Roles remains a normal row directly above display identity');

  let add = host.querySelector('#agent-spawn-role-add');
  assert.equal(add.options[0].textContent, '＋ Add role…');
  harness.document.body.classList.add('wizard');
  harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:wizard', { detail: { active: true } }));
  await flush(harness);
  add = host.querySelector('#agent-spawn-role-add');
  assert.equal(add.options[0].textContent, '＋ Add class…');
  assert.equal(add.getAttribute('aria-label'), 'Add class');
  harness.document.body.classList.remove('wizard');
  harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:wizard', { detail: { active: false } }));
  await flush(harness);
  add = host.querySelector('#agent-spawn-role-add');
  for (const role of ['reviewer', 'go-maintainer']) {
    setValue(add, role);
    await harness.act(() => harness.fireEvent(add, 'change'));
  }
  assert.deepEqual([...host.querySelectorAll('.agent-spawn-role-chip .role-chip-inspect')]
    .map((button) => button.textContent), ['reviewer', 'go-maintainer']);

  const reviewer = host.querySelector('.agent-spawn-role-chip .role-chip-inspect');
  assert.match(reviewer.title, /Review carefully/);
  assert.match(reviewer.title, /human.notify/);
  reviewer.click();
  await flush(harness);
  assert.match(host.querySelector('.agent-spawn-role-inspect').textContent, /Review carefully/);

  host.querySelector('#agent-spawn-perms').click();
  const descriptor = calls.find(([kind]) => kind === 'permissions')?.[1];
  assert.deepEqual(descriptor.roleGrants, [
    { slug: 'human.notify', scope: '', role: 'reviewer' },
    { slug: 'groups.members.spawn', scope: 'group=alpha', role: 'go-maintainer' },
  ]);
  await mounted.cleanup();
});

test('Preact agent-spawn renders the unenforced-rules checkbox under the sandbox profile', async (t) => {
  const mounted = await mountSpawn(t);
  const { harness, host, state } = mounted;
  try {
    state.open({ groupName: 'alpha' });
    await flush(harness);

    const checkbox = host.querySelector('#agent-spawn-allow-unenforced-sandbox');
    assert.ok(checkbox);
    assert.equal(checkbox.hasAttribute('checked'), false);
    const row = checkbox.closest('.cron-create-row');
    assert.equal(row.id, 'agent-spawn-allow-unenforced-sandbox-row');
    assert.equal(row.previousElementSibling.id, 'agent-spawn-sandbox-profile-row');
    const label = checkbox.closest('label');
    assert.match(label.textContent, /Allow launch with unenforced rules/);
    assert.match(label.title, /Operator-only escape hatch/);
    assert.match(label.title, /individual network deny rules/);
    assert.match(label.title, /not saved and starts unchecked every time/);
    const description = host.querySelector('#agent-spawn-allow-unenforced-sandbox-description');
    assert.equal(checkbox.getAttribute('aria-describedby'), description.id);
    assert.match(description.className, /spawn-field-description/);
    assert.equal(description.textContent, label.title);
    assert.equal(label.contains(description), false,
      'the description must not become part of the checkbox accessible name');
    assertAbsent(host.querySelector('#agent-spawn-allow-unenforced-sandbox-hint'));

    Object.defineProperty(checkbox, 'checked', {
      configurable: true, writable: true, value: true,
    });
    await harness.act(() => harness.fireEvent(checkbox, 'change'));
    assert.equal(checkbox.checked, true);

    const harnessSelect = host.querySelector('#agent-spawn-harness');
    setValue(harnessSelect, 'codex');
    await harness.act(() => harness.fireEvent(harnessSelect, 'change'));
    assert.equal(
      host.querySelector('#agent-spawn-allow-unenforced-sandbox').hasAttribute('checked'), false,
      'switching harnesses consumes the UI authorization');
  } finally {
    mounted.cleanup();
  }
});

test('Preact agent-spawn owner renders profile/custom/capability states without remounting on refresh', async (t) => {
  const mounted = await mountSpawn(t);
  const { harness, host, state, calls } = mounted;
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
  assertSameNode(host.querySelector('#agent-spawn-name'), sameNameNode, 'source refresh preserves the keyed draft DOM');
  assert.equal(host.querySelector('#agent-spawn-name').value, 'my worker');

  // Claude Code has its own trust-folder dialog, so the checkbox is offered
  // here too — and its copy names the file the opt-in would edit, which differs
  // per harness and is what the operator is consenting to.
  const trustRow = host.querySelector('#agent-spawn-trust-dir-row');
  assert.equal(trustRow.hidden, false, 'claude offers the trust-dir checkbox');
  assert.match(trustRow.textContent, /~\/\.claude\.json/, 'claude copy names its own store');

  const harnessSelect = host.querySelector('#agent-spawn-harness');
  setValue(harnessSelect, 'codex');
  await harness.act(() => harness.fireEvent(harnessSelect, 'change'));
  await flush(harness);
  assert.equal(host.querySelector('#agent-spawn-model-codex-row').hidden, false);
  assert.equal(host.querySelector('#agent-spawn-approval-row').hidden, true);
  assert.equal(host.querySelector('#agent-spawn-approval-reviewer-row').hidden, true);
  assert.equal(host.querySelector('#agent-spawn-remote-control-row').hidden, true);
  assert.equal(host.querySelector('#agent-spawn-trust-dir-row').hidden, false);
  assert.match(host.querySelector('#agent-spawn-trust-dir-row').textContent, /~\/\.codex\/config\.toml/,
    'codex copy names its own store, not the one the previous harness would edit');
  assert.equal(
    [...host.querySelector('#agent-spawn-sandbox-impl').options]
      .find((option) => option.value === 'harness-builtin').textContent,
    'Codex CLI built-in',
    'the option names the implementation only; its caveat lives in the hint below the row',
  );
  // The blank row NAMES what the DAEMON said a blank field resolves to, in the
  // same words as the concrete option — not the mechanism that produced it, and
  // not the harness default the browser could have guessed on its own.
  assert.equal(host.querySelector('#agent-spawn-sandbox-impl').options[0].textContent,
    '— Resolved default (tclaude built-in OS sandbox (experimental)) —');
  assert.ok(
    calls.some(([kind, , , harnessName]) => kind === 'launch-defaults' && harnessName === 'codex'),
    'the resolved default is re-asked for the newly selected harness',
  );
  assertAbsent(host.querySelector('#agent-spawn-sandbox-impl-hint'), 'an inherited target stays neutral because the profile chain has not resolved yet');
  assert.equal(host.querySelector('#agent-spawn-sandbox-row').hidden, true,
    'the Codex mode stays hidden until its built-in sandbox is explicitly selected');
  const codexImpl = host.querySelector('#agent-spawn-sandbox-impl');
  setValue(codexImpl, 'harness-builtin');
  await harness.act(() => harness.fireEvent(codexImpl, 'change'));
  assertAbsent(host.querySelector('#agent-spawn-sandbox-impl-hint'), 'the Codex caveat is disclosure copy, never a paragraph under the row');
  const codexCaveatTrigger = host.querySelector('#agent-spawn-sandbox-impl-row .spawn-field-help-trigger');
  assert.equal(codexCaveatTrigger.textContent, '!',
    'a caveat marks its own trigger so it is not mistaken for ordinary field help');
  assert.ok(codexCaveatTrigger.classList.contains('warn'));
  assert.equal(codexCaveatTrigger.getAttribute('aria-label'), 'Show Sandbox warning',
    'the warn state is named, not left as colour a screen reader cannot see');
  // The description span is always in the DOM (the reveal is pure CSS on
  // aria-expanded), so reading its text alone would pass even if the trigger
  // never opened. Assert the toggle itself.
  assert.equal(codexCaveatTrigger.getAttribute('aria-expanded'), 'false');
  assert.equal(codexCaveatTrigger.getAttribute('aria-controls'), 'agent-spawn-sandbox-impl-help');
  await harness.act(() => harness.fireEvent(codexCaveatTrigger, 'click'));
  assert.equal(codexCaveatTrigger.getAttribute('aria-expanded'), 'true');
  assert.match(host.querySelector('#agent-spawn-sandbox-impl-help').textContent,
    /upstream proxy is experimental and off by default/);
  await harness.act(() => harness.fireEvent(codexCaveatTrigger, 'click'));
  assert.equal(codexCaveatTrigger.getAttribute('aria-expanded'), 'false',
    'the trigger is a plain toggle');
  assert.equal(host.querySelector('#agent-spawn-approval-row').hidden, false);
  assert.equal(host.querySelector('#agent-spawn-approval-reviewer-row').hidden, false);
  assert.match(host.querySelector('#agent-spawn-approval').textContent, /Never ask — no approval prompts/);
  const reviewer = host.querySelector('#agent-spawn-approval-reviewer');
  setValue(reviewer, 'auto_review');
  await harness.act(() => harness.fireEvent(reviewer, 'change'));
  assert.match(host.querySelector('#agent-spawn-approval-reviewer-hint').textContent, /No effect with/);
  const codexMode = host.querySelector('#agent-spawn-sandbox');
  assert.equal(codexMode.closest('.cron-create-row').hidden, false);
  assert.match(codexMode.closest('.cron-create-row').textContent, /Codex sandbox mode/);
  assert.deepEqual([...codexMode.options].map((option) => option.value), ['tclaude-agent']);
  assert.match(codexMode.options[0].textContent,
    /Managed workspace \+ agent coordination \(tclaude-agent, recommended\)/);
  setValue(codexImpl, '');
  await harness.act(() => harness.fireEvent(codexImpl, 'change'));
  setValue(harnessSelect, 'opencode');
  await harness.act(() => harness.fireEvent(harnessSelect, 'change'));
  assert.equal(host.querySelector('#agent-spawn-trust-dir-row').hidden, true,
    'a harness with no trust dialog hides the checkbox');
  const openCodeMode = host.querySelector('#agent-spawn-sandbox');
  assert.equal(openCodeMode.closest('.cron-create-row').hidden, true,
    'OpenCode has no built-in OS sandbox, so it has no nested mode control');
  const openCodeSandbox = host.querySelector('#agent-spawn-sandbox-impl');
  assert.deepEqual([...openCodeSandbox.options].map((option) => option.value),
    ['', 'tclaude-layer', 'stacked', 'off']);
  assert.equal(host.querySelector('#agent-spawn-sandbox-profile-row').hidden, false);
  const openCodeTools = host.querySelector('#agent-spawn-tools');
  assert.equal(openCodeTools.closest('.cron-create-row').hidden, false);
  assert.deepEqual([...openCodeTools.options].map((option) => option.value), ['allow', 'ask', 'deny']);
  assert.equal(selectedValue(openCodeTools), 'allow');
  setValue(harnessSelect, 'claude');
  await harness.act(() => harness.fireEvent(harnessSelect, 'change'));
  assert.equal(host.querySelector('#agent-spawn-tools-row').hidden, true);
  const claudeSandbox = host.querySelector('#agent-spawn-sandbox-impl');
  setValue(claudeSandbox, 'harness-builtin');
  await harness.act(() => harness.fireEvent(claudeSandbox, 'change'));
  const claudeMode = host.querySelector('#agent-spawn-sandbox');
  assert.equal(claudeMode.closest('.cron-create-row').hidden, false);
  assert.match(claudeMode.closest('.cron-create-row').textContent, /Claude Code sandbox mode/);
  assert.deepEqual([...claudeMode.options].map((option) => option.value), ['inherit', 'on'],
    'off belongs to the primary Sandbox selector, not the nested Claude mode selector');
  mounted.cleanup();
});

// The caveat used to ride in the option label, so an untouched Codex row
// carried it for free. The label now names the implementation only, which left
// the commonest Codex spawn — row never touched — silent about the one thing
// that implementation cannot do. The hint has to cover the blank row too, using
// the daemon's answer rather than a guess.
test('an untouched Codex row still discloses the built-in sandbox network gap', async (t) => {
  const mounted = await mountSpawn(t, {
    loadLaunchDefaults: async (_group, _profile, harnessName) => ({
      harness: harnessName || 'claude', sandbox: '',
      implementation: 'harness-builtin', resolved_by: 'harness default',
    }),
  });
  const { harness, host, state } = mounted;
  try {
    state.open({ groupName: 'alpha' });
    await flush(harness);
    await harness.act(() => new Promise((resolve) => setTimeout(resolve, 50)));
    await flush(harness);
    const claudeMode = host.querySelector('#agent-spawn-sandbox');
    assert.equal(selectedValue(host.querySelector('#agent-spawn-sandbox-impl')), '',
      'the initial Claude implementation row is untouched');
    assert.equal(claudeMode.closest('.cron-create-row').hidden, false,
      'a resolved Claude built-in default reveals its native mode');
    assert.match(claudeMode.closest('.cron-create-row').textContent, /Claude Code sandbox mode/);

    const harnessSelect = host.querySelector('#agent-spawn-harness');
    setValue(harnessSelect, 'codex');
    await harness.act(() => harness.fireEvent(harnessSelect, 'change'));
    await flush(harness);

    assert.equal(selectedValue(host.querySelector('#agent-spawn-sandbox-impl')), '',
      'the row is untouched — this is the resolved-default path, not an explicit pick');
    assert.equal(host.querySelector('#agent-spawn-sandbox-impl').options[0].textContent,
      '— Resolved default (Codex CLI built-in) —');
    // A blank row the daemon resolved to Codex built-in must still disclose the
    // gap — through the row's [!], which is visible without costing the dialog
    // five lines of copy that apply to one harness.
    const trigger = host.querySelector('#agent-spawn-sandbox-impl-row .spawn-field-help-trigger');
    assert.equal(trigger.textContent, '!');
    assert.ok(trigger.classList.contains('warn'));
    await harness.act(() => harness.fireEvent(trigger, 'click'));
    assert.equal(trigger.getAttribute('aria-expanded'), 'true');
    assert.match(host.querySelector('#agent-spawn-sandbox-impl-help').textContent,
      /no filtered network sandbox yet/);
    const codexMode = host.querySelector('#agent-spawn-sandbox');
    assert.equal(codexMode.closest('.cron-create-row').hidden, false,
      'a resolved Codex built-in default reveals its native mode');
    assert.match(codexMode.closest('.cron-create-row').textContent, /Codex sandbox mode/);
    assert.equal(host.querySelector('#agent-spawn-approval-row').hidden, false);
    assert.equal(host.querySelector('#agent-spawn-approval-reviewer-row').hidden, false);
  } finally {
    mounted.cleanup();
  }
});

test('the primary Sandbox off choice keeps a visible forced-none sandbox-profile choice', async (t) => {
  const mounted = await mountSpawn(t);
  const { harness, host, state } = mounted;
  try {
    state.open({ groupName: 'alpha' });
    await flush(harness);
    const harnessSelect = host.querySelector('#agent-spawn-harness');
    setValue(harnessSelect, 'codex');
    await harness.act(() => harness.fireEvent(harnessSelect, 'change'));
    const codexSandbox = host.querySelector('#agent-spawn-sandbox-impl');
    setValue(codexSandbox, 'off');
    await harness.act(() => harness.fireEvent(codexSandbox, 'change'));
    assert.equal(host.querySelector('#agent-spawn-sandbox-row').hidden, true);
    assert.match(host.querySelector('#agent-spawn-sandbox-impl-hint').textContent,
      /Sandbox OFF/);
    assert.equal(host.querySelector('#agent-spawn-approval-row').hidden, true);
    assert.equal(host.querySelector('#agent-spawn-approval-reviewer-row').hidden, true);

    const forcedNone = host.querySelector('#agent-spawn-sandbox-profile');
    assert.equal(forcedNone.closest('.cron-create-row').hidden, false,
      'the forced omission is explained instead of hiding the field');
    assert.equal(forcedNone.disabled, true);
    assert.match(forcedNone.options[0].textContent, /none.*required/i);

    setValue(codexSandbox, 'harness-builtin');
    await harness.act(() => harness.fireEvent(codexSandbox, 'change'));
    assert.equal(host.querySelector('#agent-spawn-approval-row').hidden, false);
    assert.equal(host.querySelector('#agent-spawn-approval-reviewer-row').hidden, false);
  } finally {
    mounted.cleanup();
  }
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

test('Preact agent-spawn replaces sparse profile fields but keeps manual overrides', async (t) => {
  const mounted = await mountSpawn(t);
  const { harness, host, state } = mounted;
  try {
    state.open({ groupName: 'alpha' });
    await flush(harness);
    await flush(harness);

    assert.equal(selectedValue(host.querySelector('#agent-spawn-load-profile')), 'group-default');
    assert.equal(host.querySelector('#agent-spawn-owner').hasAttribute('checked'), true);
    assert.equal(host.querySelector('#agent-spawn-role').value, 'reviewer');
    assert.match(host.querySelector('#agent-spawn-perms-indicator').textContent, /1 grant/);

    const role = host.querySelector('#agent-spawn-role');
    setValue(role, 'manual-role');
    await harness.act(() => harness.fireEvent(role, 'input'));

    const worktree = host.querySelector('#agent-spawn-worktree');
    setValue(worktree, '__new__');
    await harness.act(() => harness.fireEvent(worktree, 'change'));
    const branch = host.querySelector('#agent-spawn-wt-branch');
    setValue(branch, 'manual-branch');
    await harness.act(() => harness.fireEvent(branch, 'input'));
    assert.equal(host.querySelector('#agent-spawn-wt-new-row').hidden, false);
    assert.equal(host.querySelector('#agent-spawn-wt-branch').value, 'manual-branch');

    const picker = host.querySelector('#agent-spawn-load-profile');
    setValue(picker, 'codex-profile');
    await harness.act(() => harness.fireEvent(picker, 'change'));
    await flush(harness);

    assert.equal(host.querySelector('#agent-spawn-owner').hasAttribute('checked'), false,
      'an owner opt-in from the previous profile must not leak into a sparse profile');
    assert.equal(host.querySelector('#agent-spawn-perms-indicator').hidden, true,
      'permission overrides from the previous profile must be cleared');
    assert.equal(host.querySelector('#agent-spawn-role').value, 'manual-role',
      'a field directly edited by the operator remains a per-spawn override');
    assert.equal(host.querySelector('#agent-spawn-wt-new-row').hidden, false);
    assert.equal(host.querySelector('#agent-spawn-wt-branch').value, 'manual-branch',
      'an explicitly chosen new worktree and custom branch survive profile replacement');
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
  permissions.onSave({ 'groups.members.spawn': 'deny' });
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

// Every dropdown whose help is static per-mode documentation collapses behind a
// [?]; nothing but live validation and a ⚠ caveat is allowed to sit permanently
// under a control. Regressing any mode documentation back to a paragraph is
// what padded the dialog in the first place.
test('Preact agent-spawn collapses mode help behind [?] and keeps only ⚠ caveats visible', async (t) => {
  const mounted = await mountSpawn(t);
  const { harness, host, state } = mounted;
  state.open({ groupName: 'alpha' });
  await flush(harness);

  // The sandbox-profile field keeps its own stable description id; the rest
  // take HelpField's `${id}-hint` default.
  const described = {
    'agent-spawn-sandbox': 'agent-spawn-sandbox-hint',
    'agent-spawn-sandbox-profile': 'agent-spawn-sandbox-profile-preview',
    'agent-spawn-approval': 'agent-spawn-approval-hint',
    'agent-spawn-approval-reviewer': 'agent-spawn-approval-reviewer-hint',
    'agent-spawn-ask-timeout': 'agent-spawn-ask-timeout-hint',
  };
  for (const [id, descriptionID] of Object.entries(described)) {
    const row = host.querySelector(`#${id}-row`);
    assert.ok(row, `${id} renders a row`);
    assert.ok(row.querySelector('.spawn-field-help-trigger'), `${id} exposes a [?] trigger`);
    assert.equal(row.querySelector(`#${id}`).getAttribute('aria-describedby'), descriptionID);
    assert.match(row.querySelector(`#${descriptionID}`).getAttribute('class'), /spawn-field-description/,
      `${id} help is a collapsed description, not a paragraph`);

    // Pin the row's exact shape. Asserting only that a [?] exists would still
    // pass if a help paragraph were reintroduced alongside it, which is the
    // regression this test exists to prevent.
    assert.deepEqual([...row.querySelector('.cron-create-target').children].map((node) => node.className),
      ['spawn-field-with-help'], `${id} renders nothing beside the control group`);
    assert.deepEqual([...row.querySelector('.spawn-field-with-help').children].map((node) => node.tagName),
      ['SELECT', 'BUTTON', 'SPAN'], `${id} renders only the select, its [?], and the collapsed help`);
  }

  // Only persistent, field-specific hints survive; mode explanations remain
  // behind their [?] controls. Count exact ids so accidental prose cannot ride along.
  const persistent = [...host.querySelectorAll('.spawn-field-hint')];
  assert.deepEqual(persistent.map((node) => node.id), ['agent-spawn-name-hint']);

  // Fixture help carries no ⚠, so no caveat line is on screen at all. The
  // caveat path itself is covered against real harness copy in
  // help-field.test.mjs.
  assertAbsent(host.querySelector('.spawn-field-caveat'));
  mounted.cleanup();
});

// The unsandboxed-autonomy warning (TCL-586): the dialog probes the daemon for
// the effective sandbox and, when the daemon says the chosen posture runs
// unconfined, renders the warning as a live alert between the sandbox and
// permission-mode fields. Nothing gates submit on it — it is advisory, and the
// daemon repeats it on the spawn response.
test('Preact agent-spawn shows the daemon unsandboxed-autonomy warning and clears it', async (t) => {
  const probes = [];
  let warn = true;
  const mounted = await mountSpawn(t, {
    loadUnsandboxedAutonomy: async (input) => {
      probes.push(input);
      return warn
        ? { warnings: ['⚠ permission mode "auto" lets this agent run commands unattended, but no sandbox.'], sandboxState: 'unconfigured', sandboxSource: '' }
        : { warnings: [], sandboxState: 'on', sandboxSource: '~/.claude/settings.json' };
    },
  });
  const { harness, host, state } = mounted;
  try {
    state.open({ groupName: 'alpha' });
    await flush(harness);
    // The debounced probe fires on a timer; let it land.
    await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 400)); });
    await flush(harness);

    const alert = host.querySelector('#agent-spawn-autonomy-warning');
    assert.ok(alert, 'the warning row renders when the daemon reports unconfined autonomy');
    assert.equal(alert.querySelector('[role="alert"]').getAttribute('role'), 'alert');
    assert.match(alert.textContent, /run commands unattended/);
    assert.ok(probes.length >= 1, 'the dialog probed the daemon for the effective sandbox');
    assert.equal(probes.at(-1).sandboxImplementation, '',
      'the preview probe includes the currently selected sandbox implementation');

    // Submit is NOT blocked by the warning: it is advisory only.
    assert.equal(host.querySelector('#agent-spawn-submit').disabled, false);

    // A subsequent probe that comes back clean (e.g. the operator picked sandbox
    // `on`) removes the alert rather than leaving a stale warning on screen.
    warn = false;
    const sandbox = host.querySelector('#agent-spawn-sandbox');
    setValue(sandbox, 'on');
    await harness.act(() => harness.fireEvent(sandbox, 'change'));
    await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 400)); });
    await flush(harness);
    assertAbsent(host.querySelector('#agent-spawn-autonomy-warning'), 'a clean re-probe clears the warning');

    const sandboxImpl = host.querySelector('#agent-spawn-sandbox-impl');
    setValue(sandboxImpl, 'tclaude-layer');
    await harness.act(() => harness.fireEvent(sandboxImpl, 'change'));
    await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 400)); });
    await flush(harness);
    assert.equal(probes.at(-1).sandboxImplementation, 'tclaude-layer',
      'changing the implementation re-probes with the new selection');
  } finally {
    mounted.cleanup();
  }
});

test('Preact agent-spawn renders sandbox boundary disclosures as info, not warnings', async (t) => {
  const mounted = await mountSpawn(t, {
    loadUnsandboxedAutonomy: async () => ({
      info: ["OpenCode's tool-executing server runs inside tclaude's built-in OS sandbox."],
      warnings: [],
      sandboxState: '',
      sandboxSource: '',
    }),
  });
  const { harness, host, state } = mounted;
  try {
    state.open({ groupName: 'alpha' });
    await flush(harness);
    const initialNotice = host.querySelector('#agent-spawn-sandbox-info');
    assert.ok(initialNotice, 'the empty status region exists before the async disclosure');
    assert.equal(initialNotice.hidden, false, 'the live region remains in the accessibility tree');
    assert.match(initialNotice.className, /sandbox-info-pending/,
      'the empty live region is visually clipped without using hidden');
    await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 400)); });
    await flush(harness);

    const notice = host.querySelector('#agent-spawn-sandbox-info');
    assert.ok(notice);
    assert.equal(notice.hidden, false);
    assert.doesNotMatch(notice.className, /sandbox-info-pending/);
    assert.equal(notice.querySelector('[role="status"]').getAttribute('role'), 'status');
    assert.match(notice.querySelector('.spawn-field-hint.info').textContent,
      /tclaude's built-in OS sandbox/);
    assertAbsent(notice.querySelector('.spawn-field-hint.warn'));
    assertAbsent(host.querySelector('#agent-spawn-autonomy-warning'));
  } finally {
    mounted.cleanup();
  }
});

// The spawn dialog's auto-memory checkbox. Off is the load-bearing default:
// it is what makes the launch inject CLAUDE_CODE_DISABLE_AUTO_MEMORY=1, so
// several agents on one repo don't cross-pollute Claude Code's shared
// per-project memory store. Codex has no such system, so the control is hidden
// and the field never reaches the wire.
test('spawn dialog defaults auto memory off and hides it for a harness without memory', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = { groups, harnesses, userDefaultModel: '', normalizeNames: true };

  const draft = model.createSpawnDraft({ groups, harnesses, groupName: 'alpha' });
  assert.equal(draft.autoMemory, false, 'a fresh spawn draft must default auto memory off');
  assert.equal(model.spawnCapabilityView(draft, context).showAutoMemory, true,
    'Claude Code exposes the auto-memory control');

  // Opting in reaches the wire.
  const on = model.buildSpawnRequest({ ...draft, name: 'w', autoMemory: true }, context, null, []);
  assert.equal(on.body.auto_memory, true);

  // Codex hides the control and omits the field entirely, rather than sending
  // a value the server would have to reject.
  const codex = model.selectSpawnHarness(draft, 'codex', context);
  assert.equal(model.spawnCapabilityView(codex, context).showAutoMemory, false);
  assert.equal(codex.autoMemory, false, 'switching to a memory-less harness clears the opt-in');
  const codexReq = model.buildSpawnRequest({ ...codex, name: 'w' }, context, null, []);
  assert.equal(codexReq.body.auto_memory, undefined);
});

// A profile that explicitly turned auto memory on pre-fills the dialog; one
// that said nothing leaves the dialog's own default (off) alone.
test('spawn dialog applies a profile auto memory default', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = { groups, harnesses, userDefaultModel: '', normalizeNames: true };
  const draft = model.createSpawnDraft({ groups, harnesses, groupName: 'alpha' });

  const withMemory = model.applySpawnProfile(
    draft, { name: 'keeper', harness: 'claude', auto_memory: true }, context,
  );
  assert.equal(withMemory.autoMemory, true);

  const silent = model.applySpawnProfile(
    draft, { name: 'quiet', harness: 'claude' }, context,
  );
  assert.equal(silent.autoMemory, false, 'a profile that says nothing leaves auto memory off');
});

test('Codex SSH workaround defaults on and a spawn or profile can opt out', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = { groups, harnesses, userDefaultModel: '', normalizeNames: true };
  const initial = model.createSpawnDraft({ groups, harnesses, groupName: 'alpha' });
  const codex = model.selectSpawnHarness(initial, 'codex', context);

  assert.equal(model.spawnCapabilityView(codex, context).showSSHWorkaround, true);
  assert.equal(model.spawnCapabilityView(codex, context).sshWorkaroundAvailable, true);
  assert.equal(codex.sshWorkaround, true, 'managed Codex defaults the workaround on');
  assert.equal(model.buildSpawnRequest({ ...codex, name: 'ssh-on' }, context, null, [])
    .body.ssh_workaround, true);
  assert.equal(model.buildSpawnRequest(
    { ...codex, name: 'ssh-off', sshWorkaround: false }, context, null, [],
  ).body.ssh_workaround, false, 'an unchecked box is authoritative');

  const optedOut = model.applySpawnProfile(
    codex, { name: 'no-ssh', harness: 'codex', ssh_workaround: false }, context,
  );
  assert.equal(optedOut.sshWorkaround, false);
  const inheritedDefault = model.applySpawnProfile(
    optedOut, { name: 'default-ssh', harness: 'codex' }, context,
  );
  assert.equal(inheritedDefault.sshWorkaround, true,
    'a sparse Codex profile returns to the default-on posture');

  const raw = { ...codex, name: 'raw-codex', sandbox: 'workspace-write' };
  assert.equal(model.spawnCapabilityView(raw, context).sshWorkaroundAvailable, false);
  assert.equal(model.buildSpawnRequest(raw, context, null, []).body.ssh_workaround, false,
    'raw Codex modes cannot claim the managed-sandbox workaround is active');

  const layered = { ...raw, name: 'layered-codex', sandboxImpl: 'tclaude-layer' };
  assert.equal(model.spawnCapabilityView(layered, context).sshWorkaroundAvailable, true);
  assert.equal(model.buildSpawnRequest(layered, context, null, []).body.ssh_workaround, true,
    'the daemon applies the enabled workaround only when caller identity needs it');
});

// TCL-609: a policy loaded for a previous selection (or still in flight) must
// never be submit-eligible — the preview the operator is reading could
// describe profile A while the request selects profile B.
//
// TCL-791 removed the break-glass confirmation this guard used to protect, and
// with it the break-glass fingerprint the frozen policy token carried. The
// drift check itself survives — see the post-await test below.
test('Preact agent-spawn blocks submit until the resolved sandbox policy matches the selection', async (t) => {
  const loads = [];
  const spawnRequests = [];
  const mounted = await mountSpawn(t, {
    loadSandboxPolicy: (group, selected) => {
      const pending = deferred();
      loads.push({ group, selected, pending });
      return pending.promise;
    },
    spawn: async (request) => { spawnRequests.push(request); return { conv_id: '1234567890' }; },
  });
  const { harness, host, state } = mounted;
  try {
    state.open({ groupName: 'alpha' });
    await settleWorktrees(harness);
    const name = host.querySelector('#agent-spawn-name');
    setValue(name, 'worker');
    await harness.act(() => harness.fireEvent(name, 'input'));
    await settleWorktrees(harness);

    host.querySelector('#agent-spawn-submit').click();
    await flush(harness);
    assert.equal(spawnRequests.length, 0, 'an unresolved policy blocks submit');
    assert.match(host.querySelector('#agent-spawn-error').textContent, /sandbox policy preview to finish loading/);

    loads[0].pending.resolve({ profiles: [{ name: 'strict' }], selected: '', preview: 'no profiles applied' });
    await flush(harness);
    const picker = host.querySelector('#agent-spawn-sandbox-profile');
    setValue(picker, 'strict');
    await harness.act(() => harness.fireEvent(picker, 'change'));
    assert.equal(loads.length, 2, 'a selection change starts a fresh policy load');

    host.querySelector('#agent-spawn-submit').click();
    await flush(harness);
    assert.equal(spawnRequests.length, 0, 'the previous selection’s resolved policy is not submit-eligible for the new one');
    assert.match(host.querySelector('#agent-spawn-error').textContent, /sandbox policy preview to finish loading/);

    loads[1].pending.resolve({
      profiles: [{ name: 'strict' }], selected: 'strict',
      preview: 'explicit:strict · deny /home/op (explicit)',
    });
    await flush(harness);
    host.querySelector('#agent-spawn-submit').click();
    await flush(harness);
    assert.equal(spawnRequests.length, 1, 'the matching policy is submit-eligible');
    assert.equal(spawnRequests[0].body.sandbox_profile, 'strict');
    assert.equal(spawnRequests[0].body.break_glass_acknowledged, undefined,
      'no surface remains that could attach the retired acknowledgement');
  } finally {
    mounted.cleanup();
  }
});

// The pre-submit guard above is not enough on its own: submit resolves a
// worktree and uploads attachments before it spawns, and the profile can be
// edited elsewhere while those are in flight. The daemon resolves the profile
// by NAME at spawn time, so a spawn that proceeded here would launch the agent
// under a policy whose preview the operator never saw.
//
// This check predates TCL-791 and is not break-glass machinery — it was only
// entangled with it because both hung off the same frozen policy token. It is
// covered here so the next reader does not mistake it for leftovers.
test('Preact agent-spawn aborts when the sandbox policy drifts mid-submit', async (t) => {
  const loads = [];
  const spawnRequests = [];
  let releaseWorktree;
  const mounted = await mountSpawn(t, {
    loadSandboxPolicy: (group, selected) => {
      const pending = deferred();
      loads.push({ group, selected, pending });
      return pending.promise;
    },
    resolveWorktree: () => {
      const pending = deferred();
      releaseWorktree = () => pending.resolve({ path: '', branch: '' });
      return pending.promise;
    },
    spawn: async (request) => { spawnRequests.push(request); return { conv_id: '1234567890' }; },
  });
  const { harness, host, state } = mounted;
  try {
    state.open({ groupName: 'alpha' });
    await settleWorktrees(harness);
    const name = host.querySelector('#agent-spawn-name');
    setValue(name, 'worker');
    await harness.act(() => harness.fireEvent(name, 'input'));
    await settleWorktrees(harness);

    // Resolve for the selection the draft already has, so no second load
    // starts and the pre-submit guard passes.
    loads[0].pending.resolve({
      profiles: [{ name: 'strict' }], selected: '', preview: 'no profiles applied',
    });
    await flush(harness);

    host.querySelector('#agent-spawn-submit').click();
    await flush(harness);
    assert.ok(releaseWorktree, 'submit reached worktree resolution');
    assert.equal(spawnRequests.length, 0, 'spawn waits on worktree resolution');

    // The operator edits the selected profile in another tab.
    await harness.act(() => state.refreshSandboxPolicy());
    releaseWorktree();
    await flush(harness);

    assert.equal(spawnRequests.length, 0,
      'a policy edited mid-submit must not be spawned under without a fresh preview');
    assert.match(host.querySelector('#agent-spawn-error').textContent,
      /sandbox policy changed while spawning/);
  } finally {
    mounted.cleanup();
  }
});

// TCL-597 — startup-context trimming. The subtlest rule in the feature lives
// here rather than server-side: the dialog must send `context_features` even when
// EMPTY, because the form is the authoritative statement of what the agent loads.
// If it omitted the field, the daemon's profile tier stack would silently
// re-apply a profile's trims the operator had just cleared.
test('spawn dialog sends startup-context trims, including an explicit empty set', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = { groups, harnesses, userDefaultModel: '', normalizeNames: true };

  const draft = model.createSpawnDraft({ groups, harnesses, groupName: 'alpha' });
  assert.deepEqual(draft.contextFeatures, {}, 'a fresh draft trims nothing');
  const view = model.spawnCapabilityView(draft, context);
  assert.equal(view.showContextFeatures, true, 'Claude Code exposes the trim control');
  assert.equal(view.contextFeatureCatalog.length, 3, 'the harness catalog reaches the dialog');

  // A selection reaches the wire.
  const trimmed = model.buildSpawnRequest(
    { ...draft, name: 'w', contextFeatures: { 'bundled-skills': 'off', artifact: 'on' } },
    context, null, [],
  );
  assert.deepEqual(trimmed.body.context_features, { 'bundled-skills': 'off', artifact: 'on' });

  // THE load-bearing case: an empty selection is still sent, so it can override
  // a profile tier the daemon would otherwise apply.
  const cleared = model.buildSpawnRequest({ ...draft, name: 'w' }, context, null, []);
  assert.deepEqual(cleared.body.context_features, {},
    'an empty selection must be SENT, not omitted, or the profile tier wins');

  // Codex hides the control and omits the field entirely rather than sending
  // something the server would reject.
  const codex = model.selectSpawnHarness(draft, 'codex', context);
  assert.equal(model.spawnCapabilityView(codex, context).showContextFeatures, false);
  assert.deepEqual(codex.contextFeatures, {}, 'switching to a trim-less harness clears the selection');
  const codexReq = model.buildSpawnRequest({ ...codex, name: 'w' }, context, null, []);
  assert.equal(codexReq.body.context_features, undefined);
});

// A profile's trims REPLACE the form's rather than merging, matching the
// daemon's whole-tier resolution: one profile always tells the whole story of
// what its agents load.
test('spawn dialog replaces startup-context trims when applying a profile', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = { groups, harnesses, userDefaultModel: '', normalizeNames: true };
  const draft = model.createSpawnDraft({ groups, harnesses, groupName: 'alpha' });

  // Start from a hand-picked selection, then apply a profile with a DIFFERENT one.
  const handPicked = { ...draft, contextFeatures: { artifact: 'off' } };
  const applied = model.applySpawnProfile(
    handPicked,
    { name: 'lean', harness: 'claude', context_features: { 'bundled-skills': 'off' } },
    context,
  );
  assert.deepEqual(applied.contextFeatures, { 'bundled-skills': 'off' },
    'the profile replaces the selection outright — no union of the two');

  // A profile that says nothing CLEARS the selection rather than leaving stale
  // trims from a previously-selected profile.
  const silent = model.applySpawnProfile(handPicked, { name: 'quiet', harness: 'claude' }, context);
  assert.deepEqual(silent.contextFeatures, {},
    'a profile with no trims must clear, not inherit, the previous selection');
});

// The dirty check must survive JS object insertion order, or reopening the
// dialog and re-picking the same rows in a different order would falsely warn
// about discarding changes.
test('spawn draft dirty check ignores startup-context key order', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const baseline = {
    ...model.createSpawnDraft({ groups, harnesses, groupName: 'alpha' }),
    contextFeatures: { 'bundled-skills': 'off', artifact: 'on' },
  };
  const reordered = { ...baseline, contextFeatures: { artifact: 'on', 'bundled-skills': 'off' } };
  assert.equal(model.spawnDraftIsDirty(reordered, baseline), false,
    'the same trims in a different key order are not a change');

  const changed = { ...baseline, contextFeatures: { 'bundled-skills': 'off' } };
  assert.equal(model.spawnDraftIsDirty(changed, baseline), true,
    'dropping a trim IS a change');
});

// "Save as profile" seeds only real intent, so a profile never persists a row the
// operator set and then reverted.
test('spawn profile seed carries only steered startup-context trims', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = { groups, harnesses, userDefaultModel: '', normalizeNames: true };
  const draft = model.createSpawnDraft({ groups, harnesses, groupName: 'alpha' });

  const bare = model.spawnProfileSeed(draft, context);
  assert.equal(bare.context_features, undefined,
    'an untouched dialog seeds no context_features at all');

  const seeded = model.spawnProfileSeed(
    { ...draft, contextFeatures: { 'bundled-skills': 'off' } }, context,
  );
  assert.deepEqual(seeded.context_features, { 'bundled-skills': 'off' });
});

// The row must never invent an answer. Three ways the daemon's answer can be
// absent — a failed request, a host that wired the dialog without the action,
// and an answer naming an implementation this harness does not offer — all have
// to land on the unnamed form rather than on a plausible-looking guess.
test('the resolved-default row names nothing when the daemon has not answered', async (t) => {
  for (const [label, override] of [
    ['a failed request', { loadLaunchDefaults: async () => { throw new Error('nope'); } }],
    ['a missing action', { loadLaunchDefaults: undefined }],
    ['an answer with no matching option', {
      loadLaunchDefaults: async () => ({ implementation: 'harness-builtin' }),
    }],
  ]) {
    const mounted = await mountSpawn(t);
    const { harness, host, state, actions } = mounted;
    Object.assign(actions, override);
    if (override.loadLaunchDefaults === undefined) delete actions.loadLaunchDefaults;
    state.open({ groupName: 'alpha' });
    await flush(harness);
    // OpenCode is the harness whose option list omits harness-builtin, which is
    // exactly what a blank field resolves to server-side.
    const harnessSelect = host.querySelector('#agent-spawn-harness');
    setValue(harnessSelect, 'opencode');
    await harness.act(() => harness.fireEvent(harnessSelect, 'change'));
    await flush(harness);
    const row = host.querySelector('#agent-spawn-sandbox-impl');
    assert.ok(row, `${label}: the implementation row must still render`);
    assert.equal(row.options[0].textContent, '— Resolved default —',
      `${label} must leave the resolved default unnamed`);
    mounted.cleanup();
  }
});
