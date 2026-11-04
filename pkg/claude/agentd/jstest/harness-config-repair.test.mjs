import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const AGENTS = [
  { agent_id: 'agt_wide', conv_id: 'conv-wide', title: 'Wide', requested: 'read', current: 'write', source: 'chain', change: 'widened', online: true },
  { agent_id: 'agt_loose', conv_id: 'conv-loose', title: 'Loose', requested: 'write', current: 'write', source: 'chain', change: 'unpinned', online: false },
];

function json(body, status = 200) {
  return { ok: status < 400, status, text: async () => JSON.stringify(body) };
}

async function flush(harness, turns = 8) {
  await harness.act(async () => { for (let i = 0; i < turns; i += 1) await Promise.resolve(); });
}

function button(container, label) {
  return [...container.querySelectorAll('button')].find((b) => b.textContent.trim() === label);
}

async function mountRepair(t) {
  const harness = await createPreactHarness(t);
  const { HarnessConfigRepairControl } = await harness.importDashboardModule('js/harness-config-repair.js');
  const calls = [];
  const notes = [];
  const fetchImpl = async (url, init = {}) => {
    calls.push({ url, method: init.method || 'GET', body: init.body ? JSON.parse(init.body) : null });
    if (url === '/api/harness-config-repairs' && !init.method) return json({ agents: AGENTS, limits: ['Sweeper spawns are not checked.'] });
    if (url === '/api/harness-config-repairs') {
      return json({ results: calls.at(-1).body.agent_ids.map((id) => ({ agent_id: id, repinned: true, online: id === 'agt_wide' })) });
    }
    return json({});
  };
  const view = await harness.mount(harness.html`<${HarnessConfigRepairControl} fetchImpl=${fetchImpl}
    notify=${(msg, err) => notes.push({ msg, err })} />`);
  return { harness, view, calls, notes };
}

test('harness_config repair: review previews every agent selected, with the limits', async (t) => {
  const { harness, view } = await mountRepair(t);
  await harness.act(() => button(view.container, 'Review agents').click());
  await flush(harness);
  const rows = view.container.querySelectorAll('.batch-preview-table tbody tr');
  assert.equal(rows.length, 2);
  assert.match(view.container.textContent, /2 of 2 agents selected/, 'all selected by default');
  assert.match(view.container.textContent, /write \(profile chain\)/);
  assert.match(view.container.textContent, /Sweeper spawns are not checked\./, 'the limits are shown');
});

test('harness_config repair: only the agents left selected are re-pinned, and a running one is offered a restart', async (t) => {
  const { harness, view, calls, notes } = await mountRepair(t);
  await harness.act(() => button(view.container, 'Review agents').click());
  await flush(harness);
  const loose = view.container.querySelector('tr[data-key="agt_loose"] input[type=checkbox]');
  loose.checked = false;
  await harness.act(() => harness.fireEvent(loose, 'change'));
  assert.match(view.container.textContent, /1 of 2 agents selected/);
  await harness.act(() => button(view.container, 'Restore selected').click());
  await flush(harness);
  const post = calls.find((c) => c.method === 'POST' && c.url === '/api/harness-config-repairs');
  assert.deepEqual(post.body, { agent_ids: ['agt_wide'] });
  assert.equal(notes.at(-1).err, false);

  await harness.act(() => button(view.container, 'Restart now').click());
  await flush(harness);
  assert.ok(calls.some((c) => c.method === 'POST' && c.url === '/api/agents/conv-wide/restart'), 'restart goes to the running agent');
  assert.ok(!calls.some((c) => c.url.includes('conv-loose')), 'never to a deselected one');
});

test('batch preview: select none disables the apply', async (t) => {
  const { harness, view } = await mountRepair(t);
  await harness.act(() => button(view.container, 'Review agents').click());
  await flush(harness);
  await harness.act(() => button(view.container, 'Select none').click());
  assert.equal(button(view.container, 'Restore selected').disabled, true);
});
