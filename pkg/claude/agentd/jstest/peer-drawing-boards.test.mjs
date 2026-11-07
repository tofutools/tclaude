// tcl-ug8bb8: a linked peer's collab drawing boards on that peer's dashboard page.
// The peer shares a drawing board through the drawing board's group (groups.resources.read
// reads it, .write edits it); the page reaches it through the peer proxy.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const wait = (ms = 0) => new Promise((r) => setTimeout(r, ms));
const respond = (body, status = 200) => ({ ok: status < 400, status, json: async () => body });
const OFFSET = 40;

const PEER_VIEW = {
  level: 'restricted', included: ['drawing-boards', 'drawing-boards.write', 'groups'],
  groups: { crew: ['groups.resources.write'], ops: ['groups.resources.read'], quiet: ['groups.roster.read'] },
};
const SNAP = { groups: [{ id: 11, name: 'crew' }, { id: 12, name: 'ops' }, { id: 13, name: 'quiet' }], peer_view: PEER_VIEW };

test('access follows the grant in the drawing board\'s group on the peer, and only on a peer\'s page', async (t) => {
  const harness = await createPreactHarness(t);
  const pb = await harness.importDashboardModule('js/peer-drawing-boards.js');
  assert.equal(pb.peerDrawingBoardsShared(SNAP, true), true);
  assert.equal(pb.peerDrawingBoardsShared(SNAP, false), false, 'this node\'s own page has no peer drawing boards');
  assert.equal(pb.peerDrawingBoardsShared({ peer_view: { level: 'restricted', included: ['groups'], omitted: ['drawing-boards'] } }, true), false);
  assert.equal(pb.peerDrawingBoardsShared({ peer_view: { level: 'unrestricted' } }, true), true);
  assert.equal(pb.peerDrawingBoardAccess('crew', SNAP, true), 'write');
  assert.equal(pb.peerDrawingBoardAccess('ops', SNAP, true), 'read');
  assert.equal(pb.peerDrawingBoardAccess('quiet', SNAP, true), '', 'another grant is no drawing board access');
  assert.equal(pb.peerDrawingBoardAccess('crew', SNAP, false), '');
  assert.equal(pb.peerDrawingBoardAccess('any', { peer_view: { level: 'unrestricted' } }, true), 'write');

  const listed = [];
  const fetchImpl = async (url) => { listed.push(url); return respond({ drawingBoards: [
    { id: 'plan', title: 'Plan', group: 'crew', agents_locked: true }, { id: 'map', title: 'Map', group: 'ops' }, { id: 'loose', title: 'Loose' },
  ] }); };
  await pb.loadPeerDrawingBoards(fetchImpl);
  assert.deepEqual(listed, ['/api/drawing-boards']);
  assert.deepEqual(pb.peerDrawingBoards.value.map((b) => b.id), ['plan', 'map'], 'a drawing board in no group is no peer\'s');
  assert.deepEqual(pb.peerDrawingBoardResources(pb.peerDrawingBoards.value, SNAP), [
    { kind: 'drawing_board', id: 'plan', group_id: 11, title: 'Plan', state: 'closed', locked: true, peer: true },
    { kind: 'drawing_board', id: 'map', group_id: 12, title: 'Map', state: 'closed', locked: false, peer: true },
  ], 'by the id the peer\'s snapshot lists its group by');
  await pb.loadPeerDrawingBoards(async () => respond({ code: 'peer_view_feature' }, 403));
  assert.deepEqual(pb.peerDrawingBoards.value, [], 'a refusal empties the list');
});

