// Tests for the collab board (tcl-4f86kp): the pure element model (merge,
// arrows, change summaries, export), the board list, the editor's sync with
// the daemon (operator edits PATCHed and announced, agent edits merged and
// shown) and the /boards routes.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const A = `agt_${'a'.repeat(32)}`;

function ok(body, status = 200) {
  return { ok: true, status, json: async () => body };
}

const wait = (ms = 0) => new Promise((r) => setTimeout(r, ms));

// The editor polls and takes keys only while the Boards section is active.
function boardsSection(harness, active = true) {
  const section = harness.window.document.createElement('section');
  section.id = 'tab-boards';
  if (active) section.classList.add('active');
  harness.window.document.body.appendChild(section);
  return section;
}

test('board model merges by version, then by lower nonce, and keeps tombstones', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/board-model.js');
  const map = new Map();
  const a1 = m.newElement('rectangle', { id: 'a', x: 0, y: 0, width: 10, height: 10 });
  assert.deepEqual(m.mergeElements(map, [a1]), ['a']);
  const a2 = m.bump(a1, { x: 5 });
  assert.deepEqual(m.mergeElements(map, [a1]), [], 'an older copy never replaces a newer one');
  assert.deepEqual(m.mergeElements(map, [a2]), ['a']);
  const tieLow = { ...a2, versionNonce: 1, x: 7 };
  const tieHigh = { ...a2, versionNonce: 2 ** 31 - 1, x: 9 };
  m.mergeElements(map, [tieLow, tieHigh]);
  assert.equal(map.get('a').x, 7, 'a tie goes to the lower versionNonce');
  assert.deepEqual(m.mergeElements(map, [m.bump(map.get('a'), { x: 1 })], new Set(['a'])), [], 'ids being dragged are skipped');
  m.mergeElements(map, [m.bump(map.get('a'), { isDeleted: true })]);
  assert.equal(m.liveElements(map).length, 0);
  assert.equal(map.size, 1, 'deletion is a tombstone');
  assert.equal(m.boardIdFromTitle('  Release Flow v2! '), 'release-flow-v2');
  assert.equal(m.isBoardId('Bad_Id'), false);
});

test('board model binds arrows to shapes and describes the operator edits in words', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/board-model.js');
  const box = m.newElement('rectangle', { id: 'box', x: 0, y: 0, width: 100, height: 50 });
  const label = m.newElement('text', { id: 'lbl', x: 20, y: 15, width: 60, height: 20, text: 'API' });
  const db = m.newElement('ellipse', { id: 'db', x: 300, y: 0, width: 80, height: 50 });
  const arrow = m.newElement('arrow', { id: 'ar', startBinding: { elementId: 'box' }, endBinding: { elementId: 'db' }, ...m.connectArrow(box, db) });
  assert.ok(arrow.x >= 100 && arrow.x < 110, `arrow starts at the box edge: ${arrow.x}`);
  const map = new Map([box, label, db, arrow].map((el) => [el.id, el]));
  const moved = { ...db, y: 200 };
  map.set('db', moved);
  const [rebound] = m.reboundArrows(map, ['db']);
  assert.equal(rebound.id, 'ar');
  assert.ok(rebound.points[1][1] > 100, 'the arrow follows the moved shape');
  const before = new Map([['db', db]]);
  const stroke = m.newElement('freedraw', { id: 'st', x: 40, y: 60, points: [[0, 0], [10, 4]], width: 10, height: 4 });
  map.set('st', stroke);
  before.set('st', undefined);
  assert.equal(m.describeChanges(map, ['db', 'st'], before), 'drew 1 freehand stroke near rectangle “API”; changed ellipse');
  const file = JSON.parse(m.toExcalidraw(m.liveElements(map)));
  assert.equal(file.type, 'excalidraw');
  assert.equal(file.elements.length, 5);
});

