import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent, assertSameNode } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';

function snapshot({ members = [], groups, online = true, slugs = [], permissions } = {}) {
  const team = { name: 'team', permissions: [], members };
  return {
    agents: [{ agent_id: 'agt_sender', conv_id: 'conv-s', title: 'sender', online }],
    groups: groups === undefined ? [team] : groups,
    permissions: permissions || { defaults: [], overrides: {} },
    slugs,
    sudo: [],
  };
}

function member(name) {
  return { agent_id: `agt_${name}`, conv_id: `conv-${name}`, title: name, online: true };
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((ok, fail) => { resolve = ok; reject = fail; });
  return { promise, resolve, reject };
}

async function mountDialogs(harness, state, actions, currentSnapshot, confirmDiscard = async () => true) {
  const { MessageAccessDialogApp } = await harness.importDashboardModule('js/message-access-dialog-island.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const view = (value) => harness.html`<${MessageAccessDialogApp} state=${state} actions=${actions}
    snapshot=${value} confirmDiscard=${confirmDiscard}/>`;
  const mounted = await harness.mount(view(currentSnapshot), host);
  return { host, mounted, rerender: (value) => mounted.rerender(view(value)) };
}

test('child chooser keeps the keyed parent draft mounted and cancellation returns focus', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createMessageAccessDialogState }, { MessageAccessDialogApp }] = await Promise.all([
    harness.importDashboardModule('js/message-access-dialog-state.js'),
    harness.importDashboardModule('js/message-access-dialog-island.js'),
  ]);
  const state = createMessageAccessDialogState();
  state.openMessage({ from: 'agt_sender' });
  const snapshot = {
    agents: [{ agent_id: 'agt_sender', conv_id: 'conv-s', title: 'sender', online: true }],
    groups: [], permissions: { defaults: [], overrides: {} }, slugs: [], sudo: [],
  };
  const actions = {
    sendMessage: async () => {}, replyHuman: async () => {}, grantSudo: async () => {}, savePermissions: async () => {},
  };
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`<${MessageAccessDialogApp} state=${state} actions=${actions}
    snapshot=${snapshot} confirmDiscard=${async () => true}/>` , host);
  const parent = host.querySelector('#message-create-modal');
  const body = host.querySelector('#message-create-body');
  await harness.input(body, 'draft survives');
  const pickerButton = host.querySelector('#message-create-from-pick');
  pickerButton.focus();
  pickerButton.click();
  await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#cron-pick-target-modal'));
  assertSameNode(host.querySelector('#message-create-modal'), parent, 'opening the keyed child does not recreate its parent');
  assertSameNode(host.querySelector('#message-create-body'), body);
  assert.equal(body.value, 'draft survives');

  await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }));
  assertAbsent(host.querySelector('#cron-pick-target-modal'));
  assert.notEqual(host.querySelector('#message-create-modal'), null, 'stacked Escape closes only the chooser');
  assert.equal(host.querySelector('#message-create-body').value, 'draft survives');
  assertSameNode(harness.document.activeElement, pickerButton, 'child teardown restores its invoker');
  await mounted.unmount();
});

test('chooser keeps keyboard highlight visible and exposes the active option', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogState } = await harness.importDashboardModule('js/message-access-dialog-state.js');
  const state = createMessageAccessDialogState();
  const picked = state.pickAgent({ title: 'Pick target', identity: 'agent' });
  const rows = Array.from({ length: 24 }, (_, index) => ({
    agent_id: `agt_${String(index).padStart(2, '0')}`,
    conv_id: `conv-${index}`,
    title: `agent ${String(index).padStart(2, '0')}`,
    online: true,
  }));
  const original = Object.getOwnPropertyDescriptor(harness.window.HTMLElement.prototype, 'scrollIntoView');
  let scrolled = '';
  Object.defineProperty(harness.window.HTMLElement.prototype, 'scrollIntoView', {
    configurable: true,
    value() { scrolled = this.id; },
  });
  t.after(() => {
    if (original) Object.defineProperty(harness.window.HTMLElement.prototype, 'scrollIntoView', original);
    else delete harness.window.HTMLElement.prototype.scrollIntoView;
  });
  const actions = { sendMessage: async () => {}, replyHuman: async () => {}, grantSudo: async () => {}, savePermissions: async () => {} };
  const { host, mounted } = await mountDialogs(harness, state, actions, {
    agents: rows, groups: [], permissions: { defaults: [], overrides: {} }, slugs: [], sudo: [],
  });
  scrolled = '';
  const search = host.querySelector('#cron-pick-target-search');
  await harness.act(() => harness.fireEvent(search, 'keydown', { key: 'ArrowDown' }));
  assert.equal(search.getAttribute('aria-activedescendant'), 'cron-pick-target-option-1');
  assert.equal(host.querySelector('#cron-pick-target-option-1').getAttribute('aria-selected'), 'true');
  assert.equal(scrolled, 'cron-pick-target-option-1');
  await harness.act(() => harness.fireEvent(search, 'keydown', { key: 'Enter' }));
  assert.equal(await picked, 'agt_01');
  await mounted.unmount();
});

test('mounted island registers the launcher seam and snapshot subscription preserves component identity', async (t) => {
  const harness = await createPreactHarness(t);
  const [island, stateModule, controller] = await Promise.all([
    harness.importDashboardModule('js/message-access-dialog-island.js'),
    harness.importDashboardModule('js/message-access-dialog-state.js'),
    harness.importDashboardModule('js/message-access-dialog-controller.js'),
  ]);
  const state = stateModule.createMessageAccessDialogState();
  const snapshotSignal = harness.signals.signal(snapshot({ members: [member('a')] }));
  const dialogHost = harness.document.body.appendChild(harness.document.createElement('div'));
  const cleanups = [];
  const actions = { sendMessage: async () => {}, replyHuman: async () => {}, grantSudo: async () => {}, savePermissions: async () => {} };
  await harness.act(() => island.mountMessageAccessDialogIsland({
    dialogHost, state, actions, snapshot: snapshotSignal,
    confirmDiscard: async () => true, registerCleanup: (cleanup) => cleanups.push(cleanup),
  }));
  await harness.act(() => { controller.openMessageCreateModal({ from: 'agt_sender', targetMode: 'group', groupName: 'team' }); });
  const body = dialogHost.querySelector('#message-create-body');
  await harness.input(body, 'retained by subscription');
  await harness.act(() => { snapshotSignal.value = snapshot({ members: [member('a'), member('b')] }); });
  assertSameNode(dialogHost.querySelector('#message-create-body'), body);
  assert.equal(body.value, 'retained by subscription');
  assert.equal(dialogHost.querySelector('#message-create-members-count').textContent, '2 of 2 selected');

  const canceled = controller.pickAgent({ title: 'Pick sender' });
  await harness.act(() => Promise.resolve());
  assert.ok(dialogHost.querySelector('#cron-pick-target-modal'));
  await harness.act(() => { cleanups.reverse().forEach((cleanup) => cleanup()); });
  assert.equal(await canceled, '', 'island teardown cancels an outstanding chooser promise');
  assert.equal(dialogHost.childElementCount, 0);
  dialogHost.remove();
});

test('partial island initialization rolls back the controller and permits a clean remount', async (t) => {
  const harness = await createPreactHarness(t);
  const [island, stateModule, controller] = await Promise.all([
    harness.importDashboardModule('js/message-access-dialog-island.js'),
    harness.importDashboardModule('js/message-access-dialog-state.js'),
    harness.importDashboardModule('js/message-access-dialog-controller.js'),
  ]);
  const dialogHost = harness.document.body.appendChild(harness.document.createElement('div'));
  const actions = { sendMessage: async () => {}, replyHuman: async () => {}, grantSudo: async () => {}, savePermissions: async () => {} };
  const failedCleanups = [];
  assert.throws(() => island.mountMessageAccessDialogIsland({
    dialogHost,
    state: stateModule.createMessageAccessDialogState(),
    actions,
    snapshot: {
      value: snapshot({ members: [] }),
      subscribe() { throw new Error('snapshot subscription failed'); },
    },
    confirmDiscard: async () => true,
    registerCleanup: (cleanup) => failedCleanups.push(cleanup),
  }), /snapshot subscription failed/);
  assert.equal(failedCleanups.length, 0, 'failed initialization never publishes a page cleanup');
  assert.equal(dialogHost.childElementCount, 0);
  assert.throws(() => controller.openMessageCreateModal(), /dialogs are not ready/,
    'failed initialization releases the global launcher seam');

  const cleanups = [];
  const state = stateModule.createMessageAccessDialogState();
  await harness.act(() => island.mountMessageAccessDialogIsland({
    dialogHost,
    state,
    actions,
    snapshot: harness.signals.signal(snapshot({ members: [] })),
    confirmDiscard: async () => true,
    registerCleanup: (cleanup) => cleanups.push(cleanup),
  }));
  await harness.act(() => { controller.openMessageCreateModal({ from: 'agt_sender', target: 'agt_a' }); });
  assert.ok(dialogHost.querySelector('#message-create-modal'));
  await harness.act(() => { cleanups.reverse().forEach((cleanup) => cleanup()); });
  dialogHost.remove();
});

test('scoped message follows live-all membership and sends exact group role payload', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogState } = await harness.importDashboardModule('js/message-access-dialog-state.js');
  const state = createMessageAccessDialogState();
  state.openMessage({ from: 'agt_sender', targetMode: 'group', groupName: 'team' });
  const sent = [];
  const actions = {
    sendMessage: async (payload) => { sent.push(payload); },
    replyHuman: async () => {}, grantSudo: async () => {}, savePermissions: async () => {},
  };
  const first = snapshot({ members: [member('a'), member('b')] });
  const { host, mounted, rerender } = await mountDialogs(harness, state, actions, first);
  const body = host.querySelector('#message-create-body');
  await harness.act(() => Promise.resolve());
  assertSameNode(harness.document.activeElement, body, 'the group message body owns initial focus');
  await harness.input(body, 'hello live roster');
  await harness.input(host.querySelector('#message-create-subject'), 'status');
  await harness.input(host.querySelector('#message-create-role'), ' reviewer ');
  await harness.act(() => { host.querySelector('#message-create-members-all').click(); });

  const next = snapshot({ members: [member('a'), member('b'), { ...member('c'), online: false }] });
  await rerender(next);
  assertSameNode(host.querySelector('#message-create-body'), body, 'snapshot reconciliation keeps the keyed draft node');
  assert.equal(body.value, 'hello live roster');
  assert.equal(host.querySelector('#message-create-members-count').textContent, '3 of 3 selected');
  assert.equal(host.querySelectorAll('#message-create-members input[type="checkbox"]:checked').length, 3,
    'all follows a newcomer, offline or not');

  await harness.act(async () => { host.querySelector('#message-create-submit').click(); await Promise.resolve(); });
  assert.deepEqual(sent, [{
    from: 'agt_sender', to: 'group:team', subject: 'status', body: 'hello live roster', role: 'reviewer',
  }], 'all omits members so the daemon reads the live roster');
  assert.equal(state.dialog.value, null);
  await mounted.unmount();
});

test('scoped message ticks online members by default, follows presence, and quick-picks combine', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogState } = await harness.importDashboardModule('js/message-access-dialog-state.js');
  const state = createMessageAccessDialogState();
  state.openMessage({ from: 'agt_sender', targetMode: 'group', groupName: 'team' });
  const sent = [];
  const actions = {
    sendMessage: async (payload) => { sent.push(payload); },
    replyHuman: async () => {}, grantSudo: async () => {}, savePermissions: async () => {},
  };
  const withPR = (m, checks) => ({ ...m, branch_pr_number: 7, branch_pr_url: `https://github.com/o/r/pull/7${m.title}`, branch_pr_state: 'open', branch_checks: checks });
  const offline = (m) => ({ ...m, online: false });
  const roster = (bOnline) => snapshot({ members: [
    withPR(member('a'), { total: 2, passed: 1, failed: 1, state: 'failing' }),
    bOnline ? member('b') : offline(member('b')),
    withPR(offline(member('c')), { total: 1, passed: 1, state: 'passing' }),
  ] });
  const { host, mounted, rerender } = await mountDialogs(harness, state, actions, roster(false));
  const ticked = () => [...host.querySelectorAll('#message-create-members input:checked')].map((input) => input.getAttribute('data-conv'));
  const badges = () => [...host.querySelectorAll('#message-create-members .cleanup-badge')].map((badge) => badge.textContent);
  assert.deepEqual(ticked(), ['conv-a'], 'offline members start unticked');
  assert.deepEqual(badges(), ['online', 'offline', 'offline']);
  assert.ok(host.querySelector('#message-create-members .ci-badge.ci-failing'), 'a member\'s open PR shows its CI pill');
  assert.equal(host.querySelector('#message-create-members label .pr-chain'), null,
    'PR badges sit outside the row label so their hover panels never toggle the recipient');

  await rerender(roster(true));
  assert.deepEqual(ticked(), ['conv-a', 'conv-b'], 'the default follows live presence');

  await harness.act(() => harness.fireEvent(host.querySelector('#message-create-pick-offline'), 'click', { shiftKey: true }));
  assert.deepEqual(ticked(), ['conv-a', 'conv-b', 'conv-c'], 'shift-click adds');
  await harness.act(() => harness.fireEvent(host.querySelector('#message-create-pick-pr'), 'click', { ctrlKey: true }));
  assert.deepEqual(ticked(), ['conv-a', 'conv-c'], 'ctrl-click narrows');
  await harness.act(() => harness.fireEvent(host.querySelector('#message-create-pick-ci-failing'), 'click'));
  assert.deepEqual(ticked(), ['conv-a'], 'a plain click replaces');
  await harness.act(() => { host.querySelector('#message-create-pick-online').click(); });
  assert.deepEqual(ticked(), ['conv-a', 'conv-b']);

  await harness.input(host.querySelector('#message-create-body'), 'online only');
  await harness.act(async () => { host.querySelector('#message-create-submit').click(); await Promise.resolve(); });
  assert.deepEqual(sent, [{ from: 'agt_sender', to: 'group:team', subject: '', body: 'online only', members: ['agt_a', 'agt_b'] }],
    'the online default names its recipients so an offline member is never reached');
  await mounted.unmount();
});

