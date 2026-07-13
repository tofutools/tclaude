import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('credits counter keeps its exact shell contract and bumps reactively', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createCreditsState }, { mountCreditsIsland }] = await Promise.all([
    harness.importDashboardModule('js/credits-state.js'),
    harness.importDashboardModule('js/credits-island.js'),
  ]);
  const state = createCreditsState({ now: () => 1 });
  const counterHost = harness.document.body.appendChild(harness.document.createElement('span'));
  const leaderboardHost = harness.document.body.appendChild(harness.document.createElement('div'));
  const cleanups = [];
  mountCreditsIsland({
    counterHost, leaderboardHost, state,
    registerCleanup: (cleanup) => cleanups.push(cleanup),
  });
  await harness.act(() => Promise.resolve());

  const counter = counterHost.querySelector('#slop-credits');
  assert.ok(counter);
  assert.equal(counter.className, 'slop-credits');
  assert.equal(counter.title, 'Slop credits — climbs on every jackpot this session');
  assert.equal(counter.textContent, '🪙 0');
  assert.equal(leaderboardHost.querySelector('.vegas-leaderboard-title').textContent.trim(),
    '🏆 High rollers');
  assert.match(leaderboardHost.querySelector('.vegas-leaderboard-empty').textContent,
    /No jackpots yet/);

  await harness.act(() => state.recordWin('win-mega'));
  assert.equal(counterHost.querySelector('#slop-credits'), counter, 'the shell node remains keyed in place');
  assert.equal(counter.textContent, '🪙 777');
  assert.ok(counter.classList.contains('slop-credits-bump'));

  await harness.act(() => state.recordWin('win-idle', 'conv'));
  assert.equal(counter.textContent, '🪙 877');
  assert.ok(counter.classList.contains('slop-credits-bump'), 'a later payout restarts the bump');

  assert.equal(cleanups.length, 2);
  await harness.act(() => cleanups.forEach((cleanup) => cleanup()));
  assert.equal(counterHost.childElementCount, 0);
  assert.equal(leaderboardHost.childElementCount, 0);
  counterHost.remove();
  leaderboardHost.remove();
});

test('credits event bridge feeds a keyed Preact leaderboard and cleans up listeners', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createCreditsState }, { mountCreditsIsland }, { bindSlopCredits }] = await Promise.all([
    harness.importDashboardModule('js/credits-state.js'),
    harness.importDashboardModule('js/credits-island.js'),
    harness.importDashboardModule('js/slop-credits.js'),
  ]);
  let clock = 100;
  let snapshot = { agents: [
    { conv_id: 'conv-a', title: 'Alice' },
    { conv_id: 'conv-b', title: 'Bob' },
  ] };
  const state = createCreditsState({ now: () => clock });
  const counterHost = harness.document.body.appendChild(harness.document.createElement('span'));
  const board = harness.document.body.appendChild(harness.document.createElement('div'));
  const islandCleanups = [];
  mountCreditsIsland({
    counterHost, leaderboardHost: board, state,
    registerCleanup: (task) => islandCleanups.push(task),
  });
  const registered = [];
  const cleanup = bindSlopCredits({
    state,
    documentRef: harness.document,
    getSnapshot: () => snapshot,
    schedule: (task) => task(),
    registerCleanup: (task) => registered.push(task),
  });
  assert.equal(registered[0], cleanup);

  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent(
    'tclaude:slopfx', { detail: { fx: 'win-idle', conv: 'conv-a' } },
  )));
  const row = board.querySelector('[data-key="conv-a"]');
  const whoText = row.querySelector('.who').firstChild;
  assert.equal(row.querySelector('.who').textContent, 'Alice');
  assert.equal(state.view.value.credits, 100);

  await harness.act(() => {
    harness.document.dispatchEvent(new harness.window.CustomEvent(
      'tclaude:slopfx', { detail: { fx: 'win-idle', conv: 'conv-b' } },
    ));
    harness.document.dispatchEvent(new harness.window.CustomEvent(
      'tclaude:slopfx', { detail: { fx: 'win-pull', conv: 'conv-b' } },
    ));
  });
  assert.equal(board.querySelector('[data-key="conv-a"]'), row);
  assert.equal(row.querySelector('.who').firstChild, whoText,
    'a keyed rank reorder preserves the existing browser-selection anchor');
  assert.equal(board.querySelector('li').dataset.key, 'conv-b', 'higher win count reorders by key');

  snapshot = { agents: [
    { conv_id: 'conv-a', title: 'Alicia' },
    { conv_id: 'conv-b', title: 'Bob' },
  ] };
  clock += 10;
  await harness.act(() => harness.document.dispatchEvent(
    new harness.window.CustomEvent('tclaude:snapshot'),
  ));
  assert.equal(board.querySelector('[data-key="conv-a"]'), row);
  assert.equal(row.querySelector('.who').textContent, 'Alicia');

  cleanup();
  assert.doesNotThrow(cleanup, 'cleanup is idempotent');
  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent(
    'tclaude:slopfx', { detail: { fx: 'win-mega' } },
  )));
  assert.equal(state.view.value.credits, 250, 'removed event listener cannot mutate state');
  await harness.act(() => islandCleanups.forEach((task) => task()));
  counterHost.remove();
  board.remove();
});
