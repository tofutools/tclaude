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

const alpha = {
  agent_id: 'agt_alpha', conv_id: 'alpha-1111-2222-3333-444444444444',
  title: 'Alpha worker', roles: ['builder', 'reviewer'], groups: ['alpha', 'beta'],
};
const beta = {
  agent_id: '', conv_id: 'beta-1111-2222-3333-444444444444',
  title: 'Beta worker', roles: [], groups: ['alpha'],
};
const loose = {
  agent_id: 'agt_loose', conv_id: 'loose-1111-2222-3333-44444444444',
  title: 'Loose worker', roles: [], groups: [],
};

async function openWindowDialog(t, descriptor, action) {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, island] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-island.js'),
  ]);
  const state = createTransactionDialogState();
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const opener = harness.document.body.appendChild(harness.document.createElement('button'));
  opener.id = 'window-selection-opener';
  opener.focus();
  const actions = {
    close: state.close,
    selectAgentWindows: action || (async (request) => {
      const result = { ok: true, request };
      state.handoff();
      state.finish(result);
      return result;
    }),
  };
  const mounted = await harness.mount(harness.html`
    <${island.TransactionDialogApp}
      state=${state} actions=${actions} confirmDiscard=${async () => true}
    />
  `, host);
  let pending;
  await harness.act(() => { pending = state.open(descriptor); });
  await harness.act(() => Promise.resolve());
  return { harness, state, host, opener, mounted, pending };
}

test('window launcher freezes a deduped running roster with overlapping buckets', async (t) => {
  const harness = await createPreactHarness(t);
  const controller = await harness.importDashboardModule('js/transaction-dialog-controller.js');
  const snapshot = {
    agents: [
      { ...alpha, online: true },
      { ...alpha, title: 'duplicate poll row', online: true },
      { ...beta, online: true },
      { ...loose, online: true },
      { conv_id: 'offline', title: 'Offline', online: false },
    ],
    groups: [
      { name: 'alpha', members: [
        { ...alpha, role: 'builder', online: true },
        { ...beta, role: '', online: true },
      ] },
      { name: 'beta', members: [
        { ...alpha, role: 'reviewer', online: true },
        { ...alpha, role: 'reviewer', online: true },
      ] },
    ],
  };
  const descriptor = controller.buildWindowSelectionDescriptor(snapshot, 'all', '', true);
  assert.equal(descriptor.kind, 'window-selection');
  assert.equal(descriptor.scope, 'all');
  assert.equal(descriptor.webTerminal, true);
  assert.deepEqual(descriptor.candidates, [alpha, beta, loose]);
  assert.deepEqual(controller.normalizeWindowSelectionCandidates([
    { ...alpha, agent_id: '', title: '', roles: ['builder'], groups: ['alpha'] },
    { ...alpha, roles: ['reviewer'], groups: ['beta'] },
  ]), [alpha], 'conv dedupe merges overlapping buckets and fills missing identity copy');
  snapshot.agents[0].title = 'late title';
  snapshot.groups[0].members[0].role = 'late role';

  const stateModule = await harness.importDashboardModule('js/transaction-dialog-state.js');
  const state = stateModule.createTransactionDialogState();
  const unregister = controller.registerTransactionDialogController(state);
  const source = descriptor.candidates;
  const pending = controller.openWindowSelectionDialog(descriptor);
  const frozen = state.dialog.value.descriptor;
  assert.ok(Object.isFrozen(frozen));
  assert.ok(Object.isFrozen(frozen.candidates));
  assert.ok(Object.isFrozen(frozen.candidates[0].roles));
  source[0].roles.push('caller mutation');
  assert.deepEqual(frozen.candidates[0].roles, ['builder', 'reviewer']);
  state.close();
  await pending;

  const group = controller.buildWindowSelectionDescriptor(snapshot, 'group', 'alpha');
  assert.deepEqual(group.candidates.map((candidate) => ({
    conv: candidate.conv_id, roles: candidate.roles, groups: candidate.groups,
  })), [
    { conv: alpha.conv_id, roles: ['late role'], groups: ['alpha'] },
    { conv: beta.conv_id, roles: [], groups: ['alpha'] },
  ]);
  unregister();
});

