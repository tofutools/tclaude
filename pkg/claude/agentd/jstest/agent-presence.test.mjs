import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const fleet = {
  self: { id: 'inst_a', name: 'node-a', color: '#58a6ff', local: true },
  peers: [
    { id: 'inst_forge', name: 'forge', color: '#f0883e', local: false },
    { id: 'inst_lab', name: 'lab', color: '#39c5cf', local: false },
  ],
};
const NOW = Date.parse('2026-10-11T12:00:00Z');
const minutesAgo = (m) => new Date(NOW - m * 60000).toISOString();

test('presence kinds: away at home, visiting elsewhere, back home for ten minutes', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/agent-presence.js');
  const away = { state: 'away', home_instance: 'inst_a', current_instance: 'inst_forge', hop_count: 2 };
  const visiting = { state: 'here', home_instance: 'inst_a', current_instance: 'inst_forge', hop_count: 2, arrived_at: minutesAgo(14) };
  const back = { state: 'here', home_instance: 'inst_a', current_instance: 'inst_a', predecessor_instance: 'inst_forge', hop_count: 2, arrived_at: minutesAgo(1) };
  assert.equal(m.presenceKind(away, NOW), 'away');
  assert.equal(m.presenceKind(visiting, NOW), 'visiting');
  assert.equal(m.presenceKind(back, NOW), 'back');
  assert.equal(m.presenceKind({ ...back, arrived_at: minutesAgo(11) }, NOW), '', 'the back-home tag fades after ten minutes');
  assert.equal(m.presenceKind({ ...back, arrived_at: '0001-01-01T00:00:00Z' }, NOW), '', 'an unset Go time is no arrival');
  assert.equal(m.presenceKind({ ...back, hop_count: 0 }, NOW), '', 'an agent that never left has no tag');
  assert.equal(m.presenceKind(null, NOW), '');

  const member = (p) => ({ agent_id: 'agt_b1d3', title: 'builder-1', federation_presence: p });
  const v = m.presenceView(member(visiting), fleet, NOW);
  assert.equal(v.label, 'away from node-a · hop 2');
  assert.equal(v.home.color, '#58a6ff', 'the badge uses the home node colour');
  assert.match(v.title, /arrived 14 min ago/);
  const a = m.presenceView(member(away), fleet, NOW);
  assert.equal(a.label, 'away → on forge · hop 2');
  assert.equal(a.at.name, 'forge');
  assert.equal(m.presenceView(member(back), fleet, NOW).label, 'back home · from forge · 2 hops');
  assert.equal(m.nodeRef('inst_unknown_node_id', fleet).name, 'inst_unknown_', 'an unknown node shows a short instance ID');
});