test('custom group selection survives identity promotion/newcomer/removal, blocks a missing group, and retries exactly', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogState } = await harness.importDashboardModule('js/message-access-dialog-state.js');
  const state = createMessageAccessDialogState();
  state.openMessage({ from: 'agt_sender', targetMode: 'group', groupName: 'team' });
  let attempts = 0;
  const sent = [];
  const actions = {
    sendMessage: async (payload) => {
      attempts++;
      sent.push(payload);
      if (attempts === 1) throw new Error('temporary failure');
    },
    replyHuman: async () => {}, grantSudo: async () => {}, savePermissions: async () => {},
  };
  const { host, mounted, rerender } = await mountDialogs(
    harness, state, actions, snapshot({ members: [member('a'), { ...member('b'), agent_id: '' }] }),
  );
  await harness.input(host.querySelector('#message-create-body'), 'only b');
  await harness.act(() => { host.querySelector('#message-create-members-none').click(); });
  const b = [...host.querySelectorAll('#message-create-members input')]
    .find((input) => input.getAttribute('data-conv') === 'conv-b');
  b.checked = true;
  await harness.act(() => harness.fireEvent(b, 'change'));

  await rerender(snapshot({ members: [member('b'), member('c')] }));
  assert.equal(host.querySelector('#message-create-members-count').textContent, '1 of 2 selected');

  await rerender(snapshot({ groups: [] }));
  assert.equal(host.querySelector('#message-create-submit').disabled, true);
  assert.match(host.querySelector('#message-create-group-hint').textContent, /missing/i);
  assert.equal(host.querySelector('#message-create-body').value, 'only b');
  await rerender(snapshot({ members: [member('b'), member('c')] }));

  await harness.act(async () => { host.querySelector('#message-create-submit').click(); await Promise.resolve(); });
  assert.equal(host.querySelector('#message-create-error').textContent, 'temporary failure');
  assert.equal(host.querySelector('#message-create-body').value, 'only b', 'failed send keeps the draft for retry');
  await harness.act(async () => { host.querySelector('#message-create-submit').click(); await Promise.resolve(); });
  assert.deepEqual(sent, [
    { from: 'agt_sender', to: 'group:team', subject: '', body: 'only b', members: ['agt_b'] },
    { from: 'agt_sender', to: 'group:team', subject: '', body: 'only b', members: ['agt_b'] },
  ], 'promoted selected member stays selected while the newcomer remains excluded');
  assert.equal(state.dialog.value, null);
  await mounted.unmount();
});