test('the board list creates a board and opens it', async (t) => {
  const harness = await createPreactHarness(t);
  const { BoardsApp, createBoardsState } = await harness.importDashboardModule('js/board-island.js');
  const requests = [];
  const fetchImpl = async (url, init = {}) => {
    requests.push({ url: String(url), method: init.method || 'GET', body: init.body ? JSON.parse(init.body) : null });
    if (url === '/api/boards' && !init.method) return ok({ boards: [{ id: 'old', title: 'Old board', element_count: 3, participants: [A] }] });
    if (url === '/api/boards') return ok({ id: 'release-flow', title: 'Release flow' }, 201);
    return ok({ id: 'release-flow', title: 'Release flow', version: 0, participants: [], elements: [] });
  };
  const state = createBoardsState();
  const snapshot = harness.signals.signal({ groups: [] });
  const view = await harness.mount(harness.html`<${BoardsApp} state=${state} snapshot=${snapshot} fetchImpl=${fetchImpl} win=${harness.window} />`);
  await harness.act(() => wait());
  assert.match(view.container.textContent, /Old board/);
  assert.match(view.container.textContent, /3 elements · 1 agent/);
  const input = view.container.querySelector('input[aria-label="New board title"]');
  await harness.input(input, 'Release flow');
  await harness.act(async () => {
    input.closest('form').dispatchEvent(new harness.window.Event('submit', { bubbles: true, cancelable: true }));
    await wait();
  });
  const create = requests.find((r) => r.method === 'POST');
  assert.deepEqual(create.body, { title: 'Release flow', id: 'release-flow' });
  assert.deepEqual(state.location.value, { tab: 'boards', selection: 'release-flow' });
  await harness.act(() => wait());
  assert.ok(view.container.querySelector('svg.board-canvas'), 'the editor opened');
});

// Preact listens for the lowercase event only where the element has an
// on<event> property; the test DOM's SVG elements do not, so Preact binds the
// event under its JSX casing (PointerDown) instead. Fire whichever it bound.
const JSX_CASE = { pointerdown: 'PointerDown', pointermove: 'PointerMove', pointerup: 'PointerUp' };
function pointer(harness, target, type, x, y) {
  if (!(`on${type}` in target)) type = JSX_CASE[type];
  harness.fireEvent(target, type, { button: 0, pointerId: 1, clientX: x, clientY: y });
}

test('drawing a rectangle PATCHes it as the operator and tells participating agents', async (t) => {
  const harness = await createPreactHarness(t);
  const { BoardsApp, createBoardsState, BOARD_FLUSH_MS, BOARD_EVENT_IDLE_MS } = await harness.importDashboardModule('js/board-island.js');
  const requests = [];
  const fetchImpl = async (url, init = {}) => {
    requests.push({ url: String(url), method: init.method || 'GET', body: init.body && typeof init.body === 'string' ? JSON.parse(init.body) : null });
    if (init.method === 'PATCH') return ok({ version: 2, accepted: 1 });
    if (init.method === 'POST') return ok({ sent: 1 }, 202);
    return ok({ id: 'plan', title: 'Plan', version: 1, participants: [A], elements: [] });
  };
  const state = createBoardsState();
  state.location.value = { tab: 'boards', selection: 'plan' };
  const snapshot = harness.signals.signal({ groups: [{ name: 'team', members: [{ agent_id: A, conv_id: 'c', title: 'alpha', online: true, state: { status: 'idle' } }] }] });
  const section = boardsSection(harness);
  const boxes = [];
  const rasterize = async (_svg, box) => { boxes.push(box); return new Blob(['PNG-region']); };
  const view = await harness.mount(harness.html`<${BoardsApp} state=${state} snapshot=${snapshot} fetchImpl=${fetchImpl} win=${harness.window} rasterize=${rasterize} />`);
  await harness.act(() => wait());
  assert.match(view.container.querySelector('.board-people').textContent, /alpha/);
  await harness.act(() => harness.getByRole(view.container, 'button', { name: 'Rectangle' }).click());
  const svg = view.container.querySelector('svg.board-canvas');
  await harness.act(() => pointer(harness, svg, 'pointerdown', 100, 100));
  await harness.act(() => pointer(harness, svg, 'pointermove', 220, 160));
  await harness.act(() => pointer(harness, svg, 'pointerup', 220, 160));
  assert.equal(svg.querySelectorAll('rect.board-el').length, 1);
  await harness.act(() => wait(BOARD_FLUSH_MS + 20));
  const patch = requests.find((r) => r.method === 'PATCH');
  assert.ok(patch, 'the new element was saved');
  assert.equal(patch.url, '/api/boards/plan');
  const [el] = patch.body.elements;
  assert.equal(el.type, 'rectangle');
  assert.deepEqual([el.width, el.height], [120, 60]);
  assert.equal(el.customData.author, 'operator');
  await harness.act(() => wait(BOARD_EVENT_IDLE_MS + 20));
  const event = requests.find((r) => r.url === '/api/boards/plan/events');
  assert.ok(event, 'the participants got an event');
  assert.equal(event.body.text, 'The operator drew rectangle.');
  // The event carries an image of the changed region with some context.
  assert.equal(atob(event.body.image), 'PNG-region');
  // The rectangle is at board (60, 60) 120x60 (the view starts at -40,-40).
  assert.deepEqual(boxes[0], { x: -20, y: -20, width: 280, height: 220 });
  // Undo is a new version that deletes it.
  // Board keys do nothing while another tab shows.
  section.classList.remove('active');
  await harness.act(() => harness.fireEvent(harness.window.document.body, 'keydown', { key: 'z', ctrlKey: true }));
  assert.equal(svg.querySelectorAll('rect.board-el').length, 1);
  section.classList.add('active');
  await harness.act(() => harness.fireEvent(harness.window.document.body, 'keydown', { key: 'z', ctrlKey: true }));
  assert.equal(svg.querySelectorAll('rect.board-el').length, 0, 'Ctrl+Z undoes the drawing');
});

