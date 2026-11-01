import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';
import { companionAgents, stewardAgent } from '../dashboard/js/companion-model.js';

const A = `agt_${'a'.repeat(32)}`;
const B = `agt_${'b'.repeat(32)}`;
const C = `agt_${'c'.repeat(32)}`;

function snapshot(active) {
  return {
    steward_name: 'Ada',
    steward_active: active,
    groups: [
      { name: 'team', members: [{ agent_id: B, title: 'worker', online: true }, { agent_id: C, title: 'other', online: true }] },
      { name: 'steward', scribe: true, members: [{ agent_id: A, title: 'steward-1', online: true }, { agent_id: B, title: 'worker', online: true }] },
    ],
  };
}

test('the seat, not the listing order, picks the Steward and carries the name', () => {
  const agents = companionAgents(snapshot(B));
  assert.equal(stewardAgent(agents).agentId, B);
  assert.equal(agents.get(B).name, 'Ada', 'a promoted agent listed first in its own group still takes the name');
  assert.equal(agents.get(A).name, 'steward-1', 'a Steward without the seat keeps its own');
  assert.equal(agents.get(B).scribe, true, 'it gets a companion tab like any Steward');
});

test('an offline Steward in the seat stays the Steward', () => {
  const snap = snapshot(B);
  snap.groups = snap.groups.map((group) => ({ ...group, members: group.members.map((m) => (m.agent_id === B ? { ...m, online: false } : m)) }));
  assert.equal(stewardAgent(companionAgents(snap)).agentId, B);
});

test('with nobody seated the newest behaviour stands', () => {
  const snap = snapshot('');
  snap.groups[1].members.pop();
  const agents = companionAgents(snap);
  assert.equal(stewardAgent(agents).agentId, A);
  assert.equal(agents.get(A).name, 'Ada');
});

async function roster(t) {
  const harness = await createPreactHarness(t);
  return { harness, m: await harness.importDashboardModule('js/steward-roster.js') };
}

test('a switch asks first only when the agent brings access above a Steward\'s', async (t) => {
  const { m } = await roster(t);
  assert.equal(m.holdsMoreThanSteward({ extra: {} }), false);
  assert.equal(m.holdsMoreThanSteward({ extra: { standing: ['agent.spawn'] } }), true);
  assert.equal(m.holdsMoreThanSteward({ extra: {}, extra_sudo: ['x'] }), true);
});

test('the roster offers running agents that are not already on it', async (t) => {
  const { m } = await roster(t);
  const agents = companionAgents(snapshot(B));
  assert.deepEqual(m.rosterCandidates(agents, [{ agent_id: A }, { agent_id: B }]).map((a) => a.agentId), [C]);
  assert.deepEqual(m.rosterCandidates(agents, [], 'oth').map((a) => a.agentId), [C]);
});

test('the summary says what became of the old Steward', async (t) => {
  const { m } = await roster(t);
  assert.equal(m.handoverSummary({ name: 'worker' }), 'worker is the Steward.');
  assert.equal(m.handoverSummary({ name: 'worker', previous: 'old', then: 'retire' }), 'worker is the Steward. old is retired.');
});

async function openRoster(t, preview) {
  const { harness, m } = await roster(t);
  const posts = [];
  const handedTo = [];
  const notes = [];
  const shown = [];
  const fetchImpl = async (url, init = {}) => {
    if (init.method === 'POST') posts.push({ url, body: JSON.parse(init.body) });
    return { ok: true, json: async () => (init.method === 'POST' ? { name: 'standby-one', previous: 'steward-1', then: 'roster' } : preview) };
  };
  const view = await harness.mount(harness.html`<${m.StewardRoster}
    roster=${[{ agent_id: A, name: 'steward-1', seat: 'active', online: true }, { agent_id: B, name: 'standby-one', seat: 'standby', online: false }]}
    agents=${companionAgents(snapshot(A))} fetchImpl=${fetchImpl} notify=${(text) => notes.push(text)}
    openHandover=${(id) => handedTo.push(id)} showAgent=${(id) => shown.push(id)} />`);
  const { container } = view;
  await harness.act(() => container.querySelector('.companion-steward-roster').click());
  const switchButton = [...container.querySelectorAll('button')].find((b) => b.textContent === 'Switch');
  await harness.act(async () => { switchButton.click(); await new Promise((r) => setTimeout(r, 0)); });
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  return { posts, handedTo, notes, shown, container };
}

test('one click switches back to a standby', async (t) => {
  const { posts, handedTo, notes, shown, container } = await openRoster(t, { extra: {} });
  assert.deepEqual(posts, [{ url: '/api/steward/handover', body: { agent: B, then: 'roster' } }]);
  assert.deepEqual(handedTo, []);
  assert.deepEqual(shown, [B]);
  assert.match(notes[0], /standby-one is the Steward\. steward-1 is on the roster/);
  assert.equal(container.querySelector('.steward-roster-menu'), null);
});

test('a standby that picked up more access goes through the confirmation', async (t) => {
  const { posts, handedTo } = await openRoster(t, { extra: { standing: ['agent.spawn'] } });
  assert.deepEqual(posts, []);
  assert.deepEqual(handedTo, [B]);
});

test('the dialog controller can open the confirmation', async (t) => {
  const harness = await createPreactHarness(t);
  const { createActionDialogState } = await harness.importDashboardModule('js/action-dialog-state.js');
  const { createActionDialogActions } = await harness.importDashboardModule('js/action-dialog-actions.js');
  const state = createActionDialogState();
  const actions = createActionDialogActions({ state, fetchImpl: async () => ({}), refresh: async () => {}, notify: () => {} });
  assert.equal(actions.openStewardHandover({ agent: A, label: 'worker' }), true);
  assert.equal(state.dialog.value.kind, 'steward-handover');
});
