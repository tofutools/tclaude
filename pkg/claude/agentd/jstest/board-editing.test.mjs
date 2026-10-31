// Tests for putting a graph together on a board (tcl-29ocuy): ends that snap to
// connection points or attach to the border, shapes that move and open their
// text from anywhere inside, and quick connect's choice of the next shape.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const wait = (ms = 0) => new Promise((r) => setTimeout(r, ms));
const ok = (body, status = 200) => ({ ok: true, status, json: async () => body });

// The view starts at (-40, -40): client = board + 40.
const OFFSET = 40;
const JSX_CASE = { pointerdown: 'PointerDown', pointermove: 'PointerMove', pointerup: 'PointerUp', dblclick: 'DblClick' };

async function setup(t, shapes) {
  const harness = await createPreactHarness(t);
  const island = await harness.importDashboardModule('js/board-island.js');
  const m = await harness.importDashboardModule('js/board-model.js');
  const elements = shapes(m);
  const patches = [];
  const fetchImpl = async (url, init = {}) => {
    if (init.method === 'PATCH') { patches.push(...JSON.parse(init.body).elements); return ok({ version: 2, accepted: 1 }); }
    if (init.method === 'POST') return ok({}, 202);
    return ok({ id: 'plan', title: 'Plan', version: 1, participants: [], elements: String(url).includes('since') ? [] : elements });
  };
  const section = harness.window.document.createElement('section');
  harness.window.document.body.appendChild(section);
  const view = await harness.mount(harness.html`<${island.BoardEditor} boardId="plan" rasterize=${null} snapshot=${harness.signals.signal({ groups: [] })} fetchImpl=${fetchImpl} win=${harness.window} />`);
  await harness.act(() => wait());
  const root = view.container;
  const svg = root.querySelector('svg.board-canvas');
  const fire = (type, x, y, init = {}) => harness.act(() => harness.fireEvent(svg, `on${type}` in svg ? type : JSX_CASE[type], {
    button: 0, pointerId: 1, clientX: x + OFFSET, clientY: y + OFFSET, ...init,
  }));
  const drag = async (from, to, init = {}) => {
    await fire('pointerdown', ...from, init);
    await fire('pointermove', ...to, init);
    await fire('pointerup', ...to, init);
  };
  const click = async (x, y, init) => { await fire('pointerdown', x, y, init); await fire('pointerup', x, y, init); };
  const saved = async () => {
    await harness.act(() => wait(island.BOARD_FLUSH_MS + 20));
    return patches.splice(0);
  };
  const key = (name, target = harness.window.document.body, init = {}) => harness.act(() => harness.fireEvent(target, 'keydown', { key: name, ...init }));
  return { harness, m, root, svg, fire, drag, click, saved, key };
}

const twoShapes = (m) => [
  m.newElement('rectangle', { id: 'a', x: 100, y: 100, width: 100, height: 60 }),
  m.newElement('rectangle', { id: 'b', x: 400, y: 100, width: 100, height: 60 }),
];

test('a line end takes a connection point unless the border key is held', async (t) => {
  const { svg, fire, drag, saved, root, click } = await setup(t, twoShapes);

  // Dropped inside the shape, near its top: the top connection point.
  await drag([200, 130], [450, 112]);
  const [line] = await saved();
  assert.deepEqual(line.endBinding, { elementId: 'b', fixedPoint: [0.5, 0] });

  // Moving the existing end snaps too, and shows what it would take.
  await fire('pointerdown', 450, 96);
  await fire('pointermove', 450, 150);
  assert.equal(svg.querySelectorAll('.board-snap').length, 4, 'the candidates show while dragging');
  assert.equal(svg.querySelectorAll('.board-snap.on').length, 1, 'the one it would take stands out');
  assert.match(root.textContent, /Hold Ctrl to attach to the border/);
  await fire('pointerup', 450, 150);
  const [moved] = await saved();
  assert.deepEqual(moved.endBinding, { elementId: 'b', fixedPoint: [0.5, 1] });

  // With Ctrl, or Cmd on a Mac, the end meets the border instead.
  for (const held of [{ ctrlKey: true }, { metaKey: true }]) {
    await fire('pointerdown', 450, 164);
    await fire('pointermove', 460, 120, held);
    assert.equal(svg.querySelectorAll('.board-snap').length, 0, 'no candidates over the border');
    assert.ok(svg.querySelector('.board-aim'), 'the shape stands out instead');
    await fire('pointerup', 460, 120, held);
    const [free] = await saved();
    assert.deepEqual(free.endBinding, { elementId: 'b' });
    await drag([free.x + free.points.at(-1)[0], free.y + free.points.at(-1)[1]], [450, 150]);
    await saved();
  }
  await click(300, 300);
});

test('pressing or letting go of the border key mid-drag shows at once', async (t) => {
  const { harness, svg, fire } = await setup(t, twoShapes);
  await fire('pointerdown', 200, 130);
  await fire('pointermove', 450, 130);
  assert.equal(svg.querySelectorAll('.board-snap.on').length, 1);
  await harness.act(() => harness.fireEvent(harness.window.document.body, 'keydown', { key: 'Control', ctrlKey: true }));
  assert.equal(svg.querySelectorAll('.board-snap').length, 0);
  await harness.act(() => harness.fireEvent(harness.window.document.body, 'keyup', { key: 'Control' }));
  assert.equal(svg.querySelectorAll('.board-snap.on').length, 1);
});

