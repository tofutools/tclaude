import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent, assertDifferentNode, assertSameNode } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';

const catalog = [{ name: 'claude', display_name: 'Claude Code', models: ['sonnet'], effort_levels: ['low', 'high'], can_sandbox: true, can_builtin_os_sandbox: true, sandbox_modes: ['inherit', 'on'], default_sandbox: 'inherit', can_approval: true, approval_modes: ['inherit', 'plan'], default_approval: 'inherit', approval_mode_help: { inherit: 'keep settings', plan: 'plan only' }, can_tools: false, tools_modes: [], default_tools: '', tools_mode_help: {}, can_auto_review: false, can_ask_timeout: true, ask_timeout_modes: ['inherit', '60s'], default_ask_timeout: 'inherit', can_remote_control: true, can_auto_memory: true, can_dir_trust: true, dir_trust_store: '~/.claude.json' }, { name: 'codex', display_name: 'Codex CLI', models: [], can_sandbox: true, can_builtin_os_sandbox: true, sandbox_modes: ['workspace-write'], default_sandbox: 'workspace-write', can_approval: true, approval_modes: ['never', 'untrusted', 'on-failure', 'on-request'], default_approval: 'never', approval_mode_help: { never: 'never prompt', untrusted: 'ask for untrusted', 'on-failure': 'deprecated retry', 'on-request': 'ask when requested' }, can_tools: false, tools_modes: [], default_tools: '', tools_mode_help: {}, can_auto_review: true, can_remote_control: false, can_auto_memory: false, can_ssh_workaround: true, can_dir_trust: true, dir_trust_store: '~/.codex/config.toml' }, { name: 'opencode', display_name: 'OpenCode', models: [], effort_levels: [], can_sandbox: true, can_builtin_os_sandbox: false, sandbox_modes: ['access-control', 'tclaude-layer', 'off'], default_sandbox: 'access-control', sandbox_mode_help: { 'access-control': 'soft rules', 'tclaude-layer': 'OS containment', off: '⚠ No tclaude OS containment' }, can_approval: true, approval_modes: ['deny', 'ask', 'allow-tools'], default_approval: 'deny', profile_recommended_approval: 'allow-tools', approval_mode_help: { deny: 'deny edits', ask: 'ask for edits', 'allow-tools': 'allow scoped edits' }, profile_recommended_sandbox_implementation: 'tclaude-layer', can_tools: true, tools_modes: ['allow', 'ask', 'deny'], default_tools: 'allow', tools_mode_help: { allow: 'allow tools', ask: 'ask for tools', deny: 'deny tools' }, can_auto_review: false, can_remote_control: false, can_auto_memory: false, can_dir_trust: false, dir_trust_store: '' }];

const sandboxImpl = {
  options: [
    { value: 'harness-builtin', label: '{harness} built-in' },
    { value: 'tclaude-layer', label: 'tclaude built-in OS sandbox (experimental)' },
    { value: 'stacked', label: 'Stacked: tclaude + {harness} (experimental)' },
    { value: 'off', label: 'Off' },
  ],
  default: 'harness-builtin',
  host_available: true,
};

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

function segmentedValue(control) {
  return control.querySelector('[role="radio"][aria-checked="true"]')?.dataset.value ?? '';
}

function segment(control, value) {
  return control.querySelector(`[role="radio"][data-value="${value}"]`);
}

test('management model preserves full-replace profile and role semantics', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/management-model.js');
  const original = { name: 'old', aliases: ['codex-reviewer'], harness: 'codex', approval: 'never', auto_review: true, model: 'gpt-5', disabled: false, disabled_reason: 'previous outage', operator_only: true };
  const draft = model.profileDraft(original, {}, catalog); draft.name = 'renamed'; draft.trust_dir = '1';
  assert.equal(draft.approval_reviewer, 'auto_review');
  draft.aliases_text += ', cold-reviewer';
  const payload = model.profilePayload(draft, original, catalog);
  assert.equal(payload.name, 'renamed'); assert.equal(payload.approval, 'never'); assert.equal(payload.auto_review, true); assert.equal(payload.trust_dir, true);
  assert.equal(payload.disabled, false); assert.equal(payload.disabled_reason, 'previous outage');
  assert.equal(payload.operator_only, true);
  draft.approval_reviewer = 'human'; assert.equal(model.profilePayload(draft, original, catalog).auto_review, false);
  assert.deepEqual(payload.aliases, ['codex-reviewer', 'cold-reviewer']);
  draft.harness = 'claude'; draft.approval = 'plan'; draft.sandbox = 'on';
  const switched = model.profilePayload(draft, original, catalog);
  // A harness switch drops only the toggles the NEW harness cannot deliver.
  // auto_review is Codex-only, so it falls away; trust_dir is not — Claude Code
  // has its own trust-folder dialog — so the operator's intent carries over
  // rather than being silently discarded.
  assert.equal(switched.approval, 'plan'); assert.equal(switched.auto_review, undefined); assert.equal(switched.trust_dir, true);
  const toOpenCode = model.profilePayload({ ...draft, harness: 'opencode' }, original, catalog);
  assert.equal(toOpenCode.trust_dir, undefined, 'a harness with no trust dialog drops trust_dir');
  const role = model.roleDraft({ name: 'reviewer', permissions: ['read'] }, catalog);
  assert.deepEqual(model.rolePayload(role, catalog).permissions, ['read']);
  const openCodeRole = model.roleDraft({ name: 'locked', harness: 'opencode', tools: 'ask' }, catalog);
  assert.equal(model.rolePayload(openCodeRole, catalog).tools, 'ask');
  const defaults = model.profileDraft(null, {}, catalog); assert.equal(defaults.sandbox, 'inherit'); assert.equal(defaults.approval, 'inherit'); assert.equal(defaults.ask_user_question_timeout, 'inherit');
  const legacyCodex = model.profileDraft({ name: 'legacy', harness: 'codex', approval: '' }, {}, catalog);
  assert.equal(legacyCodex.approval, 'never', 'an empty legacy Codex profile renders the explicit daemon default');
  const legacyPayload = model.profilePayload(legacyCodex, { name: 'legacy', harness: 'codex', approval: '' }, catalog);
  assert.equal(legacyPayload.approval, 'never');
  assert.equal('auto_review' in legacyPayload, false, 'unset reviewer stays sparse for lower-tier resolution');
  const legacyOff = model.profileDraft({
    name: 'legacy-off', harness: 'codex', sandbox: 'danger-full-access',
  }, {}, catalog);
  assert.equal(legacyOff.sandbox_implementation, '',
    'editing a legacy native-off profile preserves its independent implementation tier');
  assert.equal(model.profilePayload(legacyOff, {
    name: 'legacy-off', harness: 'codex', sandbox: 'danger-full-access',
  }, catalog).sandbox_implementation, undefined);
  const recommendedOpenCode = model.profileDraft({ name: 'open', harness: 'opencode' }, {}, catalog);
  assert.equal(recommendedOpenCode.approval, 'allow-tools');
  assert.equal(recommendedOpenCode.sandbox_implementation, 'tclaude-layer');
  const recommendedOpenCodePayload = model.profilePayload(
    recommendedOpenCode, { name: 'open', harness: 'opencode' }, catalog,
  );
  assert.equal(recommendedOpenCodePayload.approval, 'allow-tools');
  assert.equal(recommendedOpenCodePayload.sandbox_implementation, 'tclaude-layer');
  const unconfinedOpenCode = model.profileDraft({
    name: 'open-off', harness: 'opencode', sandbox_implementation: 'off',
  }, {}, catalog);
  assert.equal(unconfinedOpenCode.approval, 'deny',
    'an explicit sandbox opt-out must not independently inherit the autonomous recommendation');
  assert.deepEqual(model.harnessDefaults({ sandbox_modes: ['on'], approval_modes: ['plan'], tools_modes: ['deny'], ask_timeout_modes: ['60s'] }), { sandbox: 'on', approval: 'plan', tools: 'deny', approval_reviewer: '', ask_user_question_timeout: '60s' });
});

test('Codex profile permission modes populate, survive harness switches, save, and reopen', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({
    kind: 'profile-editor',
    seed: { name: 'legacy', harness: 'codex', approval: '', auto_review: true },
    options: {}, catalog, sandboxImpl,
  });
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
  assertAbsent(host.querySelector('#profile-editor-approval-caveat'), 'help with no ⚠ leaves nothing on screen');
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
  const sandboxImplementation = host.querySelector('#profile-editor-sandbox-impl');
  choose(sandboxImplementation, 'off');
  await harness.act(() => harness.fireEvent(sandboxImplementation, 'change'));
  assert.equal(host.querySelector('#profile-editor-approval-row').hidden, true);
  assert.equal(host.querySelector('#profile-editor-approval-reviewer-row').hidden, true);
  choose(sandboxImplementation, 'harness-builtin');
  await harness.act(() => harness.fireEvent(sandboxImplementation, 'change'));
  assert.equal(host.querySelector('#profile-editor-approval-row').hidden, false);
  assert.equal(host.querySelector('#profile-editor-approval-reviewer-row').hidden, false);

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

test('OpenCode profile editor recommends its sandboxed autonomous pair', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({
    kind: 'profile-editor', seed: null,
    options: {}, catalog, sandboxImpl,
  });
  const recommendedSaves = [];
  const recommendedCleanups = [];
  const recommendedHost = harness.document.createElement('div');
  harness.document.body.appendChild(recommendedHost);
  mountManagementIsland({
    host: recommendedHost, state,
    actions: { async saveProfile(value) { recommendedSaves.push(value); }, async loadUnsandboxedAutonomy() { return { warnings: [] }; } },
    confirmDiscard: async () => true, openProfilePermissions() {}, registerCleanup(fn) { recommendedCleanups.push(fn); },
  });
  await harness.act(() => Promise.resolve());
  const name = recommendedHost.querySelector('#profile-editor-name');
  name.value = 'opencode-defaults';
  await harness.act(() => harness.fireEvent(name, 'input'));
  const harnessSelect = recommendedHost.querySelector('#profile-editor-harness');
  choose(harnessSelect, 'opencode');
  await harness.act(() => harness.fireEvent(harnessSelect, 'change'));
  const recommendedSandbox = recommendedHost.querySelector('#profile-editor-sandbox-impl');
  assert.equal(selectedValue(recommendedSandbox), 'tclaude-layer');
  assert.match([...recommendedSandbox.options]
    .find((option) => option.value === 'tclaude-layer').textContent, /recommended/);
  const recommendedApproval = recommendedHost.querySelector('#profile-editor-approval');
  assert.equal(selectedValue(recommendedApproval), 'allow-tools');
  assert.match([...recommendedApproval.options]
    .find((option) => option.value === 'allow-tools').textContent, /recommended/);
  await harness.act(() => harness.fireEvent(recommendedHost.querySelector('#profile-editor-submit'), 'click'));
  assert.equal(recommendedSaves[0].payload.sandbox_implementation, 'tclaude-layer');
  assert.equal(recommendedSaves[0].payload.approval, 'allow-tools');
  recommendedCleanups.reverse().forEach((fn) => fn());
});

test('OpenCode profile editor preserves explicit sandbox and tool overrides', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({
    kind: 'profile-editor',
    seed: {
      name: 'opencode-off', harness: 'opencode',
      sandbox_implementation: 'off', tools: 'deny',
    },
    options: {}, catalog, sandboxImpl,
  });
  const saves = [];
  const cleanups = [];
  const host = harness.document.createElement('div');
  harness.document.body.appendChild(host);
  mountManagementIsland({
    host, state,
    actions: { async saveProfile(value) { saves.push(value); }, async loadUnsandboxedAutonomy() { return { warnings: [] }; } },
    confirmDiscard: async () => true, openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); },
  });
  await harness.act(() => Promise.resolve());

  const sandbox = host.querySelector('#profile-editor-sandbox-impl');
  assert.deepEqual([...sandbox.options].map((option) => option.value),
    ['', 'tclaude-layer', 'stacked', 'off']);
  assert.equal(selectedValue(sandbox), 'off');
  const approval = host.querySelector('#profile-editor-approval');
  assert.equal(selectedValue(approval), 'deny',
    'an unpinned permission mode stays fail-closed when the profile explicitly disables containment');
  assert.equal(host.querySelector('#profile-editor-sandbox-row').hidden, true,
    'OpenCode has no built-in OS sandbox mode to reveal');
  assert.match(host.textContent, /Sandbox OFF/);
  const tools = host.querySelector('#profile-editor-tools');
  assert.equal(tools.closest('.cron-create-row').hidden, false);
  assert.deepEqual([...tools.options].map((option) => option.value), ['allow', 'ask', 'deny']);
  assert.equal(selectedValue(tools), 'deny');
  await harness.act(() => harness.fireEvent(host.querySelector('#profile-editor-submit'), 'click'));
  assert.equal(saves.length, 1);
  assert.equal(saves[0].payload.harness, 'opencode');
  assert.equal(saves[0].payload.sandbox_implementation, 'off');
  assert.equal(saves[0].payload.approval, 'deny');
  assert.equal(saves[0].payload.tools, 'deny');
  const harnessSelect = host.querySelector('#profile-editor-harness');
  choose(harnessSelect, 'claude');
  await harness.act(() => harness.fireEvent(harnessSelect, 'change'));
  choose(harnessSelect, 'opencode');
  await harness.act(() => harness.fireEvent(harnessSelect, 'change'));
  assert.equal(selectedValue(host.querySelector('#profile-editor-sandbox-impl')), 'off');
  assert.equal(selectedValue(host.querySelector('#profile-editor-approval')), 'deny',
    'a preserved Off implementation also suppresses the autonomous recommendation on a harness switch');
  cleanups.reverse().forEach((fn) => fn());
});

// The sandbox-implementation option that means "the harness owns containment"
// must name the harness the operator actually picked. "Harness built-in" reads
// as if tclaude were the harness, which is exactly the confusion this row can
// least afford. The editor renders the same host-wide catalog the spawn dialog
// does, so it has to fill the catalog's {harness} placeholder too.
test('profile editor names the harness-owned sandbox after the selected harness', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({
    kind: 'profile-editor',
    seed: { name: 'impl', harness: 'claude', sandbox_implementation: 'harness-builtin' },
    options: {},
    catalog,
    sandboxImpl,
  });
  const cleanups = [];
  const host = harness.document.createElement('div');
  harness.document.body.appendChild(host);
  mountManagementIsland({
    host, state,
    actions: { async saveProfile() {}, async loadUnsandboxedAutonomy() { return { warnings: [] }; } },
    confirmDiscard: async () => true, openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); },
  });
  await harness.act(() => Promise.resolve());

  const impl = host.querySelector('#profile-editor-sandbox-impl');
  assert.match(impl.closest('.cron-create-row').textContent, /^Sandbox/);
  assert.equal(host.querySelector('#profile-editor-sandbox-row').hidden, false);
  assert.match(host.querySelector('#profile-editor-sandbox-row').textContent,
    /Claude Code sandbox mode/);
  assert.deepEqual(
    [...impl.options].map((option) => option.textContent),
    [
      'Unset (resolved defaults at spawn)',
      'Claude Code built-in',
      'tclaude built-in OS sandbox (experimental)',
      'Stacked: tclaude + Claude Code (experimental)',
      'Off',
    ],
  );

  // The label follows the harness selection, which changes in the browser
  // without refetching the host-wide catalog.
  const harnessSelect = host.querySelector('#profile-editor-harness');
  choose(harnessSelect, 'codex');
  await harness.act(() => harness.fireEvent(harnessSelect, 'change'));
  assert.equal(
    [...host.querySelector('#profile-editor-sandbox-impl').options]
      .find((option) => option.value === 'harness-builtin').textContent,
    'Codex CLI built-in',
  );
  // The caveat left the option label but not the dialog: this draft pins
  // harness-builtin (see the seed above), so the hint under the row states it.
  assert.match(host.textContent, /upstream proxy is experimental and off by default/);
  assert.equal(host.querySelector('#profile-editor-sandbox-row').hidden, false,
    'the explicit harness-builtin selection survives a capable harness switch');

  choose(harnessSelect, 'opencode');
  await harness.act(() => harness.fireEvent(harnessSelect, 'change'));
  assert.deepEqual(
    [...host.querySelector('#profile-editor-sandbox-impl').options].map((option) => option.textContent),
    [
      'Unset (resolved defaults at spawn)',
      'tclaude built-in OS sandbox (experimental) (recommended)',
      'Stacked: tclaude + OpenCode (experimental)',
      'Off',
    ],
  );
  assert.match(host.textContent, /harness-builtin is invalid for OpenCode/);
  assert.equal(host.querySelector('#profile-editor-sandbox-row').hidden, false,
    'an invalid preserved built-in pin stays inspectable until the server refuses or the operator changes it');
  choose(host.querySelector('#profile-editor-sandbox-impl'), 'off');
  await harness.act(() => harness.fireEvent(
    host.querySelector('#profile-editor-sandbox-impl'), 'change',
  ));
  assert.equal(host.querySelector('#profile-editor-sandbox-row').hidden, true);
  assert.match(host.textContent, /Sandbox OFF/);
  cleanups.reverse().forEach((fn) => fn());
});

// TCL-586 follow-up: the profile editor warns, before save, when the chosen
// posture pairs an unattended command-running mode with a sandbox that won't
// confine it. The daemon decides (an explicit `off` is unsafe anywhere;
// `inherit` only when the host's global settings enable no sandbox), so the
// editor probes /api/spawn/effective-sandbox with no dir and renders whatever
// warning comes back.
test('profile editor shows the unsandboxed-autonomy warning and clears it on a safe change', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  // Seed the dangerous pairing directly: the fixture catalog doesn't list
  // off/auto as options, but the draft carries the values the probe reads.
  state.openDialog({ kind: 'profile-editor', seed: { name: 'risky', harness: 'claude', sandbox: 'off', approval: 'auto' }, options: {}, catalog });

  const probes = [];
  const cleanups = [];
  const host = harness.document.createElement('div');
  harness.document.body.appendChild(host);
  // Mimic the daemon: warn only for an unconfined command-running posture.
  const loadUnsandboxedAutonomy = async ({ harness: h, sandbox, approval }) => {
    probes.push({ h, sandbox, approval });
    const dangerous = h === 'claude' && sandbox === 'off' && (approval === 'auto' || approval === 'bypassPermissions');
    return { warnings: dangerous ? [`⚠ permission mode "${approval}" lets this agent run commands unattended, but this launch forces the OS sandbox off.`] : [] };
  };
  mountManagementIsland({
    host, state,
    actions: { async saveProfile() {}, loadUnsandboxedAutonomy },
    confirmDiscard: async () => true, openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); },
  });
  await harness.act(() => Promise.resolve());
  // The probe is debounced; let the timer fire and the state settle.
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 260)); });

  const warning = host.querySelector('#profile-editor-autonomy-warning');
  assert.ok(warning, 'off + auto renders the warning row');
  assert.equal(warning.querySelector('[role="alert"]').getAttribute('role'), 'alert');
  assert.match(warning.textContent, /run commands unattended/);
  assert.ok(probes.some((p) => p.sandbox === 'off' && p.approval === 'auto'), 'the editor probed the daemon for the off+auto posture');

  // The warning is advisory — it never disables save.
  assert.equal(host.querySelector('#profile-editor-submit').disabled, false);

  // Picking a confining sandbox (`on` is a catalog option) clears it.
  choose(host.querySelector('#profile-editor-sandbox'), 'on');
  await harness.act(() => harness.fireEvent(host.querySelector('#profile-editor-sandbox'), 'change'));
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 260)); });
  assertAbsent(host.querySelector('#profile-editor-autonomy-warning'), 'a safe sandbox clears the warning');

  cleanups.reverse().forEach((fn) => fn());
});