test('window action preserves native payloads and exact returned unfocus identities', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, { createTransactionDialogActions }] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-actions.js'),
  ]);
  const requests = [];
  const notices = [];
  const closed = [];
  const state = createTransactionDialogState();
  const actions = createTransactionDialogActions({
    state,
    fetchImpl: async (url, init) => {
      requests.push([url, init]);
      return new Response(JSON.stringify({
        direction: 'unfocus', scope: 'group', targeted: 3,
        detached: 1, no_window: 1, failed: 1,
        agents: [
          { agent_id: 'agt_returned', conv_id: 'returned-conv', outcome: 'detached' },
          { agent_id: 'agt_headless', conv_id: 'headless-conv', outcome: 'no_window' },
        ],
      }), { status: 200 });
    },
    notify: (message, error) => notices.push([message, !!error]),
    closeTerminalsForWindowOp: (agents) => closed.push(agents),
  });
  const pending = state.open({ kind: 'window-selection' });
  const request = Object.freeze({
    direction: 'unfocus', scope: 'group', group: 'alpha/team',
    convs: Object.freeze(['agt_alpha', beta.conv_id]),
    webTerminal: true,
    targets: Object.freeze([]),
  });
  const result = await actions.selectAgentWindows(request);
  assert.equal(requests[0][0], '/api/agent-windows');
  assert.equal(requests[0][1].method, 'POST');
  assert.equal(requests[0][1].credentials, 'same-origin');
  assert.deepEqual(JSON.parse(requests[0][1].body), {
    direction: 'unfocus', scope: 'group', group: 'alpha/team',
    convs: ['agt_alpha', beta.conv_id],
  });
  assert.equal(requests[0][1].body.includes('webTerminal'), false);
  assert.equal(closed.length, 1);
  assert.equal(closed[0], result.agents,
    'terminal cleanup receives the daemon response identities, not submitted candidates');
  assert.deepEqual(notices, [[
    'unfocus windows (3 targeted): 1 detached, 1 had no window, 1 failed', true,
  ]]);
  assert.equal(state.dialog.value, null);
  assert.equal(await pending, result);
});

test('web focus branches outside HTTP and opens the exact frozen selectors', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, { createTransactionDialogActions }] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-actions.js'),
  ]);
  const state = createTransactionDialogState();
  const opened = [];
  const notices = [];
  const actions = createTransactionDialogActions({
    state,
    fetchImpl: async () => { throw new Error('native fetch must not run'); },
    openWebWindowPane: (...args) => opened.push(args),
    notify: (...args) => notices.push(args),
  });
  const pending = state.open({ kind: 'window-selection' });
  const result = await actions.selectAgentWindows({
    direction: 'focus', scope: 'all', convs: ['agt_alpha', beta.conv_id],
    webTerminal: true,
    targets: [
      { selector: 'agt_alpha', label: 'Alpha worker' },
      { selector: beta.conv_id, label: 'Beta worker' },
    ],
  });
  assert.deepEqual(opened, [
    ['agt_alpha', 'Alpha worker'], [beta.conv_id, 'Beta worker'],
  ]);
  assert.deepEqual(notices, [['focus web terminals: 2 focused']]);
  assert.equal(result.terminal, 'web');
  assert.deepEqual(await pending, result);
});

