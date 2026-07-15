import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

function escape(harness) {
  const event = new harness.window.Event('keydown', { bubbles: true });
  Object.defineProperty(event, 'key', { value: 'Escape' });
  harness.document.dispatchEvent(event);
}

function checkboxOn(input) {
  return typeof input.checked === 'boolean' ? input.checked : input.hasAttribute('checked');
}

const alpha = {
  agent_id: 'agt_alpha', conv_id: 'alpha-1111-2222-3333-444444444444',
  title: 'Alpha worker', online: true, state: { status: 'idle' }, role: 'builder',
};
const beta = {
  agent_id: 'agt_beta', conv_id: 'beta-1111-2222-3333-444444444444',
  title: 'Beta worker', online: false, role: 'reviewer',
};
const gamma = {
  agent_id: '', conv_id: 'gamma-1111-2222-3333-444444444444',
  title: 'Gamma worker', online: true, state: { status: 'working' },
};

function deleteSnapshot() {
  return {
    groups: [
      { name: 'root', members: [beta] },
      { name: 'child', parent: 'root', members: [alpha, beta, gamma] },
      { name: 'nested-peer', parent: 'root', members: [gamma] },
      { name: 'other-root', members: [beta] },
    ],
  };
}

function requestFor(group, members, memberCount = members.length) {
  return Object.freeze({
    group,
    memberCount,
    retireMembers: Object.freeze(members.map((member) => Object.freeze({ ...member }))),
  });
}

async function openDeleteGroup(t, options = {}) {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, controller, island] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-controller.js'),
    harness.importDashboardModule('js/transaction-dialog-island.js'),
  ]);
  const state = createTransactionDialogState();
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const opener = harness.document.body.appendChild(harness.document.createElement('button'));
  opener.id = 'delete-group-opener';
  opener.focus();
  const actions = {
    close: state.close,
    deleteGroupPlan: async () => ({ ok: true, retired: 0, detached: 0 }),
    finishDeleteGroup: async (result) => {
      state.handoff();
      state.finish(result);
      return result;
    },
    ...options.actions,
  };
  await harness.mount(harness.html`
    <${island.TransactionDialogApp}
      state=${state}
      actions=${actions}
      confirmDiscard=${options.confirmDiscard || (async () => true)}
    />
  `, host);
  const descriptor = controller.buildDeleteGroupDescriptor(
    options.snapshot || deleteSnapshot(), options.group || 'child',
  );
  let pending;
  await harness.act(() => { pending = state.open(descriptor); });
  await harness.act(() => Promise.resolve());
  return { harness, state, host, opener, actions, descriptor, pending };
}

test('delete-group launcher freezes nested and multiple-group membership with stable defaults', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, controller] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-controller.js'),
  ]);
  const state = createTransactionDialogState();
  const unregister = controller.registerTransactionDialogController(state);
  const snapshot = deleteSnapshot();
  const pending = controller.openDeleteGroupDialog(snapshot, 'child');
  const descriptor = state.dialog.value.descriptor;

  assert.equal(descriptor.kind, 'delete-group');
  assert.equal(descriptor.group, 'child');
  assert.equal(descriptor.parent, 'root', 'the target nested position is part of the frozen plan');
  assert.deepEqual(descriptor.members.map((member) => member.selector), [
    alpha.agent_id, beta.agent_id, gamma.conv_id,
  ]);
  assert.deepEqual(descriptor.members.map((member) => member.defaultRetire), [true, false, false]);
  assert.deepEqual(descriptor.members[1].memberships, [
    { name: 'root', parent: '' },
    { name: 'child', parent: 'root' },
    { name: 'other-root', parent: '' },
  ]);
  assert.deepEqual(descriptor.members[2].otherGroups, [
    { name: 'nested-peer', parent: 'root' },
  ]);
  assert.ok(Object.isFrozen(descriptor));
  assert.ok(Object.isFrozen(descriptor.members));
  assert.ok(Object.isFrozen(descriptor.members[1].memberships));

  snapshot.groups[0].name = 'mutated-root';
  snapshot.groups[1].members.splice(0);
  snapshot.groups.push({ name: 'late', members: [alpha] });
  assert.equal(descriptor.members.length, 3);
  assert.equal(descriptor.members[1].memberships[0].name, 'root');
  assert.equal(descriptor.members[0].defaultRetire, true);

  state.close();
  await pending;
  unregister();
});