test('OpenCode profile editor replaces the unsandboxed warning with its tclaude-layer boundary notice', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const openCodeCatalog = catalog.map((entry) => entry.name === 'opencode'
    ? {
      ...entry,
      sandbox_modes: ['access-control', 'tclaude-layer', 'off'],
      default_sandbox: 'access-control',
      sandbox_mode_help: {
        'access-control': 'Lexical soft disk access control',
        'tclaude-layer': 'tclaude OS containment',
        off: '⚠ No tclaude OS containment',
      },
    }
    : entry);
  const state = createManagementState();
  state.openDialog({
    kind: 'profile-editor',
    seed: {
      name: 'contained-opencode', harness: 'opencode', sandbox: 'access-control',
      sandbox_implementation: 'tclaude-layer',
    },
    options: {},
    catalog: openCodeCatalog,
    sandboxImpl: {
      options: [{ value: 'tclaude-layer', label: 'tclaude built-in OS sandbox (experimental)' }],
      default: 'harness-builtin',
      host_available: true,
    },
  });
  const probes = [];
  const cleanups = [];
  const host = harness.document.createElement('div');
  harness.document.body.appendChild(host);
  mountManagementIsland({
    host, state,
    actions: {
      async saveProfile() {},
      async loadUnsandboxedAutonomy(input) {
        probes.push(input);
        return {
          info: input.sandboxImplementation === 'tclaude-layer'
            ? ["OpenCode's tool-executing server runs inside tclaude's built-in OS sandbox."]
            : [],
          warnings: input.sandboxImplementation === 'tclaude-layer'
            ? []
            : ['⚠ OpenCode has no built-in OS sandbox.'],
        };
      },
    },
    confirmDiscard: async () => true,
    openProfilePermissions() {},
    registerCleanup(fn) { cleanups.push(fn); },
  });
  await harness.act(() => Promise.resolve());
  const initialNotice = host.querySelector('#profile-editor-sandbox-info');
  assert.ok(initialNotice, 'the empty status region exists before the async disclosure');
  assert.equal(initialNotice.hidden, false, 'the live region remains in the accessibility tree');
  assert.match(initialNotice.className, /sandbox-info-pending/,
    'the empty live region is visually clipped without using hidden');
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 260)); });

  assert.equal(probes.at(-1).sandboxImplementation, 'tclaude-layer');
  const notice = host.querySelector('#profile-editor-sandbox-info');
  assert.ok(notice, 'the tclaude-layer boundary notice remains visible');
  assert.equal(notice.hidden, false);
  assert.doesNotMatch(notice.className, /sandbox-info-pending/);
  assert.equal(notice.querySelector('[role="status"]').getAttribute('role'), 'status');
  assert.match(notice.querySelector('.spawn-field-hint.info').textContent,
    /tool-executing server runs inside tclaude's built-in OS sandbox/);
  assertAbsent(notice.querySelector('.spawn-field-hint.warn'));
  assertAbsent(host.querySelector('#profile-editor-autonomy-warning'));
  assert.doesNotMatch(notice.textContent, /no built-in OS sandbox/,
    'the profile editor does not show the access-control warning for tclaude-layer');
  cleanups.reverse().forEach((fn) => fn());
});

// The role editor shares HarnessFields, so it must show the same warning under
// its own element id — asserted here, not just by comment.
test('role editor shows the unsandboxed-autonomy warning', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'role-editor', seed: { name: 'risky-role', harness: 'claude', sandbox: 'off', approval: 'auto' }, options: {}, catalog });
  const cleanups = [];
  const host = harness.document.createElement('div');
  harness.document.body.appendChild(host);
  const loadUnsandboxedAutonomy = async ({ sandbox, approval }) => ({
    warnings: sandbox === 'off' && approval === 'auto' ? ['⚠ this role runs commands unattended with no sandbox.'] : [],
  });
  mountManagementIsland({
    host, state,
    actions: { async saveRole() {}, loadUnsandboxedAutonomy },
    confirmDiscard: async () => true, openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); },
  });
  await harness.act(() => Promise.resolve());
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 260)); });

  const warning = host.querySelector('#role-editor-autonomy-warning');
  assert.ok(warning, 'the role editor renders the warning under its own id');
  assert.equal(warning.querySelector('[role="alert"]').getAttribute('role'), 'alert');
  assert.match(warning.textContent, /commands unattended/);
  cleanups.reverse().forEach((fn) => fn());
});

// A role editor shares HarnessFields, so it gets the same warning; and an
// actions object without the probe method (older callers, or a harness that
// never wires it) must simply render no warning rather than throw.
test('profile editor tolerates an actions object without the autonomy probe', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'profile-editor', seed: { name: 'risky', harness: 'claude', sandbox: 'off', approval: 'auto' }, options: {}, catalog });
  const cleanups = [];
  const host = harness.document.createElement('div');
  harness.document.body.appendChild(host);
  mountManagementIsland({ host, state, actions: { async saveProfile() {} }, confirmDiscard: async () => true, openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); } });
  await harness.act(() => Promise.resolve());
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 260)); });
  assertAbsent(host.querySelector('#profile-editor-autonomy-warning'), 'no probe method → no warning, no crash');
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
    'harness claude', 'model sonnet', 'effort high', 'sandbox Claude settings decide (inherit)', 'approval plan',
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
  assert.match(host.querySelector('#sandbox-profile-editor-title').textContent, /Clone sandbox profile: restricted/);
  assert.equal(host.querySelector('#sandbox-profile-editor-modal input').value, 'restricted-copy-3');
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

// TCL-791 removed break-glass, and with it the two conditions that used to
// make the sandbox editor's Ctrl+Enter carry its own block: an unacknowledged
// rule set and a failed acknowledgement-recovery reload. What survives is the
// plain rule that the hotkey may never reach a save the Save button refuses.
test('sandbox editor Ctrl+Enter cannot save while a save is already in flight', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const baseActions = {
    load: async () => false, openSandboxEditor() {}, removeSandbox() {}, configureSandboxWithAgent() {},
    loadCommonRuleCatalog: async () => ({ version: 1, categories: [], informational: [] }),
    inspectDirectories: async () => ({ missing: [], creatable: [] }),
  };
  const state = createManagementState();
  const attempts = [];
  state.openDialog({ kind: 'sandbox-editor', seed: { name: 'restricted', filesystem: [] }, options: {} });
  const cleanups = []; const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  mountManagementIsland({
    host,
    state,
    actions: { ...baseActions, saveSandbox: async (payload) => { attempts.push(payload); } },
    confirmDiscard: async () => true,
    openProfilePermissions() {},
    registerCleanup(fn) { cleanups.push(fn); },
  });
  await harness.act(() => Promise.resolve());
  const input = host.querySelector('#sandbox-profile-editor-modal input');
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter', ctrlKey: true }));
  assert.equal(attempts.length, 1);
  state.busy.value = 'sandbox-save';
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#sandbox-profile-editor-submit').disabled, true, 'an in-flight save disables Save');
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter', ctrlKey: true }));
  assert.equal(attempts.length, 1, 'Ctrl+Enter cannot save past the Save button guard');
  cleanups.reverse().forEach((fn) => fn());
  host.remove();
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
  assertAbsent(host.querySelector('#profile-editor-modal'), 'a clean editor closes even though the spawn overlay is later in the DOM');
  assert.equal(confirms, 0, 'a clean editor needs no discard confirmation');
  assert.equal(spawn.classList.contains('show'), true, 'closing the editor leaves the underlying spawn dialog open');
  assert.equal(spawnCloses, 0);

  await openEditor();
  const name = host.querySelector('#profile-editor-name'); name.value = 'new-pattern'; name.dispatchEvent(new harness.window.Event('input', { bubbles: true })); await harness.act(() => Promise.resolve());
  await pressEscape();
  assert.ok(host.querySelector('#profile-editor-modal'), 'rejecting discard keeps the dirty editor open');
  assert.equal(confirms, 1, 'a dirty editor offers discard confirmation');
  discard = true; await pressEscape();
  assertAbsent(host.querySelector('#profile-editor-modal'), 'accepting discard closes the dirty editor');
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
  assertSameNode(harness.document.activeElement, host.querySelector('#profile-editor-harness'), 'hidden autofocus fields cannot retain focus behind the modal');
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
    previewSandboxProfile: async (name, body) => { calls.push(['preview', name, body]); return {
      before: null, after: body, revision: 'r1',
      notices: [{ class: 'composition', detail: 'authoritative preview composition warning' }],
    }; },
    saveSandboxProfile: async (...args) => { calls.push(['save', ...args]); }, deleteSandboxProfile: async (name) => calls.push(['delete', name]),
    inspectSandboxImport: async (value) => ({ profiles: value.profiles }), importSandboxProfiles: async (...args) => { calls.push(['import', ...args]); return {}; },
  };
  const actions = createManagementActions({ state, confirm: async () => { genericConfirms += 1; return true; }, notify() {}, refreshSandboxSpawn: async () => { refreshed += 1; }, sandboxAPI });
  const draft = {
    name: 'safe', filesystem: [{ path: '/tmp', access: 'write' }], environment: [],
    includes: ['base'], agent_directories: ['GOCACHE'], network_access: 'internet',
    resource_limits: { memory: '8GB' },
    darwin_allow_mach_register: true,
  };
  // The save body always carries the full-replace shape. The retired
  // read_baseline and break_glass_filesystem fields are gone from the wire
  // entirely (TCL-791).
  const body = { ...draft };
  const create = actions.saveSandbox({ draft, original: null }); await Promise.resolve();
  assert.deepEqual(state.sandboxDiff.value, {
    before: null, after: body,
    notices: [{ class: 'composition', detail: 'authoritative preview composition warning' }],
  }); state.cancelSandboxDiff(true);
  assert.equal(await create, true);
  assert.deepEqual(calls[0], ['preview', '', body]); assert.deepEqual(calls[1], ['save', '', body, 'r1']); assert.equal(refreshed, 1);
  const replacement = { ...draft, name: 'renamed', darwin_allow_mach_register: false }; const replacementBody = { ...body, name: 'renamed', darwin_allow_mach_register: false }; const update = actions.saveSandbox({ draft: replacement, original: replacement, options: { targetName: 'safe' } }); await Promise.resolve(); state.cancelSandboxDiff(true); await update;
  assert.deepEqual(calls[2], ['preview', 'safe', replacementBody]); assert.deepEqual(calls[3], ['save', 'safe', replacementBody, 'r1']);
  const copied = { ...draft, name: 'safe-copy' }; const copiedBody = { ...body, name: 'safe-copy' }; const clone = actions.saveSandbox({ draft: copied, original: draft, options: { editExisting: false } }); await Promise.resolve(); state.cancelSandboxDiff(true); await clone;
  assert.deepEqual(calls[4], ['preview', '', copiedBody]); assert.deepEqual(calls[5], ['save', '', copiedBody, 'r1']);
  assert.equal(genericConfirms, 0, 'sandbox saves use the dedicated diff instead of the generic JSON confirmation blob');
  await actions.removeSandbox('safe'); assert.deepEqual(calls.find((call) => call[0] === 'delete'), ['delete', 'safe']);
  assert.equal(genericConfirms, 1, 'ordinary destructive confirmations still use the shared prompt');
  await actions.importSandboxBundle({ profiles: [draft] }, 'skip'); assert.equal(calls.find((call) => call[0] === 'import')[2], 'skip');
});

test('sandbox pre-launch-only changes always reach the diff and delete-last commits an explicit empty list', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { createManagementActions }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/management-actions.js'),
    harness.importDashboardModule('js/management-island.js'),
  ]);
  const scenarios = [
    {
      label: 'add',
      before: undefined,
      next: [{ name: 'setup', script: 'export FOO=added\n', exports: ['FOO'] }],
      diffText: 'added',
    },
    {
      label: 'edit',
      before: [{ name: 'setup', script: 'export FOO=old\n', exports: ['FOO'] }],
      next: [{ name: 'setup', script: 'export FOO=new\n', exports: ['FOO'] }],
      diffText: 'new',
    },
    {
      label: 'delete-last',
      before: [{ name: 'setup', script: 'export FOO=old\n', exports: ['FOO'] }],
      next: [],
      diffText: 'pre_launch',
    },
  ];

  for (const scenario of scenarios) {
    const state = createManagementState();
    const notices = []; const commits = [];
    const sandboxAPI = {
      async loadSandboxProfiles() { return []; },
      async previewSandboxProfile(_name, body) {
        const before = structuredClone(body);
        if (scenario.before === undefined) delete before.pre_launch;
        else before.pre_launch = structuredClone(scenario.before);
        const after = structuredClone(body);
        if (scenario.next.length === 0) delete after.pre_launch;
        return { before, after, revision: `r-${scenario.label}`, notices: [] };
      },
      async saveSandboxProfile(...args) { commits.push(args); },
    };
    const actions = createManagementActions({
      state, sandboxAPI, confirm: async () => true,
      notify(text) { notices.push(text); }, refreshSandboxSpawn: async () => {},
    });
    const cleanups = []; const host = harness.document.createElement('div');
    harness.document.body.appendChild(host);
    mountManagementIsland({
      host, state, actions, confirmDiscard: async () => true,
      openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); },
    });
    const draft = {
      name: 'scripts', filesystem: [], environment: [], includes: [], agent_directories: [],
      pre_launch: structuredClone(scenario.next),
    };
    const pending = actions.saveSandbox({ draft, original: { name: 'scripts' } });
    await harness.act(() => Promise.resolve());
    const modal = host.querySelector('#sandbox-profile-diff-modal');
    assert.ok(modal, `${scenario.label}: a pre-launch-only change opens the save diff`);
    assert.ok(modal.querySelectorAll('.dl.add, .dl.del').length > 0,
      `${scenario.label}: the rendered delta is non-empty`);
    assert.match(modal.querySelector('#sandbox-profile-diff-body').textContent,
      new RegExp(scenario.diffText), `${scenario.label}: delta names the changed script content`);
    assert.equal(notices.includes('No sandbox profile changes to save'), false,
      `${scenario.label}: the no-op guard must not discard the change`);
    state.cancelSandboxDiff(true);
    assert.equal(await pending, true);
    assert.equal(commits.length, 1);
    if (scenario.label === 'delete-last') {
      assert.deepEqual(commits[0][1].pre_launch, [],
        'omitempty preview omission is repaired before the actual save');
    } else {
      assert.deepEqual(commits[0][1].pre_launch, scenario.next);
    }
    cleanups.reverse().forEach((fn) => fn());
    host.remove();
  }
});

test('sandbox access-axis model preserves legacy meaning and validates structured rows', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/sandbox-profiles-data.js');
  assert.deepEqual(model.sandboxAccessAxes({ network_access: 'none' }), {
    network: { mode: 'closed', allow: [] },
    unix_sockets: { mode: 'closed', allow: [] },
  });
  const draft = {
    name: 'scoped', filesystem: [], environment: [], includes: [], agent_directories: [],
    network: { mode: 'list', allow: [{ domain: 'api.example.com', include_subdomains: true, ports: '443, 8443' }] },
    unix_sockets: { mode: 'list', allow: [{ path_glob: '/tmp/ssh-*/agent.*' }] },
  };
  assert.deepEqual(model.sandboxAccessDraftErrors(draft), []);
  assert.deepEqual(model.sandboxProfileForWire(draft).network.allow[0].ports, [443, 8443]);
  assert.deepEqual(model.sandboxNetworkAuthoring({
    network: { mode: 'list', allow: [{ loopback: true }] },
  }), {
    baseline: 'deny', packs: [], deny_packs: [], allow: [{ loopback: true }], deny: [],
  }, 'legacy lists remain manual and never infer pack references');
  assert.deepEqual(model.sandboxProfileForWire({
    ...draft,
    network: {
      baseline: 'deny',
      packs: ['net-openai-codex', 'net-local'],
      deny_packs: ['net-npm'],
      allow: [{ host: 'api.example.com', ports: '8443, 443' }],
      deny: [{ domain: 'blocked.example', ports: '443' }],
    },
  }).network, {
    baseline: 'deny',
    packs: ['net-local', 'net-openai-codex'],
    deny_packs: ['net-npm'],
    allow: [{ host: 'api.example.com', ports: [8443, 443] }],
    deny: [{ domain: 'blocked.example', include_subdomains: false, ports: [443] }],
  });
  assert.equal(
    model.sandboxNetworkEntryKey({ host: 'api.example.com', ports: '8443, 443' }),
    model.sandboxNetworkEntryKey({ host: 'api.example.com', ports: [443, 8443] }),
  );
  assert.notEqual(
    model.sandboxNetworkModeEntryKey('allow', { host: 'api.example.com' }),
    model.sandboxNetworkModeEntryKey('deny', { host: 'api.example.com' }),
  );
  const broken = structuredClone(draft);
  broken.network.allow[0].domain = 'https://example.com';
  broken.unix_sockets.allow[0].path_glob = 'relative/**/agent';
  assert.match(model.sandboxAccessDraftErrors(broken).join(' '), /scheme.*absolute.*\*\*/i);
  assert.match(model.sandboxAccessDraftErrors({
    network: {
      baseline: 'deny', packs: ['net-local'], deny_packs: ['net-local'],
      allow: [], deny: [],
    },
  }).join(' '), /must use exactly one Allow or Deny mode/);
  const buckets = model.sandboxRuleBuckets({
    filesystem: { outcome: 'enforced_partial', detail: 'filesystem carve-out detail' },
    environment: { outcome: 'enforced', detail: 'environment detail' },
    agent_directories: { outcome: 'enforced', detail: 'agent-directory detail' },
    network: { outcome: 'refused', detail: 'network detail from resolver' },
    unix_sockets: { outcome: 'enforced', detail: 'socket detail' },
  }, {
    filesystem: [
      { path: '/home/operator', access: 'deny' },
      { path: '/home/operator/work', access: 'write' },
    ],
    environment: ['POLICY_OWNER'],
    agent_directories: ['GOCACHE'],
    network: {
      mode: 'list',
      allow: [
        { domain: 'api.example.com', ports: [443] },
        { cidr: '198.51.100.0/24' },
      ],
    },
    unix_sockets: { mode: 'closed' },
    agentd_socket: 'always reachable',
  });
  assert.deepEqual(buckets.applied.rules, [
    'Set environment: POLICY_OWNER',
    'Private read/write directory: $GOCACHE',
    'Block Unix sockets',
    'Allow Unix socket: tclaude agent control',
  ]);
  assert.deepEqual(buckets.partial.rules, [
    'Block: /home/operator',
    'Read/write: /home/operator/work',
  ]);
  assert.deepEqual(buckets.notApplied.rules, [
    'Allow network: domain api.example.com · port 443',
    // TCL-864: a CIDR row names the selector, not the axis word a second time.
    'Allow network: CIDR 198.51.100.0/24',
    'Block all other network destinations',
  ]);
  assert.equal(buckets.launchRefused, true);
  assert.deepEqual(buckets.partial.reasons, [
    { label: 'Limitation', detail: 'filesystem carve-out detail' },
  ]);
  assert.deepEqual(buckets.notApplied.reasons, [
    { label: 'Launch blocked', detail: 'network detail from resolver' },
  ]);
  assert.equal(buckets.applied.label, 'Fully supported rules');
  assert.equal(buckets.partial.label, 'Partially supported rules');
  assert.equal(buckets.notApplied.label, 'Unsupported rules');
  // TCL-798: a target that builds its own filesystem root states the implicit
  // half of that posture as a rule row, next to the rules the operator wrote.
  // Without it, adding a SOCKET rule silently changes which host paths exist
  // and the preview looks identical.
  const constructedRootBuckets = model.sandboxRuleBuckets({
    filesystem: { outcome: 'enforced', detail: '' },
    unix_sockets: { outcome: 'enforced_partial', detail: 'socket detail' },
    constructed_root: true,
  }, {
    filesystem: [{ path: '/home/operator/work', access: 'write' }],
    network: { mode: 'open' },
    unix_sockets: { mode: 'closed' },
  });
  assert.ok(
    constructedRootBuckets.applied.rules.some(
      (rule) => rule.startsWith('Block: every other host path')),
    'a constructed root must appear as a visible filesystem rule',
  );
  const inheritedRootBuckets = model.sandboxRuleBuckets({
    filesystem: { outcome: 'enforced', detail: '' },
    unix_sockets: { outcome: 'enforced', detail: '' },
  }, {
    filesystem: [{ path: '/home/operator/work', access: 'write' }],
    network: { mode: 'open' },
  });
  assert.ok(
    !inheritedRootBuckets.applied.rules.concat(
      inheritedRootBuckets.partial.rules, inheritedRootBuckets.notApplied.rules,
    ).some((rule) => rule.startsWith('Block: every other host path')),
    'an inherited host root must not claim the row',
  );

  const denyBuckets = model.sandboxRuleBuckets({
    network: { outcome: 'enforced_partial', detail: 'mixed network axis' },
  }, {
    network: {
      mode: 'open',
      deny: [
        { cidr: '192.0.2.0/24' },
        { domain: 'partial.example', ports: [443] },
        { host: 'unsupported.example' },
      ],
    },
  }, [
    {
      mode: 'deny',
      keys: ['deny:{"cidr":"192.0.2.0/24"}'],
      outcome: 'enforced',
      detail: 'CIDR deny detail',
    },
    {
      mode: 'deny',
      keys: ['deny:{"domain":"partial.example","ports":[443]}'],
      outcome: 'enforced_partial',
      detail: 'DNS deny partial detail',
    },
    {
      mode: 'deny',
      keys: ['deny:{"host":"unsupported.example"}'],
      outcome: 'not_enforced',
      detail: 'deny target unsupported detail',
    },
  ]);
  assert.ok(denyBuckets.applied.rules.includes(
    'Deny network: CIDR 192.0.2.0/24'));
  assert.ok(denyBuckets.partial.rules.includes(
    'Deny network: domain partial.example · port 443'));
  assert.ok(denyBuckets.notApplied.rules.includes(
    'Deny network: host unsupported.example'));
  assert.equal(denyBuckets.partial.items.find(
    (item) => item.label.includes('partial.example')).detail,
  'DNS deny partial detail');
  assert.deepEqual(model.sandboxOtherAssignmentWarnings({
    filesystem: { outcome: 'refused', detail: 'another assignment refuses its carve-out' },
    network: { outcome: 'not_enforced', detail: 'same network outcome' },
  }, {
    filesystem: { outcome: 'enforced', detail: 'selected assignment is safe' },
    network: { outcome: 'not_enforced', detail: 'same network outcome' },
  }), [{
    axis: 'filesystem',
    label: 'Directory rules',
    outcome: 'refused',
    detail: 'another assignment refuses its carve-out',
  }]);
  assert.equal(model.sandboxTargetLabel({
    implementation: 'harness-builtin', harness: 'codex', platform: 'darwin',
  }), 'Codex on macOS · built-in sandbox · no filtered network sandbox yet');
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
  const decision = state.confirmSandboxDiff(before, after, [
    { class: 'composition', detail: 'server-authoritative empty intersection warning' },
  ]); await harness.act(() => Promise.resolve());
  const modal = host.querySelector('#sandbox-profile-diff-modal');
  assert.ok(modal); assert.equal(modal.querySelectorAll('.dl.add').length, 1); assert.equal(modal.querySelectorAll('.dl.del').length, 1); assert.ok(modal.querySelectorAll('.dl.ctx').length > 0);
  assert.match(modal.querySelector('#sandbox-profile-diff-sub').textContent, /1 line\(s\) added, 1 removed/);
  const diffBody = modal.querySelector('#sandbox-profile-diff-body');
  const evaluation = modal.querySelector('#sandbox-profile-diff-evaluation');
  assertSameNode(diffBody.nextElementSibling, evaluation,
    'evaluation warnings render in their bottom section after the diff, never at the top');
  assert.equal(evaluation.querySelector('h4').textContent, 'Evaluation warnings');
  assert.equal(evaluation.querySelector('.sbx-composition-warning').getAttribute('role'), 'alert');
  assert.match(evaluation.querySelector('.sbx-composition-warning').textContent,
    /server-authoritative empty intersection warning/);
  assert.equal(harness.document.activeElement.id, 'sandbox-profile-diff-confirm');
  const editor = host.querySelector('#sandbox-profile-editor-modal'); assert.equal(editor.inert, true); assert.equal(editor.getAttribute('aria-hidden'), 'true');
  modal.querySelector('#sandbox-profile-diff-cancel').click(); state.busy.value = ''; await harness.act(() => Promise.resolve());
  assert.equal(await decision, false); assertAbsent(host.querySelector('#sandbox-profile-diff-modal')); assert.equal(editor.inert, false); assert.equal(editor.hasAttribute('aria-hidden'), false); assert.ok(host.querySelector('#sandbox-profile-editor-modal')); assertSameNode(harness.document.activeElement, submit, 'focus returns after the editor is interactive again');
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
  const network = host.querySelector('#sandbox-profile-editor-network-baseline'); assert.ok(network.querySelector('option[value="allow"]')); network.querySelector('option[value="deny"]').selected = true; network.dispatchEvent(new harness.window.Event('change', { bubbles: true })); await harness.act(() => Promise.resolve());
  [...host.querySelectorAll('.sbx-add-row')].find((button) => /directory/.test(button.textContent)).click(); await harness.act(() => Promise.resolve());
  const path = host.querySelector('.sbx-path'); path.value = '/cache'; path.dispatchEvent(new harness.window.Event('input', { bubbles: true })); await harness.act(() => Promise.resolve());
  assert.equal(harness.document.activeElement === path || path.value === '/cache', true);
  host.querySelector('.sbx-include-add').click(); host.querySelector('.sbx-agent-add').click(); await harness.act(() => Promise.resolve());
  const access = host.querySelector('.sbx-access'); const include = host.querySelector('.sbx-inc-name'); assert.ok(access); assert.ok(include); assertDifferentNode(access, include, 'access and included-profile selects have distinct layout contracts'); include.querySelector('option[value="base"]').selected = true; include.dispatchEvent(new harness.window.Event('change', { bubbles: true })); const agentDir = host.querySelector('.sbx-agent-name'); agentDir.value = 'GOCACHE'; agentDir.dispatchEvent(new harness.window.Event('input', { bubbles: true })); await harness.act(() => Promise.resolve());
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
  assert.equal(scribe.value.filesystem[0].path, '/raw'); assert.equal(scribe.value.network.baseline, 'deny'); assert.equal(scribe.value.network_access, ''); assert.equal(scribe.options.targetName, 'dev'); assertAbsent(host.querySelector('#sandbox-profile-editor-modal'), 'scribe handoff closes the editor so its returned draft can be delivered');
  cleanups.reverse().forEach((fn) => fn());
});

