// The lanes view's own layout (groups-taskboard-lanes-layout.js): resizable
// columns and reorderable lanes and headings, per group and per viewer.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const row = (id, title, extra = {}) => ({ id, group: 'crew', title, stage: 'next', status: 'open', depends_on: [], ...extra });
const board = (group) => ({ group, group_id: 1, counts: { live: 1, next: 1 }, tasks: [row('tk_00000001', 'queued one')], closed: [] });
const members = () => [
  { agent_id: 'agt_lead', title: 'lead', role: 'lead', online: true, owner: true },
  { agent_id: 'agt_builder', title: 'builder', role: 'dev', online: true },
  { agent_id: 'agt_tester', title: 'tester', role: 'dev', online: true },
  { agent_id: 'agt_writer', title: 'writer', role: 'docs', online: true },
];

async function mountLanes(t, name, { by = null, groupMembers = members() } = {}) {
  const harness = await createPreactHarness(t);
  const { TaskboardSection } = await harness.importDashboardModule('js/groups-taskboard.js');
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  dashPrefs.setItem(`tclaude.dash.groups.taskboard.view.${name}`, 'lanes');
  dashPrefs.setItem(`tclaude.dash.groups.fused.taskboard.${name}`, '1');
  if (by) dashPrefs.setItem(`tclaude.dash.groups.taskboard.lanes-by.${name}`, by);
  const source = { context: harness.signals.signal({ groups: [board(name)] }), status: harness.signals.signal('ready'), attach: () => () => {}, refresh: async () => {} };
  const group = { name, members: groupMembers };
  const api = { capture: async () => ({}), create: async () => ({}), assign: async () => ({}) };
  const mounted = await harness.mount(harness.html`<${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} source=${source} api=${api} />`);
  const c = mounted.container;
  const lanes = () => [...c.querySelectorAll('[data-lane-row]')].map((el) => el.dataset.laneRow);
  const heads = () => [...c.querySelectorAll('[data-lane-group]')].map((el) => el.dataset.laneGroup);
  const transfer = () => ({ types: ['application/x-tclaude-lane'], setData() {}, effectAllowed: '', dropEffect: '' });
  const dragOnto = async (from, to) => {
    const dt = transfer();
    await harness.act(async () => { harness.fireEvent(from, 'dragstart', { dataTransfer: dt }); });
    const over = harness.fireEvent(to, 'dragover', { dataTransfer: dt, clientY: 0 });
    await harness.act(async () => { harness.fireEvent(to, 'drop', { dataTransfer: dt, clientY: 0 }); });
    await harness.act(async () => { harness.fireEvent(from, 'dragend', {}); });
    return over.defaultPrevented;
  };
  const head = (id) => c.querySelector(`[data-lane-agent="${id}"]`);
  return { harness, c, lanes, heads, head, dragOnto, dashPrefs, group };
}

test('resizeColumns trades width between neighbours and keeps the minimums', async (t) => {
  const harness = await createPreactHarness(t);
  const { resizeColumns, gridColumns, gridMinWidth, DEFAULT_LANE_PX, MIN_PX } = await harness.importDashboardModule('js/groups-taskboard-lanes-layout.js');
  const measured = [170, 200, 200, 200, 200, 200];
  assert.deepEqual(resizeColumns(measured, 0, 40), { lane: 210, stages: [200, 200, 200, 200, 200] }, 'the lane column grows into the stages');
  assert.equal(resizeColumns(measured, 0, -500).lane, MIN_PX.lane, 'not below its minimum');
  assert.equal(resizeColumns(measured, 0, 5000).lane, 170 + 1000 - 5 * MIN_PX.stage, 'nor past what the stages can give');
  assert.deepEqual(resizeColumns(measured, 2, 50).stages, [200, 250, 150, 200, 200], 'a stage edge trades with its right neighbour');
  assert.deepEqual(resizeColumns(measured, 2, 500).stages, [200, 400 - MIN_PX.stage, MIN_PX.stage, 200, 200]);
  assert.equal(gridColumns({ lane: 200, stages: [1, 2, 1, 1, 1] }),
    `200px minmax(${MIN_PX.stage}px, 1fr) minmax(${MIN_PX.stage}px, 2fr) minmax(${MIN_PX.stage}px, 1fr) minmax(${MIN_PX.stage}px, 1fr) minmax(${MIN_PX.stage}px, 1fr)`);
  assert.equal(gridColumns(null), '');
  assert.equal(gridMinWidth(null), DEFAULT_LANE_PX + 5 * MIN_PX.stage, 'the default grid\'s minimum width');
  assert.equal(gridMinWidth({ lane: 200, stages: [1, 2, 1, 1, 1] }), 200 + 5 * MIN_PX.stage, 'a stored lane width counts');
});

