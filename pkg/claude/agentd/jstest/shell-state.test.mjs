import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('shell feedback state replaces and cleans timers and resolves confirmations', async (t) => {
  const harness = await createPreactHarness(t);
  const { createShellState } = await harness.importDashboardModule('js/shell-state.js');
  const timers = new Map();
  let nextTimer = 0;
  const state = createShellState({
    setTimer: (fn, ms) => { const id = ++nextTimer; timers.set(id, { fn, ms }); return id; },
    clearTimer: (id) => timers.delete(id),
  });

  state.showStatus('live');
  assert.deepEqual(state.status.value, { text: 'live', error: false });
  state.notify('first');
  const firstId = [...timers.keys()][0];
  state.notify('second', true);
  assert.equal(timers.has(firstId), false, 'replacement clears the prior timer');
  assert.deepEqual(state.toast.value, { id: 2, message: 'second', error: true, visible: true });
  const timer = [...timers.values()][0];
  assert.equal(timer.ms, 3000);
  timer.fn();
  assert.equal(state.toast.value.visible, false);

  const first = state.confirm({ title: 'One' });
  const second = state.confirm({ title: 'Two', cancelLabel: 'Keep' });
  assert.equal(await first, false, 'a newer singleton confirm cancels its predecessor');
  assert.equal(state.confirmation.value.cancelLabel, 'Keep');
  state.resolveConfirmation(true);
  assert.equal(await second, true);

  const pending = state.confirm({ title: 'Pending' });
  state.dispose();
  assert.equal(await pending, false, 'unmount cannot leave a confirmation promise pending');
});

test('a confirm with an action blocks until the work settles', async (t) => {
  const harness = await createPreactHarness(t);
  const { createShellState } = await harness.importDashboardModule('js/shell-state.js');
  const state = createShellState({ setTimer: () => 0, clearTimer: () => {} });

  // Confirming a blocking action keeps the SAME dialog up in its busy state
  // instead of dismissing it and running the work invisibly.
  let release;
  const work = new Promise((resolve) => { release = resolve; });
  const answered = state.confirm({
    title: 'Shutdown?', okLabel: 'Shut down', busyLabel: 'Shutting down…',
    action: () => work,
  });
  state.resolveConfirmation(true);
  await Promise.resolve();
  assert.equal(state.confirmation.value?.busy, true, 'the modal stays up, busy');
  assert.equal(state.confirmation.value?.busyLabel, 'Shutting down…');

  // A busy dialog ignores the dismissal paths, so an Escape or backdrop click
  // cannot hide work that is already in flight.
  state.resolveConfirmation(false);
  assert.equal(state.confirmation.value?.busy, true, 'a busy dialog is not dismissible');

  release('response');
  assert.equal(await answered, 'response', 'the confirm resolves with the action result');
  assert.equal(state.confirmation.value, null, 'settling takes the dialog down');

  // A rejected action rejects the confirm and still clears the dialog, so a
  // failed request cannot strand the operator on a spinner.
  const boom = state.confirm({ title: 'Power on?', action: () => Promise.reject(new Error('nope')) });
  state.resolveConfirmation(true);
  await assert.rejects(boom, /nope/);
  assert.equal(state.confirmation.value, null);

  // Cancelling never runs the action and answers false, as an ordinary confirm.
  let ran = false;
  const cancelled = state.confirm({ title: 'Shutdown?', action: () => { ran = true; } });
  state.resolveConfirmation(false);
  assert.equal(await cancelled, false);
  assert.equal(ran, false, 'a cancelled confirm must not run its action');
  assert.equal(state.confirmation.value, null);

  // A late settle from a finished operation must not tear down a dialog that
  // something else has since put up.
  let releaseSlow;
  const slow = new Promise((resolve) => { releaseSlow = resolve; });
  const slowAnswer = state.confirm({ title: 'Slow', action: () => slow });
  state.resolveConfirmation(true);
  await Promise.resolve();
  state.confirmation.value = { title: 'Newer', busy: false };
  releaseSlow('done');
  assert.equal(await slowAnswer, 'done');
  assert.equal(state.confirmation.value?.title, 'Newer',
    'a late settle must not dismiss an unrelated dialog');
});