test('sandbox editor blocks scribe handoff for invalid structured resource limits', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  for (const [field, value, expected] of [
    ['memory', '512', /unit/],
    ['cpu', '0.009', /at least 0.01/],
  ]) {
    const state = createManagementState();
    state.openDialog({
      kind: 'sandbox-editor',
      seed: {
        name: `invalid-${field}`, filesystem: [], environment: [], includes: [], agent_directories: [],
        resource_limits: { [field]: value },
      },
      options: {},
    });
    let scribe = null;
    const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
      configureSandboxWithAgent(draft) { scribe = draft; },
    });
    await harness.act(() => Promise.resolve());

    host.querySelector('#sandbox-profile-editor-scribe').click();
    await harness.act(() => Promise.resolve());
    assert.equal(scribe, null, `${field}: invalid structured value must not reach the scribe`);
    assert.match(host.querySelector('.cron-create-error').textContent, expected);
    assert.ok(host.querySelector('#sandbox-profile-editor-modal'), `${field}: editor remains open`);
    unmount();
  }
});

test('sandbox filesystem and socket rows reuse accessible segmented controls while compact rows keep their authored shapes', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [{ name: 'base' }, { name: 'segments' }]);
  state.openDialog({ kind: 'sandbox-editor', seed: {
    name: 'segments',
    filesystem: [
      { path: '/read', access: 'read' },
      { path: '/write', access: 'write' },
      { path: '/deny', access: 'deny' },
    ],
    environment: [{ name: 'GOCACHE', value: '/cache/go-build' }],
    includes: ['base'],
    agent_directories: ['NODE_COMPILE_CACHE'],
    unix_sockets: {
      mode: 'list',
      allow: [{ path: '/run/example.sock' }, { path_glob: '/tmp/ssh-*/agent.*' }],
    },
  }, options: {} });
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state);
  await harness.act(() => Promise.resolve());

  const filesystemRows = [...host.querySelectorAll('.sbx-filesystem-row')];
  const filesystemControls = filesystemRows.map((row) => row.querySelector('.sbx-filesystem-access'));
  assert.equal(filesystemRows.length, 3);
  assert.deepEqual(filesystemControls.map(segmentedValue), ['read', 'write', 'deny']);
  assert.equal(filesystemControls.every((control) => control.getAttribute('role') === 'radiogroup'), true);
  assert.deepEqual([...filesystemControls[0].querySelectorAll('[role="radio"]')].map((radio) => radio.textContent),
    ['Read', 'Write', 'Deny']);
  for (const [control, value] of filesystemControls.map((item) => [item, segmentedValue(item)])) {
    assert.equal(segment(control, value).classList.contains(`sbx-state-${value}`), true,
      `${value} filesystem state exposes its permission-color CSS class`);
    assert.equal(segment(control, value).classList.contains('is-selected'), true);
  }
  const readRadio = segment(filesystemControls[0], 'read');
  readRadio.focus();
  await harness.act(() => harness.fireEvent(readRadio, 'keydown', { key: 'ArrowRight' }));
  assert.equal(segmentedValue(filesystemControls[0]), 'write');
  assertSameNode(harness.document.activeElement, segment(filesystemControls[0], 'write'));
  await harness.act(() => harness.fireEvent(segment(filesystemControls[0], 'write'), 'keydown', { key: 'End' }));
  assert.equal(segmentedValue(filesystemControls[0]), 'deny');
  await harness.act(() => harness.fireEvent(segment(filesystemControls[0], 'deny'), 'keydown', { key: 'Home' }));
  assert.equal(segmentedValue(filesystemControls[0]), 'read');
  assert.equal(segment(filesystemControls[0], 'read').getAttribute('tabindex'), '0');
  assert.equal(segment(filesystemControls[0], 'write').getAttribute('tabindex'), '-1');

  const socketMode = host.querySelector('#sandbox-profile-editor-unix-sockets-mode');
  const socketControls = [...host.querySelectorAll('.sbx-socket-selector')];
  assert.equal(socketMode.tagName, 'SELECT', 'the three-state section posture remains a dropdown');
  assert.deepEqual(socketControls.map(segmentedValue), ['path', 'path_glob']);
  assert.deepEqual([...socketControls[0].querySelectorAll('[role="radio"]')].map((radio) => radio.textContent),
    ['Path', 'Glob']);
  assert.equal(segment(socketControls[0], 'path').classList.contains('sbx-state-path'), true);
  assert.equal(segment(socketControls[1], 'path_glob').classList.contains('sbx-state-path_glob'), true);
  const socketValue = host.querySelector('.sbx-socket-value');
  await harness.act(() => harness.fireEvent(segment(socketControls[0], 'path'), 'click'));
  assert.equal(socketValue.value, '/run/example.sock',
    'reactivating the selected syntax does not clear the authored socket value');
  await harness.act(() => harness.fireEvent(segment(socketControls[0], 'path'), 'keydown', { key: 'ArrowRight' }));
  assert.equal(segmentedValue(socketControls[0]), 'path_glob');
  assert.equal(segment(socketControls[0], 'path_glob').classList.contains('is-selected'), true);

  const include = host.querySelector('.sbx-inc-name');
  assert.equal(include.tagName, 'SELECT');
  assert.ok(include.closest('.sbx-include-row'), 'includes opt into the intrinsic-width row hook');
  const environmentRow = host.querySelector('.sbx-environment-row');
  assert.ok(environmentRow.querySelector('.sbx-env-name'));
  assert.ok(environmentRow.querySelector('.sbx-env-value'));
  assert.equal(environmentRow.querySelectorAll('input').length, 2,
    'environment keeps free-form name/value controls under the fixed-grid hooks');
  const agentRow = host.querySelector('.sbx-agent-name').closest('.sbx-row');
  assertAbsent(agentRow.querySelector('.sbx-segmented-control'), 'agent-owned directory rows remain exactly the free-form input control');
  unmount();
  host.remove();
});

test('sandbox editor discloses missing includes and preserves their authored names on save', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [
    { name: 'base' },
    { name: 'dev' },
  ]);
  state.openDialog({ kind: 'sandbox-editor', seed: {
    name: 'dev', filesystem: [], environment: [],
    includes: ['base', 'base-caches'], agent_directories: [],
  }, options: {} });
  let saved = null;
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    async saveSandbox(value) { saved = value; },
  });
  await harness.act(() => Promise.resolve());

  const includes = [...host.querySelectorAll('.sbx-inc-name')];
  assert.equal(includes.length, 2);
  assert.equal(selectedValue(includes[0]), 'base');
  assertAbsent(includes[0].closest('.sbx-include-row').querySelector('.sbx-include-warning'), 'a resolvable include remains an ordinary compact select');
  const missing = includes[1];
  assert.equal(selectedValue(missing), 'base-caches',
    'the sentinel option keeps the authored value selected in the DOM');
  assert.equal(missing.querySelector('option[value="base-caches"]').textContent, '— missing —');
  assert.equal(missing.getAttribute('aria-invalid'), 'true');
  const warning = missing.closest('.sbx-include-row').querySelector('.sbx-include-warning');
  assert.equal(warning.getAttribute('role'), 'alert');
  assert.equal(missing.getAttribute('aria-describedby'), warning.id);
  assert.match(warning.textContent, /⚠ "base-caches" not found in registry/);

  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(saved.draft.includes, ['base', 'base-caches'],
    'an untouched load→save round-trip never drops or rewrites the missing include');
  unmount();
  host.remove();
});

// Pre-launch blocks reach the editor only through the raw JSON panel, so the two things worth pinning
// are that an untouched profile keeps them and that an explicit empty array survives as a real clear.
test('the sandbox editor round-trips pre-launch blocks and forwards an explicit clear', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [{ name: 'dev' }]);
  const blocks = [{ name: 'playwright', script: 'export FOO=bar\n', exports: ['FOO'] }];
  state.openDialog({ kind: 'sandbox-editor', seed: {
    name: 'dev', filesystem: [], environment: [], includes: [], agent_directories: [],
    pre_launch: blocks,
  }, options: {} });
  let saved = null;
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    async saveSandbox(value) { saved = value; },
  });
  await harness.act(() => Promise.resolve());

  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(saved.draft.pre_launch, blocks,
    'an untouched load→save round-trip never drops the operator-authored blocks');

  const toggle = host.querySelector('.sbx-advanced-toggle');
  await harness.act(() => toggle.click());
  const raw = host.querySelector('#sandbox-profile-editor-pre-launch');
  assert.deepEqual(JSON.parse(raw.value), blocks, 'the raw panel opens seeded from the draft');
  raw.value = '[]';
  raw.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(saved.draft.pre_launch, [],
    'an explicit empty array reaches the save path so the daemon can tell "clear" from "leave alone"');
  unmount();
  host.remove();
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
      { harness: 'claude', source: '~/.claude/settings.json', setting: 'sandbox.filesystem.denyRead + denyWrite', access: 'deny', note: "Claude Code's global sandbox is enabled." },
      { harness: 'codex', source: 'generated tclaude-agent-<launch-id>.config.toml', setting: 'permissions.tclaude-agent-<launch-id>.filesystem', access: 'deny', note: "Canonical baseline applied to every tclaude-managed Codex launch profile." },
    ] },
    { path: '~/.codex', access: 'deny-read', harnesses: ['claude'], origins: [
      { harness: 'claude', source: '~/.claude/settings.json', setting: 'sandbox.filesystem.denyRead', access: 'deny-read', note: "Claude Code's global sandbox is enabled." },
    ] },
    { path: '~/.tclaude/api/agentd.sock', access: 'write', harnesses: ['claude', 'codex'], origins: [
      { harness: 'claude', source: '~/.claude/settings.json', setting: 'sandbox.filesystem.allowRead', access: 'read', note: "Claude Code's global sandbox is enabled." },
      { harness: 'codex', source: 'generated tclaude-agent-<launch-id>.config.toml', setting: 'permissions.tclaude-agent-<launch-id>.filesystem', access: 'write', note: "Canonical baseline applied to every tclaude-managed Codex launch profile." },
    ] },
    { path: '/tmp/tmux-1000/tclaude', access: 'deny', harnesses: ['claude'], origins: [
      { harness: 'claude', source: 'generated claude --settings launch override', setting: 'sandbox.filesystem.denyRead + denyWrite', access: 'deny', note: "Canonical tclaude tmux socket boundary." },
    ] },
  ],
  global_network: [{ mode: 'list', entry: { domain: 'global.example' }, origin: { harness: 'claude', setting: 'sandbox.network.allowedDomains' } }],
  global_unix_sockets: [{ mode: 'list', entry: { path: '/tmp/global.sock' }, origin: { harness: 'claude', setting: 'sandbox.network.allowUnixSockets' } }],
  network_packs: [
    { id: 'net-local', label: 'Local access', entries: [{ loopback: true }], note: 'local services' },
    { id: 'net-anthropic', label: 'Anthropic API', group: 'Cloud model APIs', entries: [{ domain: 'api.anthropic.com', ports: [443] }] },
    { id: 'net-openai-codex', label: 'OpenAI API', group: 'Cloud model APIs', entries: [{ domain: 'api.openai.com', ports: [443] }] },
  ],
  network_templates: [{ id: 'net-anthropic', label: 'Anthropic API', mode: 'list', entries: [{ domain: 'api.anthropic.com' }], note: 'official API endpoint' }],
  socket_templates: [{ id: 'sockets-agentd-only', label: 'tclaude agentd only', mode: 'closed', entries: [], note: 'floor is always reachable' }],
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
      async predictSandbox() { return { targets: [], contexts: [] }; },
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

test('sandbox editor offers Mach registration only on a macOS agentd', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  const saves = [];
  state.openDialog({
    kind: 'sandbox-editor', seed: { name: 'chrome', filesystem: [], environment: [] },
    options: {}, sandboxImpl: { ...sandboxImpl, platform: 'darwin' },
  });
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    async saveSandbox(value) { saves.push(value); },
  });
  await harness.act(() => Promise.resolve());
  const checkbox = host.querySelector('#sandbox-profile-editor-allow-mach-register');
  assert.ok(checkbox, 'macOS exposes the compatibility capability');
  checkbox.checked = true;
  await harness.act(() => harness.fireEvent(checkbox, 'change'));
  await harness.act(() => harness.fireEvent(host.querySelector('#sandbox-profile-editor-submit'), 'click'));
  assert.equal(saves[0].draft.darwin_allow_mach_register, true);

  state.closeDialog();
  await harness.act(() => Promise.resolve());
  state.openDialog({
    kind: 'sandbox-editor', seed: { name: 'linux', filesystem: [], environment: [] },
    options: {}, sandboxImpl: { ...sandboxImpl, platform: 'linux' },
  });
  await harness.act(() => Promise.resolve());
  assertAbsent(host.querySelector('#sandbox-profile-editor-compatibility-section'));
  unmount();
});

test('sandbox editor sections start collapsed and keep disclosure help reachable', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({
    kind: 'sandbox-editor',
    seed: {
      name: 'compact', filesystem: [], environment: [], includes: [], agent_directories: [],
      network: { baseline: 'deny', packs: ['net-local'], allow: [] },
      unix_sockets: { mode: '' },
    },
    options: {},
  });
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state);
  await harness.act(() => Promise.resolve());

  const sectionIDs = [
    'sandbox-profile-editor-network-section',
    'sandbox-profile-editor-unix-sockets-section',
    'sandbox-profile-editor-filesystem-section',
    'sandbox-profile-editor-environment-section',
    'sandbox-profile-editor-pre-launch-section',
    'sandbox-profile-editor-includes-section',
    'sandbox-profile-editor-agent-directories-section',
    'sandbox-profile-editor-effective-policy-section',
  ];
  const sections = sectionIDs.map((id) => host.querySelector(`#${id}`));
  assert.equal(sections.every((section) => section?.tagName === 'DETAILS'), true);
  assert.equal(sections.every((section) => !section.hasAttribute('open')), true,
    'every structured section starts collapsed on editor mount');

  const filesystem = sections[2];
  // LinkeDOM does not implement the browser's native summary-click default
  // action, so reflect the state transition the platform owns.
  filesystem.setAttribute('open', '');
  filesystem.dispatchEvent(new harness.window.Event('toggle'));
  await harness.act(() => Promise.resolve());
  assert.equal(filesystem.hasAttribute('open'), true, 'a collapsed section expands without changing draft state');
  assert.ok(filesystem.querySelector('.sbx-add-row'), 'expanded section controls remain reachable');

  const network = sections[0];
  const help = network.querySelector('.sbx-section-summary .spawn-field-help-trigger');
  assert.equal(help.getAttribute('aria-controls'), 'sandbox-profile-editor-network-help-hint');
  help.click();
  await harness.act(() => Promise.resolve());
  assert.equal(help.getAttribute('aria-expanded'), 'true');
  assert.equal(network.hasAttribute('open'), false,
    'opening associated help does not accidentally expand the section');
  assert.match(network.querySelector('#sandbox-profile-editor-network-help-hint').textContent,
    /Deny all starts closed/);
  unmount();
});