test('message busy state prevents duplicates and submit hotkey ignores IME composition', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogState } = await harness.importDashboardModule('js/message-access-dialog-state.js');
  const state = createMessageAccessDialogState();
  state.openMessage({ from: 'agt_sender', target: 'agt_a' });
  const pending = deferred();
  let calls = 0;
  const actions = {
    sendMessage: () => { calls++; return pending.promise; },
    replyHuman: async () => {}, grantSudo: async () => {}, savePermissions: async () => {},
  };
  const { host, mounted } = await mountDialogs(harness, state, actions, snapshot({ members: [] }));
  const body = host.querySelector('#message-create-body');
  await harness.input(body, 'hello');
  await harness.act(() => harness.fireEvent(body, 'keydown', { key: 'Enter', ctrlKey: true, isComposing: true }));
  await harness.act(() => harness.fireEvent(body, 'keydown', { key: 'Enter', ctrlKey: true, keyCode: 229 }));
  assert.equal(calls, 0, 'IME Enter is never treated as submit');
  await harness.act(() => harness.fireEvent(body, 'keydown', { key: 'Enter', ctrlKey: true }));
  assert.equal(calls, 1);
  assert.equal(host.querySelector('#message-create-submit').disabled, true);
  assert.match(host.querySelector('#message-create-submit').textContent, /Sending/);
  await harness.act(() => { host.querySelector('#message-create-submit').click(); });
  assert.equal(calls, 1, 'busy button/direct submit cannot enqueue a duplicate');
  pending.resolve({});
  await harness.act(() => pending.promise);
  assert.equal(state.dialog.value, null);
  await mounted.unmount();
});

test('human reply follows live online state and trusts server offline until a newer snapshot', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogState } = await harness.importDashboardModule('js/message-access-dialog-state.js');
  const state = createMessageAccessDialogState();
  state.openHumanReply({ id: '42', agent: 'agt_sender', conv: 'conv-old', label: 'sender', subject: 'help' });
  let calls = 0;
  const actions = {
    sendMessage: async () => {}, grantSudo: async () => {}, savePermissions: async () => {},
    replyHuman: async () => { calls++; const error = new Error('went offline'); error.code = 'offline'; throw error; },
  };
  const offline = snapshot({ online: false, groups: [] });
  const { host, mounted, rerender } = await mountDialogs(harness, state, actions, offline);
  await harness.act(() => Promise.resolve());
  assertSameNode(harness.document.activeElement, host.querySelector('#human-reply-body'), 'the reply body owns initial focus');
  await harness.input(host.querySelector('#human-reply-body'), 'answer');
  assert.equal(host.querySelector('#human-reply-submit').disabled, true);
  await rerender(snapshot({ online: true, groups: [] }));
  assert.equal(host.querySelector('#human-reply-body').value, 'answer');
  assert.equal(host.querySelector('#human-reply-submit').disabled, false);
  await harness.act(async () => { host.querySelector('#human-reply-submit').click(); await Promise.resolve(); });
  assert.equal(calls, 1);
  assert.equal(host.querySelector('#human-reply-submit').disabled, true, 'server offline verdict wins over stale live state');
  assert.match(host.querySelector('#human-reply-error').textContent, /offline/);
  await rerender(snapshot({ online: true, groups: [] }));
  assert.equal(host.querySelector('#human-reply-submit').disabled, false, 'new accepted snapshot reconciles the verdict');
  await mounted.unmount();
});

test('a notification on another node is answered through that node, text only', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogState } = await harness.importDashboardModule('js/message-access-dialog-state.js');
  const state = createMessageAccessDialogState();
  const sent = [];
  state.openHumanReply({ id: 5, agent: 'agt_sender', conv: 'conv-forge', label: 'sender', nodeName: 'forge', online: true,
    send: async (body) => { sent.push(body); } });
  let local = 0;
  const actions = { sendMessage: async () => {}, grantSudo: async () => {}, savePermissions: async () => {}, replyHuman: async () => { local++; } };
  // This node's snapshot does not know forge's agent; forge's view decides.
  const { host, mounted } = await mountDialogs(harness, state, actions, snapshot({ online: false, groups: [] }));
  assert.match(host.querySelector('#human-reply-to').textContent, /sender on forge/);
  assert.equal(host.querySelector('#human-reply-attach-btn'), null, 'the peer takes no attachments');
  await harness.input(host.querySelector('#human-reply-body'), 'on it');
  assert.equal(host.querySelector('#human-reply-submit').disabled, false);
  await harness.act(async () => { host.querySelector('#human-reply-submit').click(); await Promise.resolve(); });
  assert.deepEqual(sent, ['on it']);
  assert.equal(local, 0, 'never through this node\'s own reply route');
  assert.equal(state.dialog.value, null);
  await mounted.unmount();
});

