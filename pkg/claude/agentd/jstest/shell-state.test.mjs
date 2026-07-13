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