test('sandbox editor section summaries show live profile entry counts', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({
    kind: 'sandbox-editor',
    seed: {
      name: 'counted',
      filesystem: [
        { path: '/workspace', access: 'read' },
        { path: '$GOCACHE', access: 'write' },
      ],
      environment: [{ name: 'POLICY_OWNER', value: 'agent' }],
      pre_launch: [
        { name: 'paths', script: 'export PATH=/tools:$PATH\n', exports: ['PATH'] },
        { name: 'session', script: 'export SESSION=ready\n', exports: ['SESSION'] },
      ],
      includes: [],
      agent_directories: ['GOCACHE'],
      network: {
        baseline: 'deny',
        packs: ['net-local'],
        allow: [{ domain: 'example.com' }, { loopback: true }],
        deny: [{ cidr: '192.0.2.0/24' }],
      },
      unix_sockets: { mode: 'list', allow: [{ path: '/tmp/example.sock' }] },
    },
    options: {},
  });
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    async loadCommonRuleCatalog() { return new Promise(() => {}); },
  });
  await harness.act(() => Promise.resolve());

  const count = (id) =>
    host.querySelector(`#${id} > .sbx-section-summary > .sbx-section-count`);
  assert.equal(count('sandbox-profile-editor-network-section').textContent, '4 entries',
    'network counts authored pack selections plus manual destinations without waiting for the catalog');
  assert.equal(count('sandbox-profile-editor-unix-sockets-section').textContent, '1 entry');
  assert.equal(count('sandbox-profile-editor-filesystem-section').textContent, '2 entries');
  assert.equal(count('sandbox-profile-editor-environment-section').textContent, '1 entry');
  assert.equal(count('sandbox-profile-editor-pre-launch-section').textContent, '2 entries');
  const includesCount = count('sandbox-profile-editor-includes-section');
  assert.equal(includesCount.textContent, '0 entries');
  assert.equal(includesCount.classList.contains('sbx-section-count-empty'), true,
    'zero is explicitly present but visually subdued');
  assert.equal(count('sandbox-profile-editor-agent-directories-section').textContent, '1 entry');
  assert.equal(count('sandbox-profile-editor-effective-policy-section'), null,
    'the evaluation preview is not presented as an authored entry category');

  await harness.act(() => harness.fireEvent(
    host.querySelector('#sandbox-profile-editor-includes-section .sbx-include-add'), 'click'));
  assert.equal(count('sandbox-profile-editor-includes-section').textContent, '1 entry');
  assert.equal(count('sandbox-profile-editor-includes-section')
    .classList.contains('sbx-section-count-empty'), false);

  const filesystemButtons = host.querySelectorAll(
    '#sandbox-profile-editor-filesystem-section .sbx-filesystem-row > button');
  await harness.act(() => harness.fireEvent(filesystemButtons[filesystemButtons.length - 1], 'click'));
  assert.equal(count('sandbox-profile-editor-filesystem-section').textContent, '1 entry',
    'counts update immediately when a row is removed');
  unmount();
});

test('sandbox pre-launch editor is a first-class ordered multiline section and deletes the last block explicitly', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({
    kind: 'sandbox-editor',
    seed: {
      name: 'scripts', filesystem: [], environment: [], includes: [], agent_directories: [],
      pre_launch: [
        { name: 'first', script: 'export FIRST=1\n', exports: ['FIRST'] },
        { name: 'second', script: 'export SECOND=2\n', exports: ['SECOND'] },
        { name: 'third', script: 'export THIRD=3\n', exports: ['THIRD'] },
      ],
    },
    options: {},
  });
  const saves = [];
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    async saveSandbox(value) { saves.push(value); },
  });
  await harness.act(() => Promise.resolve());

  const section = host.querySelector('#sandbox-profile-editor-pre-launch-section');
  assert.equal(section.tagName, 'DETAILS');
  assert.equal(section.hasAttribute('open'), false, 'the peer section starts folded like the others');
  assert.equal(section.querySelector('.sbx-section-count').textContent, '3 entries');
  assert.match(section.querySelector('.sbx-prelaunch-intro').textContent, /top to bottom/);
  assert.deepEqual([...section.querySelectorAll('.sbx-prelaunch-order')].map((node) => node.textContent), ['1', '2', '3']);
  assert.equal(section.querySelectorAll('.sbx-prelaunch-script textarea').length, 3,
    'each entry is a real multiline script box');
  assert.deepEqual([...section.querySelectorAll('.sbx-prelaunch-card')].map((card) => card.getAttribute('aria-label')),
    ['Block 1: first', 'Block 2: second', 'Block 3: third'],
    'repeated field labels are scoped by an accessible position-and-name group');

  const movingUp = section.querySelector('button[aria-label="Move block 3 up"]');
  movingUp.focus();
  await harness.act(() => harness.fireEvent(movingUp, 'click'));
  assertSameNode(harness.document.activeElement, movingUp,
    'a moved block keeps keyboard focus on its own stable keyed control');
  assert.deepEqual([...section.querySelectorAll('.sbx-prelaunch-name input')].map((input) => input.value),
    ['first', 'third', 'second'], 'up/down controls change execution order on screen');
  assert.equal(movingUp.getAttribute('aria-label'), 'Move block 2 up');
  await harness.act(() => harness.fireEvent(movingUp, 'click'));
  assertSameNode(harness.document.activeElement, movingUp,
    'repeating the keyboard action continues moving the same block');
  assert.deepEqual([...section.querySelectorAll('.sbx-prelaunch-name input')].map((input) => input.value),
    ['third', 'first', 'second']);
  const exports = section.querySelectorAll('.sbx-prelaunch-exports input')[0];
  exports.value = 'THIRD, PATH XDG_CONFIG_HOME';
  await harness.act(() => harness.fireEvent(exports, 'input'));
  await harness.act(() => harness.fireEvent(host.querySelector('#sandbox-profile-editor-submit'), 'click'));
  assert.deepEqual(saves[0].draft.pre_launch.map((block) => block.name), ['third', 'first', 'second']);
  assert.deepEqual(saves[0].draft.pre_launch[0].exports, ['THIRD', 'PATH', 'XDG_CONFIG_HOME']);
  assert.equal('_exports_text' in saves[0].draft.pre_launch[0], false,
    'editor-only export text never reaches the daemon');
  assert.equal('_editor_id' in saves[0].draft.pre_launch[0], false,
    'stable editor row identities never reach the daemon');

  await harness.act(() => harness.fireEvent(section.querySelector('.sbx-prelaunch-remove'), 'click'));
  await harness.act(() => harness.fireEvent(section.querySelector('.sbx-prelaunch-remove'), 'click'));
  await harness.act(() => harness.fireEvent(section.querySelector('.sbx-prelaunch-remove'), 'click'));
  assert.equal(section.querySelector('.sbx-section-count').textContent, '0 entries');
  await harness.act(() => harness.fireEvent(host.querySelector('#sandbox-profile-editor-submit'), 'click'));
  assert.deepEqual(saves[1].draft.pre_launch, [],
    'removing the final card retains explicit clear intent');
  unmount();
  host.remove();
});

test('sandbox pre-launch editor and Advanced raw JSON stay synchronized in both directions', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-editor', seed: {
    name: 'scripts', filesystem: [], environment: [], includes: [], agent_directories: [],
    pre_launch: [{ name: 'setup', script: 'export OLD=1\n', exports: ['OLD'] }],
  }, options: {} });
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state);
  await harness.act(() => Promise.resolve());

  const script = host.querySelector('.sbx-prelaunch-script textarea');
  script.value = 'export FORM=1\n';
  await harness.act(() => harness.fireEvent(script, 'input'));
  await harness.act(() => harness.fireEvent(host.querySelector('.sbx-advanced-toggle'), 'click'));
  const raw = host.querySelector('#sandbox-profile-editor-pre-launch');
  assert.match(raw.value, /FORM/,
    'opening Advanced serializes the current first-class form instead of stale seed data');
  raw.value = JSON.stringify([{ name: 'raw', script: 'export RAW=1\n', exports: ['RAW'] }], null, 2);
  await harness.act(() => harness.fireEvent(raw, 'input'));
  await harness.act(() => harness.fireEvent(host.querySelector('.sbx-advanced-toggle'), 'click'));
  assert.equal(host.querySelector('.sbx-prelaunch-name input').value, 'raw');
  assert.equal(host.querySelector('.sbx-prelaunch-script textarea').value, 'export RAW=1\n');
  assert.equal(host.querySelector('.sbx-prelaunch-exports input').value, 'RAW');
  unmount();
  host.remove();
});

test('Advanced clear overrides newly added pre-launch editor rows without leaking private fields', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-editor', seed: {
    name: 'scripts-clear', filesystem: [], environment: [], includes: [], agent_directories: [],
  }, options: {} });
  let saved = null;
  const predictions = [];
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    async saveSandbox(value) { saved = value; },
    async predictSandbox(draft) { predictions.push(draft); return { targets: [], contexts: [] }; },
  });
  await harness.act(() => Promise.resolve());

  const section = host.querySelector('#sandbox-profile-editor-pre-launch-section');
  await harness.act(() => harness.fireEvent(section.querySelector('.sbx-prelaunch-add'), 'click'));
  const name = section.querySelector('.sbx-prelaunch-name input');
  name.value = 'setup';
  await harness.act(() => harness.fireEvent(name, 'input'));
  const script = section.querySelector('.sbx-prelaunch-script textarea');
  script.value = 'export TOOL_HOME=/tmp/tool\n';
  await harness.act(() => harness.fireEvent(script, 'input'));
  const exportsInput = section.querySelector('.sbx-prelaunch-exports input');
  exportsInput.value = 'TOOL_HOME';
  await harness.act(() => harness.fireEvent(exportsInput, 'input'));

  await harness.act(() => harness.fireEvent(host.querySelector('.sbx-advanced-toggle'), 'click'));
  const raw = host.querySelector('#sandbox-profile-editor-pre-launch');
  assert.equal(JSON.parse(raw.value)[0].name, 'setup');
  raw.value = '[]';
  await harness.act(() => harness.fireEvent(raw, 'input'));
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));

  const hasPrivateKey = (value) => value != null && typeof value === 'object'
    && Object.entries(value).some(([key, child]) => key.startsWith('_') || hasPrivateKey(child));
  assert.deepEqual(predictions.at(-1).pre_launch, [],
    'the effective preview treats the authoritative raw empty array as an explicit clear');
  assert.equal(hasPrivateKey(predictions.at(-1)), false,
    'editor-private fields never reach the prediction payload');

  await harness.act(() => harness.fireEvent(host.querySelector('#sandbox-profile-editor-submit'), 'click'));
  assert.deepEqual(saved.draft.pre_launch, [],
    'the authoritative raw empty array clears structured blocks added after a block-less baseline');
  assert.equal(hasPrivateKey(saved.draft), false,
    'no editor-private field reaches any part of the save payload');
  unmount();
  host.remove();
});

test('sandbox pre-launch validation mirrors daemon limits without reserving PATH or XDG names', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/sandbox-pre-launch.js');
  const valid = Array.from({ length: 32 }, (_, index) => ({
    name: index === 0 ? 'A'.repeat(128) : `block-${index}`,
    script: index === 0 ? 'x'.repeat(64 * 1024) : 'true\n',
    exports: index === 0
      ? ['PATH', 'XDG_CONFIG_HOME', ...Array.from({ length: 62 }, (__, i) => `VALUE_${i}`)]
      : [],
  }));
  assert.deepEqual(model.sandboxPreLaunchValidation(valid).errors, [],
    'all four inclusive maxima and reserved-but-intentional exports are accepted');
  assert.deepEqual(model.sandboxPreLaunchExportNames('PATH,,  TOOL_HOME,\t,'),
    ['PATH', 'TOOL_HOME'],
    'repeated and trailing separators do not create empty export names');

  const invalid = [
    { name: 'duplicate', script: 'true\n', exports: [] },
    { name: 'duplicate', script: ' \n', exports: ['NOT-AN-ENV'] },
    { name: 'x'.repeat(129), script: `${'x'.repeat(64 * 1024)}x`, exports: [] },
  ];
  const result = model.sandboxPreLaunchValidation(invalid);
  assert.equal(result.blocks[0].name.some((error) => /unique/.test(error)), true);
  assert.equal(result.blocks[1].name.some((error) => /unique/.test(error)), true);
  assert.equal(result.blocks[1].script.some((error) => /required/.test(error)), true);
  assert.equal(result.blocks[1].exports.some((error) => /environment-variable/.test(error)), true);
  assert.equal(result.blocks[2].name.some((error) => /128 bytes/.test(error)), true);
  assert.equal(result.blocks[2].script.some((error) => /65536/.test(error)), true);
  assert.match(model.sandboxPreLaunchValidation([...valid, { name: 'extra', script: 'true' }]).errors[0],
    /at most 32 blocks/);
  assert.match(model.sandboxPreLaunchValidation([{ name: 'nul', script: 'x\0y' }]).errors[0],
    /NUL/);
});

test('sandbox editor tolerates legacy and modern sparse profile payloads', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const fixtures = [
    ['legacy', {
      // A real pre-access-axis/pre-spellings row: the new fields are absent,
      // rather than materialized as empty objects or documents.
      id: 1, name: 'legacy', filesystem: [], environment: [],
    }],
    ['modern-empty', {
      id: 2, name: 'modern-empty', filesystem: [], environment: [],
      filesystem_spellings: { version: 1, rules: [] },
      network: { mode: 'list' }, unix_sockets: { mode: 'list' },
    }],
    ['modern-populated', {
      id: 3, name: 'modern-populated',
      filesystem: [{ path: '/work', access: 'read' }],
      filesystem_spellings: {
        version: 1,
        rules: [{ resolved_path: '/work', spellings: ['/workspace'] }],
      },
      environment: [],
      network: { mode: 'list', allow: [{ domain: 'example.com' }] },
      unix_sockets: { mode: 'list', allow: [{ path: '/tmp/example.sock' }] },
    }],
  ];

  for (const [label, seed] of fixtures) {
    const state = createManagementState();
    state.openDialog({ kind: 'sandbox-editor', seed, options: {} });
    let saved = null;
    const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
      async saveSandbox(value) { saved = value; },
      async predictSandbox() {
        const enforced = {
          filesystem: { outcome: 'enforced', detail: 'directory detail' },
          environment: { outcome: 'enforced', detail: 'environment detail' },
          agent_directories: { outcome: 'enforced', detail: 'agent-directory detail' },
          network: { outcome: 'enforced', detail: 'network detail' },
          unix_sockets: { outcome: 'enforced', detail: 'socket detail' },
        };
        return {
          targets: [{
            target: { implementation: 'tclaude-layer', harness: 'claude', platform: 'linux' },
            axes: enforced, context_axes: [enforced],
          }],
          contexts: [{
            context: {},
            // Empty Go slices are intentionally omitted from these rule
            // objects. This is the exact async response shape that crashed the
            // effective-policy preview after an existing profile was loaded.
            network: { mode: 'list' },
            unix_sockets: { mode: 'list' },
            agentd_socket: 'always reachable',
          }],
        };
      },
    });
    await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));

    assert.ok(host.querySelector('#sandbox-profile-editor-modal'), `${label} renders`);
    const applied = host.querySelector('.sbx-rule-bucket-applied');
    assert.equal(applied.hasAttribute('open'), false,
      `${label} keeps fully supported rules folded even when every rule is supported`);
    assert.equal(applied.querySelector('.sbx-rule-count').textContent, '3');
    assert.match(applied.textContent,
      /Block outbound network \(allow list is empty\).*Block Unix sockets \(allow list is empty\)/s,
      `${label} renders sparse effective axes as concrete empty-list rules`);
    const partial = host.querySelector('.sbx-rule-bucket-partial');
    assert.equal(partial.hasAttribute('open'), true);
    assert.equal(partial.querySelector('.sbx-rule-count').textContent, '0',
      `${label} keeps an empty partial category visible`);
    const unsupported = host.querySelector('.sbx-rule-bucket-not-applied');
    assert.equal(unsupported.hasAttribute('open'), true);
    assert.equal(unsupported.querySelector('.sbx-rule-count').textContent, '0',
      `${label} keeps an empty unsupported category visible`);

    if (label === 'legacy') {
      const baseline = host.querySelector('#sandbox-profile-editor-network-baseline');
      baseline.querySelector('option[value="deny"]').selected = true;
      await harness.act(() => harness.fireEvent(baseline, 'change'));
      const networkSection = baseline.closest('#sandbox-profile-editor-network-section');
      await harness.act(() => harness.fireEvent(networkSection.querySelector('.sbx-add-row'), 'click'));
      const hostInput = networkSection.querySelector('.sbx-network-value');
      hostInput.value = 'api.example.com';
      await harness.act(() => harness.fireEvent(hostInput, 'input'));
      assert.equal(networkSection.querySelectorAll('.sbx-network-row').length, 1,
        'a legacy profile can add a network destination');
      await harness.act(() => harness.fireEvent(host.querySelector('#sandbox-profile-editor-submit'), 'click'));
      assert.equal(saved.draft.network.baseline, 'deny');
      assert.deepEqual(saved.draft.network.packs, []);
      assert.equal(saved.draft.network.allow[0].host, 'api.example.com');
    }

    unmount();
    host.remove();
  }
});

test('sandbox editor renders one authored-spelling row and keeps authority pinned through preview', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({
    kind: 'sandbox-editor',
    seed: {
      name: 'spelled',
      filesystem: [{ path: '/canonical/work', access: 'read' }],
      filesystem_spellings: {
        version: 1,
        rules: [{ resolved_path: '/canonical/work', spellings: ['/workspace', '/Volumes/Work'] }],
      },
      environment: [], includes: [], agent_directories: [],
    },
    options: {},
  });
  const saves = [];
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    async saveSandbox(value) { saves.push(value); },
  });
  await harness.act(() => Promise.resolve());

  const rows = [...host.querySelectorAll('.sbx-section .sbx-row')]
    .filter((row) => row.querySelector('.sbx-path') && !row.classList.contains('sbx-global-row'));
  assert.equal(rows.length, 1, 'retained spellings never become duplicate authority rows');
  assert.equal(rows[0].querySelector('.sbx-path').value, '/workspace');
  assert.match(rows[0].querySelector('.sbx-binding-target').textContent,
    /binds → \/canonical\/work · also retained: \/Volumes\/Work/);

  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(saves[0].draft.filesystem, [{ path: '/canonical/work', access: 'read' }]);
  assert.deepEqual(saves[0].draft.filesystem_spellings.rules[0].spellings,
    ['/workspace', '/Volumes/Work'], 'ordinary preview revalidates the pinned spelling sidecar');

  const input = rows[0].querySelector('.sbx-path');
  input.value = '/new-spelling';
  input.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(saves[1].draft.filesystem, [{ path: '/new-spelling', access: 'read' }]);
  assert.equal(saves[1].draft.filesystem_spellings, null,
    'editing a path explicitly reauthors it instead of recomputing old authority');

  host.querySelector('.sbx-advanced-toggle').click();
  await harness.act(() => Promise.resolve());
  assert.match(host.querySelector('#sandbox-profile-editor-filesystem').value, /new-spelling/);
  assert.equal(host.querySelector('#sandbox-profile-editor-filesystem-spellings').value, 'null',
    'raw JSON exposes the complete authority/sidecar pair');
  unmount();
});

