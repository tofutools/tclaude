import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((ok, no) => { resolve = ok; reject = no; });
  return { promise, resolve, reject };
}

function snapshot(revision = 1) {
  return {
    revision,
    agents: [
      { agent_id: 'agt_one', conv_id: 'conv-one', title: 'One', online: true },
      { agent_id: 'agt_two', conv_id: 'conv-two', title: 'Two', online: true },
    ],
    groups: [{ name: 'alpha', members: [
      { agent_id: 'agt_one', conv_id: 'conv-one', title: 'One', online: true, role: 'dev' },
      { agent_id: 'agt_two', conv_id: 'conv-two', title: 'Two', online: true, role: 'reviewer' },
    ] }],
  };
}

function createDescriptor(overrides = {}) {
  return {
    kind: 'create', launchID: 1, originalExpr: '', originalTarget: '',
    prefill: {
      targetMode: 'solo', target: 'agt_one', interval: '5m', body: 'status', enabled: true,
    },
    ...overrides,
  };
}

test('cron dialog owns focus and dirty Escape/backdrop dismissal', async (t) => {
  const harness = await createPreactHarness(t);
  const { CronDialog } = await harness.importDashboardModule('js/jobs-dialog-island.js');
  let closed = 0;
  let discard = false;
  const actions = {
    closeCronDialog: () => { closed += 1; },
    saveCron: async () => ({}),
    explainCron: async () => ({ valid: true }),
  };
  const invoker = harness.document.body.appendChild(harness.document.createElement('button'));
  invoker.focus();
  const mounted = await harness.mount(harness.html`<${CronDialog} descriptor=${createDescriptor()}
    snapshot=${snapshot()} actions=${actions} confirmDiscard=${async () => discard} />`);
  await Promise.resolve();
  assert.equal(harness.document.activeElement.id, 'cron-create-name', 'the first field receives focus');

  await harness.input(mounted.container.querySelector('#cron-create-body'), 'changed');
  const overlay = mounted.container.querySelector('#cron-create-modal');
  await harness.act(() => harness.fireEvent(overlay, 'mousedown'));
  assert.equal(closed, 0, 'dirty backdrop close waits for confirmation');

  discard = true;
  await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }));
  assert.equal(closed, 1, 'confirmed Escape closes the dirty draft');
  await mounted.unmount();
  assert.equal(harness.document.activeElement, invoker, 'unmount restores the launcher focus');
  invoker.remove();
});

test('cron dialog submit is IME-safe, single-flight, and closes independently of refresh', async (t) => {
  const harness = await createPreactHarness(t);
  const { CronDialog } = await harness.importDashboardModule('js/jobs-dialog-island.js');
  const saving = deferred();
  const saves = [];
  let closed = 0;
  const actions = {
    closeCronDialog: () => { closed += 1; },
    saveCron: (mutation) => { saves.push(mutation); return saving.promise; },
    explainCron: async () => ({ valid: true }),
  };
  const mounted = await harness.mount(harness.html`<${CronDialog} descriptor=${createDescriptor()}
    snapshot=${snapshot()} actions=${actions} confirmDiscard=${async () => true} />`);
  const dialog = getByRole(mounted.container, 'dialog');
  await harness.act(() => harness.fireEvent(dialog, 'keydown', {
    key: 'Enter', ctrlKey: true, isComposing: true, keyCode: 229,
  }));
  assert.equal(saves.length, 0, 'composition Enter never submits');
  await harness.act(() => harness.fireEvent(dialog, 'keydown', {
    key: 'Enter', ctrlKey: true, isComposing: false, keyCode: 13,
  }));
  await harness.act(async () => {
    harness.fireEvent(mounted.container.querySelector('#cron-create-submit'), 'click');
    await Promise.resolve();
  });
  assert.equal(saves.length, 1, 'busy ref blocks keyboard/click double submission');
  assert.equal(mounted.container.querySelector('#cron-create-cancel').disabled, true);
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('#cron-create-modal'), 'mousedown'));
  assert.equal(closed, 0, 'busy overlays cannot close through the backdrop');
  saving.resolve({ id: 9 });
  await harness.act(() => saving.promise);
  assert.equal(closed, 1);
  assert.equal(saves[0].method, 'POST');
  await mounted.unmount();
});

