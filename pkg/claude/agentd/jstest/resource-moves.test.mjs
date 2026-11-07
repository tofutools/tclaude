import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// tcl-eq8pq9: one set of move rules for the ⋯ menu, the Groups tab's
// resource drag and the Collab panel's drag.
const SNAP = {
  groups: [
    { id: 1, name: 'crew', members: [{ agent_id: 'agt_a' }] },
    { id: 2, name: 'lab', members: [{ agent_id: 'agt_a' }] },
    { id: 3, name: 'ops', members: [] },
  ],
  agents: [
    { agent_id: 'agt_a', conv_id: 'ca', title: 'alpha', groups: ['crew', 'lab'], spawn_group: 'crew' },
    { agent_id: 'agt_z', conv_id: 'cz', title: 'loner', groups: [] },
  ],
};

test('canMoveResource: a board anywhere or out of every group, a page only where it may live', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/resource-moves.js');
  const board = { kind: 'board', id: 'b', group: 'crew' };
  assert.equal(m.canMoveResource(board, 'ops', SNAP), true);
  assert.equal(m.canMoveResource(board, '', SNAP), true, 'a board may leave every group');
  assert.equal(m.canMoveResource(board, 'crew', SNAP), false, 'not to its own group');
  assert.equal(m.canMoveResource(board, 'gone', SNAP), false, 'not to a group that is not listed');
  assert.equal(m.canMoveResource({ kind: 'board', id: 'b', group: '' }, 'crew', SNAP), true, 'a loose board into a group');
  const page = { kind: 'page', id: 'p', group: 'crew', owner: 'agt_a', closed: false };
  assert.equal(m.canMoveResource(page, 'lab', SNAP), true, "an agent's page to a group its agent is in");
  assert.equal(m.canMoveResource(page, 'ops', SNAP), false, 'not to one it is not in');
  assert.equal(m.canMoveResource(page, '', SNAP), false, 'a page always has a group');
  const blank = { kind: 'page', id: 'q', group: 'crew', owner: '', closed: false };
  assert.equal(m.canMoveResource(blank, 'ops', SNAP), false, "a group's own page stays while it is open");
  assert.equal(m.canMoveResource({ ...blank, closed: true }, 'ops', SNAP), true, 'and moves once closed');
  assert.equal(m.canMoveResource({ kind: 'shell', id: 's', group: 'crew' }, 'ops', SNAP), false, 'a shell is its group\'s own');
});

test('moveResource calls the ⋯ menu\'s move and says how it went', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/resource-moves.js');
  const calls = [];
  const notes = [];
  const api = { moveBoard: async (...a) => calls.push(['board', ...a]), moveGroupPage: async (...a) => { calls.push(['page', ...a]); throw new Error('not_member'); } };
  const notify = (...a) => notes.push(a);
  assert.equal(await m.moveResource({ kind: 'board', id: 'b', label: 'Plan' }, '', { api, notify }), true);
  assert.equal(await m.moveResource({ kind: 'page', id: 'p', label: 'App' }, 'lab', { api, notify }), false);
  assert.deepEqual(calls, [['board', 'b', ''], ['page', 'p', 'lab']]);
  assert.deepEqual(notes, [['Moved Plan out of its group'], ['Could not move App: not_member', true]]);
});

// railSection builds a panel section as collab-rail.js renders one.
function railSection(doc, id) {
  const rail = doc.body.querySelector('[data-collab-rail]') || doc.body.appendChild(Object.assign(doc.createElement('div'), {}));
  rail.setAttribute('data-collab-rail', '');
  const section = rail.appendChild(doc.createElement('div'));
  section.className = 'cr-section';
  section.setAttribute('data-rail-group-id', String(id));
  return section.appendChild(doc.createElement('div'));
}

const agentItem = (agent, group) => ({ key: `pane:agent:${agent.agent_id}`, kind: 'terminal', title: agent.title, group, status: { agent } });