test('sandbox editor groups concrete rules by the selected assignment outcome', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-editor', seed: {
    id: 41, name: 'access', filesystem: [], environment: [], includes: [], agent_directories: [],
    network: { mode: 'list', allow: [{ domain: 'api.anthropic.com', ports: [443] }] },
    unix_sockets: { mode: 'closed' },
  }, options: { group: 'crew' }, catalog: [
    { name: 'claude', display_name: 'Claude Code', can_builtin_os_sandbox: true, can_tclaude_layer: true, can_stacked: true },
    { name: 'codex', display_name: 'Codex', can_builtin_os_sandbox: true, can_tclaude_layer: true, can_stacked: true },
    { name: 'opencode', display_name: 'OpenCode', can_builtin_os_sandbox: false, can_tclaude_layer: true, can_stacked: false },
  ] });
  const predictions = [];
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    async predictSandbox(draft, targets, context) {
      predictions.push({ draft, targets, context });
      const target = targets[0]
        || { implementation: 'tclaude-layer', harness: 'claude', platform: 'linux' };
      const codexBuiltin = target.implementation === 'harness-builtin'
        && target.harness === 'codex';
      const networkDetail = codexBuiltin
        ? 'Codex has no filtered network sandbox yet. Its upstream proxy is experimental and off by default; it admits only proxy-aware clients and on Linux prevents access to the tclaude agentd socket, so it cannot enforce this profile’s ordinary TCP/UDP access list. Use tclaude-layer filtering on Linux, or choose network open (Allow all).'
        : 'resolver-owned network detail';
      return {
        targets: [{ target, resolved_by: 'harness default', predicted: true, axes: {
          filesystem: { tier: '11 effective contexts', outcome: 'refused', detail: 'another assignment refuses its carve-out' },
          environment: { tier: '1 variable', outcome: 'enforced', detail: 'resolver-owned environment detail' },
          agent_directories: { tier: '1 directory', outcome: 'enforced', detail: 'resolver-owned agent-directory detail' },
          network: { tier: 'list', outcome: 'not_enforced', detail: networkDetail },
          unix_sockets: { tier: 'closed', outcome: 'enforced', detail: 'resolver-owned socket detail' },
        }, network_entries: [{
          entry: { domain: 'api.anthropic.com', ports: [443] },
          outcome: codexBuiltin ? 'not_enforced' : 'enforced_partial',
          detail: codexBuiltin ? networkDetail : 'DNS identity follows a bounded lease.',
        }], context_axes: [{
          filesystem: { tier: '1 deny · 1 write', outcome: 'enforced_partial', detail: 'built-in tools cannot preserve this carve-out' },
          environment: { tier: '1 variable', outcome: 'enforced', detail: 'resolver-owned environment detail' },
          agent_directories: { tier: '1 directory', outcome: 'enforced', detail: 'resolver-owned agent-directory detail' },
          network: { tier: 'list', outcome: 'not_enforced', detail: networkDetail },
          unix_sockets: { tier: 'closed', outcome: 'enforced', detail: 'resolver-owned socket detail' },
        }] }],
        contexts: [{
          context: { global: 'base', group: 'access', group_name: 'crew' },
          darwin_allow_mach_register: true,
          filesystem: [{ path: '/home/operator', access: 'deny' }, { path: '/home/operator/work', access: 'write' }],
          environment: ['POLICY_OWNER'],
          agent_directories: ['GOCACHE'],
          network: { mode: 'list', allow: [] }, unix_sockets: { mode: 'closed' }, agentd_socket: 'always reachable', notices: [{
          class: 'composition', axis: 'network', reason: 'empty_intersection', effect: 'nothing_allowed',
          detail: 'global “base” ∩ group “access” leaves no network destinations', tiers: ['global "base"', 'group "access"'],
        }] }],
      };
    },
  });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  assert.ok(host.querySelector('#sandbox-profile-editor-network-baseline'));
  assert.ok(host.querySelector('#sandbox-profile-editor-unix-sockets-mode'));
  assertAbsent(host.querySelector('#sandbox-profile-editor-evaluate-for'), 'the preview does not encode every target permutation in one selector');
  const evaluationHarness = host.querySelector('#sandbox-profile-editor-evaluate-harness');
  const evaluationImplementation = host.querySelector('#sandbox-profile-editor-evaluate-implementation');
  const evaluationPlatform = host.querySelector('#sandbox-profile-editor-evaluate-platform');
  assert.deepEqual([...evaluationHarness.options].map((option) => option.value),
    ['', 'claude', 'codex', 'opencode']);
  assert.equal(evaluationImplementation.disabled, true,
    'the resolved default keeps the other target axes under daemon control');
  choose(evaluationHarness, 'codex');
  await harness.act(() => harness.fireEvent(evaluationHarness, 'change'));
  assert.deepEqual(
    [...host.querySelector('#sandbox-profile-editor-evaluate-implementation').options]
      .map((option) => option.textContent),
    [
      'Codex built-in sandbox',
      'tclaude sandbox',
      'Stacked sandboxes',
    ],
  );
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  assertAbsent(host.querySelector('.sbx-network-badge'), 'allow rows do not duplicate the Effective policy preview with a per-row verdict');
  assert.match(host.querySelector('.sbx-policy-target').textContent,
    /Codex on Linux · built-in sandbox · no filtered network sandbox yet/);
  assert.match(host.querySelector('.sbx-rule-bucket-not-applied').textContent,
    /Unsupported:.*ordinary TCP\/UDP access list.*network open \(Allow all\)/s);

  choose(evaluationHarness, 'opencode');
  await harness.act(() => harness.fireEvent(evaluationHarness, 'change'));
  assert.deepEqual(
    [...host.querySelector('#sandbox-profile-editor-evaluate-implementation').options]
      .map((option) => option.value),
    ['tclaude-layer'],
    'OpenCode offers only its real tclaude OS sandbox, never soft access-control',
  );
  choose(evaluationPlatform, 'darwin');
  await harness.act(() => harness.fireEvent(evaluationPlatform, 'change'));
  assertAbsent(host.querySelector('.sbx-network-badge'), 'target changes keep evaluation status in the preview instead of adding row verdicts');
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  assert.deepEqual(predictions.at(-1).targets, [{
    implementation: 'tclaude-layer',
    harness: 'opencode',
    platform: 'darwin',
    sandbox: 'tclaude-layer',
  }]);
  assert.equal(host.querySelector('.sbx-network-ports').value, '443');
  assert.match(host.querySelector('.sbx-policy-target').textContent,
    /OpenCode on macOS · tclaude sandbox/);
  assert.match(host.querySelector('.sbx-mach-register-evaluation').textContent,
    /Mach service registration.*Allowed by the tclaude Seatbelt layer for this target/s,
    'the preview discloses that the composed compatibility capability applies to this target');
  const applied = host.querySelector('.sbx-rule-bucket-applied');
  assert.equal(applied.hasAttribute('open'), false,
    'fully supported rules always start folded');
  assert.equal(applied.querySelector('.sbx-rule-count').textContent, '4');
  assert.match(applied.textContent,
    /Set environment: POLICY_OWNER.*Private read\/write directory: \$GOCACHE.*Block Unix sockets.*tclaude agent control/s);
  const partial = host.querySelector('.sbx-rule-bucket-partial');
  assert.equal(partial.hasAttribute('open'), true);
  assert.equal(partial.querySelector('.sbx-rule-count').textContent, '2');
  assert.match(partial.textContent,
    /Partially supported rules.*Block: \/home\/operator.*Read\/write: \/home\/operator\/work.*built-in tools cannot preserve this carve-out/s);
  assert.doesNotMatch(partial.textContent, /another assignment refuses/,
    'the selected assignment uses its own verdict instead of the aggregate worst case');
  const notApplied = host.querySelector('.sbx-rule-bucket-not-applied');
  assert.equal(notApplied.hasAttribute('open'), true);
  assert.equal(notApplied.querySelector('.sbx-rule-count').textContent, '1');
  assert.match(notApplied.textContent,
    /Block outbound network \(allow list is empty\).*resolver-owned network detail/s);
  const otherAssignments = host.querySelector('.sbx-other-assignments');
  assert.equal(otherAssignments.getAttribute('role'), 'alert');
  assert.match(otherAssignments.textContent,
    /Other assignments need attention.*including any omitted.*Directory rules:.*another assignment refuses its carve-out/s);
  const announcements = [...host.querySelectorAll('.sbx-a11y-status')];
  assert.equal(announcements[0].getAttribute('role'), 'status');
  assert.match(announcements[0].textContent, /2 partially supported rules and 1 unsupported rule/);
  assert.match(announcements[1].textContent,
    /Policy composition warning:.*leaves no network destinations/);
  const composition = host.querySelector('.sbx-composition-details');
  assert.equal(composition.hasAttribute('open'), false);
  assert.match(composition.textContent, /How these rules were combined/);
  assert.doesNotMatch(composition.textContent, /leaves no network destinations/,
    'evaluation warnings are not buried in the secondary composition disclosure');
  const effectiveSection = host.querySelector('#sandbox-profile-editor-effective-policy-section');
  assert.equal(effectiveSection.open, true,
    'a composition warning opens its owning evaluation section');
  assert.match(effectiveSection.querySelector('.sbx-section-body').textContent,
    /leaves no network destinations/);
  assertAbsent(host.querySelector('.sbx-capability-preview'), 'raw evaluator axes are not exposed in the primary read model');
  assert.equal(host.querySelector('#sandbox-profile-editor-submit').disabled, false,
    'empty intersections warn but never block save');
  assert.equal(predictions[0].draft.id, 41);
  assert.equal(predictions[0].context.group, 'crew');
  const rawToggle = host.querySelector('.sbx-advanced-toggle');
  rawToggle.click();
  await harness.act(() => Promise.resolve());
  assert.match(host.querySelector('#sandbox-profile-editor-network').value, /api\.anthropic\.com/);
  assert.match(host.querySelector('#sandbox-profile-editor-unix-sockets').value, /closed/);
  unmount();
});

test('effective-policy target alerts open their collapsed section without a composition notice', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-editor', seed: {
    name: 'attention', filesystem: [{ path: '/blocked', access: 'deny' }],
    environment: [], includes: [], agent_directories: [],
    network: { baseline: 'allow', packs: [], allow: [] }, unix_sockets: { mode: '' },
  }, options: {} });
  const contextAxes = {
    filesystem: { outcome: 'enforced', detail: 'selected assignment is enforced' },
    environment: { outcome: 'enforced', detail: 'environment is enforced' },
    agent_directories: { outcome: 'enforced', detail: 'agent directories are enforced' },
    network: { outcome: 'enforced', detail: 'network is enforced' },
    unix_sockets: { outcome: 'enforced', detail: 'sockets are enforced' },
  };
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    async predictSandbox() {
      return {
        targets: [{
          target: { implementation: 'tclaude-layer', harness: 'claude', platform: 'linux' },
          axes: {
            ...contextAxes,
            filesystem: { outcome: 'refused', detail: 'another assignment refuses this rule' },
          },
          context_axes: [contextAxes],
        }],
        contexts: [{
          context: {}, filesystem: [{ path: '/blocked', access: 'deny' }],
          environment: [], agent_directories: [], network: { mode: 'open' },
          unix_sockets: { mode: '' }, notices: [],
        }],
      };
    },
  });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  assert.equal(host.querySelector('#sandbox-profile-editor-effective-policy-section').open, true,
    'a target alert opens Effective policy preview even without a composition warning');
  assert.match(host.querySelector('.sbx-other-assignments').textContent,
    /Other assignments need attention.*another assignment refuses this rule/s);
  unmount();
});

test('blank new sandbox drafts do not request an enforcement prediction', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-editor', seed: null, options: {} });
  let predictionCalls = 0;
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    async predictSandbox() { predictionCalls++; throw new Error('blank drafts must not reach prediction'); },
  });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  assert.equal(predictionCalls, 0);
  assertAbsent(host.querySelector('.sbx-capability-error'));
  unmount();
});

test('sandbox enforcement preview pauses for an incomplete access row and resumes when completed', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-editor', seed: {
    name: 'network-draft', filesystem: [], environment: [], includes: [], agent_directories: [],
    network: { mode: 'list', allow: [] }, unix_sockets: { mode: 'closed' },
  }, options: {} });
  const predictions = [];
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    async predictSandbox(draft) {
      predictions.push(draft);
      return { targets: [], contexts: [] };
    },
  });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  assert.equal(predictions.length, 1);

  const network = host.querySelector('#sandbox-profile-editor-network-section');
  await harness.act(() => harness.fireEvent(network.querySelector('.sbx-add-row'), 'click'));
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  assert.equal(predictions.length, 1, 'an incomplete row never reaches the enforcement endpoint');
  assert.match(host.querySelector('.sbx-preview-status').textContent,
    /preview paused: Network allow row 1 must set exactly one selector/);

  const value = network.querySelector('.sbx-network-value');
  value.value = 'api.example.com';
  await harness.act(() => harness.fireEvent(value, 'input'));
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  assert.equal(predictions.length, 2, 'preview resumes after the row becomes valid');
  assertAbsent(host.querySelector('.sbx-preview-status'));
  assert.equal(predictions.at(-1).network.allow[0].host, 'api.example.com');
  unmount();
});

test('sandbox network selector retains empty domain and CIDR kinds through save and reload', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const cases = [
    ['domain', 'api.example.com', 'example.com'],
    ['cidr', '192.0.2.0/24', '192.0.2.0/24'],
  ];

  for (const [kind, authored, placeholder] of cases) {
    const state = createManagementState();
    state.openDialog({ kind: 'sandbox-editor', seed: {
      name: `network-${kind}`, filesystem: [], environment: [], includes: [], agent_directories: [],
      network: { mode: 'list', allow: [{ host: 'old.example.com' }] },
      unix_sockets: { mode: 'closed' },
    }, options: {} });
    let saved = null;
    const mounted = mountSandboxEditor(harness, mountManagementIsland, state, {
      async saveSandbox(value) { saved = value; },
    });
    await harness.act(() => Promise.resolve());

    const selector = mounted.host.querySelector('.sbx-network-selector');
    choose(selector, kind);
    await harness.act(() => harness.fireEvent(selector, 'change'));
    const renderedSelector = mounted.host.querySelector('.sbx-network-selector');
    const value = mounted.host.querySelector('.sbx-network-value');
    assert.equal(renderedSelector.querySelector('option:checked')?.value, kind,
      `${kind} remains selected with an empty value`);
    assert.equal(value.value, '');
    assert.equal(value.placeholder, placeholder);

    value.value = authored;
    await harness.act(() => harness.fireEvent(value, 'input'));
    await harness.act(() => harness.fireEvent(
      mounted.host.querySelector('#sandbox-profile-editor-submit'), 'click'));
    assert.equal(saved.draft.network.allow[0][kind], authored);
    assert.equal(Object.hasOwn(saved.draft.network.allow[0], 'host'), false,
      `${kind} selection drops the previous host selector`);
    mounted.unmount();
    mounted.host.remove();

    const reopenedState = createManagementState();
    reopenedState.openDialog({ kind: 'sandbox-editor', seed: saved.draft, options: {} });
    const reopened = mountSandboxEditor(harness, mountManagementIsland, reopenedState);
    await harness.act(() => Promise.resolve());
    assert.equal(selectedValue(reopened.host.querySelector('.sbx-network-selector')), kind);
    assert.equal(reopened.host.querySelector('.sbx-network-value').value, authored);
    reopened.unmount();
    reopened.host.remove();
  }

  const mixedState = createManagementState();
  mixedState.openDialog({ kind: 'sandbox-editor', seed: {
    name: 'network-mixed', filesystem: [], environment: [], includes: [], agent_directories: [],
    network: { mode: 'list', allow: [{ domain: '', cidr: '192.0.2.0/24' }] },
    unix_sockets: { mode: 'closed' },
  }, options: {} });
  const mixed = mountSandboxEditor(harness, mountManagementIsland, mixedState);
  await harness.act(() => Promise.resolve());
  assert.equal(selectedValue(mixed.host.querySelector('.sbx-network-selector')), 'cidr',
    'a truthy selector takes precedence over an unrelated empty key');
  assert.equal(mixed.host.querySelector('.sbx-network-value').value, '192.0.2.0/24');
  mixed.unmount();
  mixed.host.remove();

  const falseLoopbackState = createManagementState();
  falseLoopbackState.openDialog({ kind: 'sandbox-editor', seed: {
    name: 'network-false-loopback', filesystem: [], environment: [], includes: [], agent_directories: [],
    network: { mode: 'list', allow: [{ loopback: false }] },
    unix_sockets: { mode: 'closed' },
  }, options: {} });
  const falseLoopback = mountSandboxEditor(harness, mountManagementIsland, falseLoopbackState);
  await harness.act(() => Promise.resolve());
  assert.equal(selectedValue(falseLoopback.host.querySelector('.sbx-network-selector')), 'host');
  assert.equal(falseLoopback.host.querySelector('.sbx-network-value').value, '');
  assert.match(falseLoopback.host.querySelector('.sbx-access-validation').textContent,
    /must set exactly one selector/);
  falseLoopback.unmount();
  falseLoopback.host.remove();
});

test('new deny drafts apply default pack references once and pack rows stay read-only', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-editor', seed: {
    name: 'existing-list', filesystem: [], environment: [], includes: [], agent_directories: [],
    network: { mode: 'list', allow: [{ loopback: true }] }, unix_sockets: { mode: '' },
  }, options: {} });
  let saved = null;
  const mounted = mountSandboxEditor(harness, mountManagementIsland, state, {
    async saveSandbox(value) { saved = value; },
  });
  await harness.act(() => Promise.resolve());

  const baseline = mounted.host.querySelector('#sandbox-profile-editor-network-baseline');
  assert.deepEqual([...baseline.options].map((option) => [option.value, option.textContent]), [
    ['deny', 'Deny all'],
    ['allow', 'Allow all'],
    ['inherit', 'No override'],
  ]);
  assert.equal(selectedValue(baseline), 'deny');
  assert.equal(mounted.host.querySelectorAll('.sbx-network-manual-rows .sbx-network-row').length, 1,
    'a legacy list stays authored as a manual row');
  assert.equal(mounted.host.querySelectorAll('.sbx-network-pack-rows .sbx-network-row').length, 0,
    'an exact legacy preset never infers a stored pack reference');
  await harness.act(() => harness.fireEvent(
    mounted.host.querySelector('#sandbox-profile-editor-submit'), 'click'));
  assert.deepEqual(saved.draft.network, {
    baseline: 'deny', packs: [], deny_packs: [], allow: [{ loopback: true }], deny: [],
  });
  mounted.unmount();
  mounted.host.remove();

  const newState = createManagementState();
  newState.openDialog({ kind: 'sandbox-editor', seed: null, options: {} });
  const newDraft = mountSandboxEditor(harness, mountManagementIsland, newState);
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 50)));
  const newBaseline = newDraft.host.querySelector('#sandbox-profile-editor-network-baseline');
  assert.equal(selectedValue(newBaseline), 'inherit');
  await harness.act(() => { choose(newBaseline, 'deny'); harness.fireEvent(newBaseline, 'change'); });
  const packModes = [...newDraft.host.querySelectorAll('.sbx-network-pack-mode')];
  const packDisclosure = newDraft.host.querySelector('.sbx-network-packs');
  assert.equal(packDisclosure.hasAttribute('open'), false,
    'built-in packs use the editor collapsed-by-default disclosure pattern');
  const allowed = packModes.filter((control) => segmentedValue(control) === 'allow');
  assert.equal(allowed.length, 3);
  assert.match(allowed.map((control) => control.parentElement.textContent).join(' '),
    /Local access.*Anthropic API.*OpenAI API/s);
  const packRows = [...newDraft.host.querySelectorAll('.sbx-network-pack')];
  assert.equal(packRows.length, 3);
  assert.equal(packRows.every((row) => row.querySelectorAll('.sbx-network-pack-mode').length === 1), true,
    'built-in packs render as one dense three-state row each');
  assert.equal(packRows.every((row) => row.querySelector('.sbx-network-pack-mode').getAttribute('role') === 'radiogroup'), true,
    'each pack mode is exposed as one named radio group');
  assert.equal(packRows.every((row) => row.querySelector('.sbx-network-pack-label strong') === null), true,
    'pack labels use normal body typography instead of heading emphasis');
  assert.equal(packRows.every((row) => row.querySelector('small') === null), true,
    'pack disclosure copy is no longer permanently rendered under the label');
  const localHelp = packRows[0].querySelector('.spawn-field-help-trigger');
  assert.ok(localHelp, 'a pack with explanatory copy has an associated help trigger');
  await harness.act(() => harness.fireEvent(localHelp, 'click'));
  assert.equal(localHelp.getAttribute('aria-expanded'), 'true');
  assert.match(packRows[0].querySelector('.spawn-field-description').textContent, /local services/);
  assert.equal(newDraft.host.querySelectorAll('.sbx-network-pack-row').length, 3);
  assertAbsent(newDraft.host.querySelector('.sbx-network-pack-row input'), 'expanded pack destinations are read-only');
  assertAbsent(newDraft.host.querySelector('.sbx-network-pack-row .sbx-segmented-control'), 'read-only expanded destinations never instantiate the segmented component');
  assert.ok(newDraft.host.querySelector('.sbx-network-pack-row .sbx-network-mode-readonly.sbx-state-allow'),
    'read-only modes use the same explicit color-state mapping as editable controls');

  const keyboardMode = packModes[0];
  const allowRadio = segment(keyboardMode, 'allow');
  allowRadio.focus();
  assert.equal(allowRadio.getAttribute('tabindex'), '0');
  assert.equal(segment(keyboardMode, 'off').getAttribute('tabindex'), '-1');
  assert.equal(segment(keyboardMode, 'deny').getAttribute('tabindex'), '-1');
  await harness.act(() => harness.fireEvent(allowRadio, 'keydown', { key: 'ArrowRight' }));
  assert.equal(segmentedValue(keyboardMode), 'deny');
  assert.equal(segment(keyboardMode, 'deny').classList.contains('sbx-state-deny'), true);
  assert.equal(segment(keyboardMode, 'deny').classList.contains('is-selected'), true);
  assert.equal(segment(keyboardMode, 'deny').getAttribute('tabindex'), '0');
  assertSameNode(harness.document.activeElement, segment(keyboardMode, 'deny'),
    'arrow selection moves the roving focus with the authored value');
  await harness.act(() => harness.fireEvent(segment(keyboardMode, 'deny'), 'keydown', { key: 'ArrowLeft' }));
  assert.equal(segmentedValue(keyboardMode), 'allow');
  assert.equal(segment(keyboardMode, 'allow').classList.contains('sbx-state-allow'), true);
  await harness.act(() => harness.fireEvent(segment(keyboardMode, 'allow'), 'keydown', { key: 'Home' }));
  assert.equal(segmentedValue(keyboardMode), 'off');
  assert.equal(segment(keyboardMode, 'off').classList.contains('sbx-state-off'), true);
  await harness.act(() => harness.fireEvent(segment(keyboardMode, 'off'), 'keydown', { key: 'End' }));
  assert.equal(segmentedValue(keyboardMode), 'deny');
  await harness.act(() => harness.fireEvent(segment(keyboardMode, 'deny'), 'keydown', { key: 'ArrowDown' }));
  assert.equal(segmentedValue(keyboardMode), 'off', 'Down wraps to the first segment');
  await harness.act(() => harness.fireEvent(segment(keyboardMode, 'off'), 'keydown', { key: 'ArrowUp' }));
  assert.equal(segmentedValue(keyboardMode), 'deny', 'Up wraps to the last segment');
  await harness.act(() => harness.fireEvent(segment(keyboardMode, 'allow'), 'click'));
  assert.equal(segmentedValue(keyboardMode), 'allow',
    'click and keyboard both author through the same segmented control');

  await harness.act(() => { choose(newBaseline, 'allow'); harness.fireEvent(newBaseline, 'change'); });
  assert.equal([...newDraft.host.querySelectorAll('.sbx-network-pack-mode')]
    .filter((control) => segmentedValue(control) === 'allow').length, 3,
    'allow-all retains explicitly authored allow packs');
  assert.equal([...newDraft.host.querySelectorAll('.sbx-network-pack-mode')]
    .every((control) => [...control.querySelectorAll('[role="radio"]')].every((radio) => !radio.disabled)), true,
    'allow-all unlocks pack authoring for deny restrictions');
  assert.match(newDraft.host.querySelector('.sbx-network-redundant').textContent,
    /Redundant under Allow all/);
  assert.equal(newDraft.host.querySelector('.sbx-network-pack .spawn-field-help-trigger')
    .hasAttribute('disabled'), false,
    'pack disclosure stays reachable');
  assert.equal(newDraft.host.querySelector('.sbx-network-unlocks').hasAttribute('aria-disabled'), false,
    'the container does not contradict its intentionally operable help controls');
  await harness.act(() => { choose(newBaseline, 'deny'); harness.fireEvent(newBaseline, 'change'); });
  assert.equal([...newDraft.host.querySelectorAll('.sbx-network-pack-mode')]
    .filter((control) => segmentedValue(control) === 'allow').length, 3,
    'returning to Deny all retains authored pack modes');
  const turnOff = newDraft.host.querySelector('.sbx-network-pack-mode');
  await harness.act(() => harness.fireEvent(segment(turnOff, 'off'), 'click'));
  assert.equal(newDraft.host.querySelectorAll('.sbx-network-pack-row').length, 2,
    'turning off a default pack takes effect');
  const currentBaseline = newDraft.host.querySelector('#sandbox-profile-editor-network-baseline');
  await harness.act(() => { choose(currentBaseline, 'inherit'); harness.fireEvent(currentBaseline, 'change'); });
  assert.equal([...newDraft.host.querySelectorAll('.sbx-network-pack-mode')]
    .every((control) => [...control.querySelectorAll('[role="radio"]')].every((radio) => radio.disabled)), true,
    'No override disables all pack mode controls');
  const inheritedBaseline = newDraft.host.querySelector('#sandbox-profile-editor-network-baseline');
  await harness.act(() => { choose(inheritedBaseline, 'deny'); harness.fireEvent(inheritedBaseline, 'change'); });
  assert.equal(newDraft.host.querySelectorAll('.sbx-network-pack-row').length, 2,
    'an Off default stays Off across No override flips; defaults are never re-applied');
  newDraft.unmount();
  newDraft.host.remove();
});