test('a group keeps an away placeholder row but counts it apart from members', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/prefs.js', `
    const values = new Map();
    export const dashPrefs = { getItem: (k) => values.get(k) ?? null, setItem: (k, v) => values.set(k, String(v)), removeItem: (k) => values.delete(k) };
  `);
  const [{ GroupsNativeList }, { GroupsInteractionProvider }, { dashPrefs }, { skynetState }, vm] = await Promise.all([
    harness.importDashboardModule('js/groups-list.js'),
    harness.importDashboardModule('js/groups-interactions.js'),
    harness.importDashboardModule('js/prefs.js'),
    harness.importDashboardModule('js/skynet-state.js'),
    harness.importDashboardModule('js/groups-view-model.js'),
  ]);
  skynetState.fleet.value = fleet;
  const now = new Date().toISOString();
  const members = [
    { conv_id: 'c1', agent_id: 'agt_1', title: 'builder-2', online: true, state: { status: 'idle' } },
    { conv_id: 'c2', agent_id: 'agt_b1d3', title: 'builder-1', online: false, state: {}, federation_presence: { state: 'away', home_instance: 'inst_a', current_instance: 'inst_forge', hop_count: 2 } },
    { conv_id: 'c3', agent_id: 'agt_3', title: 'returned', online: true, state: { status: 'idle' }, federation_presence: { state: 'here', home_instance: 'inst_a', current_instance: 'inst_a', predecessor_instance: 'inst_forge', hop_count: 1, arrived_at: now } },
  ];
  const group = { name: 'builders', members, online: 2 };

  const view = vm.groupMembersView(group, false);
  assert.deepEqual(view.members.map((x) => x.title), ['builder-2', 'returned'], 'the away row is not a member');
  assert.equal(view.away, 1);
  assert.deepEqual(view.visible.map((x) => x.title), ['builder-2', 'builder-1', 'returned'], 'hiding offline never hides the placeholder');
  assert.equal(view.hiddenOffline, 0);

  dashPrefs.setItem('tclaude.dash.group.builders', '1');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  await harness.mount(harness.html`<${GroupsInteractionProvider}><${GroupsNativeList} groups=${[group]} snapshot=${{ activity_bots: { regular: 'emoji', slop: 'off', wizard: 'off' }, links: [] }} actions=${{}} /><//>`, host);
  const summary = host.querySelector('details[data-group-key="builders"] > summary');
  assert.match(summary.textContent, /👥 2\/∞/, 'the header counts members only');
  assert.equal(summary.querySelector('.group-away-hint').textContent, '+1 away');
  const placeholder = host.querySelector('tr.away-placeholder');
  assert.ok(placeholder, 'the away agent keeps a placeholder row');
  assert.equal(placeholder.getAttribute('draggable'), null, 'a placeholder cannot be dragged');
  assert.equal(placeholder.querySelector('[data-presence="away"]').textContent, 'away → on forge · hop 2');
  assert.equal(placeholder.querySelector('.away-goto').textContent, 'open on forge ↗', 'one node\'s view links to where it runs');
  assert.equal(placeholder.querySelector('.row-actions'), null, 'no menu or terminal on a placeholder');
  assert.equal(host.querySelector('.presence-badge[data-presence="back"]').textContent, '⌂ back home · from forge · 1 hop');
  assert.ok(host.querySelector('tr.presence-row-back'));
  const modes = [...host.querySelectorAll('[data-act="send-elsewhere"][data-conv="c1"]')].map((b) => b.dataset.mode);
  assert.deepEqual(modes, ['move', 'teleport', 'clone', 'morph'], 'recall home shows only on a visiting agent');
  const recall = [...host.querySelectorAll('tr.away-placeholder [data-act="send-elsewhere"]')].map((b) => b.dataset.mode);
  assert.deepEqual(recall, ['home'], 'the away placeholder offers recall home');

  const fused = { ...group, name: 'builders@node-a', fleet_node: { id: 'inst_a', name: 'node-a', color: '#58a6ff', local: true } };
  dashPrefs.setItem('tclaude.dash.group.builders@node-a', '1');
  const host2 = harness.document.body.appendChild(harness.document.createElement('div'));
  await harness.mount(harness.html`<${GroupsInteractionProvider}><${GroupsNativeList} groups=${[fused]} snapshot=${{ activity_bots: { regular: 'emoji', slop: 'off', wizard: 'off' }, links: [] }} actions=${{}} /><//>`, host2);
  assert.equal(host2.querySelector('tr.away-placeholder .away-goto').textContent, '↓ show', 'a fused view scrolls to the visiting row');

  const peerGroup = { ...group, name: 'builders@forge', fleet_node: { id: 'inst_forge', name: 'forge', color: '#f0883e', local: false } };
  dashPrefs.setItem('tclaude.dash.group.builders@forge', '1');
  const host3 = harness.document.body.appendChild(harness.document.createElement('div'));
  await harness.mount(harness.html`<${GroupsInteractionProvider}><${GroupsNativeList} groups=${[peerGroup]} snapshot=${{ activity_bots: { regular: 'emoji', slop: 'off', wizard: 'off' }, links: [] }} actions=${{}} /><//>`, host3);
  assert.deepEqual([...host3.querySelectorAll('[data-act="send-elsewhere"][data-conv="c1"]')].map((b) => b.dataset.mode), ['move'], 'another node\'s agent can only be moved here');
});