test('the Collab panel: a drop on another group\'s section moves an agent, board or page; anything else is no move', async (t) => {
  const harness = await createPreactHarness(t);
  const a = await harness.importDashboardModule('js/collab-rail-actions.js');
  const doc = harness.document;
  const crew = railSection(doc, 1);
  const lab = railSection(doc, 2);
  const ops = railSection(doc, 3);
  const none = railSection(doc, 'other');
  const stray = railSection(doc, 'other-sessions');
  const alpha = agentItem(SNAP.agents[0], 'crew');
  assert.equal(a.railGroupDrop(alpha, lab, SNAP, false).to, 'lab');
  assert.equal(a.railGroupDrop(alpha, crew, SNAP, false), null, 'its own group');
  assert.equal(a.railGroupDrop(alpha, none, SNAP, false).to, '', 'Not in a group takes it out');
  assert.equal(a.railGroupDrop(alpha, stray, SNAP, false), null, 'Other sessions is no group');
  assert.equal(a.railGroupDrop(alpha, doc.body, SNAP, false), null, 'outside every section');
  assert.equal(a.railGroupDrop(alpha, lab, SNAP, true), null, "a peer's page moves nothing");
  assert.equal(a.railGroupDrop({ ...alpha, pane: { seed: { remote: { peer: 'p', agent: 'x' } } } }, lab, SNAP, false), null, "another node's agent");
  const loner = agentItem(SNAP.agents[1], '');
  assert.equal(a.railGroupDrop(loner, none, SNAP, false), null, 'an ungrouped agent onto Not in a group');
  const board = { key: 'board:plan', kind: 'board', title: 'Plan', group: 'crew', resource: { kind: 'board', id: 'plan', state: 'closed' } };
  assert.deepEqual(a.railGroupDrop(board, none, SNAP, false).resource, { kind: 'board', id: 'plan', label: 'Plan', group: 'crew', owner: '', closed: true });
  const page = { key: 'page:p1', kind: 'page', title: 'App', group: 'crew', resource: { kind: 'page', id: 'p1', owner: 'agt_a', state: 'running' } };
  assert.equal(a.railGroupDrop(page, lab, SNAP, false).to, 'lab', "an agent's page to its agent's other group");
  assert.equal(a.railGroupDrop(page, ops, SNAP, false), null, 'as the ⋯ menu would not offer');
  const shell = { key: 'shell:crew', kind: 'shell', title: 'crew shell', group: 'crew', resource: { kind: 'shell', id: 'crew' } };
  assert.equal(a.railGroupDrop(shell, lab, SNAP, false), null, 'a group shell stays');
});

test('the Collab panel moves an agent through the Groups tab\'s drop operations, and a board as its ⋯ menu does', async (t) => {
  const harness = await createPreactHarness(t);
  const a = await harness.importDashboardModule('js/collab-rail-actions.js');
  const calls = [];
  const groupOps = {
    context: () => 'ctx',
    move: async (payload, to, ctx) => calls.push(['move', payload, to, ctx]),
    add: async (payload, to, ctx) => calls.push(['add', payload, to, ctx]),
    remove: async (payload, ctx) => calls.push(['remove', payload, ctx]),
  };
  const alpha = agentItem(SNAP.agents[0], 'crew');
  await a.railItemToGroup(alpha, { to: 'lab', agent: SNAP.agents[0] }, { groupOps });
  await a.railItemToGroup(alpha, { to: '', agent: SNAP.agents[0] }, { groupOps });
  const loner = agentItem(SNAP.agents[1], '');
  await a.railItemToGroup(loner, { to: 'ops', agent: SNAP.agents[1] }, { groupOps });
  const payload = { conv: 'ca', agent: 'agt_a', label: 'alpha', sourceGroup: 'crew', sourceUngrouped: false };
  assert.deepEqual(calls, [
    ['move', payload, 'lab', 'ctx'],
    ['remove', payload, 'ctx'],
    ['add', { conv: 'cz', agent: 'agt_z', label: 'loner', sourceGroup: '', sourceUngrouped: true }, 'ops', 'ctx'],
  ]);
  const moved = [];
  const api = { moveBoard: async (...args) => moved.push(args) };
  await a.railItemToGroup({ kind: 'board' }, { to: 'ops', resource: { kind: 'board', id: 'plan', label: 'Plan', group: 'crew' } }, { api });
  assert.deepEqual(moved, [['plan', 'ops']]);
});

