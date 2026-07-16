import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function flush(harness, turns = 8) {
  await harness.act(async () => {
    for (let turn = 0; turn < turns; turn += 1) await Promise.resolve();
  });
}

function choose(select, value) {
  for (const option of select.options) {
    if (option.value === value) option.setAttribute('selected', '');
    else option.removeAttribute('selected');
  }
  Object.defineProperty(select, 'value', {
    configurable: true, writable: true, value,
  });
}

test('policy dialog preserves its draft while live wizard copy changes', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ mountSpawnHarnessPolicyIsland }] = await Promise.all([
    harness.importDashboardModule('js/spawn-harness-policy-island.js'),
  ]);
  const view = {
    scope: 'global',
    harnesses: [
      { name: 'claude', display_name: 'Claude Code' },
      { name: 'codex', display_name: 'Codex CLI' },
    ],
    rules: [],
  };
  const previousFetch = globalThis.fetch;
  globalThis.fetch = async () => ({ ok: true, json: async () => view });
  t.after(() => {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  });

  const opener = harness.document.body.appendChild(harness.document.createElement('button'));
  opener.id = 'spawn-harness-policy-open';
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const cleanups = [];
  await harness.act(() => mountSpawnHarnessPolicyIsland({
    host,
    confirmDiscard: async () => true,
    notify: () => {},
    registerCleanup: (cleanup) => cleanups.push(cleanup),
  }));

  await harness.act(() => harness.fireEvent(opener, 'click'));
  await flush(harness);
  assert.equal(host.querySelector('#spawn-harness-policy-title').textContent,
    'Global cross-harness spawn policy');
  const select = host.querySelector('select[aria-label="claude to codex decision"]');
  assert.ok(select);
  choose(select, 'deny');
  await harness.act(() => harness.fireEvent(select, 'change'));
  const reason = host.querySelector('textarea[aria-label="claude to codex denial reason"]');
  assert.ok(reason, 'denial reason has a persistent accessible name');
  await harness.input(reason, 'reserve credits');

  harness.document.body.classList.add('wizard');
  await harness.act(() => harness.document.dispatchEvent(
    new harness.window.CustomEvent('tclaude:wizard', { detail: { active: true } }),
  ));
  assert.equal(host.querySelector('#spawn-harness-policy-title').textContent,
    'Global cross-realm summons');
  const themedSelect = host.querySelector('select[aria-label="claude to codex summoning ward"]');
  assert.equal(themedSelect.value, 'deny');
  assert.equal([...themedSelect.options].find((option) => option.value === 'deny').textContent, 'forbid');
  const themedReason = host.querySelector(
    'textarea[aria-label="claude to codex forbidden-summon reason"]',
  );
  assert.equal(themedReason.value, 'reserve credits');
  assert.equal(themedReason.placeholder, 'Reason revealed to the summoning familiar');

  await harness.act(() => cleanups.reverse().forEach((cleanup) => cleanup()));
  opener.remove();
  host.remove();
});