test('delete-group renderer preserves detach defaults, explicit selection, and retire toggle choices', async (t) => {
  const first = deferred();
  const second = deferred();
  const submitted = [];
  const opened = await openDeleteGroup(t, {
    actions: {
      deleteGroupPlan: (request) => {
        submitted.push(request);
        return submitted.length === 1 ? first.promise : second.promise;
      },
      finishDeleteGroup: async (result) => {
        opened.state.handoff();
        opened.state.finish(result);
        return result;
      },
    },
  });
  const { harness, host, opener } = opened;
  const rows = [...host.querySelectorAll('#delete-group-list input[data-agent]')];
  assert.equal(rows.length, 3);
  assert.deepEqual(rows.map(checkboxOn), [true, false, false]);
  assert.equal(host.querySelector('#delete-group-count .theme-copy-regular').textContent,
    '3 agents: 1 to retire, 2 detach');
  assert.equal(host.querySelector('#delete-group-count .theme-copy-wizard').textContent,
    '3 familiars: 1 to banish, 2 detach');
  assert.match(rows[1].closest('.cleanup-row').textContent, /detach only/);
  assert.match(rows[1].closest('.cleanup-row').textContent, /root, other-root/);
  assert.equal(harness.document.activeElement.id, 'delete-group-submit');

  rows[1].checked = true;
  await harness.act(() => harness.fireEvent(rows[1], 'change'));
  assert.equal(host.querySelector('#delete-group-count .theme-copy-regular').textContent,
    '3 agents: 2 to retire, 1 detach');
  assert.match(rows[1].closest('.cleanup-row').textContent, /explicitly included/);

  const retire = host.querySelector('#delete-group-retire');
  retire.checked = false;
  await harness.act(() => harness.fireEvent(retire, 'change'));
  assert.deepEqual(
    [...host.querySelectorAll('#delete-group-list input[data-agent]')]
      .map(checkboxOn),
    [false, false, false],
  );
  assert.equal(host.querySelector('#delete-group-count .theme-copy-regular').textContent,
    '3 agents: 0 to retire, 3 detach');
  const retireAgain = host.querySelector('#delete-group-retire');
  retireAgain.checked = true;
  await harness.act(() => harness.fireEvent(retireAgain, 'change'));
  assert.deepEqual(
    [...host.querySelectorAll('#delete-group-list input[data-agent]')]
      .map(checkboxOn),
    [true, true, false],
    'turning retirement back on restores the explicit selection',
  );

  host.querySelector('#delete-group-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(submitted[0].retireMembers.map((member) => member.selector), [
    alpha.agent_id, beta.agent_id,
  ]);
  assert.ok(Object.isFrozen(submitted[0]));
  assert.ok(Object.isFrozen(submitted[0].retireMembers));
  assert.equal(host.querySelector('#delete-group-submit').getAttribute('aria-busy'), 'true');
  assert.equal(host.querySelector('#delete-group-cancel').disabled, true);

  escape(harness);
  host.querySelector('#delete-group-modal').click();
  await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#delete-group-modal'), 'Escape/backdrop cannot dismiss a busy delete');

  first.reject(Object.assign(new Error('offline'), { phase: 'delete', network: true }));
  await harness.act(() => first.promise.catch(() => {}));
  assert.equal(host.querySelector('#delete-group-error .theme-copy-regular').textContent,
    'delete failed: offline');
  assert.equal(host.querySelector('#delete-group-error .theme-copy-wizard').textContent,
    'disband failed: offline');
  assert.equal(host.querySelector('#delete-group-retire').disabled, true,
    'the submitted plan stays immutable for retry');

  host.querySelector('#delete-group-submit').click();
  await harness.act(() => Promise.resolve());
  assert.equal(submitted[1], submitted[0], 'retry reuses the exact frozen request identity');
  second.resolve({ ok: true, retired: 2, detached: 1 });
  await harness.act(() => second.promise);
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#delete-group-modal'), null);
  assert.equal(harness.document.activeElement, opener, 'successful completion restores opener focus');
  assert.deepEqual(await opened.pending, { ok: true, retired: 2, detached: 1 });
});

