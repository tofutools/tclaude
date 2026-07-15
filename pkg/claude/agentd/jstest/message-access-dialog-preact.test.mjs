import test from 'node:test';
import assert from 'node:assert/strict';
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
  assert.equal(host.querySelector('#message-create-modal'), parent, 'opening the keyed child does not recreate its parent');
  assert.equal(host.querySelector('#message-create-body'), body);
  assert.equal(body.value, 'draft survives');

  await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }));
  assert.equal(host.querySelector('#cron-pick-target-modal'), null);
  assert.notEqual(host.querySelector('#message-create-modal'), null, 'stacked Escape closes only the chooser');
  assert.equal(host.querySelector('#message-create-body').value, 'draft survives');
  assert.equal(harness.document.activeElement, pickerButton, 'child teardown restores its invoker');
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
  const cronTargetHost = harness.document.body.appendChild(harness.document.createElement('div'));
  const cleanups = [];
  const actions = { sendMessage: async () => {}, replyHuman: async () => {}, grantSudo: async () => {}, savePermissions: async () => {} };
  await harness.act(() => island.mountMessageAccessDialogIsland({
    dialogHost, cronTargetHost, state, actions, snapshot: snapshotSignal,
    confirmDiscard: async () => true, registerCleanup: (cleanup) => cleanups.push(cleanup),
  }));
  assert.ok(cronTargetHost.querySelector('#cron-create-target-picker'));
  await harness.act(() => { controller.openMessageCreateModal({ from: 'agt_sender', targetMode: 'group', groupName: 'team' }); });
  const body = dialogHost.querySelector('#message-create-body');
  await harness.input(body, 'retained by subscription');
  await harness.act(() => { snapshotSignal.value = snapshot({ members: [member('a'), member('b')] }); });
  assert.equal(dialogHost.querySelector('#message-create-body'), body);
  assert.equal(body.value, 'retained by subscription');
  assert.equal(dialogHost.querySelector('#message-create-members-count').textContent, '2 of 2 selected');

  const canceled = controller.pickAgent({ title: 'Pick sender' });
  await harness.act(() => Promise.resolve());
  assert.ok(dialogHost.querySelector('#cron-pick-target-modal'));
  await harness.act(() => { cleanups.reverse().forEach((cleanup) => cleanup()); });
  assert.equal(await canceled, '', 'island teardown cancels an outstanding chooser promise');
  assert.equal(dialogHost.childElementCount, 0);
  assert.equal(cronTargetHost.childElementCount, 0);
  dialogHost.remove();
  cronTargetHost.remove();
});