test('network packs and manual destinations author deny mode without inline verdicts', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-editor', seed: {
    name: 'deny-modes', filesystem: [], environment: [], includes: [], agent_directories: [],
    network: {
      baseline: 'allow', packs: [], deny_packs: ['net-local'],
      allow: [], deny: [{ domain: 'blocked.example', ports: [443] }],
    },
    unix_sockets: { mode: '' },
  }, options: {} });
  let saved = null;
  const mounted = mountSandboxEditor(harness, mountManagementIsland, state, {
    async predictSandbox() {
      return {
        targets: [{
          network_entries: [
            {
              mode: 'deny', entry: { loopback: true },
              keys: ['deny:{"loopback":true}'],
              outcome: 'not_enforced',
              detail: 'This deny rule is saved, but this launch target does not apply network deny entries.',
            },
            {
              mode: 'deny', entry: { domain: 'blocked.example', ports: [443] },
              keys: ['deny:{"domain":"blocked.example","ports":[443]}'],
              outcome: 'not_enforced',
              detail: 'This deny rule is saved, but this launch target does not apply network deny entries.',
            },
          ],
        }],
        contexts: [],
      };
    },
    async saveSandbox(value) { saved = value; },
  });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 50)));
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));

  const network = mounted.host.querySelector('#sandbox-profile-editor-network-section');
  const localPack = [...network.querySelectorAll('.sbx-network-pack')]
    .find((row) => /Local access/.test(row.textContent));
  assert.ok(localPack);
  assert.equal(segmentedValue(localPack.querySelector('.sbx-network-pack-mode')), 'deny');
  const authoredMode = network.querySelector('.sbx-network-rule-mode');
  assert.equal(segmentedValue(authoredMode), 'deny');
  const authoredDeny = segment(authoredMode, 'deny');
  authoredDeny.focus();
  await harness.act(() => harness.fireEvent(authoredDeny, 'keydown', { key: 'ArrowLeft' }));
  const movedMode = network.querySelector('.sbx-network-rule-mode');
  assert.equal(segmentedValue(movedMode), 'allow');
  assertSameNode(harness.document.activeElement, segment(movedMode, 'allow'),
    'a manual row keeps roving focus when its existing mode handler moves it between buckets');
  await harness.act(() => harness.fireEvent(segment(movedMode, 'allow'), 'keydown', { key: 'ArrowRight' }));
  assert.equal(segmentedValue(network.querySelector('.sbx-network-rule-mode')), 'deny');
  assertAbsent(network.querySelector('.sbx-network-badge'), 'deny destinations do not duplicate evaluation-style verdict chips');
  const denyNote = network.querySelector('.sbx-network-deny-note');
  assert.equal(denyNote.textContent,
    'Deny enforcement depends on the launch target — see Effective policy preview.');
  assert.equal(network.querySelectorAll('.sbx-network-deny-note').length, 1,
    'one section-level disclosure covers deny packs and manual rows');

  const networkHelp = mounted.host.querySelector(
    '[aria-controls="sandbox-profile-editor-network-help-hint"]');
  await harness.act(() => harness.fireEvent(networkHelp, 'click'));
  assert.match(mounted.host.querySelector('#sandbox-profile-editor-network-help-hint').textContent,
    /Deny wins.*rule order does not matter/);

  const add = network.querySelector('.sbx-add-row');
  assert.match(add.textContent, /add deny destination/);
  await harness.act(() => harness.fireEvent(add, 'click'));
  const manualModes = [...network.querySelectorAll('.sbx-network-rule-mode')];
  assert.equal(manualModes.length, 2);
  assert.equal(segmentedValue(manualModes[1]), 'deny',
    'Allow all gives actively added destinations a visible Deny mode');
  const deleteButtons = [...network.querySelectorAll('[aria-label="Delete network row"]')];
  await harness.act(() => harness.fireEvent(deleteButtons.at(-1), 'click'));

  const baseline = network.querySelector('#sandbox-profile-editor-network-baseline');
  await harness.act(() => { choose(baseline, 'deny'); harness.fireEvent(baseline, 'change'); });
  const redundant = network.querySelector('.sbx-network-manual-rows .sbx-network-redundant');
  assert.match(redundant.textContent,
    /Redundant under Deny all/);
  assert.equal(redundant.closest('.sbx-network-mode-cell') !== null, true,
    'the subdued redundancy label stays visible beneath the compact mode control');
  assert.match(network.querySelector('.sbx-add-row').textContent, /add allow destination/);

  const manualMode = network.querySelector('.sbx-network-rule-mode');
  await harness.act(() => harness.fireEvent(segment(manualMode, 'allow'), 'click'));
  await harness.act(() => harness.fireEvent(network.querySelector('.sbx-add-row'), 'click'));
  let overlapRow = [...network.querySelectorAll('.sbx-network-manual-rows .sbx-network-row')].at(-1);
  const overlapMode = overlapRow.querySelector('.sbx-network-rule-mode');
  await harness.act(() => harness.fireEvent(segment(overlapMode, 'deny'), 'click'));
  overlapRow = [...network.querySelectorAll('.sbx-network-manual-rows .sbx-network-row')].at(-1);
  const overlapValue = overlapRow.querySelector('.sbx-network-value');
  overlapValue.value = 'telemetry.example';
  await harness.act(() => harness.fireEvent(overlapValue, 'input'));
  assertAbsent(overlapRow.querySelector('.sbx-network-redundant'), 'different DNS names may share an address, so a deny can narrow the release');
  await harness.act(() => harness.fireEvent(
    [...network.querySelectorAll('[aria-label="Delete network row"]')].at(-1), 'click'));
  await harness.act(() => harness.fireEvent(
    mounted.host.querySelector('#sandbox-profile-editor-submit'), 'click'));
  assert.deepEqual(saved.draft.network, {
    baseline: 'deny',
    packs: [],
    deny_packs: ['net-local'],
    allow: [{ domain: 'blocked.example', ports: [443] }],
    deny: [],
  });
  mounted.unmount();
  mounted.host.remove();
});

test('effective preview buckets normalized deny rows with target-specific help', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-editor', seed: {
    name: 'noncanonical-network', filesystem: [], environment: [], includes: [], agent_directories: [],
    network: {
      baseline: 'allow', packs: [], deny_packs: [],
      allow: [], deny: [
        { domain: 'API.EXAMPLE.COM', ports: [443, 443] },
        { cidr: '192.0.2.0/24' },
        { host: 'unsupported.example' },
      ],
    },
    unix_sockets: { mode: '' },
  }, options: {} });
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    async predictSandbox() {
      return {
        targets: [{
          target: {
            implementation: 'tclaude-layer', harness: 'codex', platform: 'linux',
          },
          axes: {
            network: { outcome: 'enforced_partial', detail: 'mixed deny outcomes' },
          },
          context_axes: [{
            network: { outcome: 'enforced_partial', detail: 'mixed deny outcomes' },
          }],
          context_network_entries: [[
            {
              mode: 'deny',
              entry: { domain: 'api.example.com', ports: [443] },
              keys: [
                'deny:{"domain":"api.example.com","ports":[443]}',
                'deny:{"domain":"API.EXAMPLE.COM","ports":[443]}',
              ],
              outcome: 'enforced_partial',
              detail: 'tclaude-layer bubblewrap + supervised DNS/pasta/nftables gateway: tclaude blocks addresses observed for this denied name through the sandbox DNS broker. With Allow all, another address for the same service, or encrypted DNS that bypasses the broker, can remain reachable. A blocked shared address also affects other names until the DNS lease expires. At launch, bubblewrap, pasta, and nft must pass live checks. If any check fails, these rules are not enforced and outbound traffic is open.',
            },
            {
              mode: 'deny',
              entry: { cidr: '192.0.2.0/24' },
              keys: ['deny:{"cidr":"192.0.2.0/24"}'],
              outcome: 'enforced',
              detail: 'tclaude-layer bubblewrap + supervised DNS/pasta/nftables gateway enforces this deny destination. At launch, bubblewrap, pasta, and nft must pass live checks. If any check fails, these rules are not enforced and outbound traffic is open.',
            },
            {
              mode: 'deny',
              entry: { host: 'unsupported.example' },
              keys: ['deny:{"host":"unsupported.example"}'],
              outcome: 'not_enforced',
              detail: 'This deny rule is saved, but this launch target does not apply network deny entries; traffic matching this destination is not blocked by this rule. Choose Linux tclaude sandbox for enforced deny rules.',
            },
          ]],
        }],
        contexts: [{
          context: {},
          network: {
            mode: 'open',
            deny: [
              { domain: 'api.example.com', ports: [443] },
              { cidr: '192.0.2.0/24' },
              { host: 'unsupported.example' },
            ],
          },
          unix_sockets: { mode: '' },
          agentd_socket: 'always reachable',
        }],
      };
    },
  });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  assert.equal(host.querySelector('.sbx-network-value').value, 'API.EXAMPLE.COM');
  assertAbsent(host.querySelector('.sbx-network-badge'));
  assert.equal(host.querySelector('.sbx-network-deny-note').textContent,
    'Deny enforcement depends on the launch target — see Effective policy preview.');

  const applied = host.querySelector('.sbx-rule-bucket-applied');
  const partial = host.querySelector('.sbx-rule-bucket-partial');
  const unsupported = host.querySelector('.sbx-rule-bucket-not-applied');
  assert.match(applied.textContent, /Deny network: CIDR 192\.0\.2\.0\/24/);
  assert.match(partial.textContent,
    /Deny network: domain api\.example\.com · port 443/);
  assert.match(unsupported.textContent,
    /Deny network: host unsupported\.example/);
  assert.equal(host.querySelectorAll('.sbx-rule-help .spawn-field-help-trigger').length >= 3,
    true, 'effective rules expose keyboard-reachable target detail');

  const partialRow = [...partial.querySelectorAll('.sbx-rule-row')]
    .find((row) => /api\.example\.com/.test(row.textContent));
  const partialHelp = partialRow.querySelector('.spawn-field-help-trigger');
  await harness.act(() => harness.fireEvent(partialHelp, 'click'));
  assert.equal(partialHelp.getAttribute('aria-expanded'), 'true');
  assert.match(partialRow.querySelector('.spawn-field-description').textContent,
    /Partial on Codex on Linux · tclaude sandbox\./);
  assert.match(partialRow.querySelector('.spawn-field-description').textContent,
    /If any check fails, these rules are not enforced and outbound traffic is open/);
  unmount();
});

test('sandbox access rows expose aligned grid cells for network and Unix sockets', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-editor', seed: {
    name: 'aligned-access', filesystem: [], environment: [], includes: [], agent_directories: [],
    network: {
      mode: 'list',
      allow: [{ domain: 'example.com', include_subdomains: true }, { loopback: true }],
    },
    unix_sockets: { mode: 'list', allow: [{ path: '/tmp/example.sock' }] },
  }, options: {} });
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state);
  await harness.act(() => Promise.resolve());

  const networkRows = [...host.querySelectorAll('.sbx-network-row')];
  assert.equal(networkRows.length, 2);
  const networkTable = host.querySelector('.sbx-network-table');
  assert.ok(networkTable, 'release-owned and manual destinations share one grid owner');
  assert.deepEqual(
    [...networkTable.children].map((rows) => rows.className),
    ['sbx-rows sbx-network-rows sbx-network-pack-rows', 'sbx-rows sbx-network-rows sbx-network-manual-rows'],
  );
  assert.ok(networkRows.every((row) => row.classList.contains('sbx-access-row')));
  assert.ok(networkRows.every((row) => row.closest('.sbx-network-table') === networkTable));
  assertAbsent(host.querySelector('.sbx-network-badge'), 'ordinary allow rows have no duplicate per-row evaluation verdict');
  assert.ok(networkRows[0].querySelector('.sbx-network-modifier .sbx-inline-check'));
  assert.equal(networkRows[0].querySelector('.sbx-network-modifier').textContent.trim(), 'subdomains');
  assert.equal(networkRows[0].querySelector('.sbx-network-ports').placeholder, 'ports');
  assert.equal([...networkRows[0].querySelector('.sbx-network-selector').options]
    .some((option) => option.textContent === 'loopback'), true,
  'the kind dropdown retains its longest label');
  assertAbsent(networkRows[1].querySelector('.sbx-network-modifier'));
  assert.equal(networkRows[1].classList.contains('sbx-network-row-no-modifier'), true,
    'rows without a subdomains checkbox give the dead modifier gutter to the value cell');
  const loopbackValue = networkRows[1].querySelector('span.sbx-network-value.sbx-network-value-readonly');
  assert.equal(loopbackValue.textContent, '—');
  assert.equal(loopbackValue.getAttribute('aria-hidden'), 'true');
  assertAbsent(loopbackValue.querySelector('input'), 'a read-only no-value cell is plain muted text rather than input-look markup');
  const networkHelp = host.querySelector('#sandbox-profile-editor-network-help-hint');
  assert.match(networkHelp.textContent, /Host matches one exact DNS name/);
  assert.match(networkHelp.textContent, /blank allows all ports/);
  assert.match(networkHelp.textContent, /ordinary IPv4\/IPv6 TCP and UDP/);
  assert.match(networkHelp.textContent, /QUIC is UDP/);
  assert.match(networkHelp.textContent, /Raw and packet sockets are not an authored class/);
  assert.match(networkHelp.textContent, /For Linux tclaude-layer filtered networking/);
  assert.match(networkHelp.textContent, /Host and domain rules allow IP addresses returned by DNS/);
  assert.match(networkHelp.textContent, /sandbox can also reach other sites hosted on that same IP/);
  assert.match(networkHelp.textContent, /Only a new DNS lookup refreshes the allowed IP/);
  assert.match(networkHelp.textContent, /Existing connections may continue after the DNS answer expires/);
  assert.match(networkHelp.textContent, /new connections need another lookup/);
  assert.match(networkHelp.textContent, /bubblewrap, pasta, and nft must pass live checks/);
  assert.match(networkHelp.textContent, /rules are not enforced and outbound traffic is open/);
  assert.match(networkHelp.textContent, /local-machine rules use host\.tclaude\.internal/);
  assert.match(networkHelp.textContent, /127\.0\.0\.1 and ::1 refer to the sandbox itself/);
  assert.match(networkHelp.textContent, /compose by intersection/);
  assert.match(networkHelp.textContent, /Codex’s built-in filesystem sandbox remains available/);
  assert.match(networkHelp.textContent, /upstream proxy is experimental and off by default/);
  assert.match(networkHelp.textContent, /tclaude-layer filtering on Linux/);
  assert.match(networkHelp.textContent, /network open \(Allow all\)/);
  assert.ok(host.querySelector('.sbx-socket-row.sbx-access-row'));

  unmount();
  host.remove();
});

test('raw access JSON can repair a structured access validation error', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-editor', seed: {
    name: 'repair', filesystem: [], environment: [], includes: [], agent_directories: [],
    network: { mode: 'list', allow: [{ domain: 'https://invalid.example/path' }] },
    unix_sockets: { mode: 'closed' },
  }, options: {} });
  let saved = null;
  const predictions = [];
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    async saveSandbox(value) { saved = value; },
    async predictSandbox(draft) {
      predictions.push(draft);
      return {
        targets: [],
        contexts: draft.network.mode === 'list' && draft.network.allow.length === 0
          ? [{
            context: {}, network: { mode: 'list', allow: [] },
            unix_sockets: { mode: 'closed' }, agentd_socket: 'always reachable',
            notices: [{ class: 'composition', detail: 'raw policy leaves an empty intersection' }],
          }]
          : [],
      };
    },
  });
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#sandbox-profile-editor-submit').disabled, true);
  assert.ok(host.querySelector('.sbx-access-validation'));
  host.querySelector('.sbx-advanced-toggle').click();
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#sandbox-profile-editor-submit').disabled, false,
    'the stale structured error cannot make raw repair unreachable');
  assertAbsent(host.querySelector('.sbx-access-validation'));
  const rawNetwork = host.querySelector('#sandbox-profile-editor-network');
  const rawSockets = host.querySelector('#sandbox-profile-editor-unix-sockets');
  rawNetwork.value = '{"mode":"list","allow":[null]}';
  rawNetwork.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#sandbox-profile-editor-modal'),
    'an in-progress raw network row cannot crash the editor');
  assert.match(host.querySelector('.sbx-preview-status').textContent,
    /preview paused: Network row 1 must be a JSON object/);
  rawNetwork.value = '{"mode":"list","allow":[]}';
  rawNetwork.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  rawSockets.value = '{"mode":"list","allow":[null]}';
  rawSockets.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  assert.match(host.querySelector('.sbx-preview-status').textContent,
    /preview paused: Unix-socket row 1 must be a JSON object/);
  rawSockets.value = '{"mode":"closed","allow":[]}';
  rawSockets.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  rawNetwork.value = '"open"';
  rawNetwork.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.equal(saved, null);
  assert.match(host.querySelector('.cron-create-error').textContent,
    /network and unix sockets must be JSON objects/,
    'primitive raw axes receive the intended validation error');
  rawNetwork.value = '{"mode":"list","allow":false}';
  rawNetwork.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.equal(saved, null);
  assert.match(host.querySelector('.cron-create-error').textContent,
    /network allow\/deny and Unix-socket allow fields must be arrays/,
    'malformed allow values are rejected instead of normalized as sparse lists');
  rawNetwork.value = '{"baseline":"allow","deny":[null]}';
  rawNetwork.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  assert.match(host.querySelector('.sbx-preview-status').textContent,
    /preview paused: Network deny row 1 must be a JSON object/);
  rawNetwork.value = '{"baseline":"allow","deny":false}';
  rawNetwork.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.equal(saved, null);
  assert.match(host.querySelector('.cron-create-error').textContent,
    /network allow\/deny and Unix-socket allow fields must be arrays/);
  rawNetwork.value = '{"baseline":"allow","deny_packs":false}';
  rawNetwork.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.equal(saved, null);
  assert.match(host.querySelector('.cron-create-error').textContent,
    /network packs and deny_packs must be arrays/);
  rawNetwork.value = '{"mode":"list","allow":[]}';
  rawNetwork.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  assert.deepEqual(predictions.at(-1).network, { mode: 'list', allow: [] },
    'prediction consumes the authoritative raw access value');
  assert.match(host.querySelector('.sbx-composition-warning').textContent,
    /raw policy leaves an empty intersection/);
  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(saved.draft.network, { mode: 'list', allow: [] });
  unmount();
});

