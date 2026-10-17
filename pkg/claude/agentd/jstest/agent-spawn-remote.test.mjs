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
  assert.equal(mod.remoteFieldAllowed('queue', granted), false, 'queueing is this node\'s, never a remote request');
  assert.match(mod.remoteLockReason('queue', { node: 'forge', options: granted }), /Queued spawns wait on this node/);
  assert.match(mod.remoteLockReason('owner', { node: 'forge', options: granted }), /forge applies its own setting/);

  const request = { body: { initial_message: ' fix it ', name: 'w', role: 'r', profile: 'reviewer', cwd: '/local/dir', harness: 'claude', model: 'opus', effort: 'high', sandbox: 'on' } };
  assert.deepEqual(mod.remoteSpawnBody(request, ['model', 'cwd', 'profile'], granted), { brief: 'fix it', name: 'w', role: 'r', profile: 'reviewer', model: 'opus' },
    'only allowed overrides the operator set are sent; nothing local-only rides along');
  assert.deepEqual(mod.remoteSpawnBody(request, [], granted), { brief: 'fix it', name: 'w', role: 'r' }, 'an untouched prefill (even its profile) leaves the peer in charge');
  assert.deepEqual(mod.remoteSpawnBody({ body: { ...request.body, profile: 'local:mine' } }, ['profile'], granted), { brief: 'fix it', name: 'w', role: 'r' }, 'a local profile is never named to the peer');
  assert.deepEqual(mod.remoteSpawnBody({ body: { ...request.body, profile: 'other' } }, ['profile'], granted), { brief: 'fix it', name: 'w', role: 'r' }, 'nor a peer profile the grant does not list');
  assert.deepEqual(mod.remoteSpawnBody(request, [], granted, ['first', 'second']), { brief: 'first\n\nsecond\n\nfix it', name: 'w', role: 'r' }, 'prompt bodies open the brief');
  assert.throws(() => mod.remoteSpawnBody({ body: { initial_message: '  ' } }, [], granted), /write a brief/);

  // Roles and a sandbox profile ride only under their own grants, when the
  // operator set them; the omit-all sentinel never does.
  const wide = mod.normalizeRemoteSpawnOptions({ approval: 'automatic', overrides: { roles: true, sandbox: true } });
  const listed = { body: { initial_message: 'go', role_refs: ['builder'], sandbox_profile: 'tight' } };
  assert.deepEqual(mod.remoteSpawnBody(listed, ['roleRefs', 'sandboxProfile'], wide), { brief: 'go', roles: ['builder'], sandbox_profile: 'tight' });
  assert.deepEqual(mod.remoteSpawnBody(listed, [], wide), { brief: 'go' }, 'pre-filled roles stay the peer\'s');
  assert.deepEqual(mod.remoteSpawnBody(listed, ['roleRefs', 'sandboxProfile'], granted), { brief: 'go' }, 'not without the grants');
  assert.deepEqual(mod.remoteSpawnBody({ body: { initial_message: 'go', sandbox_profile: '/omit-all-tclaude-sandbox-profiles' } }, ['sandboxProfile'], wide), { brief: 'go' });
  assert.equal(mod.remoteFieldAllowed('roles', wide, { target: {} }), true);
  assert.equal(mod.remoteFieldAllowed('roles', wide), false, 'the roles to pick come from the peer\'s catalogs');
  assert.match(mod.remoteLockReason('roles', { node: 'forge', options: wide }), /does not share its list/);
  assert.equal(mod.remoteFieldAllowed('roles', granted, { target: {} }), false);
  assert.match(mod.remoteLockReason('roles', { node: 'forge', options: granted }), /groups\.members\.spawn\.roles/);
  assert.throws(() => mod.remoteSpawnBody({ body: { initial_message: 'go', role_refs: [] } }, ['roleRefs'], wide), /cannot remove every role/);
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
    saveQueued: local('queue save'), deleteQueued: local('queue delete'),
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
    'agent-spawn-model-custom', 'agent-spawn-name', 'agent-spawn-role', 'agent-spawn-source-local', 'agent-spawn-source-target', 'agent-spawn-submit'], 'fail-closed: everything else is disabled');

  const name = q('#agent-spawn-name'); setValue(name, 'parser'); await harness.act(() => harness.fireEvent(name, 'input'));
  const brief = q('#agent-spawn-init-msg'); setValue(brief, 'port the parser'); await harness.act(() => harness.fireEvent(brief, 'input'));
  const modelSel = q('#agent-spawn-model'); setValue(modelSel, 'opus'); await harness.act(() => harness.fireEvent(modelSel, 'change'));
  await flush(harness);
  assert.equal(q('#agent-spawn-cwd').disabled, true, 'the lock survives re-renders');
  // The queue is this node's: its button is locked, and its shortcut neither
  // queues nor falls through to sending the request.
  assert.equal(q('#agent-spawn-queue').disabled, true);
  assert.match(q('#agent-spawn-queue').title, /Queued spawns wait on this node/);
  await harness.act(() => harness.fireEvent(brief, 'keydown', { key: 'Enter', ctrlKey: true, shiftKey: true }));
  await harness.act(() => new Promise((r) => setTimeout(r, 50)));
  assert.deepEqual(sent, [], 'the queue shortcut sends nothing');
  assert.equal(q('#agent-spawn-error')?.textContent || '', '');
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
  // Loading a profile and clearing it again resets the harness to its
  // default (claude), even one the operator picked: none of that is sent.
  await pick('#agent-spawn-harness', 'codex');
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
  for (const [sel, value, event] of [['#agent-spawn-init-msg', 'port the parser', 'input'], ['#agent-spawn-model', 'opus', 'change'], ['#agent-spawn-harness', 'codex', 'change'],
    ['#agent-spawn-effort', 'low', 'change'], ['#agent-spawn-model', '__custom__', 'change']]) {
    const el = q2(sel); setValue(el, value); await harness.act(() => harness.fireEvent(el, event)); await flush(harness);
  }
  await harness.act(() => q2('#agent-spawn-submit').click());
  await harness.act(() => new Promise((r) => setTimeout(r, 50)));
  await flush(harness);
  assert.deepEqual(sent, [['build', { brief: 'port the parser', name: 'port-the-parser', harness: 'codex', effort: 'low' }]],
    'the model the harness change replaced is not an override; switching to a custom model keeps the chosen effort');
  await mounted2.unmount();
});

