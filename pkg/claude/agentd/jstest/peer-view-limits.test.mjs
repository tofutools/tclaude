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
    <button id="retire" data-act="retire-agent">retire</button><button id="dot" data-act="dot-toggle">dot</button>
    <button id="group-create-open">+ new group</button><button id="filter">filter</button>`;
  const snapshot = harness.signals.signal({ peer_view: pv });
  const toasts = [];
  doc.documentElement.dataset.remoteNodeName = 'forge';
  const dispose = mod.installPeerViewLimits({ doc, snapshot, toast: (m) => toasts.push(m), remote: { id: 'inst_forge7' } });
  const tab = (name) => doc.querySelector(`[data-tab="${name}"]`);
  assert.equal(tab('groups').classList.contains('pv-off'), false);
  assert.equal(tab('costs').classList.contains('pv-off'), true);
  assert.equal(tab('costs').getAttribute('data-pv-hint'), 'forge does not share costs with you');
  // linkedom does not order capture before target listeners, so assert the
  // guard's decision (the dashsnap peer-view state proves the browser order).
  const prevented = Object.fromEntries(['retire', 'dot', 'group-create-open', 'filter'].map((id) => [id, harness.fireEvent(doc.getElementById(id), 'click').defaultPrevented]));
  assert.deepEqual(prevented, { retire: true, dot: false, 'group-create-open': true, filter: false }, 'mutations and management controls are stopped; view-only controls work');
  assert.equal(toasts.length, 2);
  harness.fireEvent(tab('costs'), 'click');
  assert.equal(toasts.at(-1), 'forge does not share costs with you');
  snapshot.value = { peer_view: { ...pv, included: [...pv.included, 'costs'], omitted: [] } };
  assert.equal(tab('costs').classList.contains('pv-off'), false, 'a new grant lights the tab up on the next snapshot');
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