test('a saved line keeps how it was attached until an end is moved', async (t) => {
  const { svg, saved, fire, click } = await setup(t, (m) => [
    ...twoShapes(m),
    m.newElement('arrow', { id: 'old', x: 204, y: 130, points: [[0, 0], [192, 0]], startBinding: { elementId: 'a' }, endBinding: { elementId: 'b' } }),
  ]);
  const before = svg.querySelector('[data-element="old"] path').getAttribute('d');
  await click(300, 130);
  await fire('pointermove', 320, 300);
  assert.equal(svg.querySelector('[data-element="old"] path').getAttribute('d'), before);
  assert.equal(svg.querySelectorAll('.board-line-end.on').length, 2, 'attached at both ends');
  assert.deepEqual(await saved(), [], 'nothing rewrites it');
});

test('a shape moves from anywhere inside it, alone or with the selection', async (t) => {
  const { svg, drag, click, fire, saved } = await setup(t, (model) => [
    model.newElement('rectangle', { id: 'r', x: 100, y: 100, width: 100, height: 100 }),
    model.newElement('ellipse', { id: 'e', x: 300, y: 100, width: 100, height: 100 }),
    model.newElement('diamond', { id: 'd', x: 500, y: 100, width: 100, height: 100 }),
    model.newElement('rectangle', { id: 'inner', x: 115, y: 115, width: 50, height: 50, backgroundColor: '#ffc9c9' }),
  ]);
  // The middle of each unfilled shape.
  await drag([350, 150], [350, 250]);
  assert.deepEqual((await saved()).map((el) => [el.id, el.y]), [['e', 200]]);
  await drag([550, 150], [550, 250]);
  assert.deepEqual((await saved()).map((el) => [el.id, el.y]), [['d', 200]]);

  // A plain click selects and moves nothing.
  await click(180, 180);
  assert.deepEqual(await saved(), []);
  assert.equal(svg.querySelectorAll('.board-selection').length, 1);

  // What is drawn inside stays reachable.
  await drag([140, 140], [140, 340]);
  assert.deepEqual((await saved()).map((el) => el.id), ['inner']);

  // Corners of a diamond's box are not the diamond.
  await click(502, 202);
  assert.equal(svg.querySelectorAll('.board-selection').length, 0, 'the click fell on blank board');

  // The selection moves as one from inside any member.
  await fire('pointerdown', 90, 90);
  await fire('pointermove', 420, 380);
  await fire('pointerup', 420, 380);
  assert.equal(svg.querySelectorAll('.board-selection').length, 3);
  await drag([350, 250], [370, 250]);
  assert.deepEqual((await saved()).map((el) => el.id).sort(), ['e', 'inner', 'r'].sort());

  // Holding the border key leaves the inside of a frame to the rubber band.
  await click(800, 500);
  await drag([110, 110], [195, 195], { ctrlKey: true });
  assert.deepEqual(await saved(), []);
});

test('double-clicking anywhere inside a shape of any kind opens its text', async (t) => {
  const { root, fire, click, harness } = await setup(t, (model) => [
    model.newElement('rectangle', { id: 'r', x: 100, y: 100, width: 100, height: 100 }),
    model.newElement('ellipse', { id: 'e', x: 300, y: 100, width: 100, height: 100 }),
    model.newElement('diamond', { id: 'd', x: 500, y: 100, width: 100, height: 100, backgroundColor: '#ffc9c9' }),
  ]);
  for (const [x, y] of [[130, 120], [190, 190], [350, 150], [380, 150], [550, 150]]) {
    await click(x, y);
    await click(x, y);
    await fire('dblclick', x, y);
    const input = root.querySelector('textarea.board-text-input');
    assert.ok(input, `a label edit at ${x},${y}`);
    assert.ok(input.classList.contains('centred'), 'on the shape, not a free text');
    await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Escape' }));
  }
});

test('double-clicking a connection point labels the shape rather than adding one', async (t) => {
  const { root, svg, fire, click, saved } = await setup(t, twoShapes);
  await fire('pointermove', 200, 130);
  await click(200, 130);
  await click(200, 130);
  await fire('dblclick', 200, 130);
  assert.equal(svg.querySelector('.board-suggestion'), null);
  assert.ok(root.querySelector('textarea.board-text-input'));
  assert.deepEqual(await saved(), []);
});