test('an agent edit arriving on the poll is drawn and flashed', async (t) => {
  const harness = await createPreactHarness(t);
  const { BoardsApp, createBoardsState, BOARD_POLL_MS } = await harness.importDashboardModule('js/board-island.js');
  const m = await harness.importDashboardModule('js/board-model.js');
  const agentText = { ...m.newElement('text', { id: 'idea', x: 10, y: 10, text: 'Ship it?' }), customData: { author: A } };
  let polls = 0;
  const fetchImpl = async (url) => {
    if (String(url).includes('?since=')) {
      polls += 1;
      return ok({ id: 'plan', title: 'Plan', version: 2, participants: [A], elements: [agentText] });
    }
    return ok({ id: 'plan', title: 'Plan', version: 1, participants: [A], elements: [] });
  };
  const state = createBoardsState();
  state.location.value = { tab: 'boards', selection: 'plan' };
  const snapshot = harness.signals.signal({ groups: [] });
  const section = boardsSection(harness, false);
  const view = await harness.mount(harness.html`<${BoardsApp} state=${state} snapshot=${snapshot} fetchImpl=${fetchImpl} win=${harness.window} />`);
  await harness.act(() => wait(BOARD_POLL_MS + 50));
  assert.equal(polls, 0, 'no polling while another tab shows');
  section.classList.add('active');
  await harness.act(() => wait(BOARD_POLL_MS + 50));
  assert.ok(polls >= 1, 'the editor polled with ?since');
  const text = view.container.querySelector('text.board-el');
  assert.ok(text, 'the agent text is drawn');
  assert.match(text.textContent, /Ship it\?/);
  assert.ok(text.classList.contains('board-flash'), 'an agent edit is highlighted');
});

test('boards are routed under /boards with the open board as the selection', async (t) => {
  const harness = await createPreactHarness(t);
  const core = await harness.importDashboardModule('js/nav-history-core.js');
  assert.deepEqual(core.fromPath('/boards/plan', ''), { tab: 'boards', selection: 'plan' });
  assert.equal(core.toPath({ tab: 'boards', selection: 'plan' }), '/boards/plan');
  assert.equal(core.toPath({ tab: 'boards' }), '/boards');
});