test('a peer that lends its catalogs pre-fills the dialog with what it applies, and offers both libraries', async (t) => {
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
    uploadAttachments: local('upload'), spawn: local('spawn'), complete: () => {}, pickDirectory: local('picker'),
    openProfileEditor: () => {}, openPermissions: () => {}, confirmAutoName: async () => true,
  };
  const reads = [];
  const respond = (body) => ({ ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) });
  const peerFetch = async (path) => { reads.push(String(path).split('?')[0]); return respond({}); };
  const localFetch = async (path) => respond(String(path) === '/api/saved-prompts'
    ? [{ name: 'style', body: 'Use my style.' }]
    : [{ name: 'mine', harness: 'claude', model: 'opus', prompt_refs: ['style'] }]);
  const peerCatalog = {
    '/api/spawn-profiles': [
      { name: 'house', harness: 'claude', model: 'sonnet', prompt_refs: ['house-rules'] },
      { name: 'reviewer', harness: 'claude', model: 'opus', prompt_refs: ['review'] },
    ],
    '/api/spawn-profile-default': { name: 'house' },
    '/api/saved-prompts': [{ name: 'house-rules', body: 'Follow the house rules.' }, { name: 'review', body: 'Review it.' }],
    '/api/roles': [{ name: 'builder', descr: 'builds' }],
    '/api/sandbox-profiles': [],
    '/api/claude-settings/default-model': { model: 'sonnet' },
  };
  const options = remoteMod.normalizeRemoteSpawnOptions({
    approval: 'automatic', catalog: true, overrides: { model: true, effort: true },
    profiles: [{ name: 'reviewer', harness: 'claude', model: 'opus' }], defaults: { cwd: '/srv/build' },
  });
  const catalogs = await remoteMod.loadRemoteSpawnCatalogs({
    options, localFetch, peerFetch: async (path) => respond(peerCatalog[String(path)] ?? {}),
  });
  assert.deepEqual(catalogs.target.roles.map((r) => r.name), ['builder']);
  assert.equal(catalogs.target.defaultProfile, 'house');
  assert.deepEqual(catalogs.local.prompts.map((p) => p.name), ['style']);
  assert.equal((await remoteMod.loadRemoteSpawnCatalogs({ options: { ...options, catalog: false }, localFetch, peerFetch: local('peer catalog') })).target, null,
    'without node.spawn.read nothing of the peer is read');

  const state = createAgentSpawnState({ getSnapshot: () => ({ groups: [], harnesses: [] }) });
  const sent = [];
  const api = { spawn: async (group, body) => { sent.push(body); return { id: 4, status: 'pending' }; }, status: async (id) => ({ id, status: 'approved' }) };
  harness.window.HTMLElement.prototype.scrollIntoView = () => {};
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`<${AgentSpawnApp} state=${state} actions=${actions} confirmDiscard=${async () => true} notify=${() => {}} />`, host);
  const open = async () => {
    state.open({ groupName: 'build', remote: { node: 'forge', nodeId: 'inst_forge', group: 'build', api, options, catalogs, peerFetch, snapshot: { groups: [{ name: 'build' }], harnesses } } });
    await flush(harness);
    await harness.act(() => new Promise((r) => setTimeout(r, 400)));
    await flush(harness);
  };
  await open();
  const q = (sel) => host.querySelector(sel);
  const val = (sel) => q(sel).value ?? q(sel).getAttribute('value');
  const pick = async (sel, value, event = 'change') => {
    const el = q(sel); setValue(el, value); await harness.act(() => harness.fireEvent(el, event)); await flush(harness);
  };
  const submit = async () => {
    await harness.act(() => q('#agent-spawn-submit').click());
    await harness.act(() => new Promise((r) => setTimeout(r, 50)));
    await flush(harness);
    assert.equal(q('#agent-spawn-error')?.textContent || '', '');
  };

  // The peer's default profile pre-fills what the peer will launch with.
  assert.equal(val('#agent-spawn-load-profile'), 'house');
  assert.equal(val('#agent-spawn-model'), 'sonnet');
  assert.equal(val('#agent-spawn-cwd'), '/srv/build', 'the grant pin shows in the locked directory');
  assert.ok(reads.includes('/api/spawn-launch-defaults'), 'launch defaults are read from the peer');
  const chip = [...host.querySelectorAll('#agent-spawn-prompt-refs .agent-spawn-role-chip')];
  assert.deepEqual(chip.map((c) => c.querySelector('.role-chip-inspect').textContent), ['house-rules']);
  assert.equal(chip[0].querySelector('.role-chip-remove').disabled, true, 'a prompt the peer applies itself is locked');
  assert.match(chip[0].querySelector('.role-chip-remove').title, /forge applies this prompt itself/);
  const profileOptions = [...q('#agent-spawn-load-profile').querySelectorAll('option')];
  assert.equal(profileOptions.find((o) => o.getAttribute('value') === 'house').hasAttribute('disabled'), true, 'a peer profile the grant does not list cannot be picked');
  assert.equal(profileOptions.find((o) => o.getAttribute('value') === 'reviewer').hasAttribute('disabled'), false);
  assert.ok(profileOptions.find((o) => o.getAttribute('value') === 'local:mine'), 'this node\'s profiles are offered too');
  assert.equal(q('#agent-spawn-role-add').disabled, true, 'adding roles waits for a per-field grant');
  assert.ok([...q('#agent-spawn-role-add').querySelectorAll('option')].some((o) => o.getAttribute('value') === 'builder'), 'the peer\'s roles are listed');

  // The source filter narrows both pickers.
  await harness.act(() => harness.fireEvent(q('#agent-spawn-source-local'), 'change'));
  await flush(harness);
  assert.ok(![...q('.saved-prompt-chip-add').querySelectorAll('option')].some((o) => String(o.getAttribute('value')).startsWith('local:')));
  await harness.act(() => harness.fireEvent(q('#agent-spawn-source-local'), 'change'));
  await flush(harness);

  // A local prompt is written into the brief; the peer's own stays its own.
  await pick('#agent-spawn-name', 'w', 'input');
  await pick('#agent-spawn-init-msg', 'go', 'input');
  await pick('.saved-prompt-chip-add', 'local:style');
  await submit();
  assert.deepEqual(sent.pop(), { brief: 'Use my style.\n\ngo', name: 'w' });

  // A peer profile the grant lists rides by name; the peer applies its prompts.
  await open();
  await pick('#agent-spawn-load-profile', 'reviewer');
  assert.equal(val('#agent-spawn-model'), 'opus');
  await pick('#agent-spawn-name', 'w', 'input');
  await pick('#agent-spawn-init-msg', 'go', 'input');
  await submit();
  assert.deepEqual(sent.pop(), { brief: 'go', name: 'w', profile: 'reviewer' });

  // A local profile fills only what the request carries.
  await open();
  await pick('#agent-spawn-load-profile', 'local:mine');
  assert.equal(val('#agent-spawn-model'), 'opus');
  await pick('#agent-spawn-name', 'w', 'input');
  await pick('#agent-spawn-init-msg', 'go', 'input');
  await submit();
  assert.deepEqual(sent.pop(), { brief: 'Use my style.\n\ngo', name: 'w', model: 'opus' });

  // A local profile after a peer one starts again from what the peer
  // applies: the peer profile's prompts are neither locked nor sent.
  await open();
  await pick('#agent-spawn-load-profile', 'reviewer');
  await pick('#agent-spawn-load-profile', 'local:mine');
  const chips = () => [...host.querySelectorAll('.saved-prompt-picker .agent-spawn-role-chip')].map((c) => c.querySelector('.role-chip-inspect').textContent);
  assert.deepEqual(chips(), ['house-rules', 'style · this node']);
  await pick('#agent-spawn-name', 'w', 'input');
  await pick('#agent-spawn-init-msg', 'go', 'input');
  await submit();
  assert.deepEqual(sent.pop(), { brief: 'Use my style.\n\ngo', name: 'w', model: 'opus' }, 'no peer profile rides along');

  // The group's own defaults, which a restricted peer's snapshot may
  // withhold, come from spawn-options: its default profile outranks the
  // peer-wide default, and its prompts are the peer's to apply.
  const grouped = remoteMod.normalizeRemoteSpawnOptions({
    approval: 'automatic', catalog: true, overrides: { model: true },
    profiles: [], defaults: { group_profile: 'reviewer' }, default_prompts: ['house-rules'],
  });
  state.open({ groupName: 'build', remote: { node: 'forge', nodeId: 'inst_forge', group: 'build', api, options: grouped, catalogs, peerFetch, snapshot: { groups: [{ name: 'build' }], harnesses } } });
  await flush(harness);
  await harness.act(() => new Promise((r) => setTimeout(r, 400)));
  await flush(harness);
  assert.equal(val('#agent-spawn-load-profile'), 'reviewer');
  assert.equal(val('#agent-spawn-model'), 'opus');
  assert.deepEqual(chips(), ['review'], 'the group profile\'s prompts, locked as the peer\'s');
  await mounted.unmount();
});
