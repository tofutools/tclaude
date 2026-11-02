import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const snapshotWith = (entries, capacity = {}) => ({
  capacity_queue: {
    capacity: { max_live_agents: 8, used_agents: 8, when_full: 'queue', max_queued: 50, min_free_ram_mb: 1024, queued: entries.length, ...capacity },
    entries,
  },
});

test('the Spawn queue block lists waiting spawns in order with the capacity line, and cancels one', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/spawn-queue-block.js');
  const calls = [];
  let refreshed = 0;
  m.configureSpawnQueue({
    fetchImpl: async (url, init) => { calls.push([init.method, url]); return new Response('{}', { status: 200 }); },
    confirm: async () => true, notify: () => {}, refresh: () => { refreshed++; },
  });
  const snapshot = snapshotWith([
    { id: 4, name: 'test-runner', group: 'builders', requester_kind: 'agent', requested_by: 'lead', state: 'queued', position: 1, age_seconds: 250 },
    { id: 7, name: 'scout', group: 'research', requester_kind: 'federation', requested_by: 'bob-agent@carol (remote)', state: 'queued', position: 2, age_seconds: 30 },
  ]);
  const mounted = await harness.mount(harness.html`<${m.SpawnQueueBlock} snapshot=${snapshot} />`);
  const root = mounted.container;
  const line = root.querySelector('.spawn-queue-capacity').textContent;
  assert.match(line, /Spawn queue/);
  assert.match(line, /8 \/ 8 agents/);
  assert.match(line, /full/);
  assert.match(line, /⌛ 2 waiting/);
  assert.match(line, /RAM ≥ 1024 MB/);
  assert.match(line, /no ETA/);
  const rows = [...root.querySelectorAll('.spawn-queue-row')];
  assert.deepEqual(rows.map((r) => r.querySelector('.spawn-queue-pos').textContent), ['1', '2']);
  assert.match(rows[0].textContent, /test-runner.*builders.*lead.*4m/);
  assert.ok(rows[1].querySelector('.spawn-queue-remote'), 'a peer request is tagged remote');
  const cancel = rows[1].querySelector('button[data-act="spawn-queue-cancel"]');
  assert.ok(cancel.classList.contains('ui-btn'), 'a bordered ui-btn');
  harness.fireEvent(cancel, 'click');
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.deepEqual(calls, [['DELETE', '/api/spawn-queue/7']]);
  assert.equal(refreshed, 1);
  assert.equal(m.groupCapacityQueued(snapshot, 'builders'), 1);
  assert.equal(m.groupCapacityQueued(snapshot, 'other'), 0);
});

test('the block is absent without capacity_queue and a one-line meter with room when nothing waits', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/spawn-queue-block.js');
  const empty = await harness.mount(harness.html`<${m.SpawnQueueBlock} snapshot=${{}} />`);
  assert.equal(empty.container.querySelector('.spawn-queue-block'), null);
  const idle = await harness.mount(harness.html`<${m.SpawnQueueBlock} snapshot=${snapshotWith([], { used_agents: 5, when_full: 'refuse' })} />`);
  const block = idle.container.querySelector('.spawn-queue-block');
  assert.ok(block.classList.contains('idle'));
  assert.equal(block.querySelector('table'), null);
  assert.match(block.textContent, /Capacity.*5 \/ 8 agents.*room.*when full: refuse/);
  assert.doesNotMatch(block.textContent, /no ETA/);
  assert.equal(m.formatWait(3720), '1h 2m');
});