test('a rail drag over another group\'s section outlines it, and the drop there moves the item', async (t) => {
  const harness = await createPreactHarness(t);
  const a = await harness.importDashboardModule('js/collab-rail-actions.js');
  const doc = harness.document;
  railSection(doc, 1);
  const lab = railSection(doc, 2);
  const source = doc.body.appendChild(doc.createElement('div'));
  const moved = [];
  const item = { key: 'board:plan', kind: 'board', title: 'Plan', group: 'crew', resource: { kind: 'board', id: 'plan', state: 'closed' } };
  const data = new Map();
  const dataTransfer = { setData: (k, v) => data.set(k, v), dropEffect: 'none', effectAllowed: 'all' };
  a.startRailDrag({ currentTarget: source, dataTransfer }, item, { snapshot: { value: SNAP }, notify: () => {} });
  const fire = (type, target) => {
    const event = new harness.window.Event(type, { bubbles: true, cancelable: true });
    Object.defineProperty(event, 'dataTransfer', { value: dataTransfer });
    target.dispatchEvent(event);
    return event;
  };
  const over = fire('dragover', lab);
  assert.equal(over.defaultPrevented, true, 'the section takes the drop');
  assert.ok(lab.parentElement.classList.contains('cr-drop-over'), 'and is outlined');
  // The drop runs the board move; its API is the process run API, which a
  // test cannot reach, so the route itself is pinned by the test above. Here
  // the drop only has to be taken and the outline cleared.
  const { processRunApi } = await harness.importDashboardModule('js/process-context.js');
  const real = processRunApi.moveBoard;
  processRunApi.moveBoard = async (...args) => moved.push(args);
  t.after(() => { processRunApi.moveBoard = real; });
  const dropped = fire('drop', lab);
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.equal(dropped.defaultPrevented, true);
  assert.equal(lab.parentElement.classList.contains('cr-drop-over'), false, 'the outline clears');
  assert.deepEqual(moved, [['plan', 'lab']]);
});

test('the Groups tab: a board or page row dragged onto another group\'s box moves it there', async (t) => {
  const harness = await createPreactHarness(t);
  const dnd = await harness.importDashboardModule('js/resource-dnd.js');
  const doc = harness.document;
  const box = (attr, name) => {
    const el = doc.body.appendChild(doc.createElement('details'));
    el.setAttribute(attr, name);
    return el.appendChild(doc.createElement('div'));
  };
  const crew = box('data-dnd-target-group', 'crew');
  const ops = box('data-dnd-target-group', 'ops');
  const ungrouped = box('data-dnd-target-ungrouped', '1');
  const row = crew.appendChild(doc.createElement('div'));
  row.setAttribute('data-dnd-resource', JSON.stringify({ kind: 'board', id: 'plan', label: 'Plan', group: 'crew' }));
  const button = row.appendChild(doc.createElement('button'));
  row.draggable = true;
  const moved = [];
  const notes = [];
  const unbind = dnd.bindResourceDnd(doc, { api: { moveBoard: async (...args) => moved.push(args) }, notify: (...a) => notes.push(a), snapshot: () => SNAP, refresh: () => {} });
  t.after(unbind);
  const dataTransfer = { setData: () => {}, types: ['application/x-tclaude-resource'], dropEffect: 'none', effectAllowed: 'all' };
  const member = { setData: () => {}, types: ['application/x-tclaude-member', 'text/plain'], dropEffect: 'none', effectAllowed: 'all' };
  const fire = (type, target, transfer = dataTransfer) => {
    const event = new harness.window.Event(type, { bubbles: true, cancelable: true });
    Object.defineProperty(event, 'dataTransfer', { value: transfer });
    target.dispatchEvent(event);
    return event;
  };
  fire('pointerdown', button);
  assert.equal(row.draggable, false, 'a press on a row control is a click');
  fire('pointerup', button);
  assert.equal(row.draggable, true);
  fire('dragstart', row);
  assert.equal(fire('dragover', crew).defaultPrevented, false, 'not its own group');
  assert.equal(fire('dragover', ops).defaultPrevented, true);
  assert.ok(ops.parentElement.classList.contains('dnd-drop-over'), 'the box is outlined');
  assert.equal(fire('dragover', ungrouped).defaultPrevented, true, 'a board may leave every group');
  assert.equal(ops.parentElement.classList.contains('dnd-drop-over'), false, 'one box at a time');
  fire('drop', ops);
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.deepEqual(moved, [['plan', 'ops']]);
  assert.deepEqual(notes, [['Moved Plan to ops']]);
  fire('dragend', row);
  assert.equal(doc.querySelector('.dnd-drop-over'), null);

  // A resource drag whose row a re-render detached never reaches the
  // document's dragend; a later member drag must not carry the board along.
  fire('dragstart', row);
  row.remove();
  const agentRow = ops.appendChild(doc.createElement('div'));
  fire('dragstart', agentRow, member);
  assert.equal(fire('dragover', crew, member).defaultPrevented, false, 'another drag is not a resource drag');
  fire('drop', crew, member);
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.deepEqual(moved, [['plan', 'ops']], 'no second move');
});

