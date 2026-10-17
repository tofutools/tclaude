import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const harnesses = [{
  name: 'claude', display_name: 'Claude Code',
  models: ['sonnet', 'opus'], effort_levels: ['low', 'high'],
  can_sandbox: true, sandbox_modes: ['inherit', 'on'], default_sandbox: 'inherit', sandbox_mode_help: {},
  can_approval: true, approval_modes: ['inherit', 'plan'], default_approval: 'inherit', approval_mode_help: {},
  can_tools: false, tools_modes: [], default_tools: '', tools_mode_help: {},
  can_ask_timeout: false, ask_timeout_modes: [], default_ask_timeout: '',
  can_remote_control: true, can_auto_memory: true, can_dir_trust: true, dir_trust_store: '~/.claude.json',
}];

async function flush(harness, turns = 8) {
  await harness.act(async () => {
    for (let turn = 0; turn < turns; turn += 1) await Promise.resolve();
  });
}

function setValue(element, value) {
  Object.defineProperty(element, 'value', { configurable: true, writable: true, value });
}

test('remote spawn allowances, lock reasons and request mapping', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/agent-spawn-remote.js');
  const refuse = (status, code) => ({ options: async () => { const e = new Error('x'); e.status = status; e.code = code; throw e; } });
  assert.equal((await mod.loadRemoteSpawnOptions(refuse(403, 'peer_view_local_only'), 'g')).known, false, 'an older peer has no spawn-options route');
  await assert.rejects(() => mod.loadRemoteSpawnOptions(refuse(404, 'not_found'), 'g'), /x/, 'an invisible group is an error');
  await assert.rejects(() => mod.loadRemoteSpawnOptions(refuse(502, 'peer_unreachable'), 'g'), /x/);
  const legacy = mod.normalizeRemoteSpawnOptions(null);
  assert.equal(legacy.known, false);
  for (const field of ['profile', 'cwd', 'harness', 'model', 'effort']) assert.equal(mod.remoteFieldAllowed(field, legacy), false, field);
  assert.equal(mod.remoteFieldAllowed('always', legacy), true);
  assert.match(mod.remoteLockReason('cwd', { node: 'forge', options: legacy }), /forge runs an older tclaude/);

  const operator = mod.normalizeRemoteSpawnOptions({ approval: 'operator', overrides: { cwd: false } });
  assert.match(mod.remoteLockReason('harness', { node: 'forge', options: operator }), /not given you a spawn grant.*operator decides the harness/);

  const granted = mod.normalizeRemoteSpawnOptions({ approval: 'automatic', overrides: { model: true, cwd: 'yes' }, profiles: [{ name: 'reviewer', harness: 'claude' }, { bad: 1 }], defaults: { cwd: '/srv/forge' } });
  assert.equal(mod.remoteFieldAllowed('model', granted), true);
  assert.equal(mod.remoteFieldAllowed('cwd', granted), false, 'only a literal true opens an override');
  assert.equal(mod.remoteFieldAllowed('profile', granted), true);
  assert.deepEqual(granted.profiles.map((p) => p.name), ['reviewer']);
  assert.match(mod.remoteLockReason('cwd', { node: 'forge', options: granted }), /forge's grant does not let you choose the directory — it uses \/srv\/forge/);
  assert.match(mod.remoteLockReason('sandbox', { node: 'forge', options: granted }), /forge applies its own setting/);

  const request = { body: { initial_message: ' fix it ', name: 'w', role: 'r', profile: 'reviewer', cwd: '/local/dir', harness: 'claude', model: 'opus', effort: 'high', sandbox: 'on' } };
  assert.deepEqual(mod.remoteSpawnBody(request, ['model', 'cwd'], granted), { brief: 'fix it', name: 'w', role: 'r', profile: 'reviewer', model: 'opus' },
    'only allowed overrides the operator set are sent; nothing local-only rides along');
  assert.deepEqual(mod.remoteSpawnBody(request, [], granted), { brief: 'fix it', name: 'w', role: 'r', profile: 'reviewer' }, 'an untouched prefill leaves the peer in charge');
  assert.throws(() => mod.remoteSpawnBody({ body: { initial_message: '  ' } }, [], granted), /write a brief/);
});