test('a stored order sorts lanes and headings; a lane it does not list appends at the end', async (t) => {
  const harness = await createPreactHarness(t);
  const { applyLaneOrder, moveBefore } = await harness.importDashboardModule('js/groups-taskboard-lanes-layout.js');
  const groups = [
    { key: 'role:lead', lanes: [{ id: 'a' }] },
    { key: 'role:dev', lanes: [{ id: 'b' }, { id: 'c' }, { id: 'new' }] },
    { key: 'former', lanes: [{ id: 'gone' }] },
  ];
  const out = applyLaneOrder(groups, { lanes: { role: ['c', 'b'] }, heads: { role: ['former', 'role:dev', 'role:lead'] } }, 'role');
  assert.deepEqual(out.map((g) => [g.key, g.lanes.map((l) => l.id)]),
    [['role:dev', ['c', 'b', 'new']], ['role:lead', ['a']], ['former', ['gone']]], 'former members stay last');
  assert.deepEqual(moveBefore(['a', 'b', 'c'], 'c', 'a'), ['c', 'a', 'b']);
  assert.deepEqual(moveBefore(['a', 'b', 'c'], 'a', 'c', true), ['b', 'c', 'a']);
  assert.deepEqual(moveBefore(['a', 'b'], 'x', 'a'), ['a', 'b'], 'an unknown key changes nothing');
});

test('dragging a lane head reorders the lanes, remembered per group; Unassigned stays first', async (t) => {
  const { harness, c, lanes, head, dragOnto, dashPrefs } = await mountLanes(t, 'tbo-lanes');
  assert.deepEqual(lanes(), ['operator', 'unassigned', 'agt_builder', 'agt_lead', 'agt_tester', 'agt_writer']);
  assert.equal(c.querySelector('[data-lane-row="unassigned"] .tb-lane-head').getAttribute('draggable'), null, 'Unassigned does not move');
  assert.equal(await dragOnto(head('agt_writer'), head('agt_builder')), true);
  assert.deepEqual(lanes(), ['operator', 'unassigned', 'agt_writer', 'agt_builder', 'agt_lead', 'agt_tester']);
  assert.deepEqual(JSON.parse(dashPrefs.getItem('tclaude.dash.groups.taskboard.lanes-order.tbo-lanes')).lanes,
    { agent: ['agt_writer', 'agt_builder', 'agt_lead', 'agt_tester'] }, 'kept for lanes by agent');
  // A task drag over a lane head is not a lane drag.
  const over = harness.fireEvent(head('agt_lead'), 'dragover', { dataTransfer: { types: ['application/x-tclaude-task'] } });
  assert.equal(over.defaultPrevented, false);
  assert.ok(c.querySelector('.tb-lanes-reset'), 'reset order shows once there is an order');
});

