// The per-agent "remote watching / typing" badge (tcl-yuju57): the agent row
// and the local terminal header name the peer viewing that agent's terminal,
// read by the snapshot poll at most every 5 s and never on a peer's view.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const fleet = { self: { id: 'inst_self' }, peers: [{ id: 'inst_forge', name: 'forge' }, { id: 'inst_lab', name: 'lab' }] };
const watch = { id: 'v1', peer: 'inst_forge', agent: 'agt_a1', session: 'ada', read_only: true, incoming: true };
const type = { id: 'v2', peer: 'inst_lab', agent: 'agt_a1', session: 'ada', read_only: false, incoming: true };

async function load(t) {
  const harness = await createPreactHarness(t);
  const rv = await harness.importDashboardModule('js/remote-viewers.js');
  rv.resetViewersForTest();
  t.after(() => rv.resetViewersForTest());
  return { harness, rv };
}

test('the viewers read rides the snapshot tick: local federated node only, local tabs only, at most every 5 s', async (t) => {
  const { rv } = await load(t);
  const local = { remote: '', fleet, tab: 'groups' };
  assert.equal(rv.claimViewersRead(1000, { ...local, fleet: null }), false, 'not federated: no read');
  assert.equal(rv.claimViewersRead(1000, { ...local, remote: 'inst_forge' }), false, 'a peer\'s view: its viewers are not ours');
  assert.equal(rv.claimViewersRead(1000, { ...local, tab: 'fleet-admin' }), false, 'Fleet polls its own panel');
  assert.equal(rv.claimViewersRead(1000, local), true);
  assert.equal(rv.claimViewersRead(3000, local), false, 'throttled');
  assert.equal(rv.claimViewersRead(6000, local), true);

  rv.noteViewersRead([watch, { ...type, incoming: false }]);
  assert.deepEqual(rv.remoteViewers.value.map((v) => v.id), ['v1'], 'only incoming views');
  rv.noteViewersRead(null);
  assert.equal(rv.remoteViewers.value.length, 1, 'a failed read keeps the last list');
  rv.claimViewersRead(20000, { ...local, fleet: null });
  assert.equal(rv.remoteViewers.value.length, 0, 'leaving the federation drops the badges');
});

test('the badge names the peer, typing outranks watching, and a click opens the filtered Fleet viewers', async (t) => {
  const { harness, rv } = await load(t);
  const label = (id) => rv.peerLabel(id, fleet);
  const opened = [];
  const mount = (rows, extra = {}) => harness.mount(harness.html`<${rv.ViewersBadge} agentId="agt_a1" rows=${rows} label=${label} open=${(id) => opened.push(id)} ...${extra} />`);

  let m = await mount([watch]);
  let b = m.container.querySelector('.remote-viewers-badge');
  assert.equal(b.textContent, '👁 forge'); assert.ok(!b.classList.contains('typing'));
  assert.match(b.title, /Remote watching: forge watching/); assert.equal(b.getAttribute('aria-label'), b.title);
  await harness.act(() => b.click());
  assert.deepEqual(opened, ['agt_a1']);
  await m.unmount();

  m = await mount([watch, type], { className: 'term-viewers-badge' });
  b = m.container.querySelector('.remote-viewers-badge.typing.term-viewers-badge');
  assert.equal(b.textContent, '⌨ lab +1');
  assert.match(b.title, /forge watching, lab typing \(interactive\)/);
  await m.unmount();

  m = await mount([{ ...watch, agent: 'agt_other', session: 'other', peer: 'inst_new<b>' }]);
  assert.equal(m.container.querySelector('.remote-viewers-badge'), null, 'another agent\'s viewer is not ours');
  await m.unmount();
  assert.equal(rv.peerLabel('inst_<b>x', fleet), 'inst_<b>x', 'an unknown peer shows its ID');

  const clicks = [];
  const doc = { querySelector: (sel) => ({ click: () => clicks.push(sel) }) };
  rv.openViewers('agt_a1', doc);
  assert.equal(rv.viewersFocus.value, 'agt_a1');
  assert.deepEqual(clicks, ['nav [data-tab="fleet-admin"]']);
});

test('an online agent row carries the badge on its harness line', async (t) => {
  const { harness, rv } = await load(t);
  await harness.replaceDashboardModule('js/dashboard.js', `
    export const lastSnapshot = { groups: [], ungrouped: [] };
    export function setLastSnapshot() {}
  `);
  const { HarnessLine } = await harness.importDashboardModule('js/groups-member-table.js');
  rv.remoteViewers.value = [{ ...type, peer: 'inst_x<img src=x onerror=alert(1)>' }];
  const row = (online, state = { harness: 'claude' }) => harness.mount(harness.html`<${HarnessLine} member=${{ conv_id: 'c1', agent_id: 'agt_a1', online, state }} />`);
  let m = await row(true);
  const b = m.container.querySelector('.agent-harness .remote-viewers-badge.typing');
  assert.ok(b, 'shown even with no model yet');
  assert.match(b.textContent, /⌨ inst_x<img/, 'hub-relayed label is text, not HTML');
  assert.equal(m.container.querySelector('img'), null);
  await m.unmount();
  m = await row(true, { harness: 'claude', model: 'Opus 4.8' });
  assert.ok(m.container.querySelector('.agent-harness .remote-viewers-badge'));
  await m.unmount();
  m = await row(false);
  assert.equal(m.container.querySelector('.remote-viewers-badge'), null, 'an offline agent has no live view');
  await m.unmount();
});