test('closed socket template clears incompatible authored list rows', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-editor', seed: {
    name: 'socket-list', filesystem: [], environment: [], includes: [], agent_directories: [],
    unix_sockets: { mode: 'list', allow: [{ path: '/tmp/old.sock' }] },
  }, options: {} });
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state);
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 100)));
  const template = [...host.querySelectorAll('.sbx-access-template-add')]
    .find((button) => button.textContent.includes('agentd only'));
  assert.ok(template);
  template.click();
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelectorAll('.sbx-socket-row').length, 0);
  assert.equal(host.querySelector('#sandbox-profile-editor-submit').disabled, false);
  assert.match(host.querySelector('.sbx-access-axis .sbx-common-rule-notice').textContent,
    /1 incompatible existing row removed/);
  host.querySelector('.sbx-advanced-toggle').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(JSON.parse(host.querySelector('#sandbox-profile-editor-unix-sockets').value),
    { mode: 'closed', allow: [] });
  unmount();
});

test('global harness filesystem rows start folded, remain immutable, and are never saved', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-editor', seed: { name: 'plain', filesystem: [{ path: '/work', access: 'write' }], environment: [], includes: [], agent_directories: [] }, options: {} });
  let saved = null;
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    async loadCommonRuleCatalog() { return { ...COMMON_RULES, global_config_warnings: ['Claude settings could not be parsed.'] }; },
    async saveSandbox(value) { saved = value; },
  });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));

  const toggle = host.querySelector('#sandbox-profile-editor-show-global-filesystem');
  // LinkeDOM does not implement HTMLInputElement.checked, so use the
  // state-aware selector rather than asserting only that an attribute exists.
  assert.equal(toggle.matches(':checked'), false, 'inherited context starts folded');
  assertAbsent(host.querySelector('#sandbox-profile-editor-global-filesystem'));
  assertAbsent(host.querySelector('#sandbox-profile-editor-global-harness-filter'), 'the harness filter only appears with inherited rows enabled');
  assert.match(host.querySelector('.sbx-global-warning').textContent, /could not be parsed/, 'config warnings remain visible while inherited rows are folded');
  assert.equal(host.querySelector('#sandbox-profile-editor-filesystem-section').open, true,
    'a runtime config warning opens its owning collapsed section');

  toggle.checked = true;
  toggle.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  const filter = host.querySelector('#sandbox-profile-editor-global-harness-filter');
  assert.equal(filter.querySelector('option:checked').value, 'both');
  const selectedDescriptor = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(filter.querySelector('option')), 'selected');
  const valueDescriptor = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(filter), 'value');
  let pollSelectedWrites = 0;
  let pollValueReads = 0;
  Object.defineProperty(filter, 'value', {
    configurable: true,
    get() { pollValueReads += 1; return valueDescriptor.get.call(this); },
  });
  for (const option of filter.querySelectorAll('option')) {
    Object.defineProperty(option, 'selected', {
      configurable: true,
      get() { return selectedDescriptor.get.call(this); },
      set(value) { pollSelectedWrites += 1; selectedDescriptor.set.call(this, value); },
    });
  }
  state.updateTemplates([{ name: 'snapshot-poll' }], []);
  await harness.act(() => Promise.resolve());
  assertSameNode(host.querySelector('#sandbox-profile-editor-global-harness-filter'), filter, 'snapshot polls preserve the native select node');
  assert.equal(pollValueReads, 0, 'snapshot polls do not reconcile the open editor select');
  assert.equal(pollSelectedWrites, 0, 'snapshot polls do not reapply controlled options and close the native dropdown');
  state.sandboxProfiles.value = [{ name: 'new-registry-profile', filesystem: [], environment: [] }];
  await harness.act(() => Promise.resolve());
  assert.equal(pollValueReads > 0, true, 'sandbox-registry changes still reconcile the editor');
  let inherited = [...host.querySelectorAll('.sbx-global-row')];
  assert.equal(inherited.length, COMMON_RULES.global_filesystem.length);
  assert.equal(inherited[0].getAttribute('role'), 'group');
  assert.equal(inherited[0].querySelector('.sbx-path').hasAttribute('readonly'), true);
  assert.equal(inherited[0].querySelectorAll('button').length, 0, 'an inherited row has no browse or delete actions');
  assert.match(inherited[0].textContent, /Claude \+ Codex/);
  assert.match(inherited[0].getAttribute('title'), /settings\.json.*generated tclaude-agent-<launch-id>\.config\.toml/s);
  assert.match(inherited[1].textContent, /deny read.*Claude/);
  const claudeTmux = inherited.find((row) => row.querySelector('.sbx-path').value === '/tmp/tmux-1000/tclaude');
  assert.match(claudeTmux.textContent, /deny.*Claude/);
  assert.match(claudeTmux.getAttribute('title'), /generated claude --settings launch override/);

  filter.querySelector('option[value="claude"]').selected = true;
  filter.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  inherited = [...host.querySelectorAll('.sbx-global-row')];
  assert.equal(inherited.length, 4);
  assert.equal(inherited.every((row) => row.textContent.includes('Claude') && !row.textContent.includes('Codex')), true);
  assert.equal(inherited.every((row) => !row.getAttribute('title').includes('generated tclaude-agent')), true, 'Claude-only tooltips omit Codex provenance');
  assert.equal(inherited.find((row) => row.querySelector('.sbx-path').value === '~/.tclaude/api/agentd.sock').querySelector('.sbx-access').textContent, 'read', 'Claude-only rows use Claude access, not the merged write');

  filter.querySelector('option[value="codex"]').selected = true;
  filter.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  inherited = [...host.querySelectorAll('.sbx-global-row')];
  assert.equal(inherited.length, 2);
  assert.equal(inherited.every((row) => row.textContent.includes('Codex') && !row.textContent.includes('Claude')), true);
  assert.equal(inherited.every((row) => !row.getAttribute('title').includes('settings.json')), true, 'Codex-only tooltips omit Claude provenance');
  assert.equal(inherited.find((row) => row.querySelector('.sbx-path').value === '~/.tclaude/api/agentd.sock').querySelector('.sbx-access').textContent, 'write');

  filter.querySelector('option[value="none"]').selected = true;
  filter.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  assertAbsent(host.querySelector('#sandbox-profile-editor-global-filesystem'), 'None hides all builtin rows without folding the controls');
  assert.ok(host.querySelector('#sandbox-profile-editor-global-harness-filter'));

  host.querySelector('#sandbox-profile-editor-submit').click(); await harness.act(() => Promise.resolve());
  assert.deepEqual(saved.draft.filesystem, [{ path: '/work', access: 'write' }]);

  toggle.checked = false;
  toggle.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  assertAbsent(host.querySelector('#sandbox-profile-editor-global-filesystem'), 'the checkbox folds inherited context without changing the draft');
  assertAbsent(host.querySelector('#sandbox-profile-editor-global-harness-filter'));
  assert.match(host.querySelector('.sbx-global-warning').textContent, /could not be parsed/);
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
  assert.equal(menu.closest('#sandbox-profile-editor-filesystem-section')
    .querySelector('.sbx-section-summary > span').textContent, 'Filesystem');
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
  assertAbsent(host.querySelector('#sandbox-profile-editor-common-rule-notice'));

  entries[1].querySelector('.sbx-common-rule-add').click();
  await harness.act(() => Promise.resolve());
  const rows = [...host.querySelectorAll('.sbx-section .sbx-row')].filter((row) => row.querySelector('.sbx-path'));
  const inserted = rows[rows.length - 1];
  assert.equal(inserted.querySelector('.sbx-path').value, '/home/op');
  assert.equal(segmentedValue(inserted.querySelector('.sbx-access')), 'deny');
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
  assertAbsent(host.querySelector('#sandbox-profile-editor-read-baseline'));
  assertAbsent(host.querySelector('.sbx-read-exclusions'));
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
test('a failing common-rule feed blocks hidden pack authority but leaves manual filesystem editing usable', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-editor', seed: null, options: {} });
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
  const commonRuleMenu = host.querySelector('#sandbox-profile-editor-common-rules');
  assert.equal(host.querySelector('#sandbox-profile-editor-filesystem-section').open, true,
    'the feed alert opens its owning Filesystem section');
  assert.equal(commonRuleMenu.hasAttribute('open') || commonRuleMenu.open === true, true,
    'the nested preset disclosure opens so the alert and retry are visible');
  assert.match(commonRuleMenu.querySelector('.sbx-common-rule-summary').textContent, /unavailable/,
    'the summary still names the failure if the operator folds it again');
  const baseline = host.querySelector('#sandbox-profile-editor-network-baseline');
  await harness.act(() => { choose(baseline, 'deny'); harness.fireEvent(baseline, 'change'); });
  assert.equal(host.querySelector('#sandbox-profile-editor-network-section').open, true,
    'a blocking catalog diagnostic exposes its explanation and retry control');
  assert.equal(host.querySelector('.sbx-network-packs').hasAttribute('open'), true,
    'a blocking pack-catalog diagnostic opens the nested pack disclosure');
  assert.match(host.querySelector('.sbx-network-pack-visibility-error').textContent,
    /Saving is paused.*net-local.*net-anthropic.*net-openai-codex/s);
  assert.equal(host.querySelectorAll('.sbx-network-pack-visibility-error').length, 1,
    'the authority warning is rendered and announced exactly once');
  assert.equal(host.querySelector('#sandbox-profile-editor-submit').disabled, true,
    'unrendered release-owned network authority cannot be saved');
  [...host.querySelectorAll('.sbx-add-row')].find((button) => /directory/.test(button.textContent)).click();
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelectorAll('.sbx-section .sbx-path').length, 1, 'rows can still be added by hand');
  // Retry recovers the menu without a reopen.
  feedOffline = false;
  await harness.act(() => { feedError.querySelector('button').click(); return new Promise((resolve) => setTimeout(resolve, 50)); });
  assertAbsent(host.querySelector('#sandbox-profile-editor-common-rule-feed-error'));
  assert.equal(host.querySelectorAll('.sbx-common-rule-entry').length, COMMON_RULES.categories.length);
  assert.equal(host.querySelector('.sbx-common-rule-summary').textContent.includes('unavailable'), false);
  assertAbsent(host.querySelector('.sbx-network-pack-visibility-error'));
  assert.equal(host.querySelectorAll('.sbx-network-pack-row').length, 3);
  assert.equal(host.querySelector('#sandbox-profile-editor-submit').disabled, false);
  unmount();
});

test('a failing common-rule feed also blocks hidden deny-pack intent under Allow all', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-editor', seed: {
    name: 'hidden-deny-pack', filesystem: [], environment: [], includes: [], agent_directories: [],
    network: {
      baseline: 'allow', packs: [], deny_packs: ['net-local'], allow: [], deny: [],
    },
    unix_sockets: { mode: '' },
  }, options: {} });
  let feedOffline = true;
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    async loadCommonRuleCatalog() {
      if (feedOffline) throw new Error('feed offline');
      return COMMON_RULES;
    },
  });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  assert.equal(host.querySelector('#sandbox-profile-editor-network-section').open, true);
  assert.equal(host.querySelector('.sbx-network-packs').hasAttribute('open'), true,
    'hidden deny-pack diagnostics auto-open the folded pack controls');
  assert.match(host.querySelector('.sbx-network-pack-visibility-error').textContent,
    /Saving is paused.*net-local/s);
  assert.equal(host.querySelector('#sandbox-profile-editor-submit').disabled, true,
    'unrendered deny intent is protected just like unrendered allow authority');

  feedOffline = false;
  const feedError = host.querySelector('#sandbox-profile-editor-common-rule-feed-error');
  await harness.act(() => {
    feedError.querySelector('button').click();
    return new Promise((resolve) => setTimeout(resolve, 50));
  });
  assertAbsent(host.querySelector('.sbx-network-pack-visibility-error'));
  const localPack = [...host.querySelectorAll('.sbx-network-pack')]
    .find((row) => /Local access/.test(row.textContent));
  assert.equal(segmentedValue(localPack.querySelector('.sbx-network-pack-mode')), 'deny');
  assert.equal(host.querySelectorAll('.sbx-network-pack-row').length, 1);
  assert.equal(host.querySelector('#sandbox-profile-editor-submit').disabled, false);
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
  assertAbsent(feedError());
  assert.equal(host.querySelectorAll('.sbx-common-rule-entry').length, COMMON_RULES.categories.length);
  unmount();
});

// state.error carries save and validation refusals. A catalog rejection
// landing after one of those must not replace the reason the save was refused
// with an explanation of an optional convenience.
test('a late common-rule feed rejection does not overwrite a refused save', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'), harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({
    kind: 'sandbox-editor',
    seed: { name: 'restricted', filesystem: [], environment: [], includes: [], agent_directories: [] },
    options: {},
  });
  let rejectFeed = null;
  let saved = null;
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    loadCommonRuleCatalog() { return new Promise((_, reject) => { rejectFeed = reject; }); },
    async saveSandbox(value) { saved = value; },
  });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));

  // The save is refused locally: advanced mode is authoritative and its raw
  // JSON does not parse.
  await harness.act(() => harness.fireEvent(host.querySelector('.sbx-advanced-toggle'), 'click'));
  const rawFilesystem = host.querySelector('#sandbox-profile-editor-filesystem');
  rawFilesystem.value = 'not json';
  await harness.act(() => harness.fireEvent(rawFilesystem, 'input'));
  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.equal(saved, null);
  assert.match(host.querySelector('.cron-create-error').textContent, /JSON/i);

  // Only now does the feed give up.
  rejectFeed(new Error('feed offline'));
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 50)));
  assert.match(host.querySelector('.cron-create-error').textContent, /JSON/i, 'the refusal reason survives the late rejection');
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
  assert.deepEqual([...host.querySelectorAll('.sbx-row:not(.sbx-global-row) .sbx-path')].map((input) => input.value), ['~otheruser/.ssh', '/home/op/.ssh']);
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
  assertSameNode(harness.document.activeElement, write); assert.match(host.querySelector('.cron-create-label').parentElement.parentElement.textContent, /reviewer|Role/i);
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

test('Codex profile SSH workaround is a default-on opt-out checkbox', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/management-model.js');

  const defaults = {
    ...model.profileDraft({ name: 'p', harness: 'codex' }, {}, catalog),
    sandbox: 'tclaude-agent',
  };
  assert.equal(defaults.ssh_workaround, true);
  assert.equal(model.profilePayload(defaults, null, catalog).ssh_workaround, true);

  const optedOut = model.profileDraft(
    { name: 'p', harness: 'codex', ssh_workaround: false }, {}, catalog,
  );
  assert.equal(optedOut.ssh_workaround, false);
  assert.equal(model.profilePayload(optedOut, null, catalog).ssh_workaround, false);

  const raw = { ...defaults, sandbox: 'workspace-write' };
  assert.equal(model.profilePayload(raw, null, catalog).ssh_workaround, false,
    'raw Codex profiles persist the workaround as inactive');

  const claude = model.profileDraft(
    { name: 'p', harness: 'claude', ssh_workaround: false }, {}, catalog,
  );
  assert.equal(model.profilePayload(claude, null, catalog).ssh_workaround, undefined);
});

// TCL-865. The editor exposes two different resolutions next to each other, and
// before this they were worded as if they were the same one. This pins the
// finished vocabulary at the render, not just in the source: the target
// selectors say "Resolved defaults", the composed sandbox layers are named
// on screen without opening a disclosure, and a Claude launch whose sandbox
// mode is `inherit` never reads as though its built-in sandbox is definitely on.
test('sandbox editor separates resolved launch defaults from composed sandbox layers', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({
    kind: 'sandbox-editor',
    seed: {
      id: 7, name: 'scratch', filesystem: [{ path: '/home/operator/work', access: 'write' }],
      environment: [], includes: [], agent_directories: [],
    },
    options: { group: 'crew' },
    catalog: [{
      name: 'claude', display_name: 'Claude Code',
      can_builtin_os_sandbox: true, can_tclaude_layer: true, can_stacked: true,
    }],
  });
  const axis = (outcome) => ({ tier: '1 write rule', outcome, detail: 'resolver-owned detail' });
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    async predictSandbox() {
      return {
        targets: [{
          target: {
            implementation: 'harness-builtin', harness: 'claude',
            platform: 'linux', sandbox: 'inherit',
          },
          resolved_by: 'group default profile "crew-launch"',
          predicted: true,
          axes: {
            filesystem: axis('enforced_partial'), environment: axis('enforced'),
            agent_directories: axis('enforced'), network: axis('enforced'),
            unix_sockets: axis('enforced'),
          },
        }],
        contexts: [{
          context: { global: 'house-rules', group: 'crew-rules', group_name: 'crew', explicit: 'scratch' },
          filesystem: [{ path: '/home/operator/work', access: 'write' }],
          environment: [], agent_directories: [],
          network: { mode: 'open' }, unix_sockets: { mode: 'closed' },
          agentd_socket: 'always reachable', notices: [],
        }],
      };
    },
  });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));

  // Launch-parameter resolution: one phrase, in every target selector.
  assert.equal(host.querySelector('#sandbox-profile-editor-evaluate-harness').options[0].textContent,
    'Resolved defaults');
  assert.equal(
    host.querySelector('#sandbox-profile-editor-evaluate-implementation').options[0].textContent,
    'Resolved defaults');
  assert.equal(host.querySelector('#sandbox-profile-editor-evaluate-platform').options[0].textContent,
    'Resolved defaults (this host)');
  // The explanation rides on the controls as a tooltip; a permanent paragraph
  // between a control and its result is what help-field.js exists to avoid.
  // It states the tiers THIS preview walks, not the general chain — the preview
  // has no explicit launch choice and no named spawn profile to offer.
  assertAbsent(host.querySelector('#sandbox-profile-editor-evaluate-intro'), 'the target controls must not grow a permanent block of prose');
  const targetTitle = host.querySelector('#sandbox-profile-editor-evaluate-harness')
    .closest('label').getAttribute('title');
  assert.match(targetTitle,
    /no explicit launch choice and no named spawn profile, so its resolved defaults run: group default spawn profile → global default spawn profile → harness default\./);

  // Sandbox-policy composition: every layer named, by scope, without a click.
  assert.equal(host.querySelector('#sandbox-profile-editor-policy-layers').textContent.trim(),
    'Composed sandbox-profile layers: global “house-rules” + group “crew-rules” + explicit “scratch”');

  // `inherit` is never left to explain itself, and naming the implementation
  // owner does not assert that its sandbox is switched on.
  const details = host.querySelector('.sbx-target-details').textContent;
  assert.match(details, /Sandbox mode: inherit — Claude's own settings decide whether its built-in sandbox is enabled/);
  assert.match(details, /Resolved defaults came from: group default profile "crew-launch"/);
  assert.match(host.querySelector('.sbx-policy-target').textContent,
    /Claude on Linux · built-in sandbox \(enabled only if Claude settings enable it\)/);
  unmount();
});