test('quick connect asks for the next shape where the connector was dropped', async (t) => {
  const { harness, root, svg, drag, saved, key } = await setup(t, twoShapes);
  const menu = () => root.querySelector('.board-quick');
  const field = () => menu().querySelector('input');
  const rows = () => [...menu().querySelectorAll('button')].map((b) => b.textContent.trim());
  const type = (text) => harness.input(field(), text);

  await drag([200, 130], [300, 300]);
  assert.ok(menu(), 'the menu opens');
  assert.equal(harness.window.document.activeElement, field(), 'its field has the focus');
  assert.deepEqual(rows(), ['1Rectangle', '2Ellipse', '3Diamond']);
  assert.equal(menu().querySelector('button.on').textContent.trim(), '1Rectangle', 'the source shape\'s kind to begin with');
  assert.ok(svg.querySelector('.board-suggestion'), 'a preview shows behind it');

  // Escape leaves nothing.
  await key('Escape', field());
  assert.equal(menu(), null);
  assert.equal(svg.querySelector('.board-suggestion'), null);
  assert.deepEqual(await saved(), []);

  // Typing filters by name or word, and Enter takes the first left.
  await drag([200, 130], [300, 300]);
  await type('dec');
  assert.deepEqual(rows(), ['3Diamond']);
  await key('Enter', field());
  const first = await saved();
  assert.equal(menu(), null);
  const shape = first.find((el) => el.type === 'diamond');
  const link = first.find((el) => el.type === 'arrow');
  assert.deepEqual([shape.width, shape.height], [100, 60]);
  assert.deepEqual(link.startBinding, { elementId: 'a', fixedPoint: [1, 0.5] });
  assert.equal(link.endBinding.elementId, shape.id);
  assert.ok(link.endBinding.fixedPoint, 'at a connection point');
  assert.equal(svg.querySelectorAll('.board-selection').length, 1, 'the new shape is selected, ready to carry on');

  // That kind is now the default: one keystroke.
  await drag([200, 130], [300, 450]);
  assert.equal(menu().querySelector('button.on').textContent.trim(), '3Diamond');
  await key('Enter', field());
  assert.equal((await saved()).filter((el) => el.type === 'diamond').length, 1);

  // A digit picks by the number shown, and types nothing, and the board's own
  // number keys stay out of it.
  await drag([200, 130], [300, 600]);
  await type('e');
  await key('2', field());
  assert.equal((await saved()).filter((el) => el.type === 'ellipse').length, 1);
  assert.equal(root.querySelector('.board-tool.on').getAttribute('aria-label'), 'Select', 'the tool did not change');

  await drag([200, 130], [300, 700]);
  await key('9', field());
  assert.ok(menu(), 'a digit no row shows does nothing');
  assert.equal(field().value, '');
  await key('Escape', field());
});

test('clicking elsewhere cancels quick connect and the click still counts', async (t) => {
  const { harness, root, svg, drag, saved, click } = await setup(t, twoShapes);
  await drag([200, 130], [300, 300]);
  assert.ok(root.querySelector('.board-quick'));
  await harness.act(() => svg.dispatchEvent(new harness.window.Event('pointerdown', { bubbles: true, cancelable: true })));
  assert.equal(root.querySelector('.board-quick'), null);
  assert.equal(svg.querySelector('.board-suggestion'), null);
  await click(450, 130);
  assert.equal(svg.querySelectorAll('.board-selection').length, 1);
  assert.deepEqual(await saved(), []);
});

test('a connector dropped on blank board with the border key stays a free line', async (t) => {
  const { root, drag, saved } = await setup(t, twoShapes);
  await drag([200, 130], [300, 300], { ctrlKey: true });
  assert.equal(root.querySelector('.board-quick'), null);
  const [line] = await saved();
  assert.equal(line.endBinding, undefined);
  assert.equal(line.startBinding.elementId, 'a');
});

test('the keyboard builds a graph from the selected shape', async (t) => {
  const { harness, root, svg, click, saved, key } = await setup(t, twoShapes);
  const field = () => root.querySelector('.board-quick input');
  await click(150, 130);

  // An arrow key opens the menu on that side; Enter takes the default.
  await key('ArrowDown');
  assert.ok(field());
  await key('Enter', field());
  const made = await saved();
  const below = made.find((el) => el.type === 'rectangle');
  assert.deepEqual([below.x, below.y], [100, 240], 'a step out, in line with it');
  assert.deepEqual(made.find((el) => el.type === 'arrow').startBinding, { elementId: 'a', fixedPoint: [0.5, 1] });

  // The new shape is selected, so the same key carries on, then Enter labels it.
  await key('ArrowDown');
  await key('Enter', field());
  const [next] = (await saved()).filter((el) => el.type === 'rectangle');
  assert.equal(next.y, 380);
  await key('Enter');
  assert.ok(root.querySelector('textarea.board-text-input'), 'Enter opens the selected shape\'s text');
  await harness.act(() => harness.fireEvent(root.querySelector('textarea.board-text-input'), 'keydown', { key: 'Escape' }));

  // Branching from one shape puts the branches side by side.
  await click(150, 130);
  await key('ArrowDown');
  await key('Enter', field());
  const [branch] = (await saved()).filter((el) => el.type === 'rectangle');
  assert.equal(branch.y, 240);
  assert.notEqual(branch.x, 100);
  assert.equal(svg.querySelectorAll('.board-selection').length, 1);

  // Keys do nothing without one shape selected.
  await click(800, 500);
  await key('ArrowDown');
  assert.equal(field(), null);
});
