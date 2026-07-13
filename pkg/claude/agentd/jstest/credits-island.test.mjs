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
  const host = harness.document.body.appendChild(harness.document.createElement('span'));
  const cleanups = [];
  mountCreditsIsland({ host, state, registerCleanup: (cleanup) => cleanups.push(cleanup) });
  await harness.act(() => Promise.resolve());

  const counter = host.querySelector('#slop-credits');
  assert.ok(counter);
  assert.equal(counter.className, 'slop-credits');
  assert.equal(counter.title, 'Slop credits — climbs on every jackpot this session');
  assert.equal(counter.textContent, '🪙 0');

  await harness.act(() => state.recordWin('win-mega'));
  assert.equal(host.querySelector('#slop-credits'), counter, 'the shell node remains keyed in place');
  assert.equal(counter.textContent, '🪙 777');
  assert.ok(counter.classList.contains('slop-credits-bump'));

  await harness.act(() => state.recordWin('win-idle', 'conv'));
  assert.equal(counter.textContent, '🪙 877');
  assert.ok(counter.classList.contains('slop-credits-bump'), 'a later payout restarts the bump');

  assert.equal(cleanups.length, 1);
  await harness.act(() => cleanups[0]());
  assert.equal(host.childElementCount, 0);
  host.remove();
});

test('credits event bridge cleans up listeners and keeps leaderboard morph selection-safe', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createCreditsState }, { bindSlopCredits, renderCreditsLeaderboard }] = await Promise.all([
    harness.importDashboardModule('js/credits-state.js'),
    harness.importDashboardModule('js/slop-credits.js'),
  ]);
  let clock = 100;
  let snapshot = { agents: [{ conv_id: 'conv-a', title: 'Alice' }] };
  const state = createCreditsState({ now: () => clock });
  const board = harness.document.body.appendChild(harness.document.createElement('div'));
  board.id = 'vegas-leaderboard';
  const registered = [];
  const cleanup = bindSlopCredits({
    state,
    documentRef: harness.document,
    getSnapshot: () => snapshot,
    isSlopActiveImpl: () => true,
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

  renderCreditsLeaderboard({ state, documentRef: harness.document });
  assert.equal(board.querySelector('[data-key="conv-a"]'), row);
  assert.equal(row.querySelector('.who').firstChild, whoText,
    'unchanged name remains a browser-selection anchor');

  snapshot = { agents: [{ conv_id: 'conv-a', title: 'Alicia' }] };
  clock += 10;
  await harness.act(() => harness.document.dispatchEvent(
    new harness.window.CustomEvent('tclaude:snapshot'),
  ));
  assert.equal(board.querySelector('.who').textContent, 'Alicia');

  cleanup();
  assert.doesNotThrow(cleanup, 'cleanup is idempotent');
  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent(
    'tclaude:slopfx', { detail: { fx: 'win-mega' } },
  )));
  assert.equal(state.view.value.credits, 100, 'removed event listener cannot mutate state');
  board.remove();
});