// mount renders the editor as on a peer's page, over a fake peer proxy.
async function mount(t, { group = 'crew', locked = false, patch = () => respond({ version: 2, accepted: 1 }), read = null } = {}) {
  const harness = await createPreactHarness(t);
  const island = await harness.importDashboardModule('js/drawing-board-island.js');
  const m = await harness.importDashboardModule('js/drawing-board-model.js');
  const calls = [];
  const notes = [];
  const fetchImpl = async (url, init = {}) => {
    const method = init.method || 'GET';
    calls.push(`${method} ${url}`);
    if (method === 'PATCH') return patch(init);
    if (method !== 'GET') return respond({ code: 'peer_view_local_only' }, 403);
    if (read) return read(url);
    return respond({ id: 'plan', title: 'Plan', version: 1, group, agents_locked: locked, participants: [],
      elements: String(url).includes('since') ? [] : [m.newElement('rectangle', { id: 'a', x: 100, y: 100, width: 100, height: 60 })] });
  };
  harness.window.document.documentElement.dataset.remoteNodeName = 'lab-b';
  const view = await harness.mount(harness.html`<${island.DrawingBoardEditor} drawingBoardId="plan" rasterize=${async () => new Uint8Array([1])}
    snapshot=${harness.signals.signal(SNAP)} fetchImpl=${fetchImpl} win=${harness.window} peer=${true} notify=${(...a) => notes.push(a)} />`);
  await harness.act(() => wait());
  const root = view.container;
  const svg = root.querySelector('svg.drawing-board-canvas');
  const fire = (type, x, y) => harness.act(() => harness.fireEvent(svg, { pointerdown: 'PointerDown', pointermove: 'PointerMove', pointerup: 'PointerUp' }[type],
    { button: 0, pointerId: 1, clientX: x + OFFSET, clientY: y + OFFSET }));
  const drag = async (from, to) => { await fire('pointerdown', ...from); await fire('pointermove', ...to); await fire('pointerup', ...to); };
  return { harness, island, root, svg, calls, notes, drag };
}

const tool = (root, label) => root.querySelector(`.drawing-board-toolbar [aria-label="${label}"]`);

test('a drawing board this node writes on the peer edits there, with no undo, snapshot, view or events', async (t) => {
  const { harness, island, root, calls, drag } = await mount(t);
  const banner = root.querySelector('.drawing-board-peer-banner');
  assert.match(banner.textContent, /Remote drawing board on lab-b · group crew · you can edit/);
  assert.equal(banner.dataset.drawingBoardPeer, 'write');
  assert.ok(tool(root, 'Rectangle'), 'the drawing tools are there');
  assert.equal(tool(root, 'Undo'), null, 'undo stays with the drawing board\'s own node');
  assert.equal(root.querySelector('.drawing-board-invite'), null, 'inviting its agents too');
  await harness.act(() => harness.fireEvent(tool(root, 'Rectangle'), 'click'));
  await drag([300, 300], [380, 360]);
  await harness.act(() => wait(island.DRAWING_BOARD_FLUSH_MS + 20));
  assert.equal(calls.filter((c) => c.startsWith('PATCH')).length, 1, 'the new shape is saved on the peer');
  await harness.act(() => wait(2700));
  assert.deepEqual(calls.filter((c) => !c.startsWith('GET') && !c.startsWith('PATCH')), [], 'no view, snapshot or events call');
});

test('a drawing board the peer shares read-only only pans and zooms', async (t) => {
  const { harness, island, root, calls, drag } = await mount(t, { group: 'ops' });
  const banner = root.querySelector('.drawing-board-peer-banner');
  assert.match(banner.textContent, /group ops · read-only/);
  assert.equal(banner.dataset.drawingBoardPeer, 'read');
  assert.equal(tool(root, 'Rectangle'), null, 'no drawing tools');
  assert.ok(root.querySelector('.drawing-board-zoom-controls'), 'zoom stays');
  await drag([150, 130], [400, 400]);
  await harness.act(() => harness.fireEvent(harness.window.document.body, 'keydown', { key: 'Delete' }));
  await harness.act(() => wait(island.DRAWING_BOARD_FLUSH_MS + 20));
  assert.equal(calls.filter((c) => c.startsWith('PATCH')).length, 0, 'nothing is sent');
});

test('a drawing board its peer\'s operator locked is read-only here', async (t) => {
  const { root } = await mount(t, { locked: true });
  assert.match(root.querySelector('.drawing-board-peer-banner').textContent, /read-only \(locked on lab-b\)/);
  assert.equal(tool(root, 'Rectangle'), null);
});