test('Alt+arrow moves a focused lane; reset order puts the default back', async (t) => {
  const { harness, c, lanes, head, dashPrefs } = await mountLanes(t, 'tbo-keys');
  await harness.act(async () => { harness.fireEvent(head('agt_tester'), 'keydown', { key: 'ArrowUp', altKey: true }); });
  assert.deepEqual(lanes(), ['operator', 'unassigned', 'agt_builder', 'agt_tester', 'agt_lead', 'agt_writer']);
  await harness.act(async () => { harness.fireEvent(head('agt_builder'), 'keydown', { key: 'ArrowDown', altKey: true }); });
  assert.deepEqual(lanes(), ['operator', 'unassigned', 'agt_tester', 'agt_builder', 'agt_lead', 'agt_writer']);
  await harness.act(async () => { harness.fireEvent(head('agt_tester'), 'keydown', { key: 'ArrowUp' }); });
  assert.deepEqual(lanes(), ['operator', 'unassigned', 'agt_tester', 'agt_builder', 'agt_lead', 'agt_writer'], 'without Alt nothing moves');

  // Focus stays on the moved head, so Alt+↑ can be pressed again.
  head('agt_writer').focus();
  await harness.act(async () => { harness.fireEvent(head('agt_writer'), 'keydown', { key: 'ArrowUp', altKey: true }); });
  assert.equal(harness.document.activeElement, head('agt_writer'), 'the moved head keeps the focus');
  await harness.act(async () => { harness.fireEvent(harness.document.activeElement, 'keydown', { key: 'ArrowUp', altKey: true }); });
  assert.deepEqual(lanes(), ['operator', 'unassigned', 'agt_tester', 'agt_writer', 'agt_builder', 'agt_lead']);

  const reset = c.querySelector('.tb-lanes-reset');
  assert.ok(reset);
  await harness.act(async () => { harness.fireEvent(reset, 'click'); });
  assert.deepEqual(lanes(), ['operator', 'unassigned', 'agt_builder', 'agt_lead', 'agt_tester', 'agt_writer']);
  assert.equal(dashPrefs.getItem('tclaude.dash.groups.taskboard.lanes-order.tbo-keys'), null);
  assert.equal(c.querySelector('.tb-lanes-reset'), null);
});

test('by role, lanes reorder within their heading and the headings reorder among themselves', async (t) => {
  const { c, lanes, heads, head, dragOnto } = await mountLanes(t, 'tbo-role', { by: 'role' });
  assert.deepEqual(heads(), ['role:lead', 'role:dev', 'role:docs']);
  assert.equal(await dragOnto(head('agt_tester'), head('agt_builder')), true);
  assert.deepEqual(lanes(), ['operator', 'unassigned', 'agt_lead', 'agt_tester', 'agt_builder', 'agt_writer']);
  assert.equal(await dragOnto(head('agt_writer'), head('agt_lead')), false, 'not into another heading');
  const group = (key) => c.querySelector(`[data-lane-group="${key}"]`);
  assert.equal(await dragOnto(group('role:docs'), group('role:lead')), true);
  assert.deepEqual(heads(), ['role:docs', 'role:lead', 'role:dev']);
  assert.deepEqual(lanes(), ['operator', 'unassigned', 'agt_writer', 'agt_lead', 'agt_tester', 'agt_builder']);
});

test('an order by role leaves the order by agent alone', async (t) => {
  const harness = await createPreactHarness(t);
  const { moveLane, laneOrder, applyLaneOrder } = await harness.importDashboardModule('js/groups-taskboard-lanes-layout.js');
  const byRole = [{ key: 'role:lead', label: 'lead', lanes: [{ id: 'lead' }] },
    { key: 'role:dev', label: 'dev', lanes: [{ id: 'builder' }, { id: 'tester' }] }];
  moveLane('tbo-split', byRole, 'role', 'tester', 'builder');
  const byAgent = [{ key: 'agents', lanes: [{ id: 'builder' }, { id: 'lead' }, { id: 'tester' }] }];
  assert.deepEqual(applyLaneOrder(byAgent, laneOrder('tbo-split'), 'agent')[0].lanes.map((l) => l.id), ['builder', 'lead', 'tester']);
  assert.deepEqual(applyLaneOrder(byRole, laneOrder('tbo-split'), 'role')[1].lanes.map((l) => l.id), ['tester', 'builder']);
});

test('by agent, the offline heading is not a heading to reorder', async (t) => {
  const offline = members().map((m) => (m.agent_id === 'agt_writer' ? { ...m, online: false } : m));
  const { c } = await mountLanes(t, 'tbo-offline', { groupMembers: offline });
  const heading = [...c.querySelectorAll('[data-lane-group]')];
  assert.ok(heading.length >= 1, 'the offline lanes have their heading');
  for (const el of heading) assert.equal(el.getAttribute('draggable'), null, `${el.getAttribute('data-lane-group')} stays put`);
});