test('cron create retries permission failures and keep-open resets only batch fields', async (t) => {
  const harness = await createPreactHarness(t);
  const { CronDialog } = await harness.importDashboardModule('js/jobs-dialog-island.js');
  const mutations = [];
  let attempts = 0;
  let closed = 0;
  const actions = {
    closeCronDialog: () => { closed += 1; },
    explainCron: async () => ({ valid: true }),
    saveCron: async (mutation) => {
      mutations.push(mutation);
      attempts += 1;
      if (attempts === 1) throw new Error('HTTP 403: permission denied');
      return { id: 10 };
    },
  };
  const descriptor = createDescriptor({ prefill: {
    name: 'first', targetMode: 'solo', target: 'agt_one', owner: 'agt_one',
    interval: '15m', subject: 'subject', body: 'batch body', enabled: true,
  } });
  const mounted = await harness.mount(harness.html`<${CronDialog} descriptor=${descriptor}
    snapshot=${snapshot()} actions=${actions} confirmDiscard=${async () => true} />`);
  await harness.act(async () => {
    harness.fireEvent(mounted.container.querySelector('#cron-create-submit'), 'click');
    await Promise.resolve();
  });
  assert.match(mounted.container.querySelector('#cron-create-error').textContent, /403.*permission denied/);
  assert.equal(mounted.container.querySelector('#cron-create-body').value, 'batch body');
  assert.equal(mounted.container.querySelector('#cron-create-submit').disabled, false, 'failed saves are retryable');

  await harness.act(async () => {
    harness.fireEvent(mounted.container.querySelector('#cron-create-save-another'), 'click');
    await Promise.resolve();
  });
  assert.equal(mutations.length, 2);
  assert.equal(closed, 0, 'keep-open keeps the same owned dialog mounted');
  assert.equal(mounted.container.querySelector('#cron-create-name').value, '');
  assert.equal(mounted.container.querySelector('#cron-create-subject').value, '');
  assert.equal(mounted.container.querySelector('#cron-create-body').value, '');
  assert.equal(mounted.container.querySelector('#cron-create-target').value, 'agt_one');
  assert.equal(mounted.container.querySelector('#cron-create-interval').value, '15m');
  assert.equal(harness.document.activeElement.id, 'cron-create-name');
  await mounted.unmount();
});

test('cron edit and duplicate descriptors render their distinct component modes', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ JobsCronDialogRoot }, { createJobsState }] = await Promise.all([
    harness.importDashboardModule('js/jobs-dialog-island.js'),
    harness.importDashboardModule('js/jobs-state.js'),
  ]);
  const snapshotSignal = harness.signals.signal(snapshot());
  const state = createJobsState({ snapshot: snapshotSignal, prefs: {
    getItem: () => null, setItem: () => {}, removeItem: () => {},
  } });
  const saved = [];
  const actions = {
    closeCronDialog: state.closeCronDialog,
    explainCron: async () => ({ valid: true, description: 'daily' }),
    saveCron: async (mutation) => { saved.push(mutation); return { id: 4 }; },
  };
  const view = () => harness.html`<${JobsCronDialogRoot} state=${state} actions=${actions}
    confirmDiscard=${async () => true}/>`;
  const mounted = await harness.mount(view());
  await harness.act(() => state.openCronEdit({
    id: 4, name: 'daily', target_kind: 'conv', target_agent: 'agt_one', owner_agent: 'agt_one',
    cron_expr: '@daily', subject: 'status', body: 'report', enabled: true,
  }));
  assert.equal(mounted.container.querySelector('#cron-create-modal').classList.contains('cron-editing'), true);
  assert.match(mounted.container.querySelector('#cron-create-title').textContent, /Edit cron job/);
  assert.equal(mounted.container.querySelector('#cron-create-save-another'), null);
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('#cron-create-submit'), 'click'));
  assert.equal(saved[0].method, 'PATCH');

  await harness.act(() => state.openCronDuplicate({
    id: 4, name: 'daily', target_kind: 'conv', target_agent: 'agt_one', owner_agent: 'agt_one',
    interval_seconds: 300, subject: 'status', body: 'report', enabled: true,
  }));
  assert.match(mounted.container.querySelector('#cron-create-title').textContent, /Duplicate cron job/);
  assert.equal(mounted.container.querySelector('#cron-create-name').value, 'daily-copy');
  assert.ok(mounted.container.querySelector('#cron-create-save-another'));
  await mounted.unmount();
});