test('the spawn dialog opened on a peer keeps every control and disables what the peer decides', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ AgentSpawnApp }, { createAgentSpawnState }, remoteMod] = await Promise.all([
    harness.importDashboardModule('js/agent-spawn-island.js'),
    harness.importDashboardModule('js/agent-spawn-state.js'),
    harness.importDashboardModule('js/agent-spawn-remote.js'),
  ]);
  const local = (name) => () => { throw new Error(`local ${name} must not run for a remote spawn`); };
  const actions = {
    autoFocusDefault: () => true, rememberedEffort: () => '', rememberLaunchPreferences: () => {},
    dashboardDefaultProfile: () => 'local-default',
    loadProfiles: local('profiles'), loadWorktrees: local('worktrees'), loadSandboxPolicy: local('sandbox'),
    loadUnsandboxedAutonomy: local('autonomy'), loadLaunchDefaults: local('defaults'), resolveWorktree: local('worktree'),
    uploadAttachments: local('upload'), spawn: local('spawn'), complete: local('complete'), pickDirectory: local('picker'),
    openProfileEditor: () => {}, openPermissions: () => {}, confirmAutoName: async () => true,
  };
  const state = createAgentSpawnState({ getSnapshot: () => ({ groups: [{ name: 'local-only' }], harnesses: [] }) });
  const sent = []; const notes = [];
  const api = {
    spawn: async (group, body) => { sent.push([group, body]); return { id: 9, status: 'pending' }; },
    status: async (id) => ({ id, status: 'approved', result_agent: 'agt_new' }),
  };
  const options = remoteMod.normalizeRemoteSpawnOptions({ approval: 'automatic', overrides: { model: true, effort: true }, profiles: [] });
  harness.window.HTMLElement.prototype.scrollIntoView = () => {};
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`<${AgentSpawnApp} state=${state} actions=${actions} confirmDiscard=${async () => true} notify=${(m) => notes.push(m)} />`, host);
  state.open({ groupName: 'build', remote: { node: 'forge', nodeId: 'inst_forge', group: 'build', api, options, snapshot: { groups: [{ name: 'build', default_cwd: '/srv/build' }], harnesses } } });
  await flush(harness);
  await harness.act(() => new Promise((r) => setTimeout(r, 400)));
  await flush(harness);

  const q = (sel) => host.querySelector(sel);
  assert.match(q('#agent-spawn-title').textContent, /on forge/);
  assert.match(q('#agent-spawn-remote-intro').textContent, /forge launches it under the grant/);
  for (const id of ['agent-spawn-name', 'agent-spawn-init-msg', 'agent-spawn-role', 'agent-spawn-model', 'agent-spawn-effort', 'agent-spawn-submit', 'agent-spawn-cancel']) {
    assert.equal(q(`#${id}`).disabled, false, `${id} stays live`);
  }
  for (const [id, reason] of [
    ['agent-spawn-cwd', /forge does not let you choose the directory/],
    ['agent-spawn-cwd-browse', /this machine's folders, not forge's/],
    ['agent-spawn-harness', /choose the harness/],
    ['agent-spawn-load-profile', /lists no spawn profiles/],
    ['agent-spawn-attach-btn', /forge applies its own setting/],
    ['agent-spawn-owner', /forge applies its own setting/],
    ['agent-spawn-descr', /forge applies its own setting/],
  ]) {
    const el = q(`#${id}`);
    assert.ok(el, id);
    assert.equal(el.disabled, true, `${id} is the peer's to decide`);
    assert.match(el.title, reason, id);
  }
  const live = [...host.querySelectorAll('input, select, textarea, button')]
    .filter((el) => !el.disabled && !el.classList.contains('spawn-field-help-trigger') && !el.closest('.help-disclosure'))
    .map((el) => el.id || el.outerHTML.slice(0, 120)).sort();
  assert.deepEqual(live, ['agent-spawn-cancel', 'agent-spawn-effort', 'agent-spawn-init-msg', 'agent-spawn-model', 'agent-spawn-model-codex',
    'agent-spawn-model-custom', 'agent-spawn-name', 'agent-spawn-role', 'agent-spawn-submit'], 'fail-closed: everything else is disabled');

  const name = q('#agent-spawn-name'); setValue(name, 'parser'); await harness.act(() => harness.fireEvent(name, 'input'));
  const brief = q('#agent-spawn-init-msg'); setValue(brief, 'port the parser'); await harness.act(() => harness.fireEvent(brief, 'input'));
  const modelSel = q('#agent-spawn-model'); setValue(modelSel, 'opus'); await harness.act(() => harness.fireEvent(modelSel, 'change'));
  await flush(harness);
  assert.equal(q('#agent-spawn-cwd').disabled, true, 'the lock survives re-renders');
  await harness.act(() => q('#agent-spawn-submit').click());
  await harness.act(() => new Promise((r) => setTimeout(r, 50)));
  await flush(harness);
  assert.equal(q('#agent-spawn-error')?.textContent || '', '');
  assert.deepEqual(sent, [['build', { brief: 'port the parser', name: 'parser', model: 'opus' }]]);
  assert.match(notes[0], /Spawn request #9 sent to forge/);
  await mounted.unmount();
});