test('an edit the peer refuses turns the drawing board read-only instead of retrying', async (t) => {
  let patches = 0;
  const { harness, island, root, notes, drag } = await mount(t, {
    patch: () => { patches++; return respond({ message: 'resource_locked: the drawing board is locked' }, 403); },
  });
  await harness.act(() => harness.fireEvent(tool(root, 'Rectangle'), 'click'));
  await drag([300, 300], [380, 360]);
  await harness.act(() => wait(island.DRAWING_BOARD_FLUSH_MS + 20));
  assert.equal(patches, 1);
  assert.match(root.querySelector('.drawing-board-peer-banner').textContent, /read-only \(lab-b refused an edit\)/);
  assert.match(notes.at(-1)[0], /^lab-b refused your edit, so the drawing board is read-only here/);
  await harness.act(() => wait(2200));
  assert.equal(patches, 1, 'no retry');
});

test('a drawing board moved out of this node\'s groups on the peer says so', async (t) => {
  const { root } = await mount(t, { read: () => respond({ code: 'not_found', message: 'no such drawing board' }, 404) });
  assert.match(root.querySelector('.drawing-board-gone').textContent, /no longer shared with you on lab-b/);
});

// onPeer sets the page up as a peer's: its remote node and its snapshot.
async function onPeer(t, harness) {
  const prev = globalThis.__tclaudeRemoteNode;
  globalThis.__tclaudeRemoteNode = { id: 'inst_lab_b' };
  harness.window.document.documentElement.dataset.remoteNodeName = 'lab-b';
  const { dashboardState } = await harness.importDashboardModule('js/snapshot-store.js');
  const before = dashboardState.snapshot.value;
  dashboardState.snapshot.value = SNAP;
  t.after(() => { globalThis.__tclaudeRemoteNode = prev; dashboardState.snapshot.value = before; });
}

test('placement A: the Collab panel lists the peer\'s drawing boards under their groups, with what this node may do', async (t) => {
  const harness = await createPreactHarness(t);
  await onPeer(t, harness);
  const [rail, pb, prefs] = await Promise.all([
    harness.importDashboardModule('js/collab-rail.js'),
    harness.importDashboardModule('js/peer-drawing-boards.js'),
    harness.importDashboardModule('js/prefs.js'),
  ]);
  prefs.dashPrefs.setItem('tclaude.dash.collab.filter', JSON.stringify({ states: ['closed', 'open', 'background'], kinds: ['drawing-boards'] }));
  pb.peerDrawingBoards.value = [{ id: 'plan', title: 'Plan', group: 'crew' }, { id: 'map', title: 'Map', group: 'ops' }];
  const { dashboardState } = await harness.importDashboardModule('js/snapshot-store.js');
  const { container } = await harness.mount(harness.html`<${rail.CollabRail} panes=${[]} actions=${{}} snapshot=${dashboardState.snapshot} />`);
  const root = container.querySelector('[data-collab-rail]');
  const section = (name) => root.querySelector(`[data-rail-group="${name}"]`);
  assert.ok(section('crew').querySelector('[data-rail-item="drawing_board:plan"] [data-peer-drawing-board-access="write"]'));
  assert.ok(section('ops').querySelector('[data-rail-item="drawing_board:map"] [data-peer-drawing-board-access="read"]'));
  assert.ok(section('crew').querySelector('[data-peer-drawing-board-new="crew"]'), '＋ where this node writes');
  assert.equal(section('ops').querySelector('[data-peer-drawing-board-new]'), null, 'none where it only reads');
  assert.equal(root.querySelector('.cr-new'), null, 'no ＋ New: the header makes this node\'s things');
  assert.equal(root.querySelector('.cr-gear'), null, 'no ⚙: rename and move stay with the peer\'s own page');
  const plan = section('crew').querySelector('[data-rail-item="drawing_board:plan"]');
  await harness.act(() => harness.fireEvent(plan.querySelector('.cr-more'), 'click'));
  assert.equal(plan.querySelector('[data-rail-action="delete-drawing-board"]'), null, 'a peer\'s drawing board is deleted on its own node');
});

