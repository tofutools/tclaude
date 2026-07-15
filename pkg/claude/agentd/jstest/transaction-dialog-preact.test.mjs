import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('transaction state freezes exact identities and refuses live retargeting', async (t) => {
  const harness = await createPreactHarness(t);
  const { createTransactionDialogState } = await harness.importDashboardModule(
    'js/transaction-dialog-state.js',
  );
  const state = createTransactionDialogState();
  const candidates = [
    { agent_id: 'agt_one', conv_id: 'conv-one', groups: ['alpha'] },
    { agent_id: 'agt_two', conv_id: 'conv-two', groups: ['alpha', 'nested'] },
  ];

  const first = state.open({ kind: 'bulk-retire', group: 'alpha', candidates });
  const current = state.dialog.value;
  assert.equal(current.key, 'bulk-retire:1');
  assert.notEqual(current.descriptor.candidates, candidates);
  assert.deepEqual(current.descriptor.candidates, candidates);
  assert.ok(Object.isFrozen(current.descriptor));
  assert.ok(Object.isFrozen(current.descriptor.candidates));
  assert.ok(Object.isFrozen(current.descriptor.candidates[1].groups));

  candidates[0].agent_id = 'agt_retargeted';
  candidates[1].groups.push('late-poll');
  assert.equal(current.descriptor.candidates[0].agent_id, 'agt_one');
  assert.deepEqual(current.descriptor.candidates[1].groups, ['alpha', 'nested']);

  const refused = state.open({ kind: 'delete-agent', agent: 'agt_other' });
  assert.equal(await refused, null);
  assert.equal(state.dialog.value, current, 'a second launch cannot replace the live transaction');

  state.finish({ ok: true, submitted: ['agt_one'] });
  assert.deepEqual(await first, { ok: true, submitted: ['agt_one'] });
  assert.equal(state.dialog.value, null);

  const reopened = state.open({ kind: 'bulk-retire', group: 'alpha', candidates });
  assert.equal(state.dialog.value.key, 'bulk-retire:2', 'same-kind reopen gets a fresh component key');
  state.dispose();
  assert.equal(await reopened, null, 'unmount resolves a compatibility promise');
});

test('transaction frame blocks busy dismissal and restores opener focus', async (t) => {
  const harness = await createPreactHarness(t);
  const { TransactionDialogFrame } = await harness.importDashboardModule(
    'js/transaction-dialog-island.js',
  );
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const opener = harness.document.body.appendChild(harness.document.createElement('button'));
  opener.id = 'transaction-opener';
  opener.focus();
  const calls = [];
  let busy = false;
  const renderFrame = () => harness.html`
    <${TransactionDialogFrame}
      id="transaction-test-modal"
      labelledby="transaction-test-title"
      title="Delete selected agents?"
      meta="2 selected"
      busy=${busy}
      error="request failed"
      primaryLabel="Delete"
      busyLabel="Deleting…"
      onClose=${() => calls.push('close')}
      onSubmit=${() => calls.push('submit')}
      confirmDiscard=${async () => true}
    ><label>Choice <input id="transaction-choice" /></label></${TransactionDialogFrame}>
  `;
  const mounted = await harness.mount(renderFrame(), host);
  await harness.act(() => Promise.resolve());
  assert.equal(harness.document.activeElement.id, 'transaction-test-submit');
  assert.equal(host.querySelector('[role="alert"]').textContent, 'request failed');

  busy = true;
  await mounted.rerender(renderFrame());
  assert.equal(host.querySelector('#transaction-test-cancel').disabled, true);
  assert.equal(host.querySelector('#transaction-test-submit').getAttribute('aria-busy'), 'true');

  const escapeBusy = new harness.window.Event('keydown', { bubbles: true });
  Object.defineProperty(escapeBusy, 'key', { value: 'Escape' });
  harness.document.dispatchEvent(escapeBusy);
  host.querySelector('#transaction-test-modal').dispatchEvent(
    new harness.window.Event('mousedown', { bubbles: true }),
  );
  host.querySelector('#transaction-test-modal').dispatchEvent(
    new harness.window.Event('click', { bubbles: true }),
  );
  host.querySelector('#transaction-test-cancel').click();
  host.querySelector('#transaction-test-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(calls, [], 'busy state blocks Escape, backdrop, Cancel, and duplicate submit');

  busy = false;
  await mounted.rerender(renderFrame());
  host.querySelector('#transaction-test-submit').click();
  assert.deepEqual(calls, ['submit']);

  const escapeIdle = new harness.window.Event('keydown', { bubbles: true });
  Object.defineProperty(escapeIdle, 'key', { value: 'Escape' });
  harness.document.dispatchEvent(escapeIdle);
  await harness.act(() => Promise.resolve());
  assert.deepEqual(calls, ['submit', 'close']);
  await mounted.unmount();
  assert.equal(harness.document.activeElement, opener);
});

test('transaction frame uses topmost dirty confirmation and guards backdrop drags', async (t) => {
  const harness = await createPreactHarness(t);
  const { TransactionDialogFrame } = await harness.importDashboardModule(
    'js/transaction-dialog-island.js',
  );
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const calls = [];
  let acceptDiscard = false;
  const mounted = await harness.mount(harness.html`
    <${TransactionDialogFrame}
      id="dirty-transaction-modal"
      labelledby="dirty-transaction-title"
      title="Cleanup?"
      dirty=${true}
      primaryLabel="Clean"
      onClose=${() => calls.push('close')}
      onSubmit=${() => calls.push('submit')}
      confirmDiscard=${async () => { calls.push('confirm'); return acceptDiscard; }}
    ><textarea id="dirty-choice"></textarea></${TransactionDialogFrame}>
  `, host);
  await harness.act(() => Promise.resolve());

  const overlay = host.querySelector('#dirty-transaction-modal');
  // A click whose press did not begin on the backdrop models a selection or
  // drag released outside the dialog. The guarded overlay must ignore it.
  overlay.dispatchEvent(new harness.window.Event('click', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  assert.deepEqual(calls, [], 'a press begun in the dialog cannot dismiss on backdrop release');

  overlay.dispatchEvent(new harness.window.Event('mousedown', { bubbles: true }));
  overlay.dispatchEvent(new harness.window.Event('click', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  assert.deepEqual(calls, ['confirm'], 'dirty rejected backdrop asks but stays open');

  acceptDiscard = true;
  const escape = new harness.window.Event('keydown', { bubbles: true });
  Object.defineProperty(escape, 'key', { value: 'Escape' });
  harness.document.dispatchEvent(escape);
  await harness.act(() => Promise.resolve());
  assert.deepEqual(calls, ['confirm', 'confirm', 'close']);
  await mounted.unmount();
});

test('transaction controller unregisters without orphaning callers', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, controller] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-controller.js'),
  ]);
  const state = createTransactionDialogState();
  const unregister = controller.registerTransactionDialogController(state);
  const pending = controller.openTransactionDialog({ kind: 'shutdown', agent: 'agt_one' });
  assert.equal(state.dialog.value.descriptor.agent, 'agt_one');
  unregister();
  state.dispose();
  assert.equal(await pending, null);
  assert.throws(
    () => controller.openTransactionDialog({ kind: 'shutdown', agent: 'agt_two' }),
    /transaction dialogs are not ready/,
  );
});