test('board model: labels follow shapes, align and space evenly, copies rebind, clipboard round-trips', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/board-model.js');
  const map = new Map();
  const add = (el) => { map.set(el.id, el); return el; };
  const outer = add(m.newElement('rectangle', { id: 'outer', x: 0, y: 0, width: 400, height: 300 }));
  const inner = add(m.newElement('rectangle', { id: 'inner', x: 50, y: 50, width: 100, height: 60 }));
  add(m.newLabel(inner, 'API'));
  add(m.newElement('text', { id: 'note', x: 300, y: 250, text: 'x' }));
  assert.equal(map.get('inner-label').textAlign, 'center');
  assert.equal(m.labelOwners(map).get('inner-label'), 'inner', 'the innermost shape owns a label');
  assert.deepEqual(m.withLabels(map, ['inner']).sort(), ['inner', 'inner-label']);
  assert.equal(m.labelOf(map, 'inner').id, 'inner-label');
  assert.equal(m.labelOf(map, 'outer').id, 'note', 'a text inside a shape is its label too');
  assert.deepEqual(m.elementsInBox([...map.values()], { x: 40, y: 40, width: 120, height: 80 }).map((el) => el.id).sort(), ['inner', 'inner-label']);

  const boxes = [
    m.newElement('rectangle', { id: 'p', x: 0, y: 0, width: 10, height: 10 }),
    m.newElement('rectangle', { id: 'q', x: 50, y: 30, width: 20, height: 10 }),
    m.newElement('rectangle', { id: 'r', x: 100, y: 5, width: 10, height: 10 }),
  ];
  assert.deepEqual([...m.alignMoves(boxes, 'left')], [['q', { dx: -50, dy: 0 }], ['r', { dx: -100, dy: 0 }]]);
  assert.deepEqual(m.alignMoves(boxes, 'top').get('q'), { dx: 0, dy: -30 });
  assert.deepEqual(m.alignMoves(boxes, 'middle').get('p'), { dx: 0, dy: 15 });
  // Spacing evenly: 110 wide, 40 of boxes, so a 35 gap.
  assert.deepEqual(m.alignMoves(boxes, 'distribute-x').get('q'), { dx: -5, dy: 0 });

  const arrow = m.newElement('arrow', { id: 'ar', x: 0, y: 0, points: [[0, 0], [10, 0]], startBinding: { elementId: 'inner' }, endBinding: { elementId: 'outer' } });
  const copies = m.cloneElements([inner, map.get('inner-label'), arrow], 20, 10);
  const [ci, cl, ca] = copies;
  assert.notEqual(ci.id, 'inner');
  assert.equal(cl.id, `${ci.id}-label`, 'a copied label stays its shape\'s label');
  assert.deepEqual(ca.startBinding, { elementId: ci.id });
  assert.equal(ca.endBinding, undefined, 'a binding to an element not copied is dropped');
  assert.deepEqual([ci.x, ci.y, ci.version], [70, 60, 1]);

  const text = m.clipboardText([inner]);
  assert.equal(JSON.parse(text).type, 'excalidraw/clipboard');
  assert.equal(m.parseClipboard(text)[0].id, 'inner');
  assert.equal(m.parseClipboard('hello'), null);
  assert.equal(m.parseClipboard(JSON.stringify({ type: 'excalidraw/clipboard', elements: [{ type: 'image', x: 0, y: 0 }] })), null, 'unknown types are not pasted');
  // Pasted elements are cut down to what the daemon accepts.
  const [pasted] = m.parseClipboard(JSON.stringify({ type: 'excalidraw/clipboard', elements: [{
    id: 'x', type: 'text', x: 5, y: 5, text: 'a\r\nb\u200bc\u00a0d', fontSize: 900, fillStyle: 'cross-hatch', strokeColor: 'red', seed: 7,
  }] }));
  assert.deepEqual([pasted.text, pasted.fontSize, pasted.fillStyle, pasted.strokeColor, pasted.seed], ['a\nbc d', 200, 'solid', '#1e1e1e', undefined]);
  assert.equal(m.parseClipboard(JSON.stringify({ type: 'excalidraw/clipboard', elements: [{ type: 'line', x: 0, y: 0, points: 5 }] })), null, 'malformed points are not pasted');
  assert.equal(new TextEncoder().encode(m.cleanText('é'.repeat(3000))).length, 4000, 'text is cut by bytes');
  assert.equal(m.cloneElements([inner], 2e6, 0)[0].x, 1e6, 'a copy stays on the board');
});