const sudoSlugs = [
  { slug: 'agent.stop', description: 'stop agents', countable: true },
  { slug: 'agent.spawn', description: 'spawn anywhere', scope_dims: ['peer'], countable: true },
  { slug: 'groups.members.spawn', description: 'spawn', scope_dims: ['group'], countable: true },
  { slug: 'node.exec', description: 'run commands', risky: true, not_countable_why: 'It runs too often.' },
  { slug: 'permissions.grant', description: 'grant', risky: true },
];

function sudoSnapshot(sudo = []) {
  return {
    ...snapshot({ groups: [], slugs: sudoSlugs }),
    agents: [{ agent_id: 'agt_s', conv_id: 'conv-s', title: 'steward-1', online: true }], sudo,
  };
}

function sudoActions(extra = {}) {
  return {
    sendMessage: async () => {}, replyHuman: async () => {}, savePermissions: async () => {}, grantSudo: async () => {},
    sudoPolicy: async () => ({ max_seconds: 1800, default_seconds: 600, refused: { 'permissions.grant': 'Refused: it hands out permissions itself.' } }),
    ...extra,
  };
}

async function mountSudo(harness, actions, current = sudoSnapshot()) {
  const { createMessageAccessDialogState } = await harness.importDashboardModule('js/message-access-dialog-state.js');
  const state = createMessageAccessDialogState();
  state.openSudoEditor({ agentID: 'agt_s', label: 'steward-1' });
  const mounted = await mountDialogs(harness, state, actions, current);
  await harness.act(async () => { await Promise.resolve(); await Promise.resolve(); });
  return { ...mounted, state };
}

const sudoRow = (host, slug) => host.querySelector(`.perm-row[data-slug="${slug}"]`);
const pickSudo = (harness, host, slug) => harness.act(() => { sudoRow(host, slug).querySelector('[data-effect="grant"]').click(); });

test('the sudo variant is the permission editor: areas, risk, refusal with its reason, and the server\'s limits', async (t) => {
  const harness = await createPreactHarness(t);
  const { host, mounted } = await mountSudo(harness, sudoActions());
  assert.ok(host.querySelector('#perm-edit-modal.show, #perm-edit-modal'), 'the permission editor, not another dialog');
  assert.equal(host.querySelector('#perm-edit-title').textContent.includes('Grant sudo'), true);
  assert.deepEqual([...host.querySelectorAll('.perm-area-head')].map((head) => head.textContent), ['Agents', 'Group members', 'Node and host', 'Permissions']);
  assert.ok(sudoRow(host, 'node.exec').classList.contains('risky'));
  assert.equal(sudoRow(host, 'agent.stop').classList.contains('risky'), false);
  const refused = sudoRow(host, 'permissions.grant');
  assert.equal(refused.querySelector('.perm-tristate'), null);
  assert.match(refused.textContent, /hands out permissions itself/);
  assert.equal(sudoRow(host, 'agent.stop').querySelector('[data-effect="deny"]'), null);
  assert.equal(host.querySelector('#sudo-duration').value, '10m', 'the server default');
  assert.deepEqual([...host.querySelectorAll('.sudo-presets button')].map((button) => button.textContent), ['5m', '15m', '30m'], 'presets stop at the maximum');
  await mounted.unmount();
});

test('the sudo editor\'s Clear selections drops the picks, disables itself, and keeps the filter', async (t) => {
  const harness = await createPreactHarness(t);
  const { host, mounted } = await mountSudo(harness, sudoActions());
  const reset = () => host.querySelector('#perm-edit-reset');
  assert.equal(reset().querySelector('.pe-btn-regular').textContent, 'Clear selections');
  assert.equal(reset().disabled, true, 'nothing picked');
  await pickSudo(harness, host, 'node.exec');
  await harness.input(host.querySelector('#perm-edit-filter'), 'node');
  await harness.act(() => { reset().click(); });
  assert.match(host.querySelector('#sudo-picked').textContent, /Pick at least one/);
  assert.equal(host.querySelector('#perm-edit-filter').value, 'node');
  assert.equal(reset().disabled, true);
  await mounted.unmount();
});

test('granting sends the duration, reason and scope, and keeps the draft when it fails', async (t) => {
  const harness = await createPreactHarness(t);
  const payloads = [];
  const actions = sudoActions({ grantSudo: async (payload) => { payloads.push(payload); if (payloads.length === 1) throw new Error('denied once'); } });
  const { host, mounted, state } = await mountSudo(harness, actions);
  const submit = () => host.querySelector('#perm-edit-submit');
  assert.equal(submit().disabled, true, 'nothing picked yet');
  await pickSudo(harness, host, 'groups.members.spawn');
  await pickSudo(harness, host, 'node.exec');
  assert.match(host.querySelector('#sudo-picked').textContent, /2 picked · 1 risky/);
  await harness.act(() => { sudoRow(host, 'groups.members.spawn').querySelector('.perm-scope-twisty').click(); });
  await harness.input(host.querySelector('.perm-scope-free'), 'dev');
  await harness.act(() => { host.querySelector('.sudo-presets button:nth-child(2)').click(); });
  await harness.input(host.querySelector('#sudo-reason'), 'release');
  assert.equal(submit().textContent, 'Grant 2 for 15m');
  await harness.act(async () => { submit().click(); await Promise.resolve(); });
  assert.equal(host.querySelector('#perm-edit-error').textContent, 'denied once');
  assert.equal(host.querySelector('#sudo-duration').value, '15m');
  await harness.act(async () => { submit().click(); await Promise.resolve(); });
  assert.deepEqual(payloads[1], {
    agentID: 'agt_s', slugs: ['groups.members.spawn', 'node.exec'], duration: '15m', reason: 'release',
    scopes: { 'groups.members.spawn': { group: ['dev'] } }, node: '',
  });
  assert.equal(state.dialog.value, null);
  await mounted.unmount();
});

test('a slug the agent already holds can be scoped, and the row says that narrows it', async (t) => {
  const harness = await createPreactHarness(t);
  const current = { ...sudoSnapshot(), permissions: { defaults: ['groups.members.spawn'], overrides: {} } };
  const { host, mounted } = await mountSudo(harness, sudoActions(), current);
  await pickSudo(harness, host, 'groups.members.spawn');
  const row = () => sudoRow(host, 'groups.members.spawn');
  assert.ok(row().querySelector('button.perm-scope-twisty'));
  assert.equal(row().querySelector('.perm-narrows-held'), null, 'no scope, nothing narrows');
  await harness.act(() => { row().querySelector('.perm-scope-twisty').click(); });
  await harness.input(host.querySelector('.perm-scope-free'), 'dev');
  await harness.act(() => { host.querySelector('.perm-scope-free-add').click(); });
  assert.match(row().querySelector('.perm-narrows-held').textContent, /replaces its own, group and default access/);
  await mounted.unmount();
});

