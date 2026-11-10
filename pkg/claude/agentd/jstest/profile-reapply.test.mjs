import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const PREVIEW = {
  profile: 'worker',
  agents: [{
    agent_id: 'agt_one', conv_id: 'conv-one', title: 'One', online: true, tier: 'named', copied_at: '2026-11-01 09:00',
    fields: [
      { key: 'model', label: 'Model', from: '"opus"', to: '"sonnet"', status: 'kept (fixed)' },
      { key: 'auto_memory', label: 'Auto memory', from: 'false', to: 'true', status: 'changes' },
    ],
    grants: [{ slug: 'human.notify', from: '', to: 'grant', status: 'added' }],
  }],
  apply: { enabled: false, reason: 'Takes effect once relaunch reads the spawn copy (tcl-an26ed).' },
  grants_note: 'Grants: shown only, not applied.',
  notes: ['Usage profile links: not reapplied yet.'],
};

function json(body, status = 200) {
  return { ok: status < 400, status, text: async () => JSON.stringify(body) };
}

async function flush(harness, turns = 8) {
  await harness.act(async () => { for (let i = 0; i < turns; i += 1) await Promise.resolve(); });
}

function button(container, label) {
  return [...container.querySelectorAll('button')].find((b) => b.textContent.trim() === label);
}

test('profile re-apply: review shows the per-agent diff with Apply disabled and its reason', async (t) => {
  const harness = await createPreactHarness(t);
  const { ProfileReapplyControl } = await harness.importDashboardModule('js/profile-reapply.js');
  const calls = [];
  const fetchImpl = async (url, init = {}) => { calls.push({ url, method: init.method || 'GET' }); return json(PREVIEW); };
  const view = await harness.mount(harness.html`<${ProfileReapplyControl} profileName="my worker" fetchImpl=${fetchImpl} />`);

  await harness.act(async () => { button(view.container, 'Review running agents…').click(); });
  await flush(harness);
  assert.deepEqual(calls, [{ url: '/api/spawn-profiles/my%20worker/running-agents', method: 'GET' }], 'read-only: one GET, nothing written');
  const text = view.container.textContent;
  assert.match(text, /1 of 1 running agents from this profile selected/);
  assert.match(text, /Auto memory: false → true/);
  assert.match(text, /kept \(fixed\)/);
  assert.match(text, /human\.notify: unset → grant \(added\)/);
  assert.match(text, /Grants: shown only, not applied\./);
  assert.match(text, /Usage profile links: not reapplied yet\./);
  const apply = button(view.container, 'Apply to selected');
  assert.ok(apply, 'Apply is visible');
  assert.equal(apply.disabled, true, 'and disabled');
  assert.match(text, /tcl-an26ed/, 'with the reason');
});

test('profile re-apply: the save notice counts the agents that keep their copy', async (t) => {
  const harness = await createPreactHarness(t);
  const { savedProfileNotice } = await harness.importDashboardModule('js/profile-reapply.js');
  assert.equal(savedProfileNotice('w', { running_agents_with_copy: 0 }), 'profile updated: w');
  assert.equal(savedProfileNotice('w', {}), 'profile updated: w');
  assert.equal(savedProfileNotice('w', { running_agents_with_copy: 1 }), 'profile updated: w. Applies to new spawns; 1 running agent keeps its copy.');
  assert.equal(savedProfileNotice('w', { running_agents_with_copy: 3 }), 'profile updated: w. Applies to new spawns; 3 running agents keep their copy.');
});

test('profile re-apply: Apply confirms, posts one request per selected agent, and shows each outcome', async (t) => {
  const harness = await createPreactHarness(t);
  const { ProfileReapplyControl } = await harness.importDashboardModule('js/profile-reapply.js');
  const preview = {
    ...PREVIEW,
    agents: [
      { ...PREVIEW.agents[0], agent_id: 'agt_one', title: 'One', online: false },
      { ...PREVIEW.agents[0], agent_id: 'agt_two', title: 'Two', online: true },
      { ...PREVIEW.agents[0], agent_id: 'agt_three', title: 'Three', online: true },
    ],
    apply: { enabled: true, reason: 'A busy agent is skipped.' },
  };
  const posts = [];
  const fetchImpl = async (url, init = {}) => {
    if ((init.method || 'GET') === 'GET') return json(preview);
    const { agent_id: id } = JSON.parse(init.body);
    posts.push({ url, id });
    if (id === 'agt_one') return json({ agent_id: id, outcome: 'applied' });
    if (id === 'agt_two') return json({ agent_id: id, outcome: 'busy_skipped', detail: 'busy, skipped: status is working' });
    return json({ error: 'refused, so the agent was resumed with its old settings: lineage', outcome: 'restarted_unapplied', code: 'forbidden' }, 403);
  };
  const view = await harness.mount(harness.html`<${ProfileReapplyControl} profileName="worker" fetchImpl=${fetchImpl} />`);
  await harness.act(async () => { button(view.container, 'Review running agents…').click(); });
  await flush(harness);
  assert.match(view.container.textContent, /3 of 3 running agents from this profile selected/);

  await harness.act(async () => { button(view.container, 'Apply to selected').click(); });
  assert.match(view.container.textContent, /Re-apply to 3 agents\? Idle running agents are restarted\./);
  assert.equal(posts.length, 0, 'nothing is sent before the confirm');
  await harness.act(async () => { button(view.container, 'Cancel').click(); });
  assert.equal(posts.length, 0);

  await harness.act(async () => { button(view.container, 'Apply to selected').click(); });
  await harness.act(async () => { button(view.container, 'Apply to 3').click(); });
  await flush(harness, 24);
  assert.deepEqual(posts.map((p) => p.id), ['agt_one', 'agt_two', 'agt_three']);
  assert.ok(posts.every((p) => p.url === '/api/spawn-profiles/worker/reapply'));
  const text = view.container.textContent;
  assert.match(text, /applied; takes effect on its next launch/);
  assert.match(text, /busy, skipped: status is working/);
  assert.match(text, /refused, so the agent was resumed with its old settings: lineage/);
  assert.ok(button(view.container, 'Refresh'), 'a refresh re-reads the preview');
});