test('a column edge resizes from the keyboard, is stored per group, and a double-click resets it', async (t) => {
  const { harness, c, dashPrefs } = await mountLanes(t, 'tbo-cols');
  // LinkeDOM lays nothing out: give the column heads their widths.
  const widths = [170, 200, 200, 200, 200, 200];
  [...c.querySelector('.tb-lane-columns').children].forEach((el, i) => { el.getBoundingClientRect = () => ({ width: widths[i], height: 20, top: 0 }); });
  const edges = [...c.querySelectorAll('.tb-col-resize')];
  assert.deepEqual(edges.map((el) => el.dataset.colResize), ['lane', 'ideas', 'next', 'progress', 'blocked'], 'every column but the last has a right edge');
  assert.equal(edges[0].getAttribute('role'), 'separator');
  assert.equal(edges[0].getAttribute('tabindex'), '0');
  await harness.act(async () => { harness.fireEvent(edges[0], 'keydown', { key: 'ArrowRight' }); });
  assert.deepEqual(JSON.parse(dashPrefs.getItem('tclaude.dash.groups.taskboard.lanes-widths.tbo-cols')), { lane: 186, stages: [200, 200, 200, 200, 200] });
  assert.match(c.querySelector('.tb-lanes').getAttribute('style'), /--tb-lane-cols: 186px minmax\(90px, 200fr\)/);
  await harness.act(async () => { harness.fireEvent(c.querySelector('.tb-col-resize[data-col-resize="next"]'), 'dblclick'); });
  assert.equal(dashPrefs.getItem('tclaude.dash.groups.taskboard.lanes-widths.tbo-cols'), null);
  assert.doesNotMatch(c.querySelector('.tb-lanes').getAttribute('style') || '', /--tb-lane-cols/);
});

// Fleet- and federation-wide: a linked node's group (name@node in the fused
// view) keeps its layout under that shown name, which the section's "reset
// order" reads.
test('on a linked node\'s group in the fused view, a lane reorder is kept and reset under its shown name', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ TaskboardSection }, { dashPrefs }, { setNodeSnapshotSource }] = await Promise.all([
    harness.importDashboardModule('js/groups-taskboard.js'),
    harness.importDashboardModule('js/prefs.js'),
    harness.importDashboardModule('js/node-api.js'),
  ]);
  setNodeSnapshotSource((node) => (node === 'nd_forge' ? { peer_view: { level: 'unrestricted' } } : null));
  const shown = 'tbo-peer@forge';
  dashPrefs.setItem(`tclaude.dash.groups.taskboard.view.${shown}`, 'lanes');
  dashPrefs.setItem(`tclaude.dash.groups.fused.taskboard.${shown}`, '1');
  const source = { context: harness.signals.signal({ groups: [board('tbo-peer')] }), status: harness.signals.signal('ready'), attach: () => () => {}, refresh: async () => {} };
  const api = { capture: async () => ({}), create: async () => ({}), assign: async () => ({}) };
  const group = { name: shown, members: members(), fleet_node: { id: 'nd_forge', name: 'forge', local: false } };
  const mounted = await harness.mount(harness.html`<${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} boards=${() => ({ source, api })} />`);
  const c = mounted.container;
  const lanes = () => [...c.querySelectorAll('[data-lane-row]')].map((el) => el.dataset.laneRow);
  await harness.act(async () => { harness.fireEvent(c.querySelector('[data-lane-agent="agt_writer"]'), 'keydown', { key: 'ArrowUp', altKey: true }); });
  assert.deepEqual(lanes(), ['operator', 'unassigned', 'agt_builder', 'agt_lead', 'agt_writer', 'agt_tester']);
  assert.ok(dashPrefs.getItem(`tclaude.dash.groups.taskboard.lanes-order.${shown}`), 'kept under the shown name');
  assert.equal(dashPrefs.getItem('tclaude.dash.groups.taskboard.lanes-order.tbo-peer'), null, 'not under a local group of the same name');
  const reset = c.querySelector('.tb-lanes-reset');
  assert.ok(reset, 'reset order shows');
  await harness.act(async () => { harness.fireEvent(reset, 'click'); });
  assert.deepEqual(lanes(), ['operator', 'unassigned', 'agt_builder', 'agt_lead', 'agt_tester', 'agt_writer']);
});
