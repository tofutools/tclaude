// tcl-fmd0ne: one agent picker for the message, access and job dialogs, on
// FilterCombobox, so hundreds of agents never render as one flat list.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function fleet(count) {
  const groups = ['crew', 'infra', 'web', 'ops'];
  return Array.from({ length: count }, (_, i) => ({
    agent_id: `agt_${String(i).padStart(3, '0')}`, conv_id: `conv-${i}`, title: `worker ${String(i).padStart(3, '0')}`,
    online: i % 3 !== 0, groups: [groups[i % groups.length]],
  }));
}

async function mountPicker(harness, snapshot, descriptor = { title: 'Pick target', identity: 'agent' }) {
  const { AgentPickerDialog } = await harness.importDashboardModule('js/agent-picker.js');
  const picked = [];
  let closed = 0;
  const mounted = await harness.mount(harness.html`<${AgentPickerDialog} descriptor=${descriptor} snapshot=${snapshot}
    sudo=${new Map([['agt_001', [{}]]])} onChoose=${(key) => picked.push(key)} onClose=${() => { closed++; }}
    confirmDiscard=${async () => true} />`);
  const root = mounted.container;
  return { root, picked, closed: () => closed, search: root.querySelector('#cron-pick-target-search') };
}

test('a fleet of hundreds opens to its groups, and typing finds an agent to pick', async (t) => {
  const harness = await createPreactHarness(t);
  const { root, picked, search } = await mountPicker(harness, { agents: fleet(300) });
  assert.equal(root.querySelectorAll('[role="option"]').length, 0, 'nothing typed: the groups, not 200 rows');
  assert.ok(root.querySelector('.filter-combobox-group-sum[data-group="crew"]'));
  await harness.input(search, 'worker 013');
  const rows = [...root.querySelectorAll('[role="option"]')];
  assert.ok(rows.length > 0 && rows.length <= 30, `at most the cap: ${rows.length}`);
  assert.equal(rows[0].dataset.key, 'agt_013');
  assert.ok(rows[0].querySelector('.online'), 'the online dot leads the row');
  assert.match(rows[0].querySelector('.groups-tag').textContent, /in: infra/);
  await harness.act(() => harness.fireEvent(search, 'keydown', { key: 'Enter' }));
  assert.deepEqual(picked, ['agt_013']);
});

test('offline agents join on request, conv identity picks the conversation, and sudo shows when asked', async (t) => {
  const harness = await createPreactHarness(t);
  const { root, picked, search } = await mountPicker(harness, { agents: fleet(6) }, { title: 'Pick sender', identity: 'conv', showSudo: true });
  const keys = () => [...root.querySelectorAll('[role="option"]')].map((row) => row.dataset.key);
  assert.deepEqual(keys().sort(), ['conv-1', 'conv-2', 'conv-4', 'conv-5'], 'online only, keyed by conversation');
  assert.ok(root.querySelector('[data-key="conv-1"] .sudo-badge'));
  const all = root.querySelector('#cron-pick-target-all');
  all.checked = true;
  await harness.act(() => harness.fireEvent(all, 'change'));
  assert.equal(keys().length, 6);
  await harness.input(search, 'conv-3');
  await harness.act(() => harness.fireEvent(root.querySelector('[data-key="conv-3"]'), 'mousedown'));
  assert.deepEqual(picked, ['conv-3']);
});

test('a scoped target picks among its group\'s members by typing, and shows the choice', async (t) => {
  const harness = await createPreactHarness(t);
  const { TargetPicker } = await harness.importDashboardModule('js/agent-picker.js');
  const snapshot = { groups: [{ name: 'crew', members: [
    { agent_id: 'agt_a', conv_id: 'c-a', title: 'alpha', online: true },
    { agent_id: 'agt_b', conv_id: 'c-b', title: 'beta', online: false },
  ] }] };
  function Holder() {
    const [value, setValue] = harness.hooks.useState({ mode: 'solo', target: 'agt_gone', groupName: '', scopeGroup: 'crew' });
    return harness.html`<${TargetPicker} prefix="t" value=${value} onChange=${setValue} snapshot=${snapshot} onPick=${() => {}} />`;
  }
  const { container } = await harness.mount(harness.html`<${Holder} />`);
  const box = container.querySelector('#t-scoped-member');
  assert.match(box.querySelector('.agent-combobox-chosen').textContent, /agt_gone.*missing/, 'a target no member has stays, marked');
  const input = container.querySelector('#t-scoped-member-input');
  await harness.act(() => harness.fireEvent(input, 'focus'));
  await harness.input(input, 'bet');
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter' }));
  assert.equal(container.querySelector('#t-scoped-member').dataset.value, 'agt_b');
  assert.match(container.querySelector('.agent-combobox-chosen').textContent, /beta/);
  await harness.act(() => harness.fireEvent(container.querySelector('.agent-combobox-clear'), 'click'));
  assert.equal(container.querySelector('#t-scoped-member').dataset.value, '');
});