test('cron explainer rejects stale responses while stacked pickers and live snapshots retain the draft', async (t) => {
  const harness = await createPreactHarness(t);
  const { CronDialog } = await harness.importDashboardModule('js/jobs-dialog-island.js');
  const requests = [];
  const actions = {
    closeCronDialog: () => {}, saveCron: async () => ({}),
    explainCron: (expr) => {
      const request = deferred();
      requests.push({ expr, ...request });
      return request.promise;
    },
  };
  const descriptor = createDescriptor({
    prefill: {
      targetMode: 'group', groupName: 'alpha', scopeGroup: 'alpha',
      cronExpr: '@daily', body: 'original body', enabled: true,
    },
  });
  const vnode = (value) => harness.html`<${CronDialog} descriptor=${descriptor}
    snapshot=${value} actions=${actions} confirmDiscard=${async () => true} />`;
  const mounted = await harness.mount(vnode(snapshot(1)));
  await new Promise((resolve) => setTimeout(resolve, 10));
  assert.deepEqual(requests.map((request) => request.expr), ['@daily']);
  const cron = mounted.container.querySelector('#cron-create-cron');
  await harness.input(cron, '*/5 * * * *');
  await new Promise((resolve) => setTimeout(resolve, 375));
  assert.deepEqual(requests.map((request) => request.expr), ['@daily', '*/5 * * * *']);
  requests[1].resolve({ valid: true, description: 'every five minutes', next: [], tz: 'UTC' });
  await harness.act(() => requests[1].promise);
  assert.match(mounted.container.querySelector('#cron-create-cron-explain').textContent, /every five minutes/);
  requests[0].resolve({ valid: true, description: 'stale daily', next: [], tz: 'UTC' });
  await harness.act(() => requests[0].promise);
  assert.doesNotMatch(mounted.container.querySelector('#cron-create-cron-explain').textContent, /stale daily/);

  await harness.input(mounted.container.querySelector('#cron-create-body'), 'typed draft survives');
  await mounted.rerender(vnode(snapshot(2)));
  assert.equal(mounted.container.querySelector('#cron-create-body').value, 'typed draft survives');
  assert.equal(mounted.container.querySelector('#cron-create-group').disabled, true, 'scoped group cannot be retargeted');

  await harness.act(() => harness.fireEvent(mounted.container.querySelector('#cron-create-owner-pick'), 'click'));
  assert.equal(mounted.container.querySelectorAll('.modal-overlay.show').length, 2, 'picker stacks over the cron draft');
  assert.equal(harness.document.activeElement.id, 'cron-pick-target-search');
  await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }));
  assert.equal(mounted.container.querySelector('#cron-pick-target-modal'), null, 'first Escape closes only the stacked picker');
  assert.ok(mounted.container.querySelector('#cron-create-modal'));

  await harness.act(() => harness.fireEvent(mounted.container.querySelector('#cron-create-owner-pick'), 'click'));
  const option = mounted.container.querySelector('#cron-pick-target-option-1');
  await harness.act(() => harness.fireEvent(option, 'mousedown'));
  assert.equal(mounted.container.querySelector('#cron-create-owner').value, 'agt_two');
  await mounted.unmount();
});