test('a held slug takes no use limit: what it holds is never counted', async (t) => {
  const harness = await createPreactHarness(t);
  const current = { ...sudoSnapshot(), permissions: { defaults: ['groups.members.spawn'], overrides: {} } };
  const { host, mounted } = await mountSudo(harness, sudoActions(), current);
  await pickSudo(harness, host, 'groups.members.spawn');
  assert.match(sudoRow(host, 'groups.members.spawn').querySelector('.perm-uses.none').title, /does not use a count/);
  await mounted.unmount();
});

test('a peer draft still being typed drops the use limit from the grant', async (t) => {
  const harness = await createPreactHarness(t);
  const payloads = [];
  const actions = sudoActions({ grantSudo: async (payload) => { payloads.push(payload); } });
  const { host, mounted } = await mountSudo(harness, actions);
  await pickSudo(harness, host, 'agent.spawn');
  await harness.input(sudoRow(host, 'agent.spawn').querySelector('.perm-uses input'), '3');
  await harness.act(() => { sudoRow(host, 'agent.spawn').querySelector('.perm-scope-twisty').click(); });
  await harness.input(host.querySelector('.perm-scope-free'), 'inst_peer');
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.equal(payloads[0].uses, undefined);
  assert.deepEqual(payloads[0].scopes, { 'agent.spawn': { peer: ['inst_peer'] } });
  await mounted.unmount();
});

test('a peer-scoped grant says why it takes no use limit, and sends none', async (t) => {
  const harness = await createPreactHarness(t);
  const payloads = [];
  const actions = sudoActions({ grantSudo: async (payload) => { payloads.push(payload); } });
  const { host, mounted } = await mountSudo(harness, actions);
  await pickSudo(harness, host, 'agent.spawn');
  const row = () => sudoRow(host, 'agent.spawn');
  await harness.input(row().querySelector('.perm-uses input'), '3');
  await harness.act(() => { row().querySelector('.perm-scope-twisty').click(); });
  await harness.input(host.querySelector('.perm-scope-free'), 'inst_peer');
  await harness.act(() => { host.querySelector('.perm-scope-free-add').click(); });
  assert.equal(row().querySelector('.perm-uses input'), null);
  assert.match(row().querySelector('.perm-uses.none').title, /scoped to a peer/);
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.equal(payloads[0].uses, undefined);
  await mounted.unmount();
});

test('a duration over the maximum or unreadable says so and cannot be granted', async (t) => {
  const harness = await createPreactHarness(t);
  const { host, mounted } = await mountSudo(harness, sudoActions());
  await pickSudo(harness, host, 'agent.stop');
  await harness.input(host.querySelector('#sudo-duration'), '2h');
  assert.match(host.querySelector('#sudo-duration-hint').textContent, /maximum for this agent is 30m/);
  assert.equal(host.querySelector('#perm-edit-submit').disabled, true);
  await harness.input(host.querySelector('#sudo-duration'), 'soon');
  assert.match(host.querySelector('#sudo-duration-hint').textContent, /form like 20m/);
  assert.equal(host.querySelector('#perm-edit-submit').disabled, true);
  await harness.input(host.querySelector('#sudo-duration'), '20m');
  assert.equal(host.querySelector('#perm-edit-submit').disabled, false);
  await mounted.unmount();
});

test('active grants show time left and scope, extend by the chosen duration, and revoke', async (t) => {
  const harness = await createPreactHarness(t);
  const calls = [];
  const actions = sudoActions({
    extendSudo: async (payload) => { calls.push(['extend', payload]); },
    revokeSudo: async (payload) => { calls.push(['revoke', payload]); },
  });
  const inMinutes = (minutes) => new Date(Date.now() + minutes * 60000).toISOString();
  const sudo = [
    { id: 7, agent_id: 'agt_s', slug: 'agent.stop', expires_at: inMinutes(12), scope_json: '{"group":["dev"]}' },
    { id: 8, agent_id: 'agt_s', slug: 'node.exec', expires_at: inMinutes(25) },
    { id: 9, agent_id: 'agt_other', slug: 'agent.stop', expires_at: inMinutes(5) },
  ];
  const { host, mounted } = await mountSudo(harness, actions, sudoSnapshot(sudo));
  const rows = [...host.querySelectorAll('.sudo-active-row')];
  assert.equal(rows.length, 2, 'this agent\'s grants only');
  assert.match(rows[0].textContent, /group=dev/);
  assert.match(rows[0].textContent, /1[12]m\d*s? left|12m left/);
  assert.ok(sudoRow(host, 'agent.stop').querySelector('.perm-sudo-held'), 'an active slug is extended, not granted twice');
  assert.equal(rows[0].querySelector('.sudo-extend').textContent, '+10m');
  assert.equal(rows[1].querySelector('.sudo-extend').disabled, true, '25m + 10m would pass the 30m maximum');
  await harness.act(async () => { rows[0].querySelector('.sudo-extend').click(); await Promise.resolve(); });
  await harness.act(async () => { rows[1].querySelector('.sudo-revoke').click(); await Promise.resolve(); });
  await harness.act(async () => { host.querySelector('#sudo-revoke-all').click(); await Promise.resolve(); });
  assert.deepEqual(calls, [
    ['extend', { ids: [7], duration: '10m', node: '' }],
    ['revoke', { ids: [8], node: '' }],
    ['revoke', { agentID: 'agt_s', node: '' }],
  ]);
  await mounted.unmount();
});

// The test DOM's select.value has no setter: the selected option decides it.
async function chooseAllStewards(harness, select) {
  select.querySelector('option[value="this"]').removeAttribute('selected');
  select.querySelector('option[value="all"]').setAttribute('selected', '');
  await harness.act(() => { select.dispatchEvent(new globalThis.Event('change', { bubbles: true })); });
}

function stewardSnapshot(sudo = []) {
  return {
    ...sudoSnapshot(sudo),
    permissions: { defaults: [], overrides: { 'conv-s': { 'agent.stop': 'grant', 'fleet.read': 'grant', 'node.exec': 'grant' } } },
    slugs: [...sudoSlugs, { slug: 'fleet.read', description: 'read the fleet' }],
    steward_access: { convs: ['conv-s'], config: ['fleet.read'], carried: { 'agent.stop': 'grant' } },
  };
}

async function mountStewardPermissions(harness, actions, current = stewardSnapshot()) {
  const { createMessageAccessDialogState } = await harness.importDashboardModule('js/message-access-dialog-state.js');
  const state = createMessageAccessDialogState();
  state.openAgentPermissions({ conv: 'conv-s', label: 'steward-1' });
  return { ...await mountDialogs(harness, state, actions, current), state };
}

