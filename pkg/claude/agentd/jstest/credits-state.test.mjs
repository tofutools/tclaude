import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('credits state owns payouts, attribution, titles, ranking, and hot-streak decay', async (t) => {
  const harness = await createPreactHarness(t);
  const {
    createCreditsState, CREDIT_HOT_WINDOW_MS, CREDIT_LEADERBOARD_MAX,
  } = await harness.importDashboardModule('js/credits-state.js');
  let clock = 1000;
  const state = createCreditsState({ now: () => clock });

  assert.deepEqual(state.recordWin('not-a-win', 'ignored'), {
    accepted: false, attributed: false, payout: 0,
  });
  assert.equal(state.view.value.credits, 0);
  assert.equal(state.view.value.bumpVersion, 0);

  state.recordWin('win-spawn', 'not-attributed');
  state.recordWin('win-mega');
  assert.equal(state.view.value.credits, 802);
  assert.equal(state.view.value.bumpVersion, 2);
  assert.deepEqual(state.view.value.entries, []);

  for (let index = 0; index < 3; index++) {
    clock += 10;
    state.recordWin('win-idle', 'conversation-alpha');
  }
  state.recordWin('win-pull', 'conversation-beta');
  state.publishSnapshot({
    agents: [
      { conv_id: 'conversation-alpha', title: 'Alice' },
      { conv_id: 'conversation-beta', title: 'Bob' },
    ],
  });
  assert.equal(state.view.value.credits, 1152);
  assert.deepEqual(state.view.value.entries.map(({ title, wins, hot }) => ({ title, wins, hot })), [
    { title: 'Alice', wins: 3, hot: true },
    { title: 'Bob', wins: 1, hot: false },
  ]);

  clock += CREDIT_HOT_WINDOW_MS + 1;
  state.publishSnapshot({ agents: [] });
  assert.equal(state.view.value.entries[0].title, 'conversa');
  assert.equal(state.view.value.entries[0].hot, false, 'snapshot clocks age hot streaks out');

  for (let index = 0; index < CREDIT_LEADERBOARD_MAX + 3; index++) {
    state.recordWin('win-pull', `extra-${index}`);
  }
  assert.equal(state.view.value.entries.length, CREDIT_LEADERBOARD_MAX);
});

test('credits state replaces map values instead of mutating published Signal state', async (t) => {
  const harness = await createPreactHarness(t);
  const { createCreditsState } = await harness.importDashboardModule('js/credits-state.js');
  const state = createCreditsState({ now: () => 1 });
  const beforeWins = state.winsByConv.value;
  const beforeTimes = state.winTimesByConv.value;
  state.recordWin('win-idle', 'conv');
  assert.notEqual(state.winsByConv.value, beforeWins);
  assert.notEqual(state.winTimesByConv.value, beforeTimes);
  assert.equal(beforeWins.size, 0);
  assert.equal(beforeTimes.size, 0);
});