test('a remote spawn sends only the launch settings the operator set, not harness pre-fills', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ AgentSpawnApp }, { createAgentSpawnState }, remoteMod] = await Promise.all([
    harness.importDashboardModule('js/agent-spawn-island.js'),
    harness.importDashboardModule('js/agent-spawn-state.js'),
    harness.importDashboardModule('js/agent-spawn-remote.js'),
  ]);
  const local = (name) => () => { throw new Error(`local ${name} must not run for a remote spawn`); };
  const actions = {
    autoFocusDefault: () => true, rememberedEffort: () => 'high', rememberLaunchPreferences: () => {},
    dashboardDefaultProfile: () => '',
    loadProfiles: local('profiles'), loadWorktrees: local('worktrees'), loadSandboxPolicy: local('sandbox'),
    loadUnsandboxedAutonomy: local('autonomy'), loadLaunchDefaults: local('defaults'), resolveWorktree: local('worktree'),
    uploadAttachments: local('upload'), spawn: local('spawn'), complete: local('complete'), pickDirectory: local('picker'),
    openProfileEditor: () => {}, openPermissions: () => {}, confirmAutoName: async () => true,
  };
  const state = createAgentSpawnState({ getSnapshot: () => ({ groups: [], harnesses: [] }) });
  const sent = [];
  const api = {
    spawn: async (group, body) => { sent.push([group, body]); return { id: 3, status: 'pending' }; },
    status: async (id) => ({ id, status: 'approved', result_agent: 'agt_new' }),
  };
  const codex = { ...harnesses[0], name: 'codex', display_name: 'Codex', models: ['gpt-5'], effort_levels: ['low', 'high'] };
  const options = remoteMod.normalizeRemoteSpawnOptions({ approval: 'automatic', overrides: { harness: true, model: true, effort: true }, profiles: [{ name: 'reviewer', harness: 'claude', model: 'opus' }], harnesses: ['claude', 'codex'] });
  harness.window.HTMLElement.prototype.scrollIntoView = () => {};
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`<${AgentSpawnApp} state=${state} actions=${actions} confirmDiscard=${async () => true} notify=${() => {}} />`, host);
  state.open({ groupName: 'build', remote: { node: 'forge', nodeId: 'inst_forge', group: 'build', api, options, snapshot: { groups: [{ name: 'build' }], harnesses: [...harnesses, codex] } } });
  await flush(harness);
  await harness.act(() => new Promise((r) => setTimeout(r, 400)));
  await flush(harness);

  const q = (sel) => host.querySelector(sel);
  const submit = async () => {
    await harness.act(() => q('#agent-spawn-submit').click());
    await harness.act(() => new Promise((r) => setTimeout(r, 50)));
    await flush(harness);
    assert.equal(q('#agent-spawn-error')?.textContent || '', '');
  };
  const pick = async (sel, value, event = 'change') => {
    const el = q(sel); setValue(el, value); await harness.act(() => harness.fireEvent(el, event)); await flush(harness);
  };
  // Loading a profile and clearing it again pre-fills the harness defaults
  // (harness claude): none of that is the operator's choice.
  await pick('#agent-spawn-load-profile', 'reviewer');
  await pick('#agent-spawn-load-profile', '');
  await pick('#agent-spawn-init-msg', 'port the parser', 'input');
  await submit();
  assert.deepEqual(sent, [['build', { brief: 'port the parser', name: 'port-the-parser' }]], 'profile pre-fills are not overrides');
  await mounted.unmount();

  // An explicit model, then a harness change: the harness is the operator's,
  // the model it replaced with the harness default is not.
  sent.length = 0;
  const host2 = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted2 = await harness.mount(harness.html`<${AgentSpawnApp} state=${state} actions=${actions} confirmDiscard=${async () => true} notify=${() => {}} />`, host2);
  state.open({ groupName: 'build', remote: { node: 'forge', nodeId: 'inst_forge', group: 'build', api, options, snapshot: { groups: [{ name: 'build' }], harnesses: [...harnesses, codex] } } });
  await flush(harness);
  await harness.act(() => new Promise((r) => setTimeout(r, 400)));
  await flush(harness);
  const q2 = (sel) => host2.querySelector(sel);
  for (const [sel, value, event] of [['#agent-spawn-init-msg', 'port the parser', 'input'], ['#agent-spawn-model', 'opus', 'change'], ['#agent-spawn-harness', 'codex', 'change']]) {
    const el = q2(sel); setValue(el, value); await harness.act(() => harness.fireEvent(el, event)); await flush(harness);
  }
  await harness.act(() => q2('#agent-spawn-submit').click());
  await harness.act(() => new Promise((r) => setTimeout(r, 50)));
  await flush(harness);
  assert.deepEqual(sent, [['build', { brief: 'port the parser', name: 'port-the-parser', harness: 'codex' }]],
    'the model the harness change replaced is not an override');
  await mounted2.unmount();
});