test('mount-path control discloses a projection and refuses it on a deny row', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/management-island.js'),
  ]);
  let saved = null;
  const state = createManagementState();
  state.openDialog({
    kind: 'sandbox-editor',
    seed: {
      name: 'mounted',
      filesystem: [
        { path: '/srv/corpus', access: 'read', mount_path: '/data' },
        { path: '/srv/work', access: 'write' },
        { path: '/home/operator/.ssh', access: 'deny' },
      ],
      environment: [], includes: [], agent_directories: [],
    },
    options: {},
  });
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    async saveSandbox(payload) { saved = payload; },
  });
  await harness.act(() => Promise.resolve());

  const toggles = [0, 1, 2].map((index) =>
    host.querySelector(`#sandbox-profile-editor-mount-toggle-${index}`));

  // Requirement 1: a remapped row is identifiable WITHOUT opening its popover,
  // which is the whole reason the set state is loud rather than a quiet toggle.
  assert.equal(toggles[0].classList.contains('is-set'), true,
    'a row with a mount path renders the set state');
  assert.equal(toggles[1].classList.contains('is-set'), false);
  assert.match(toggles[0].getAttribute('title'), /Mounts inside the sandbox at \/data/);
  assert.match(toggles[0].getAttribute('aria-label'), /Mounts inside the sandbox at \/data/);

  // A deny always applies to the host path, so the control is not offered.
  assert.equal(toggles[2].disabled, true);
  assert.equal(toggles[2].classList.contains('is-set'), false);
  assert.match(toggles[2].getAttribute('title'), /deny always applies to the host path/);

  // The panel is collapsed until asked for, so the common case stays two fields.
  assertAbsent(host.querySelector('#sandbox-profile-editor-mount-panel-0'));
  assert.equal(toggles[0].getAttribute('aria-expanded'), 'false');
  await harness.act(() => harness.fireEvent(toggles[0], 'click'));
  const panel = host.querySelector('#sandbox-profile-editor-mount-panel-0');
  assert.notEqual(panel, null);
  assert.equal(panel.querySelector('input').value, '/data');
  // The two explanatory paragraphs are disclosure copy: the panel stays three
  // rows tall and a [?] on its title line carries them, so the filesystem table
  // above keeps its column layout.
  const mountHelp = panel.querySelector('.sbx-mount-title .spawn-field-help-trigger');
  assert.notEqual(mountHelp, null, 'the panel offers its explanation');
  assert.equal(mountHelp.getAttribute('aria-expanded'), 'false');
  await harness.act(() => harness.fireEvent(mountHelp, 'click'));
  assert.equal(mountHelp.getAttribute('aria-expanded'), 'true');
  const mountHelpBody = mountHelp.nextElementSibling;
  assert.match(mountHelpBody.textContent, /not visible inside the sandbox at all/,
    'the disclosure states what the host path stops being');
  assert.match(mountHelpBody.textContent, /Linux tclaude-layer or stacked only/);
  assert.match(mountHelpBody.textContent, /never fall back to exposing the host path/);
  assert.match(mountHelpBody.textContent, /\/srv\/corpus/,
    'it names the row it belongs to, not "the host directory"');

  // Closing the panel forgets the open disclosure. A remembered key would
  // reopen the popover on top of the mount input this panel autofocuses, so the
  // operator would type under something they never opened.
  await harness.act(() => harness.fireEvent(toggles[0], 'click'));
  assertAbsent(host.querySelector('#sandbox-profile-editor-mount-panel-0'));
  await harness.act(() => harness.fireEvent(toggles[0], 'click'));
  assert.equal(
    host.querySelector('#sandbox-profile-editor-mount-panel-0 .spawn-field-help-trigger')
      .getAttribute('aria-expanded'),
    'false',
    'a reopened panel starts closed',
  );

  // Authoring a projection on the plain write row round-trips onto the wire.
  await harness.act(() => harness.fireEvent(toggles[1], 'click'));
  const second = host.querySelector('#sandbox-profile-editor-mount-panel-1 input');
  second.value = '/scratch';
  await harness.act(() => harness.fireEvent(second, 'input'));
  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(saved.draft.filesystem, [
    { path: '/srv/corpus', access: 'read', mount_path: '/data' },
    { path: '/srv/work', access: 'write', mount_path: '/scratch' },
    { path: '/home/operator/.ssh', access: 'deny' },
  ], 'mount_path rides the wire, and rows without one stay byte-identical');
  unmount();
});

test('switching a remapped row to deny drops the mount path instead of saving an invalid rule', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/management-island.js'),
  ]);
  let saved = null;
  const state = createManagementState();
  state.openDialog({
    kind: 'sandbox-editor',
    seed: {
      name: 'switched',
      filesystem: [{ path: '/srv/corpus', access: 'read', mount_path: '/data' }],
      environment: [], includes: [], agent_directories: [],
    },
    options: {},
  });
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    async saveSandbox(payload) { saved = payload; },
  });
  await harness.act(() => Promise.resolve());

  const control = host.querySelector('.sbx-filesystem-access');
  await harness.act(() => harness.fireEvent(segment(control, 'deny'), 'click'));
  const toggle = host.querySelector('#sandbox-profile-editor-mount-toggle-0');
  assert.equal(toggle.disabled, true);
  assert.equal(toggle.classList.contains('is-set'), false,
    'the glyph returns to unset, so the dropped value is visible rather than silent');
  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(saved.draft.filesystem, [{ path: '/srv/corpus', access: 'deny' }],
    'a deny carries no mount_path, which the daemon would reject');
  unmount();
});

test('effective preview discloses the host to sandbox mapping on the rule line', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/sandbox-profiles-data.js');

  // With the editor control collapsed behind a popover, this preview is the
  // canonical always-visible disclosure of the mapping.
  const enforced = model.sandboxRuleBuckets({
    filesystem: { outcome: 'enforced', detail: '' },
  }, {
    filesystem: [
      { path: '/srv/corpus', access: 'read', mount_path: '/data' },
      { path: '/srv/work', access: 'write' },
      { path: '/home/operator/.ssh', access: 'deny' },
    ],
  });
  assert.deepEqual(enforced.applied.rules, [
    'Read-only: /data ← /srv/corpus',
    'Read/write: /srv/work',
    'Block: /home/operator/.ssh',
  ]);

  // On a surface that cannot mount, the rule is bucketed Unsupported with the
  // named capability, never quietly re-pointed at the host path.
  const refused = model.sandboxRuleBuckets({
    filesystem: {
      outcome: 'refused',
      detail: '1 directory rule mounts a host directory at a different sandbox path'
        + ' ("/srv/corpus" → "/data"); that needs a mount namespace, which only the'
        + ' Linux tclaude-layer provides, so launch is refused rather than mounting'
        + ' at the host path',
    },
  }, {
    filesystem: [{ path: '/srv/corpus', access: 'read', mount_path: '/data' }],
  });
  assert.deepEqual(refused.notApplied.rules, ['Read-only: /data ← /srv/corpus']);
  assert.equal(refused.launchRefused, true);
  assert.equal(refused.notApplied.reasons[0].label, 'Launch blocked');
  assert.match(refused.notApplied.reasons[0].detail, /mount namespace/);

  // A rule with no mount path is unchanged, and a mount_path equal to the host
  // path is the same-path rule it actually is.
  assert.deepEqual(model.sandboxRuleBuckets({
    filesystem: { outcome: 'enforced', detail: '' },
  }, {
    filesystem: [{ path: '/srv/corpus', access: 'read', mount_path: '/srv/corpus' }],
  }).applied.rules, ['Read-only: /srv/corpus']);
});

test('authoring a mount path does not re-author the row host path or drop retained spellings', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/management-island.js'),
  ]);
  let saved = null;
  const state = createManagementState();
  // The editor shows the operator's own spelling and keeps the canonical target
  // beside it. Setting a mount path says nothing about WHICH host directory is
  // granted, so it must not look like a re-authoring: that would replace the
  // canonical path with the alias and throw the retained-spelling sidecar away.
  state.openDialog({
    kind: 'sandbox-editor',
    seed: {
      name: 'spelled',
      filesystem: [{ path: '/canonical/work', access: 'read' }],
      filesystem_spellings: {
        version: 1,
        rules: [{ resolved_path: '/canonical/work', spellings: ['/workspace', '/Volumes/Work'] }],
      },
      environment: [], includes: [], agent_directories: [],
    },
    options: {},
  });
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    async saveSandbox(value) { saved = value; },
  });
  await harness.act(() => Promise.resolve());

  await harness.act(() => harness.fireEvent(
    host.querySelector('#sandbox-profile-editor-mount-toggle-0'), 'click'));
  const field = host.querySelector('#sandbox-profile-editor-mount-panel-0 input');
  field.value = '/data';
  await harness.act(() => harness.fireEvent(field, 'input'));
  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());

  assert.deepEqual(saved.draft.filesystem,
    [{ path: '/canonical/work', access: 'read', mount_path: '/data' }],
    'the canonical host path survives; the alias spelling does not replace it');
  assert.deepEqual(saved.draft.filesystem_spellings, {
    version: 1,
    rules: [{ resolved_path: '/canonical/work', spellings: ['/workspace', '/Volumes/Work'] }],
  }, 'retained spellings belong to the host path and survive a mount-path edit');
  unmount();
});

test('the mount panel takes focus, closes on Escape, and returns focus to its row', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({
    kind: 'sandbox-editor',
    seed: {
      name: 'focus',
      filesystem: [{ path: '/srv/corpus', access: 'read' }],
      environment: [], includes: [], agent_directories: [],
    },
    options: {},
  });
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state);
  await harness.act(() => Promise.resolve());

  const toggle = host.querySelector('#sandbox-profile-editor-mount-toggle-0');
  await harness.act(() => harness.fireEvent(toggle, 'click'));
  const panel = host.querySelector('#sandbox-profile-editor-mount-panel-0');
  assert.equal(toggle.getAttribute('aria-controls'), panel.id);
  assertSameNode(harness.document.activeElement, panel.querySelector('input'),
    'the field takes focus without relying on autofocus, which Preact does not implement');

  await harness.act(() => harness.fireEvent(panel, 'keydown', { key: 'Escape' }));
  assertAbsent(host.querySelector('#sandbox-profile-editor-mount-panel-0'), 'Escape dismisses the popover rather than the whole modal');
  assertSameNode(harness.document.activeElement, toggle);
  unmount();
});

test('a deny row carrying a mount path is flagged invalid rather than painted as an active mount', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  // Only reachable through the raw-JSON escape hatch or an agent draft; the
  // daemon refuses it on save, so the row must say so rather than show a set
  // glyph on a control that is switched off.
  state.openDialog({
    kind: 'sandbox-editor',
    seed: {
      name: 'invalid',
      filesystem: [{ path: '/srv/secrets', access: 'deny', mount_path: '/data' }],
      environment: [], includes: [], agent_directories: [],
    },
    options: {},
  });
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state);
  await harness.act(() => Promise.resolve());

  const toggle = host.querySelector('#sandbox-profile-editor-mount-toggle-0');
  assert.equal(toggle.disabled, true);
  assert.equal(toggle.classList.contains('is-set'), false);
  assert.equal(toggle.classList.contains('is-invalid'), true);
  assert.match(toggle.getAttribute('title'), /must not carry a mount path/);
  unmount();
});

test('a fully supported remapped rule opens its bucket so the mapping is not two collapses deep', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/sandbox-profiles-data.js');
  const remapped = model.sandboxRuleBuckets({
    filesystem: { outcome: 'enforced', detail: '' },
  }, { filesystem: [{ path: '/srv/corpus', access: 'read', mount_path: '/data' }] });
  assert.equal(remapped.applied.hasMountPath, true);

  const plain = model.sandboxRuleBuckets({
    filesystem: { outcome: 'enforced', detail: '' },
  }, { filesystem: [{ path: '/srv/corpus', access: 'read' }] });
  assert.equal(plain.applied.hasMountPath, false,
    'a profile with no mount path keeps the collapsed Applied bucket');
});

test('spawn-dialog sandbox preview names the sandbox path a remapped grant lands at', async (t) => {
  const harness = await createPreactHarness(t);
  const preview = await harness.importDashboardModule('js/sandbox-profile-preview.js');
  const text = preview.composeSandboxProfilePreview([{
    scope: 'explicit',
    profile: {
      name: 'mounted',
      filesystem: [
        { path: '/srv/shared-datasets/corpus-v3', access: 'read', mount_path: '/data' },
        { path: '/srv/shared-datasets/corpus-v3', access: 'read', mount_path: '/models' },
        { path: '/srv/build', access: 'write' },
      ],
    },
  }]);
  // Printing the host path alone would name the one path the agent will NOT
  // see, at the exact moment the operator is deciding whether to grant it.
  assert.match(text, /read \/data ← \/srv\/shared-datasets\/corpus-v3 \(explicit\)/);
  assert.match(text, /read \/models ← \/srv\/shared-datasets\/corpus-v3 \(explicit\)/,
    'one host directory mounted twice stays two preview entries');
  assert.match(text, /write \/srv\/build \(explicit\)/,
    'an ordinary grant is unchanged');
});

test('network filtering engine is authorable, survives a baseline change, and is validated', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/sandbox-profiles-data.js');

  // The engine reaches the wire in the compositional shape, and an unset engine
  // adds no key at all — the daemon's "unset never changes behavior" contract
  // has to hold on the authoring side too.
  assert.equal(model.sandboxProfileForWire({
    name: 'engined', filesystem: [], environment: [],
    network: { baseline: 'deny', allow: [{ domain: 'example.com' }], engine: 'proxy' },
    unix_sockets: { mode: '', allow: [] },
  }).network.engine, 'proxy');
  assert.equal(Object.hasOwn(model.sandboxProfileForWire({
    name: 'plain', filesystem: [], environment: [],
    network: { baseline: 'deny', allow: [{ domain: 'example.com' }] },
    unix_sockets: { mode: '', allow: [] },
  }).network, 'engine'), false, 'an unset engine writes no key');

  // A legacy mode-based payload that already names an engine keeps it: the
  // engine is orthogonal to the mode the legacy branch reconstructs.
  assert.equal(model.sandboxNetworkAuthoring({
    network: { mode: 'list', allow: [], engine: 'packet' },
  }).engine, 'packet');

  assert.match(model.sandboxAccessDraftErrors({
    network: { baseline: 'deny', packs: [], deny_packs: [], allow: [], deny: [], engine: 'socks' },
  }).join(' '), /filtering engine is invalid/);
  assert.deepEqual(model.sandboxAccessDraftErrors({
    name: 'ok', filesystem: [], environment: [],
    network: { baseline: 'deny', packs: [], deny_packs: [], allow: [], deny: [], engine: 'proxy' },
    unix_sockets: { mode: '', allow: [] },
  }), []);

  assert.match(model.sandboxProfileSummary({
    network: { baseline: 'deny', allow: [{ domain: 'example.com' }], engine: 'proxy' },
  }), /proxy filter/);
});

test('profile editor engine control selects an engine and keeps it across a baseline change', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/management-island.js'),
  ]);
  const state = createManagementState();
  state.openDialog({
    kind: 'sandbox-editor',
    seed: {
      name: 'engined', filesystem: [], environment: [], includes: [], agent_directories: [],
      network: { baseline: 'deny', packs: [], allow: [{ domain: 'example.com' }] },
      unix_sockets: { mode: '' },
    },
    options: {},
  });
  let saved = null;
  const { host, unmount } = mountSandboxEditor(harness, mountManagementIsland, state, {
    async saveSandbox(payload) { saved = payload; return true; },
  });
  await harness.act(() => Promise.resolve());

  const engine = host.querySelector('#sandbox-profile-editor-network-engine');
  assert.ok(engine, 'the editor offers a filtering-engine control');
  assert.deepEqual([...engine.options].map((option) => option.value),
    ['', 'packet', 'proxy']);
  choose(engine, 'proxy');
  await harness.act(() => harness.fireEvent(engine, 'change'));

  // Changing the baseline must not clear the engine: it is not one of the
  // rules the baseline governs, and losing it would swap the mechanism as a
  // side effect of an unrelated edit.
  const baseline = host.querySelector('#sandbox-profile-editor-network-baseline');
  choose(baseline, 'allow');
  await harness.act(() => harness.fireEvent(baseline, 'change'));
  assert.equal(
    host.querySelector('#sandbox-profile-editor-network-engine').value, 'proxy',
    'the engine survives a baseline change');

  await harness.act(() => harness.fireEvent(
    host.querySelector('#sandbox-profile-editor-submit'), 'click'));
  assert.equal(saved.draft.network.engine, 'proxy');
  assert.equal(saved.draft.network.baseline, 'allow');

  unmount();
  host.remove();
});

test('cloning a spawn profile opens a create-mode editor on a free, alias-safe handle', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { createManagementActions }, { mountManagementIsland }] =
    await Promise.all([
      harness.importDashboardModule('js/management-state.js'),
      harness.importDashboardModule('js/management-actions.js'),
      harness.importDashboardModule('js/management-island.js'),
    ]);
  // `luna-copy` is deliberately parked as an ALIAS of an unrelated profile, not
  // as a primary name: primary names and aliases share one namespace on the
  // server, so a suggestion that only checked names would collide and 400.
  const source = {
    name: 'luna', aliases: ['moon'], operator_only: true, harness: 'codex',
    model: 'gpt-5.6-luna', effort: 'high', sandbox: 'workspace-write',
    approval: 'never', permission_overrides: { 'groups.spawn': 'grant' },
  };
  const profiles = [source, { name: 'other', aliases: ['luna-copy'] }];
  const created = [];
  const state = createManagementState();
  const actions = createManagementActions({
    state, confirm: async () => true, notify() {},
    getSnapshot: () => ({ harnesses: catalog, sandbox_impl: sandboxImpl }),
    profileAPI: {
      loadProfiles: async () => profiles,
      createProfile: async (body) => { created.push(body); return {}; },
      updateProfile: async () => { throw new Error('a clone must never PATCH its source'); },
    },
  });
  await actions.load('profiles');
  assert.equal(actions.openProfileClone(source), true);

  const descriptor = state.dialog.value;
  assert.equal(descriptor.kind, 'profile-editor');
  assert.equal(descriptor.options.editExisting, false, 'a clone saves as a create');
  assert.equal(descriptor.options.cloneSourceName, 'luna');
  assert.equal(descriptor.seed.name, 'luna-copy-2', 'the alias-held luna-copy is skipped');
  assert.deepEqual(descriptor.seed.aliases, [], 'single-holder aliases do not travel to the copy');
  assert.equal(descriptor.seed.operator_only, true, 'the copy is faithful to the source');

  const cleanups = [];
  const host = harness.document.createElement('div');
  harness.document.body.appendChild(host);
  mountManagementIsland({
    host, state, actions, confirmDiscard: async () => true,
    openProfilePermissions() {}, openProfileContextFeatures() {},
    registerCleanup(fn) { cleanups.push(fn); },
  });
  await harness.act(() => Promise.resolve());

  assert.match(host.querySelector('#profile-editor-title').textContent, /Clone profile: luna/);
  assert.equal(host.querySelector('#profile-editor-name').value, 'luna-copy-2',
    'the suggested handle reaches the field instead of being blanked as a plain create-from-seed');
  assert.equal(host.querySelector('#profile-editor-aliases').value, '');
  // LinkeDOM does not implement HTMLInputElement.checked, so read the state
  // through the selector rather than the property.
  assert.equal(host.querySelector('#profile-editor-operator-only').matches(':checked'), true);

  await harness.act(() => harness.fireEvent(host.querySelector('#profile-editor-submit'), 'click'));
  assert.equal(created.length, 1, 'the clone was created, not patched over its source');
  assert.equal('aliases' in created[0], false);
  // Everything else on the source must survive the round-trip, including the
  // launch fields the editor renders through harness-gated rows — those are the
  // ones a clone could plausibly drop while still looking right on screen.
  assert.deepEqual(
    (({ name, harness, model, effort, sandbox, approval, operator_only, permission_overrides }) =>
      ({ name, harness, model, effort, sandbox, approval, operator_only, permission_overrides }))(created[0]),
    {
      name: 'luna-copy-2', harness: 'codex', model: 'gpt-5.6-luna', effort: 'high',
      sandbox: 'workspace-write', approval: 'never', operator_only: true,
      permission_overrides: { 'groups.spawn': 'grant' },
    },
  );

  cleanups.reverse().forEach((fn) => fn());
  host.remove();
});

test('profile card summaries defer the status to the badge that already shows it', async (t) => {
  const harness = await createPreactHarness(t);
  const { profileSummary } = await harness.importDashboardModule('js/profiles.js');
  const gated = { name: 'luna', operator_only: true, model: 'gpt-5.6-luna' };
  const dead = { name: 'old', disabled: true, disabled_reason: 'superseded', model: 'opus' };
  // Surfaces with no badge of their own (dock chips, export picker) keep it.
  assert.match(profileSummary(gated), /^👤 operator only · /);
  assert.match(profileSummary(dead), /^🚫 disabled · /);
  // The manager cards badge it themselves, so the summary must not repeat it.
  assert.equal(profileSummary(gated, { status: false }), 'gpt-5.6-luna');
  assert.equal(profileSummary(dead, { status: false }), 'opus');
});

test('a plain create-from-seed still blanks the name, only a clone keeps it', async (t) => {
  const harness = await createPreactHarness(t);
  const { profileDraft } = await harness.importDashboardModule('js/management-model.js');
  const seed = { name: 'luna', harness: 'codex' };
  // Save-a-running-agent / template inline specs: the seed's handle is not a
  // profile name, so the operator must supply one.
  assert.equal(profileDraft(seed, { editExisting: false }, catalog).name, '');
  assert.equal(profileDraft(seed, { editExisting: false, cloneSourceName: 'luna' }, catalog).name, 'luna');
  assert.equal(profileDraft(seed, { local: true, cloneSourceName: 'luna' }, catalog).name, '',
    'a local per-agent override has no name field to fill');
});