test('a Steward\'s editor names each grant\'s source and offers carrying it over', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const actions = sudoActions({ savePermissions: async (...args) => { saved.push(args); } });
  const { host, mounted } = await mountStewardPermissions(harness, actions);
  assert.match(host.querySelector('#perm-steward-note').textContent, /stored and given to each new Steward/);
  assert.match(sudoRow(host, 'agent.stop').textContent, /all future Stewards/);
  assert.equal(sudoRow(host, 'agent.stop').querySelector('.perm-carry option[value="all"]').selected, true);
  assert.equal(sudoRow(host, 'node.exec').querySelector('.perm-carry option[value="this"]').selected, true, 'the default');
  const config = sudoRow(host, 'fleet.read');
  assert.match(config.textContent, /Reads and Writes/);
  assert.equal(!!config.querySelector('.perm-tristate'), true, 'a level grant can be denied');
  assert.equal(!!config.querySelector('.perm-carry'), false, 'nothing to store until it is denied');
  await harness.act(async () => { config.querySelector('[data-effect="deny"]').click(); });
  assert.equal(!!sudoRow(host, 'fleet.read').querySelector('.perm-carry'), true, 'a deny can be stored');
  await harness.act(async () => { sudoRow(host, 'fleet.read').querySelector('[data-effect="grant"]').click(); });
  const carry = sudoRow(host, 'node.exec').querySelector('.perm-carry');
  await chooseAllStewards(harness, carry);
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved[0][4], { 'node.exec': true }, 'only what the operator changed');
  await mounted.unmount();
});

test('an ordinary agent gets no carry choice, whatever its name', async (t) => {
  const harness = await createPreactHarness(t);
  const current = { ...stewardSnapshot(), steward_access: { convs: ['conv-real'], config: [], carried: {}, not_carryable: [] } };
  const { host, mounted } = await mountStewardPermissions(harness, sudoActions(), current);
  assert.equal(!!host.querySelector('.perm-carry'), false);
  assert.equal(!!host.querySelector('#perm-steward-note'), false);
  await mounted.unmount();
});

test('sudo on a Steward stays instance-only unless the operator stores it', async (t) => {
  const harness = await createPreactHarness(t);
  const payloads = [];
  const actions = sudoActions({ grantSudo: async (payload) => { payloads.push(payload); } });
  const expires = new Date(Date.now() + 600000).toISOString();
  const held = [{ id: 7, agent_id: 'agt_s', slug: 'fleet.read', expires_at: expires, carried: true }];
  const { host, mounted } = await mountSudo(harness, actions, stewardSnapshot(held));
  assert.match(host.querySelector('.sudo-active-row').textContent, /all future Stewards: revoking ends it for them too/);
  assert.match(host.querySelector('.sudo-revoke').title, /all future Stewards/);
  assert.match(host.querySelector('#sudo-revoke-all').title, /stored for future Stewards/);
  await pickSudo(harness, host, 'agent.stop');
  await pickSudo(harness, host, 'node.exec');
  const carry = sudoRow(host, 'node.exec').querySelector('.perm-carry');
  assert.equal(carry.querySelector('option[value="this"]').selected, true);
  await chooseAllStewards(harness, carry);
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(payloads[0].carry, ['node.exec']);
  await mounted.unmount();
});

test('a countable slug takes a use limit, sent with the grant and shown beside the time left', async (t) => {
  const harness = await createPreactHarness(t);
  const payloads = [];
  const actions = sudoActions({ grantSudo: async (payload) => { payloads.push(payload); } });
  const expires = new Date(Date.now() + 600000).toISOString();
  const held = [{ id: 7, agent_id: 'agt_s', slug: 'agent.stop', expires_at: expires, uses_limit: 5, uses_left: 2 }];
  const { host, mounted } = await mountSudo(harness, actions, sudoSnapshot(held));
  assert.match(host.querySelector('.sudo-active-row').textContent, /2 of 5 uses left/);

  await pickSudo(harness, host, 'node.exec');
  await pickSudo(harness, host, 'groups.members.spawn');
  assert.equal(sudoRow(host, 'node.exec').querySelector('.perm-uses input'), null, 'nothing to count');
  assert.equal(sudoRow(host, 'node.exec').querySelector('.perm-uses.none').title, 'It runs too often.');
  const uses = () => sudoRow(host, 'groups.members.spawn').querySelector('.perm-uses input');
  await harness.input(uses(), '0');
  assert.equal(host.querySelector('#perm-edit-submit').disabled, true);
  assert.match(host.querySelector('#sudo-picked').textContent, /whole number from 1 to 1000/);
  await harness.input(uses(), '3');
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(payloads[0].uses, { 'groups.members.spawn': 3 });
  await mounted.unmount();
});

test('the bundle limit reaches every countable pick, and a row can differ from it', async (t) => {
  const harness = await createPreactHarness(t);
  const payloads = [];
  const actions = sudoActions({ grantSudo: async (payload) => { payloads.push(payload); } });
  const { host, mounted } = await mountSudo(harness, actions, sudoSnapshot([]));
  await pickSudo(harness, host, 'node.exec');
  await pickSudo(harness, host, 'groups.members.spawn');
  await pickSudo(harness, host, 'agent.stop');
  await harness.input(host.querySelector('#sudo-uses'), '5');
  assert.match(host.querySelector('#sudo-uses-hint').textContent, /1 picked takes no limit/);
  assert.equal(sudoRow(host, 'agent.stop').querySelector('.perm-uses input').placeholder, '5');
  await harness.input(sudoRow(host, 'agent.stop').querySelector('.perm-uses input'), '2');
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(payloads[0].uses, { 'groups.members.spawn': 5, 'agent.stop': 2 });
  await mounted.unmount();
});

test('an older node without the policy route leaves the editor on its defaults', async (t) => {
  const harness = await createPreactHarness(t);
  const { host, mounted } = await mountSudo(harness, sudoActions({ sudoPolicy: async () => { throw new Error('405'); } }));
  assert.equal(host.querySelector('#sudo-duration').value, '5m');
  assert.equal(sudoRow(host, 'permissions.grant').querySelector('.perm-tristate'), null);
  await mounted.unmount();
});