test('the Groups tab: a resource row with a 🗑 dragged onto the trash bin is that 🗑; one without is not offered the bin', async (t) => {
  const harness = await createPreactHarness(t);
  const dnd = await harness.importDashboardModule('js/resource-dnd.js');
  const doc = harness.document;
  const bin = doc.body.appendChild(doc.createElement('div'));
  bin.id = 'dnd-trash';
  const rowOf = (resource) => {
    const row = doc.body.appendChild(doc.createElement('div'));
    row.setAttribute('data-dnd-resource', JSON.stringify(resource));
    row.draggable = true;
    return row;
  };
  const shell = rowOf({ kind: 'shell', id: 'term:ag_1:start:', label: 'worker shell', group: 'crew' });
  const board = rowOf({ kind: 'board', id: 'plan', label: 'Plan', group: 'crew' });
  const removed = [];
  t.after(dnd.registerResourceRemover('shell:term:ag_1:start:', async () => removed.push('shell')));
  const unbind = dnd.bindResourceDnd(doc, { api: {}, notify: () => {}, snapshot: () => SNAP, refresh: () => {} });
  t.after(unbind);
  const transfer = { setData: () => {}, types: ['application/x-tclaude-resource'], dropEffect: 'none', effectAllowed: 'all' };
  const fire = (type, target) => {
    const event = new harness.window.Event(type, { bubbles: true, cancelable: true });
    Object.defineProperty(event, 'dataTransfer', { value: transfer });
    target.dispatchEvent(event);
    return event;
  };

  fire('dragstart', board);
  assert.equal(bin.classList.contains('show'), false, 'a row with no remover registered gets no bin');
  assert.equal(fire('dragover', bin).defaultPrevented, false);
  fire('dragend', board);

  fire('dragstart', shell);
  assert.ok(bin.classList.contains('show'), 'the bin shows for a row with a 🗑');
  assert.ok(bin.classList.contains('dnd-trash-template-mode'), 'and says Delete, not Retire');
  assert.equal(fire('dragover', bin).defaultPrevented, true);
  assert.ok(bin.classList.contains('dnd-drop-over'));
  fire('drop', bin);
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.deepEqual(removed, ['shell']);
  assert.equal(bin.classList.contains('show'), false, 'the bin hides after the drop');

  // A drag that ends in a move (dragged already let go) still hides the bin.
  t.after(dnd.registerResourceRemover('board:plan', async () => removed.push('board')));
  const ops = doc.body.appendChild(doc.createElement('details'));
  ops.setAttribute('data-dnd-target-group', 'ops');
  const opsBox = ops.appendChild(doc.createElement('div'));
  fire('dragstart', board);
  assert.ok(bin.classList.contains('show'));
  fire('dragover', opsBox);
  fire('drop', opsBox);
  fire('dragend', board);
  assert.equal(bin.classList.contains('show'), false, 'the bin hides after a move too');
  assert.equal(bin.classList.contains('dnd-trash-template-mode'), false);
});