test('the editor rubber-band selects, aligns, labels a shape on double-click and deletes it with its label', async (t) => {
  const harness = await createPreactHarness(t);
  const { BoardsApp, createBoardsState, BOARD_FLUSH_MS } = await harness.importDashboardModule('js/board-island.js');
  const m = await harness.importDashboardModule('js/board-model.js');
  const a = m.newElement('rectangle', { id: 'a', x: 100, y: 100, width: 100, height: 50 });
  const b = m.newElement('rectangle', { id: 'b', x: 300, y: 200, width: 80, height: 40 });
  const patches = [];
  const fetchImpl = async (url, init = {}) => {
    if (init.method === 'PATCH') { patches.push(...JSON.parse(init.body).elements); return ok({ version: 2, accepted: 1 }); }
    if (init.method === 'POST') return ok({}, 202);
    return ok({ id: 'plan', title: 'Plan', version: 1, participants: [], elements: String(url).includes('since') ? [] : [a, m.newLabel(a, 'API'), b] });
  };
  const state = createBoardsState();
  state.location.value = { tab: 'boards', selection: 'plan' };
  boardsSection(harness);
  const view = await harness.mount(harness.html`<${BoardsApp} state=${state} snapshot=${harness.signals.signal({ groups: [] })} fetchImpl=${fetchImpl} win=${harness.window} />`);
  await harness.act(() => wait());
  const svg = view.container.querySelector('svg.board-canvas');
  // The view starts at (-40, -40): client = board + 40.
  await harness.act(() => pointer(harness, svg, 'pointerdown', 50, 50));
  await harness.act(() => pointer(harness, svg, 'pointermove', 500, 400));
  assert.ok(svg.querySelector('.board-band'), 'the rubber band shows while dragging');
  await harness.act(() => pointer(harness, svg, 'pointerup', 500, 400));
  assert.equal(svg.querySelectorAll('.board-selection').length, 3, 'both shapes and the label are selected');
  await harness.act(() => harness.getByRole(view.container, 'button', { name: 'Align left' }).click());
  await harness.act(() => wait(BOARD_FLUSH_MS + 20));
  assert.deepEqual(patches.map((el) => [el.id, el.x]), [['b', 100]], 'the label follows its shape, which is already left');

  // Double-click a shape to label it, under the CLI's <id>-label id.
  const dbl = 'ondblclick' in svg ? 'dblclick' : 'DblClick';
  await harness.act(() => harness.fireEvent(svg, dbl, { clientX: 180, clientY: 260 }));
  const input = view.container.querySelector('textarea.board-text-input');
  assert.ok(input, 'a label editor opened');
  await harness.input(input, 'DB');
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter' }));
  await harness.act(() => wait(BOARD_FLUSH_MS + 20));
  const label = patches.find((el) => el.id === 'b-label');
  assert.ok(label, 'the label was saved');
  assert.equal(label.text, 'DB');

  // A label deleted on its own and written again revives its tombstone, so
  // the new text outranks the deletion.
  patches.length = 0;
  await harness.act(() => pointer(harness, svg, 'pointerdown', 165, 260));
  await harness.act(() => pointer(harness, svg, 'pointerup', 165, 260));
  await harness.act(() => harness.fireEvent(harness.window.document.body, 'keydown', { key: 'Delete' }));
  await harness.act(() => wait(BOARD_FLUSH_MS + 20));
  await harness.act(() => harness.fireEvent(svg, dbl, { clientX: 180, clientY: 260 }));
  await harness.input(view.container.querySelector('textarea.board-text-input'), 'Store');
  await harness.act(() => harness.fireEvent(view.container.querySelector('textarea.board-text-input'), 'keydown', { key: 'Enter' }));
  await harness.act(() => wait(BOARD_FLUSH_MS + 20));
  const revived = patches.filter((el) => el.id === 'b-label');
  assert.deepEqual(revived.map((el) => el.isDeleted), [true, false]);
  assert.ok(revived[1].version > revived[0].version);
  assert.equal(revived[1].text, 'Store');

  // Deleting the shape deletes its label.
  patches.length = 0;
  await harness.act(() => pointer(harness, svg, 'pointerdown', 141, 250));
  await harness.act(() => pointer(harness, svg, 'pointerup', 141, 250));
  await harness.act(() => harness.fireEvent(harness.window.document.body, 'keydown', { key: 'Delete' }));
  await harness.act(() => wait(BOARD_FLUSH_MS + 20));
  assert.deepEqual(patches.filter((el) => el.isDeleted).map((el) => el.id).sort(), ['b', 'b-label']);

  // Pasting Excalidraw clipboard elements adds copies at the pointer.
  patches.length = 0;
  const paste = new harness.window.Event('paste', { bubbles: true, cancelable: true });
  paste.clipboardData = { getData: () => m.clipboardText([a]) };
  await harness.act(() => { harness.window.document.body.dispatchEvent(paste); });
  await harness.act(() => wait(BOARD_FLUSH_MS + 20));
  assert.equal(patches.length, 1);
  assert.equal(patches[0].type, 'rectangle');
  assert.notEqual(patches[0].id, 'a');
  assert.deepEqual([patches[0].x, patches[0].y], [101, 210], 'placed at the last pointer position');
  await view.unmount();
});