test('permission draft survives live registry/source changes and submits the reconciled full map', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogState } = await harness.importDashboardModule('js/message-access-dialog-state.js');
  const state = createMessageAccessDialogState();
  state.openAgentPermissions({ conv: 'conv-s', label: 'sender' });
  const saved = [];
  const actions = {
    sendMessage: async () => {}, replyHuman: async () => {}, grantSudo: async () => {},
    savePermissions: async (descriptor, selection) => { saved.push({ descriptor, selection }); },
  };
  const initial = snapshot({
    members: [{ ...member('sender'), agent_id: 'agt_sender', conv_id: 'conv-s' }],
    slugs: [{ slug: 'groups.members.spawn', description: 'spawn', owner_implied: false }],
    permissions: { defaults: [], overrides: { 'conv-s': { 'groups.members.spawn': 'deny' } } },
  });
  const { host, mounted, rerender } = await mountDialogs(harness, state, actions, initial);
  const spawnRow = host.querySelector('[data-slug="groups.members.spawn"]');
  await harness.act(() => { spawnRow.querySelector('[data-effect="grant"]').click(); });
  const updated = snapshot({
    members: [{ ...member('sender'), agent_id: 'agt_sender', conv_id: 'conv-s' }],
    slugs: [
      { slug: 'groups.members.spawn', description: 'spawn', owner_implied: false },
      { slug: 'self.rename', description: 'rename', owner_implied: false },
    ],
    permissions: { defaults: ['self.rename'], overrides: { 'conv-s': { 'groups.members.spawn': 'deny' } } },
  });
  updated.groups[0].permissions = ['groups.members.spawn'];
  await rerender(updated);
  assert.equal(host.querySelector('[data-slug="groups.members.spawn"] [data-effect="grant"]').classList.contains('active'), true,
    'live snapshot does not reset the edited tri-state');
  assert.equal(host.querySelector('[data-slug="self.rename"] [data-effect="default"]').classList.contains('active'), true,
    'new registry slug reconciles at Default without moving the baseline');
  assert.match(host.querySelector('[data-slug="self.rename"] .perm-row-eff').textContent, /global default/);
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved[0].selection, { 'groups.members.spawn': 'grant', 'self.rename': 'default' });
  await mounted.unmount();
});

test('dirty backdrop confirms before close while a clean prefill does not prompt', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogState } = await harness.importDashboardModule('js/message-access-dialog-state.js');
  const state = createMessageAccessDialogState();
  state.openMessage({ from: 'agt_sender', target: 'agt_a' });
  let allow = false;
  let confirms = 0;
  const confirmDiscard = async () => { confirms++; return allow; };
  const actions = { sendMessage: async () => {}, replyHuman: async () => {}, grantSudo: async () => {}, savePermissions: async () => {} };
  const { host, mounted } = await mountDialogs(harness, state, actions, snapshot({ groups: [] }), confirmDiscard);
  const overlay = host.querySelector('#message-create-modal');
  await harness.act(async () => { harness.fireEvent(overlay, 'mousedown'); await Promise.resolve(); });
  assert.equal(confirms, 0, 'authoritative launch prefill is the clean baseline');
  assert.equal(state.dialog.value, null);

  state.openMessage({ from: 'agt_sender', target: 'agt_a' });
  await harness.act(() => Promise.resolve());
  await harness.input(host.querySelector('#message-create-body'), 'dirty');
  const dirtyOverlay = host.querySelector('#message-create-modal');
  await harness.act(async () => { harness.fireEvent(dirtyOverlay, 'mousedown'); await Promise.resolve(); });
  assert.equal(confirms, 1);
  assert.notEqual(state.dialog.value, null);
  allow = true;
  await harness.act(async () => { harness.fireEvent(dirtyOverlay, 'mousedown'); await Promise.resolve(); });
  assert.equal(state.dialog.value, null);
  await mounted.unmount();
});

test('operator composer keeps target and attachment snapshot atomic with single-flight retry state', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogState } = await harness.importDashboardModule('js/message-access-dialog-state.js');
  const state = createMessageAccessDialogState();
  state.openOperatorMessage({ agent: 'agt_original', label: 'worker' });
  const pending = deferred();
  const drafts = [];
  let attempts = 0;
  const actions = {
    sendOperatorMessage: (draft) => {
      attempts++;
      drafts.push(draft);
      return attempts === 1 ? pending.promise : Promise.resolve();
    },
  };
  const { host, mounted } = await mountDialogs(harness, state, actions, snapshot({ groups: [] }));
  const body = host.querySelector('#operator-message-body');
  await harness.act(() => Promise.resolve());
  assertSameNode(harness.document.activeElement, body, 'the controlled body owns initial focus');
  await harness.input(body, 'queued body');
  await harness.input(host.querySelector('#operator-message-subject'), 'subject');
  const file = { name: 'proof.txt', size: 5 };
  const fileInput = host.querySelector('#operator-message-attach-input');
  Object.defineProperty(fileInput, 'files', { configurable: true, value: [file] });
  await harness.act(() => harness.fireEvent(fileInput, 'change'));
  assert.equal(host.querySelectorAll('#operator-message-attachments-list li').length, 1);

  await harness.act(() => { host.querySelector('#operator-message-submit').click(); });
  assert.equal(attempts, 1);
  assert.equal(host.querySelector('#operator-message-submit').disabled, true);
  assert.equal(body.value, 'queued body');
  assert.deepEqual(drafts[0], {
    to: 'agt_original', subject: 'subject', body: 'queued body', files: [file],
  });
  assert.equal(Object.isFrozen(drafts[0]), true);
  assert.equal(Object.isFrozen(drafts[0].files), true);
  await harness.act(() => { host.querySelector('#operator-message-submit').click(); });
  assert.equal(attempts, 1, 'busy submit cannot enqueue a duplicate');
  pending.reject(new Error('server refused once'));
  await harness.act(async () => { try { await pending.promise; } catch (_) {} });
  assert.equal(host.querySelector('#operator-message-error').textContent, 'server refused once');
  assert.equal(body.value, 'queued body', 'server errors preserve the exact draft for retry');
  await harness.act(async () => { host.querySelector('#operator-message-submit').click(); await Promise.resolve(); });
  assert.equal(attempts, 2);
  assert.equal(state.dialog.value, null);
  await mounted.unmount();
});

test('all-live announcement composer has broadcast copy and freezes the explicit fan-out mode', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogState } = await harness.importDashboardModule('js/message-access-dialog-state.js');
  const state = createMessageAccessDialogState();
  state.openOperatorMessage({ allLive: true, label: 'all live agents (currently 3)' });
  const drafts = [];
  const actions = { sendOperatorMessage: async (draft) => { drafts.push(draft); } };
  const { host, mounted } = await mountDialogs(harness, state, actions, snapshot({ groups: [] }));
  assert.equal(host.querySelector('#operator-message-title').textContent.includes('Announce to all live agents'), true);
  assert.equal(host.querySelector('#operator-message-to').textContent, 'all live agents (currently 3)');
  assert.equal(host.querySelector('#operator-message-attach-input'), null);
  await harness.input(host.querySelector('#operator-message-body'), 'attention everyone');
  await harness.act(async () => { host.querySelector('#operator-message-submit').click(); await Promise.resolve(); });
  assert.deepEqual(drafts, [{
    to: '', subject: '', body: 'attention everyone', files: [], allLive: true,
  }]);
  assert.equal(Object.isFrozen(drafts[0]), true);
  assert.equal(state.dialog.value, null);
  await mounted.unmount();
});