test('delete-group actions preserve exact retire/delete payloads and the phase barrier', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, { createTransactionDialogActions }] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-actions.js'),
  ]);
  const state = createTransactionDialogState();
  const requests = [];
  let refreshes = 0;
  const notices = [];
  const actions = createTransactionDialogActions({
    state,
    fetchImpl: async (url, init) => {
      requests.push([url, init]);
      if (url.endsWith('/retire')) {
        return new Response(JSON.stringify({
          members: [{ ...alpha, action: 'retired' }],
        }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      }
      assert.equal(requests.length, 2, 'DELETE cannot cross the retire response barrier');
      return new Response(null, { status: 204 });
    },
    refresh: async () => { refreshes += 1; },
    notify: (message) => { notices.push(message); },
    words: (_plain, wizard) => wizard,
  });
  const request = requestFor('child group', [{
    selector: alpha.agent_id, agent_id: alpha.agent_id, conv_id: alpha.conv_id,
  }], 3);
  const result = await actions.deleteGroupPlan(request);
  assert.deepEqual(result, { ok: true, group: 'child group', retired: 1, detached: 2 });
  assert.equal(requests[0][0], '/api/groups/child%20group/retire');
  assert.equal(requests[0][1].method, 'POST');
  assert.equal(requests[0][1].credentials, 'same-origin');
  assert.deepEqual(JSON.parse(requests[0][1].body), {
    convs: [alpha.agent_id], shutdown: true, delete_worktree: false,
  });
  assert.equal(requests[1][0], '/api/groups/child%20group');
  assert.deepEqual(requests[1][1], { method: 'DELETE', credentials: 'same-origin' });
  assert.equal(refreshes, 0, 'accepted phases do not refresh beneath the mounted dialog');

  const completion = state.open({ kind: 'delete-group', group: 'child group', members: [] });
  await actions.finishDeleteGroup(result, {
    plain: 'deleted group', wizard: 'disbanded party',
  });
  assert.equal(refreshes, 1);
  assert.deepEqual(notices, ['disbanded party']);
  assert.equal(state.dialog.value, null);
  assert.deepEqual(await completion, result);
});

test('member errors block deletion and retry only the exact failed frozen member', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, { createTransactionDialogActions }] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-actions.js'),
  ]);
  const requests = [];
  const actions = createTransactionDialogActions({
    state: createTransactionDialogState(),
    fetchImpl: async (url, init) => {
      requests.push([url, init]);
      if (requests.length === 1) {
        return new Response(JSON.stringify({ members: [
          { agent_id: alpha.agent_id, conv_id: alpha.conv_id, action: 'retired' },
          { agent_id: beta.agent_id, conv_id: beta.conv_id, action: 'error', detail: 'busy' },
        ] }), { status: 200 });
      }
      if (requests.length === 2) {
        return new Response(JSON.stringify({ members: [
          { agent_id: beta.agent_id, conv_id: beta.conv_id, action: 'retired' },
        ] }), { status: 200 });
      }
      return new Response(null, { status: 204 });
    },
  });
  const request = requestFor('child', [
    { selector: alpha.agent_id, agent_id: alpha.agent_id, conv_id: alpha.conv_id },
    { selector: beta.agent_id, agent_id: beta.agent_id, conv_id: beta.conv_id },
  ], 3);

  await assert.rejects(actions.deleteGroupPlan(request), (error) => {
    assert.equal(error.memberErrors, 1);
    return true;
  });
  assert.equal(requests.length, 1, 'member errors keep group deletion behind the barrier');
  const result = await actions.deleteGroupPlan(request);
  assert.deepEqual(JSON.parse(requests[1][1].body), {
    convs: [beta.agent_id], shutdown: true, delete_worktree: false,
  });
  assert.equal(requests.filter(([url]) => url.endsWith('/retire')).length, 2);
  assert.equal(requests.filter(([, init]) => init.method === 'DELETE').length, 1);
  assert.deepEqual(result, { ok: true, group: 'child', retired: 2, detached: 1 });
});

test('invalid successful retire responses keep deletion blocked and retry the frozen cohort', async (t) => {
  const cases = [
    {
      name: 'top-level error',
      response: () => new Response(JSON.stringify({ error: 'retire unavailable' }), { status: 200 }),
      message: 'retire failed: retire unavailable',
    },
    {
      name: 'top-level error with members envelope',
      response: () => new Response(JSON.stringify({
        error: 'retire unavailable', members: [],
      }), { status: 200 }),
      message: 'retire failed: retire unavailable',
    },
    {
      name: 'missing members envelope',
      response: () => new Response(JSON.stringify({}), { status: 200 }),
      message: 'retire failed: invalid response (expected a "members" array)',
    },
    {
      name: 'non-array members envelope',
      response: () => new Response(JSON.stringify({ members: {} }), { status: 200 }),
      message: 'retire failed: invalid response (expected a "members" array)',
    },
    {
      name: 'empty body',
      response: () => new Response(null, { status: 200 }),
      message: 'retire failed: invalid response (expected a "members" array)',
    },
    {
      name: 'malformed JSON',
      response: () => new Response('not-json', { status: 200 }),
      message: 'retire failed: not-json',
    },
  ];

  for (const scenario of cases) {
    await t.test(scenario.name, async (subtest) => {
      const harness = await createPreactHarness(subtest);
      const [{ createTransactionDialogState }, { createTransactionDialogActions }] = await Promise.all([
        harness.importDashboardModule('js/transaction-dialog-state.js'),
        harness.importDashboardModule('js/transaction-dialog-actions.js'),
      ]);
      const requests = [];
      const actions = createTransactionDialogActions({
        state: createTransactionDialogState(),
        fetchImpl: async (url, init) => {
          requests.push([url, init]);
          if (requests.length === 1) return scenario.response();
          if (url.endsWith('/retire')) {
            return new Response(JSON.stringify({ members: [
              { agent_id: alpha.agent_id, conv_id: alpha.conv_id, action: 'retired' },
              { agent_id: beta.agent_id, conv_id: beta.conv_id, action: 'retired' },
            ] }), { status: 200 });
          }
          return new Response(null, { status: 204 });
        },
      });
      const request = requestFor('child', [
        { selector: alpha.agent_id, agent_id: alpha.agent_id, conv_id: alpha.conv_id },
        { selector: beta.agent_id, agent_id: beta.agent_id, conv_id: beta.conv_id },
      ], 3);

      await assert.rejects(actions.deleteGroupPlan(request), (error) => {
        assert.equal(error.phase, 'retire');
        assert.equal(error.message, scenario.message);
        return true;
      });
      assert.equal(requests.length, 1, 'invalid retire response cannot issue DELETE');
      assert.equal(requests[0][1].method, 'POST');

      const result = await actions.deleteGroupPlan(request);
      assert.deepEqual(JSON.parse(requests[1][1].body), {
        convs: [alpha.agent_id, beta.agent_id], shutdown: true, delete_worktree: false,
      }, 'retry reuses every unresolved selector from the frozen cohort');
      assert.equal(requests.filter(([url]) => url.endsWith('/retire')).length, 2);
      assert.equal(requests.filter(([, init]) => init.method === 'DELETE').length, 1);
      assert.deepEqual(result, { ok: true, group: 'child', retired: 2, detached: 1 });
    });
  }
});