test('partial island initialization rolls back the controller and permits a clean remount', async (t) => {
  const harness = await createPreactHarness(t);
  const [island, stateModule, controller] = await Promise.all([
    harness.importDashboardModule('js/message-access-dialog-island.js'),
    harness.importDashboardModule('js/message-access-dialog-state.js'),
    harness.importDashboardModule('js/message-access-dialog-controller.js'),
  ]);
  const dialogHost = harness.document.body.appendChild(harness.document.createElement('div'));
  const cronTargetHost = harness.document.body.appendChild(harness.document.createElement('div'));
  const actions = { sendMessage: async () => {}, replyHuman: async () => {}, grantSudo: async () => {}, savePermissions: async () => {} };
  const failedCleanups = [];
  assert.throws(() => island.mountMessageAccessDialogIsland({
    dialogHost,
    cronTargetHost,
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
  assert.equal(cronTargetHost.childElementCount, 0);
  assert.throws(() => controller.openMessageCreateModal(), /dialogs are not ready/,
    'failed initialization releases the global launcher seam');

  const cleanups = [];
  const state = stateModule.createMessageAccessDialogState();
  await harness.act(() => island.mountMessageAccessDialogIsland({
    dialogHost,
    cronTargetHost,
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
  cronTargetHost.remove();
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
  await harness.input(body, 'hello live roster');
  await harness.input(host.querySelector('#message-create-subject'), 'status');
  await harness.input(host.querySelector('#message-create-role'), ' reviewer ');

  const next = snapshot({ members: [member('a'), member('b'), member('c')] });
  await rerender(next);
  assert.equal(host.querySelector('#message-create-body'), body, 'snapshot reconciliation keeps the keyed draft node');
  assert.equal(body.value, 'hello live roster');
  assert.equal(host.querySelector('#message-create-members-count').textContent, '3 of 3 selected');
  assert.equal(host.querySelectorAll('#message-create-members input[type="checkbox"]:checked').length, 3,
    'untouched all follows a newcomer');

  await harness.act(async () => { host.querySelector('#message-create-submit').click(); await Promise.resolve(); });
  assert.deepEqual(sent, [{
    from: 'agt_sender', to: 'group:team', subject: 'status', body: 'hello live roster', role: 'reviewer',
  }], 'untouched all omits members so the daemon reads the live roster');
  assert.equal(state.dialog.value, null);
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

test('sudo selection excludes blocklisted slugs and preserves a failed draft for retry', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogState } = await harness.importDashboardModule('js/message-access-dialog-state.js');
  const state = createMessageAccessDialogState();
  state.openSudoGrant({ conv: 'conv-s' });
  const payloads = [];
  const actions = {
    sendMessage: async () => {}, replyHuman: async () => {}, savePermissions: async () => {},
    grantSudo: async (payload) => { payloads.push(payload); if (payloads.length === 1) throw new Error('denied once'); },
  };
  const slugs = [
    { slug: 'groups.spawn', description: 'spawn' },
    { slug: 'permissions.grant', description: 'grant' },
    { slug: 'permissions.revoke', description: 'revoke' },
  ];
  const { host, mounted } = await mountDialogs(harness, state, actions, snapshot({ groups: [], slugs }));
  const blocked = [...host.querySelectorAll('#sudo-grant-slugs input')].filter((input) => input.disabled);
  assert.deepEqual(blocked.map((input) => input.value), ['permissions.grant', 'permissions.revoke']);
  await harness.act(() => { host.querySelector('#sudo-grant-select-all').click(); });
  await harness.input(host.querySelector('#sudo-grant-duration'), '30m');
  await harness.input(host.querySelector('#sudo-grant-reason'), 'release');
  await harness.act(async () => { host.querySelector('#sudo-grant-submit').click(); await Promise.resolve(); });
  assert.equal(host.querySelector('#sudo-grant-error').textContent, 'denied once');
  assert.equal(host.querySelector('#sudo-grant-duration').value, '30m');
  await harness.act(async () => { host.querySelector('#sudo-grant-submit').click(); await Promise.resolve(); });
  assert.deepEqual(payloads[1], { conv: 'conv-s', slugs: ['groups.spawn'], duration: '30m', reason: 'release' });
  assert.equal(state.dialog.value, null);
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
    slugs: [{ slug: 'groups.spawn', description: 'spawn', owner_implied: false }],
    permissions: { defaults: [], overrides: { 'conv-s': { 'groups.spawn': 'deny' } } },
  });
  const { host, mounted, rerender } = await mountDialogs(harness, state, actions, initial);
  const spawnRow = host.querySelector('[data-slug="groups.spawn"]');
  await harness.act(() => { spawnRow.querySelector('[data-effect="grant"]').click(); });
  const updated = snapshot({
    members: [{ ...member('sender'), agent_id: 'agt_sender', conv_id: 'conv-s' }],
    slugs: [
      { slug: 'groups.spawn', description: 'spawn', owner_implied: false },
      { slug: 'self.rename', description: 'rename', owner_implied: false },
    ],
    permissions: { defaults: ['self.rename'], overrides: { 'conv-s': { 'groups.spawn': 'deny' } } },
  });
  updated.groups[0].permissions = ['groups.spawn'];
  await rerender(updated);
  assert.equal(host.querySelector('[data-slug="groups.spawn"] [data-effect="grant"]').classList.contains('active'), true,
    'live snapshot does not reset the edited tri-state');
  assert.equal(host.querySelector('[data-slug="self.rename"] [data-effect="default"]').classList.contains('active'), true,
    'new registry slug reconciles at Default without moving the baseline');
  assert.match(host.querySelector('[data-slug="self.rename"] .perm-row-eff').textContent, /global default/);
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved[0].selection, { 'groups.spawn': 'grant', 'self.rename': 'default' });
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