test('operator Cancel uses dirty confirmation and deferred focus restore yields to a newer dialog', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogState } = await harness.importDashboardModule('js/message-access-dialog-state.js');
  const { hasShownOverlay } = await harness.importDashboardModule('js/overlay-stack.js');
  const state = createMessageAccessDialogState({ canRestoreFocus: () => !hasShownOverlay() });
  let restores = 0;
  let allow = false;
  let confirms = 0;
  state.openOperatorMessage({ agent: 'agt_worker', label: 'worker', restoreFocus: () => { restores++; } });
  const actions = { sendOperatorMessage: async () => {} };
  const { host, mounted } = await mountDialogs(
    harness, state, actions, snapshot({ groups: [] }),
    async () => { confirms++; return allow; },
  );
  await harness.input(host.querySelector('#operator-message-body'), 'dirty');
  await harness.act(async () => { host.querySelector('#operator-message-cancel').click(); await Promise.resolve(); });
  assert.equal(confirms, 1, 'Cancel shares the dirty Escape/backdrop transaction');
  assert.equal(state.dialog.value?.kind, 'operator-message');
  assert.equal(restores, 0);

  allow = true;
  await harness.act(async () => { host.querySelector('#operator-message-cancel').click(); await Promise.resolve(); });
  assert.equal(state.dialog.value, null);
  state.openHumanReply({ id: '9', agent: 'agt_other', conv: 'conv-other', label: 'other' });
  await harness.act(() => Promise.resolve());
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(restores, 0, 'a newer message/access dialog keeps focus ownership');
  assert.ok(host.querySelector('#human-reply-modal'));
  state.close();

  state.openOperatorMessage({ agent: 'agt_worker', label: 'worker', restoreFocus: () => { restores++; } });
  state.close();
  const otherFeature = harness.document.body.appendChild(harness.document.createElement('div'));
  otherFeature.className = 'manage-overlay show';
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(restores, 0, 'a newer non-message overlay keeps focus ownership');
  otherFeature.remove();
  await mounted.unmount();
});

test('terminal message saved prompt inserts into the editable draft without sending', async (t) => {
  const harness = await createPreactHarness(t);
  t.mock.method(globalThis, 'fetch', async () => ({ ok: true, json: async () => [{ name: 'review', body: 'Please review.' }] }));
  const { createMessageAccessDialogState } = await harness.importDashboardModule('js/message-access-dialog-state.js');
  const state = createMessageAccessDialogState();
  state.openOperatorMessage({ agent: 'agt_worker', label: 'worker' });
  const sent = [];
  const { host, mounted } = await mountDialogs(harness, state, { sendOperatorMessage: async (draft) => sent.push(draft) }, snapshot());
  await harness.act(async () => { for (let i = 0; i < 8; i++) await Promise.resolve(); });
  const body = host.querySelector('#operator-message-body');
  await harness.input(body, 'Task: ');
  body.selectionStart = 6; body.selectionEnd = 6;
  const picker = host.querySelector('[aria-label="Insert saved prompt"]');
  Object.defineProperty(picker, 'value', { configurable: true, writable: true, value: 'review' });
  await harness.act(() => harness.fireEvent(picker, 'change'));
  assert.equal(host.querySelector('#operator-message-body'), body);
  assert.equal(body.value, 'Task: Please review.');
  assert.equal(sent.length, 0);
  await harness.input(body, 'Task: Please review the diff.');
  await harness.act(async () => { host.querySelector('#operator-message-submit').click(); await Promise.resolve(); });
  assert.equal(sent[0].body, 'Task: Please review the diff.');
  await mounted.unmount();
});

test("a restricted peer's group gets the regular dialog: what it withholds is locked with the grant, and each ticked member gets its own message", async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogState } = await harness.importDashboardModule('js/message-access-dialog-state.js');
  const state = createMessageAccessDialogState();
  state.openMessage({ targetMode: 'group', groupName: 'team' }, '', 'forge');
  const sent = [];
  const actions = {
    sendMessage: async () => { throw new Error('a restricted peer refuses a group send'); },
    sendDirectMessages: async (recipients, message, node) => {
      sent.push([recipients, message, node]);
      return { sent: 1, failed: sent.length === 1 ? [{ to: 'agt_bob', error: 'not shared' }] : [] };
    },
  };
  const peer = { ...snapshot({ members: [member('ada'), member('bob')] }), peer_view: { level: 'restricted', included: ['messaging'] } };
  const { host, mounted } = await mountDialogs(harness, state, actions, peer);
  assert.match(host.querySelector('#message-create-title').textContent, /on forge/, 'the title names the node');
  assert.equal(host.querySelector('#message-create-from').disabled, true);
  assert.match(host.querySelector('#message-create-from-lock').getAttribute('data-why'), /forge delivers it from you, the operator.*unrestricted trust/);
  assert.equal(host.querySelector('#message-create-role').disabled, true);
  assert.match(host.querySelector('#message-create-role-lock').getAttribute('aria-label'), /^Locked: /);
  await harness.input(host.querySelector('#message-create-body'), 'status?');
  await harness.act(() => host.querySelector('#message-create-submit').click());
  await harness.act(() => new Promise((r) => setTimeout(r, 10)));
  assert.deepEqual(sent, [[['agt_ada', 'agt_bob'], { subject: '', body: 'status?' }, '']]);
  // A refusal keeps the draft, ticked for exactly the member it missed.
  assert.match(host.querySelector('#message-create-error').textContent, /bob: not shared/);
  assert.equal(host.querySelector('#message-create-body').value, 'status?');
  await harness.act(() => host.querySelector('#message-create-submit').click());
  await harness.act(() => new Promise((r) => setTimeout(r, 10)));
  assert.deepEqual(sent[1][0], ['agt_bob'], 'the retry reaches only who was refused');
  assert.equal(host.querySelector('#message-create-modal'), null, 'sent and closed');
  await mounted.unmount();
});

test('this node and an unrestricted peer keep every field open, with no lock', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogState } = await harness.importDashboardModule('js/message-access-dialog-state.js');
  const state = createMessageAccessDialogState();
  state.openMessage({ targetMode: 'group', groupName: 'team' });
  const actions = { sendMessage: async () => {} };
  const unrestricted = { ...snapshot({ members: [member('ada')] }), peer_view: { level: 'unrestricted' } };
  const { host, mounted } = await mountDialogs(harness, state, actions, unrestricted);
  assert.equal(host.querySelector('#message-create-from').disabled, false);
  assert.equal(host.querySelector('.lock-why'), null);
  await mounted.unmount();
});

test('the future-Stewards editor shows what a new Steward would hold and stores denies', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogState } = await harness.importDashboardModule('js/message-access-dialog-state.js');
  const state = createMessageAccessDialogState();
  const saves = [];
  const actions = sudoActions({ savePermissions: async (descriptor, selection) => { saves.push(selection); } });
  const { host, mounted } = await mountDialogs(harness, state, actions, stewardSnapshot());
  await harness.act(async () => {
    state.openBufferedPermissions({ stewardLevels: ['fleet.read'], overrides: { 'node.exec': 'grant' } });
  });
  assert.match(host.querySelector('#perm-edit-subtitle').textContent, /Future Stewards/);
  assert.match(sudoRow(host, 'fleet.read').textContent, /✓ Reads and Writes/);
  assert.match(sudoRow(host, 'node.exec').textContent, /✓ stored/);
  await harness.act(async () => { sudoRow(host, 'fleet.read').querySelector('[data-effect="deny"]').click(); });
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.equal(saves[0]['fleet.read'], 'deny');
  await mounted.unmount();
});