test('delete failure retry is idempotent and never repeats completed retirement', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, { createTransactionDialogActions }] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-actions.js'),
  ]);
  const requests = [];
  const actions = createTransactionDialogActions({
    state: createTransactionDialogState(),
    fetchImpl: async (url, init) => {
      requests.push([url, init]);
      if (url.endsWith('/retire')) {
        return new Response(JSON.stringify({ members: [
          { agent_id: alpha.agent_id, conv_id: alpha.conv_id, action: 'retired' },
        ] }), { status: 200 });
      }
      if (requests.filter(([, request]) => request.method === 'DELETE').length === 1) {
        return new Response('delete unavailable', { status: 503 });
      }
      return new Response(null, { status: 204 });
    },
  });
  const request = requestFor('child', [{
    selector: alpha.agent_id, agent_id: alpha.agent_id, conv_id: alpha.conv_id,
  }], 2);
  await assert.rejects(actions.deleteGroupPlan(request), (error) => {
    assert.equal(error.phase, 'delete');
    assert.equal(error.message, 'delete unavailable');
    return true;
  });
  const result = await actions.deleteGroupPlan(request);
  assert.equal(requests.filter(([url]) => url.endsWith('/retire')).length, 1,
    'completed retire phase is immutable across final DELETE retry');
  assert.equal(requests.filter(([, init]) => init.method === 'DELETE').length, 2);
  assert.deepEqual(result, { ok: true, group: 'child', retired: 1, detached: 1 });
  assert.equal(await actions.deleteGroupPlan(request), result,
    'a completed transaction returns its stable result without another request');
  assert.equal(requests.length, 3);
});

test('delete-group yields topmost Escape, guards backdrop drags, and restores its opener', async (t) => {
  const opened = await openDeleteGroup(t);
  const { harness, host, opener } = opened;
  const higher = harness.document.body.appendChild(harness.document.createElement('div'));
  higher.className = 'modal-overlay show';
  higher.style.zIndex = '999';
  escape(harness);
  await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#delete-group-modal'));
  higher.remove();

  host.querySelector('#delete-group-modal').dispatchEvent(
    new harness.window.Event('click', { bubbles: true }),
  );
  await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#delete-group-modal'), 'an unpaired backdrop click is guarded');
  escape(harness);
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#delete-group-modal'), null);
  assert.equal(harness.document.activeElement, opener);
  assert.equal(await opened.pending, null);
});

test('late delete-group rejection cannot repaint a dialog whose keyed owner was disposed', async (t) => {
  const pendingAction = deferred();
  const opened = await openDeleteGroup(t, {
    actions: { deleteGroupPlan: () => pendingAction.promise },
  });
  opened.host.querySelector('#delete-group-submit').click();
  await opened.harness.act(() => Promise.resolve());
  await opened.harness.act(() => { opened.state.close(); });
  assert.equal(opened.host.querySelector('#delete-group-modal'), null);
  pendingAction.reject(new Error('late failure'));
  await opened.harness.act(() => pendingAction.promise.catch(() => {}));
  assert.equal(opened.host.querySelector('#delete-group-modal'), null);
  assert.equal(opened.host.querySelector('#delete-group-error'), null);
});