test('a process moves by pinning it there and unpinning it here; Ctrl/Cmd copies a board or a process', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/resource-moves.js');
  const snap = { ...SNAP, group_resources: [{ kind: 'process', id: 'review', group_id: 2 }] };
  const proc = { kind: 'process', id: 'review', label: 'Review', group: 'crew' };
  assert.equal(m.canMoveResource(proc, 'ops', snap), true);
  assert.equal(m.canMoveResource(proc, 'lab', snap), false, 'not to a group that already lists it');
  assert.equal(m.canMoveResource(proc, '', snap), false, 'a process is always some group\'s');
  assert.equal(m.canCopyResource(proc, 'ops', snap), true);
  assert.equal(m.canCopyResource(proc, 'lab', snap), false);
  const board = { kind: 'board', id: 'plan', label: 'Plan', group: 'crew' };
  assert.equal(m.canCopyResource(board, 'crew', snap), true, 'a board copies into its own group too, as a second board');
  assert.equal(m.canCopyResource(board, '', snap), false, 'a copy goes into a group');
  assert.equal(m.canCopyResource({ kind: 'page', id: 'p', group: 'crew', owner: 'agt_a' }, 'lab', snap), false, 'a page does not copy');
  assert.equal(m.canCopyResource({ kind: 'shell', id: 'crew', group: 'crew' }, 'lab', snap), false, 'nor a shell');

  const calls = [];
  const notes = [];
  const api = {
    pinProcess: async (...a) => calls.push(['pin', ...a]),
    unpinProcess: async (...a) => calls.push(['unpin', ...a]),
    boardIds: async () => ['plan', 'plan-copy'],
    copyBoard: async (...a) => { calls.push(['copyBoard', ...a]); return { id: a[3], title: a[2] }; },
  };
  const notify = (msg) => notes.push(msg);
  assert.equal(await m.moveResource(proc, 'ops', { api, notify }), true);
  assert.deepEqual(calls.splice(0), [['pin', 'ops', 'review'], ['unpin', 'crew', 'review']], 'pinned there before it is unpinned here');
  assert.equal(await m.copyResource(proc, 'ops', { api, notify }), true);
  assert.deepEqual(calls.splice(0), [['pin', 'ops', 'review']], 'a copy only pins');
  assert.equal(await m.copyResource(board, 'ops', { api, notify }), true);
  assert.deepEqual(calls.splice(0), [['copyBoard', 'plan', 'ops', 'Plan (copy)', 'plan-copy-2']], 'a free id for the copy');
  assert.deepEqual(notes, ['Moved Review to ops', 'Added Review to ops', 'Copied Plan to ops as Plan (copy)']);
});

test('the Groups tab: a Ctrl/Cmd-drag onto a group box copies, tinted as a clone; a plain one moves', async (t) => {
  const harness = await createPreactHarness(t);
  const dnd = await harness.importDashboardModule('js/resource-dnd.js');
  const doc = harness.document;
  const box = (name) => {
    const el = doc.body.appendChild(doc.createElement('details'));
    el.setAttribute('data-dnd-target-group', name);
    return el.appendChild(doc.createElement('div'));
  };
  const crew = box('crew');
  const ops = box('ops');
  const row = crew.appendChild(doc.createElement('div'));
  row.setAttribute('data-dnd-resource', JSON.stringify({ kind: 'process', id: 'review', label: 'Review', group: 'crew' }));
  row.draggable = true;
  const calls = [];
  const api = { pinProcess: async (...a) => calls.push(['pin', ...a]), unpinProcess: async (...a) => calls.push(['unpin', ...a]) };
  t.after(dnd.bindResourceDnd(doc, { api, notify: () => {}, snapshot: () => SNAP, refresh: () => {} }));
  const fire = (type, target, keys = {}) => {
    const event = new harness.window.Event(type, { bubbles: true, cancelable: true });
    const transfer = { setData: () => {}, types: ['application/x-tclaude-resource'], dropEffect: 'none', effectAllowed: 'all' };
    Object.defineProperty(event, 'dataTransfer', { value: transfer });
    for (const [k, v] of Object.entries(keys)) Object.defineProperty(event, k, { value: v });
    target.dispatchEvent(event);
    return event;
  };
  const settle = () => harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));

  fire('dragstart', row);
  const over = fire('dragover', ops, { ctrlKey: true });
  assert.equal(over.defaultPrevented, true);
  assert.equal(over.dataTransfer.dropEffect, 'copy');
  assert.ok(ops.parentElement.classList.contains('dnd-effect-clone'), 'the box wears the clone tint');
  // macOS clears the modifier on the drop itself: the tint decides.
  fire('drop', ops);
  await settle();
  assert.deepEqual(calls.splice(0), [['pin', 'ops', 'review']], 'a copy pins it there and leaves it here');
  fire('dragend', row);

  fire('dragstart', row);
  assert.equal(fire('dragover', ops).dataTransfer.dropEffect, 'move');
  assert.equal(ops.parentElement.classList.contains('dnd-effect-clone'), false);
  fire('drop', ops);
  await settle();
  assert.deepEqual(calls.splice(0), [['pin', 'ops', 'review'], ['unpin', 'crew', 'review']], 'a plain drop moves it');
});