test('placement B: a peer group\'s Boards open here, and a group this node writes adds one', async (t) => {
  const harness = await createPreactHarness(t);
  await onPeer(t, harness);
  const [ui, pb] = await Promise.all([
    harness.importDashboardModule('js/peer-drawing-boards-ui.js'),
    harness.importDashboardModule('js/peer-drawing-boards.js'),
  ]);
  pb.peerDrawingBoards.value = [{ id: 'plan', title: 'Plan', group: 'crew', element_count: 14 }, { id: 'map', title: 'Map', group: 'ops' }];
  const opened = [];
  const made = [];
  const { container } = await harness.mount(harness.html`<div>
    <${ui.PeerGroupDrawingBoards} group=${{ name: 'crew' }} open=${(...a) => opened.push(a)} create=${(g) => made.push(g)} />
    <${ui.PeerGroupDrawingBoards} group=${{ name: 'ops' }} open=${(...a) => opened.push(a)} create=${(g) => made.push(g)} />
    <${ui.PeerGroupDrawingBoards} group=${{ name: 'quiet' }} />
  </div>`);
  const card = (name) => container.querySelector(`[data-peer-group-drawing-boards="${name}"]`);
  assert.match(card('crew').querySelector('[data-peer-drawing-board="plan"] .gr-meta').textContent, /drawing board · 14 elements/);
  assert.ok(card('crew').querySelector('[data-peer-drawing-board-access="write"]'));
  assert.ok(card('ops').querySelector('[data-peer-drawing-board-access="read"]'));
  assert.equal(card('quiet'), null, 'a group it cannot read lists none');
  await harness.act(() => harness.fireEvent(card('ops').querySelector('[data-peer-drawing-board-open="map"]'), 'click'));
  assert.deepEqual(opened, [['map', 'Map']]);
  assert.equal(card('ops').querySelector('[data-peer-drawing-board-new]'), null);
  await harness.act(() => harness.fireEvent(card('crew').querySelector('[data-peer-drawing-board-new="crew"]'), 'click'));
  assert.deepEqual(made, ['crew']);

  // A new drawing board takes its group, and a title no drawing board of the peer has.
  const sent = [];
  const fetchImpl = async (url, init = {}) => {
    if (init.method === 'POST') { sent.push(JSON.parse(init.body)); return respond({ id: 'crew-drawing-board', title: 'crew drawing board' }, 201); }
    return respond({ drawingBoards: [] });
  };
  const notes = [];
  const newOpen = [];
  pb.peerDrawingBoards.value = [{ id: 'x', title: 'crew drawing board', group: 'crew' }];
  await ui.createPeerDrawingBoard('crew', { fetchImpl, notify: (...a) => notes.push(a), open: (...a) => newOpen.push(a) });
  assert.deepEqual(sent, [{ title: 'crew drawing board 2', group: 'crew' }]);
  assert.deepEqual(newOpen, [['crew-drawing-board', 'crew drawing board']]);
  assert.match(notes[0][0], /^Created crew drawing board in crew on lab-b/);
});

test('a peer\'s drawing board dragged out of the Collab panel opens as a tab, not in this node\'s overlay', async (t) => {
  const harness = await createPreactHarness(t);
  await onPeer(t, harness);
  const [actions, overlays] = await Promise.all([
    harness.importDashboardModule('js/collab-rail-actions.js'),
    harness.importDashboardModule('js/overlay-instances.js'),
  ]);
  const item = { key: 'drawing_board:plan', title: 'Plan', view: 'closed', resource: { kind: 'drawing_board', id: 'plan', peer: true } };
  await actions.railItemToFloat(item, { x: 10, y: 10 }, { actions: {}, snapshot: { value: SNAP } });
  assert.equal(overlays.overlayTabs.value.some((tab) => tab.drawingBoard === 'plan'), false, 'no overlay window of this node\'s drawing board plan');
});