test('window picker preserves bucket overlap, synthetic buckets, and filter-independent selection', async (t) => {
  let submitted = null;
  const mounted = await openWindowDialog(t, {
    kind: 'window-selection', scope: 'all', webTerminal: false,
    candidates: [alpha, beta, loose],
  }, async (request) => {
    submitted = request;
    mounted.state.handoff();
    mounted.state.finish({ ok: true });
  });
  const { harness, host, opener } = mounted;
  assert.equal(host.querySelectorAll('#window-modal').length, 1);
  assert.equal(harness.document.activeElement.id, 'window-submit');
  assert.equal(host.querySelector('#window-title .window-title-regular').textContent, 'Agent windows');
  assert.equal(host.querySelector('#window-title .window-title-wizard').textContent, "Familiars' windows");
  assert.equal(host.querySelector('#window-hint .theme-copy-regular').textContent,
    'Open or raise a terminal window for each selected running agent in the dashboard.');
  assert.equal(host.querySelector('#window-hint .theme-copy-wizard').textContent,
    'Conjure a scrying portal for each chosen channeling familiar in the tower.');
  assert.equal(host.querySelector('#window-count').textContent, '3 of 3 selected');
  assert.equal(host.querySelector('[data-group-chip="alpha"]').textContent, 'alpha (2/2)');
  assert.equal(host.querySelector('[data-group-chip="beta"]').textContent, 'beta (1/1)');
  assert.equal(host.querySelector('[data-group-chip="(no group)"]').textContent, '(no group) (1/1)');
  assert.equal(host.querySelector('[data-role="builder"]').textContent, 'builder (1/1)');
  assert.equal(host.querySelector('[data-role="reviewer"]').textContent, 'reviewer (1/1)');
  assert.equal(host.querySelector('[data-role="(no role)"]').textContent, '(no role) (2/2)');
  assert.deepEqual(
    [...host.querySelectorAll('#window-groups .window-role-chip')].map((node) => node.getAttribute('data-group-chip')),
    ['alpha', 'beta', '(no group)'],
  );

  await harness.act(() => host.querySelector('#window-select-none').click());
  assert.equal(host.querySelector('#window-count').textContent, '0 of 3 selected');
  assert.equal(host.querySelector('#window-submit').disabled, true);
  host.querySelector('#window-submit').click();
  assert.equal(submitted, null, 'empty selection cannot dispatch');
  await harness.act(() => host.querySelector('#window-select-all').click());
  assert.equal(host.querySelector('#window-count').textContent, '3 of 3 selected');
  assert.equal(host.querySelector('[data-group-chip="alpha"]').disabled, false);
  await harness.act(() => harness.fireEvent(
    host.querySelector('[data-group-chip="alpha"]'), 'click',
  ));
  assert.equal(host.querySelector('#window-count').textContent, '1 of 3 selected');
  assert.ok(host.querySelector('[data-role="(no role)"]').classList.contains('partial'));

  const search = host.querySelector('#window-search');
  search.value = 'alpha';
  await harness.act(() => harness.fireEvent(search, 'input'));
  assert.equal(host.querySelectorAll('#window-list input[type="checkbox"]').length, 1);
  await harness.act(() => host.querySelector('#window-select-all').click());
  assert.equal(host.querySelector('#window-count').textContent, '3 of 3 selected',
    'select all acts on the frozen roster, not only the filtered row');
  await harness.act(() => host.querySelector('[data-role="builder"]').click());
  assert.equal(host.querySelector('#window-count').textContent, '2 of 3 selected');

  const unfocus = host.querySelector('input[value="unfocus"]');
  unfocus.checked = true;
  await harness.act(() => harness.fireEvent(unfocus, 'change'));
  host.querySelector('#window-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(submitted.convs, [beta.conv_id, 'agt_loose']);
  assert.equal(submitted.direction, 'unfocus');
  assert.equal(submitted.scope, 'all');
  assert.equal(submitted.group, undefined);
  assert.ok(Object.isFrozen(submitted));
  assert.ok(Object.isFrozen(submitted.convs));
  assert.equal(harness.document.activeElement, opener);
  await mounted.pending;
  await mounted.mounted.unmount();
});

test('window picker freezes retries, blocks busy dismissal, and restores focus after error recovery', async (t) => {
  const first = deferred();
  const second = deferred();
  const requests = [];
  const mounted = await openWindowDialog(t, {
    kind: 'window-selection', scope: 'group', group: 'alpha', webTerminal: false,
    candidates: [alpha, beta],
  }, async (request) => {
    requests.push(request);
    const result = await (requests.length === 1 ? first.promise : second.promise);
    mounted.state.handoff();
    mounted.state.finish(result);
    return result;
  });
  const { harness, host, opener } = mounted;
  host.querySelector('#window-submit').click();
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#window-cancel').disabled, true);
  assert.equal(host.querySelector('#window-search').disabled, true);
  escape(harness);
  const overlay = host.querySelector('#window-modal');
  overlay.dispatchEvent(new harness.window.Event('mousedown', { bubbles: true }));
  overlay.dispatchEvent(new harness.window.Event('click', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#window-modal'), 'busy Escape/backdrop cannot dismiss');

  first.reject(new Error('native windows unavailable'));
  await harness.act(() => first.promise.catch(() => {}));
  assert.equal(host.querySelector('[role="alert"]').textContent, 'native windows unavailable');
  assert.match(host.querySelector('#window-submit').textContent, /Retry Focus 2 agents/);
  host.querySelector('#window-submit').click();
  await harness.act(() => Promise.resolve());
  assert.equal(requests[1], requests[0], 'retry reuses the exact frozen request object');
  host.querySelector('#window-submit').click();
  assert.equal(requests.length, 2, 'same-render repeated submission is locked');

  second.resolve({ targeted: 2, focused: 2 });
  await harness.act(() => second.promise);
  assert.equal(host.querySelector('#window-modal'), null);
  assert.equal(harness.document.activeElement, opener);
  await mounted.pending;
  await mounted.mounted.unmount();
});

test('window picker Escape is topmost-only and idle cancellation restores its opener', async (t) => {
  const mounted = await openWindowDialog(t, {
    kind: 'window-selection', scope: 'all', webTerminal: false, candidates: [alpha],
  });
  const { harness, host, opener } = mounted;
  const covering = harness.document.body.appendChild(harness.document.createElement('div'));
  covering.className = 'modal-overlay show';
  covering.style.zIndex = '99';
  escape(harness);
  await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#window-modal'), 'a covered picker ignores Escape');
  covering.remove();
  escape(harness);
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#window-modal'), null);
  assert.equal(harness.document.activeElement, opener);
  assert.equal(await mounted.pending, null);
  await mounted.mounted.unmount();
});
