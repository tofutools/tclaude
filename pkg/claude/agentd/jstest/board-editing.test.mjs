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

test('text being edited is not also drawn under its editor', async (t) => {
  const { fire, root, svg } = await setup(t, (m) => [
    m.newElement('text', { id: 'note', x: 100, y: 100, text: 'old words', fontSize: 20 }),
    m.newElement('text', { id: 'other', x: 100, y: 200, text: 'other words', fontSize: 20 }),
  ]);
  const drawn = (id) => svg.querySelector(`[data-element="${id}"]`);

  await fire('dblclick', 110, 110);
  assert.equal(root.querySelector('.board-text-input').value, 'old words');
  assert.ok(drawn('note').classList.contains('board-editing'));
  assert.ok(!drawn('other').classList.contains('board-editing'), 'only the text in the editor is hidden');

  await fire('pointerdown', 500, 500);
  assert.equal(root.querySelector('.board-text-input'), null);
  assert.ok(!drawn('note').classList.contains('board-editing'), 'it comes back when the editor closes');
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

test('every way of opening the text editor gives it the keyboard', async (t) => {
  const { harness, fire, root, key, click, saved } = await setup(t, (m) => [
    m.newElement('rectangle', { id: 'box', x: 100, y: 100, width: 100, height: 60 }),
    m.newElement('text', { id: 'note', x: 400, y: 100, text: 'words', fontSize: 20 }),
  ]);
  const doc = harness.window.document;
  const input = () => root.querySelector('.board-text-input');
  const close = () => harness.act(() => harness.fireEvent(input(), 'keydown', { key: 'Escape' }));

  await fire('dblclick', 300, 300); // new text, past the one autofocus a page gets
  assert.equal(doc.activeElement, input());
  await close();

  await fire('dblclick', 410, 110); // existing text
  assert.equal(doc.activeElement, input());
  await close();

  await fire('dblclick', 150, 130); // a shape's label
  assert.equal(doc.activeElement, input());
  await close();

  await click(110, 100); // Enter on the selected shape
  await key('Enter');
  assert.equal(doc.activeElement, input());
  await close();

  await key('t');
  await fire('pointerdown', 300, 400);
  assert.equal(doc.activeElement, input());
  const canvas = root.querySelector('svg.board-canvas');
  let press;
  await harness.act(() => { press = harness.fireEvent(canvas, 'onmousedown' in canvas ? 'mousedown' : 'MouseDown'); });
  assert.ok(press.defaultPrevented, 'the press that opened it leaves the focus there');
  await close();
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

test('clicking a connection point out to blank board asks what goes there', async (t) => {
  const { harness, root, svg, fire, click, saved, key } = await setup(t, twoShapes);
  const menu = () => root.querySelector('.board-quick');
  const offerFrom = async () => { await fire('pointermove', 200, 130); await click(200, 130); await fire('pointermove', 300, 300); };

  // The press makes nothing, and the release opens the menu with the keys.
  await offerFrom();
  await fire('pointerdown', 300, 300);
  assert.equal(menu(), null, 'not until the release');
  assert.deepEqual(await saved(), []);
  await fire('pointerup', 300, 300);
  assert.ok(menu(), 'the menu opens');
  assert.equal(harness.window.document.activeElement, menu().querySelector('input'));
  assert.deepEqual(await saved(), [], 'it does not repeat the shape');

  await key('Escape', menu().querySelector('input'));
  assert.equal(menu(), null);
  assert.equal(svg.querySelector('.board-suggestion'), null);
  assert.deepEqual(await saved(), []);

  // Escape with the button down leaves nothing for the release to do.
  await offerFrom();
  await fire('pointerdown', 300, 300);
  await key('Escape');
  await fire('pointerup', 300, 300);
  assert.equal(menu(), null);
  assert.equal(svg.querySelector('.board-suggestion'), null);
  assert.deepEqual(await saved(), []);

  // Picking from it adds the shape and the line together.
  await offerFrom();
  await click(300, 300);
  await key('2', menu().querySelector('input'));
  const made = await saved();
  assert.deepEqual(made.map((el) => el.type).sort(), ['arrow', 'ellipse']);
});

test('a connector dropped on blank board with the border key stays a free line', async (t) => {
  const { root, drag, saved } = await setup(t, twoShapes);
  await drag([200, 130], [300, 300], { ctrlKey: true });
  assert.equal(root.querySelector('.board-quick'), null);
  const [line] = await saved();
  assert.equal(line.endBinding, undefined);
  assert.equal(line.startBinding.elementId, 'a');
});

test('the text controls style the selection, the open editor and the next text', async (t) => {
  const { harness, fire, root, svg, click, key, saved } = await setup(t, (m) => [
    m.newElement('rectangle', { id: 'box', x: 100, y: 100, width: 100, height: 60 }),
    m.newLabel({ id: 'box', x: 100, y: 100, width: 100, height: 60 }, 'label'),
    m.newElement('text', { id: 'note', x: 400, y: 100, text: 'words', fontSize: 17 }),
  ]);
  const control = (name) => root.querySelector(`[aria-label="${name}"]`);
  const choose = async (name, value) => {
    Object.defineProperty(control(name), 'value', { value: String(value), configurable: true }); // the test DOM's is read-only
    await harness.act(() => harness.fireEvent(control(name), 'change'));
  };
  const drawn = (id) => svg.querySelector(`[data-element="${id}"]`);
  assert.equal(control('Text size'), null, 'the controls show only for text');

  await click(410, 100); // the text: an agent's size is offered as itself
  assert.ok([...control('Text size').querySelectorAll('option')].some((o) => o.textContent === '17'));
  await choose('Text size', 32);
  await harness.act(() => control('Bold').click());
  const sized = await saved();
  assert.equal(sized.at(-1).fontSize, 32);
  assert.equal(sized.at(-1).fontWeight, 'bold');
  assert.equal(sized.at(-1).width, 0, 'the old measurement goes');
  assert.equal(drawn('note').getAttribute('font-size'), '32');
  assert.equal(drawn('note').getAttribute('font-weight'), 'bold');
  await harness.act(() => control('Bold').click());
  assert.equal((await saved()).at(-1).fontWeight, undefined, 'bold comes off again');

  await click(110, 100); // a shape: the text on it
  await choose('Font', 3);
  const [label] = await saved();
  assert.equal(label.id, 'box-label');
  assert.equal(label.fontFamily, 3);
  assert.match(drawn('box-label').getAttribute('font-family'), /monospace/);

  await key('Escape');
  await fire('dblclick', 410, 110); // an open editor keeps the keyboard
  const input = root.querySelector('.board-text-input');
  await harness.act(() => control('Italic').click());
  assert.equal(harness.window.document.activeElement, input);
  assert.match(input.getAttribute('style'), /font-style:\s*italic/);
  assert.deepEqual(await saved(), [], 'nothing is sent while it is open');
  await harness.act(() => harness.fireEvent(input, 'blur', { relatedTarget: control('Font') }));
  assert.ok(root.querySelector('.board-text-input'), 'moving to the controls does not close it');
  await harness.act(() => harness.fireEvent(input, 'blur', { relatedTarget: null }));
  assert.equal(root.querySelector('.board-text-input'), null);
  assert.equal((await saved()).at(-1).fontStyle, 'italic', 'a style alone is a change');

  // Everything chosen holds for the next text.
  await key('Escape');
  await fire('dblclick', 300, 400);
  const next = root.querySelector('.board-text-input');
  await harness.input(next, 'fresh');
  await harness.act(() => harness.fireEvent(next, 'keydown', { key: 'Enter' }));
  const made = (await saved()).at(-1);
  assert.deepEqual([made.text, made.fontSize, made.fontFamily, made.fontStyle], ['fresh', 32, 3, 'italic']);
});

test('pasted text keeps a style the board has and drops one it lacks', async (t) => {
  const { m } = await setup(t, () => []);
  const [kept, foreign] = m.parseClipboard(JSON.stringify({ type: 'excalidraw/clipboard', elements: [
    { id: 'a', type: 'text', x: 0, y: 0, text: 'a', fontFamily: 3, fontWeight: 'bold', fontStyle: 'italic' },
    { id: 'b', type: 'text', x: 0, y: 0, text: 'b', fontFamily: 8, fontWeight: '900' },
  ] }));
  assert.deepEqual([kept.fontFamily, kept.fontWeight, kept.fontStyle], [3, 'bold', 'italic']);
  assert.deepEqual([foreign.fontFamily, foreign.fontWeight], [undefined, undefined]);
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

// tcl-99j1u9: the right-click menu.
const menuRows = (root) => [...root.querySelectorAll('.board-context [role^="menuitem"]')].map((row) => row.textContent.trim());
const rowNamed = (root, name) => [...root.querySelectorAll('.board-context [role^="menuitem"]')].find((row) => row.textContent.includes(name));

function rightClick({ harness, svg }, x, y) {
  const type = 'oncontextmenu' in svg ? 'contextmenu' : 'ContextMenu';
  return harness.act(() => harness.fireEvent(svg, type, { button: 2, clientX: x + OFFSET, clientY: y + OFFSET }));
}

test('the context menu offers what applies to what was clicked', async (t) => {
  const env = await setup(t, (m) => [
    ...twoShapes(m),
    m.newElement('arrow', { id: 'link', x: 204, y: 130, points: [[0, 0], [192, 0]], startBinding: { elementId: 'a' }, endBinding: { elementId: 'b' } }),
    m.newElement('text', { id: 'note', x: 100, y: 300, text: 'note' }),
  ]);
  const { root, svg, key, click, saved } = env;

  await rightClick(env, 700, 500);
  assert.ok(rowNamed(root, 'Add ellipse here') && rowNamed(root, 'Paste here') && rowNamed(root, 'Select all'), 'blank board');
  assert.match(rowNamed(root, 'Select all').textContent, /Ctrl\+A/, 'a shortcut shows beside its action');
  assert.equal(rowNamed(root, 'Paste here').disabled, true, 'without a secure context the menu cannot read the clipboard');
  assert.match(root.querySelector('.board-context-note').textContent, /over plain HTTP except on localhost\. Use Ctrl\+V/, 'and says so');
  await key('Escape', root.querySelector('.board-context button'));
  assert.equal(root.querySelector('.board-context'), null, 'Escape closes it');

  await rightClick(env, 150, 130);
  assert.equal(svg.querySelectorAll('.board-selection').length, 1, 'the clicked shape is selected first');
  const shape = menuRows(root);
  assert.ok(shape.some((row) => row.includes('Edit text') && row.includes('Enter')));
  assert.ok(shape.includes('Change to ellipse') && !shape.includes('Change to rectangle'));
  assert.ok(shape.some((row) => row.includes('Bring to front')));
  await choose(env, 'Change to diamond');
  const [changed] = await saved();
  assert.equal(changed.type, 'diamond');
  assert.equal(svg.querySelector('[data-element="a"]').tagName.toLowerCase(), 'polygon');

  await rightClick(env, 300, 130);
  const line = menuRows(root);
  assert.ok(line.some((row) => row.includes('✓ Straight line')), 'the routing in use is ticked');
  assert.ok(line.some((row) => row.includes('Edit label')));
  assert.ok(line.includes('Remove arrowhead'));
  await choose(env, 'Remove arrowhead');
  assert.equal((await saved())[0].type, 'line');

  await rightClick(env, 110, 305);
  assert.ok(rowNamed(root, 'Edit text'));
  assert.equal(rowNamed(root, 'Change to'), undefined, 'text has no shape kind');
  await key('Escape', root.querySelector('.board-context button'));
  await click(700, 500);
});

// A menu row is clicked, which leaves the menu shut.
async function choose({ harness, root }, name) {
  await harness.act(() => rowNamed(root, name).click());
  await harness.act(() => wait());
}

test('the context menu acts on the selection, and selects what is outside it', async (t) => {
  const env = await setup(t, (m) => [...twoShapes(m), m.newElement('rectangle', { id: 'c', x: 100, y: 300, width: 100, height: 60 })]);
  const { root, svg, fire, saved } = env;
  await fire('pointerdown', 90, 90);
  await fire('pointermove', 510, 170);
  await fire('pointerup', 510, 170);
  assert.equal(svg.querySelectorAll('.board-selection').length, 2);

  await rightClick(env, 150, 130);
  assert.equal(svg.querySelectorAll('.board-selection').length, 2, 'inside the selection it stays');
  assert.ok(rowNamed(root, 'Align left'), 'with what a selection can do');
  assert.equal(rowNamed(root, 'Edit text'), undefined);
  await choose(env, 'Delete');
  assert.deepEqual((await saved()).filter((el) => el.isDeleted).map((el) => el.id).sort(), ['a', 'b']);

  await rightClick(env, 150, 330);
  assert.equal(svg.querySelectorAll('.board-selection').length, 1);
});

test('bring to front and send to back reorder what is drawn, labels with their shapes', async (t) => {
  const env = await setup(t, (m) => [
    ...twoShapes(m),
    m.newElement('text', { id: 'a-label', containerId: 'a', x: 120, y: 120, text: 'A' }),
    m.newElement('rectangle', { id: 'c', x: 100, y: 300, width: 100, height: 60, backgroundColor: '#ffc9c9' }),
  ]);
  const { svg, saved, m } = env;
  const order = () => [...svg.querySelectorAll('[data-board-viewport] > [data-element]')].map((node) => node.getAttribute('data-element'));
  assert.deepEqual(order(), ['a', 'b', 'a-label', 'c']);
  await rightClick(env, 150, 110);
  await choose(env, 'Bring to front');
  assert.deepEqual(order(), ['b', 'c', 'a', 'a-label']);
  const front = await saved();
  assert.deepEqual(front.map((el) => el.id).sort(), ['a', 'a-label'], 'nothing else is written');

  await rightClick(env, 450, 130);
  await choose(env, 'Bring to front');
  await rightClick(env, 150, 330);
  await choose(env, 'Send to back');
  assert.deepEqual(order(), ['c', 'a', 'a-label', 'b']);

  // A new shape from this editor lands on top of what was brought to front, and
  // a duplicate of something sent back does too.
  await rightClick(env, 900, 500);
  await choose(env, 'Add diamond here');
  assert.equal(order().at(-1), (await saved()).filter((el) => el.type === 'diamond').at(-1).id);
  await rightClick(env, 150, 330);
  await choose(env, 'Duplicate');
  const [copy] = (await saved()).filter((el) => el.id !== 'c');
  assert.equal(order().at(-1), copy.id);
});

test('the stack reads the same whatever a client still holds of deleted elements', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/board-model.js');
  const held = new Map(['a', 'b', 'c'].map((id) => [id, m.newElement('rectangle', { id })]));
  held.set('a', { ...held.get('a'), isDeleted: true });
  for (const el of m.stackOrder(held, ['b'], 'front')) held.set(el.id, el);

  // A fresh read leaves out what was deleted.
  const fresh = new Map([...held].filter(([, el]) => !el.isDeleted));
  for (const map of [held, fresh]) {
    map.set('d', m.newElement('rectangle', { id: 'd' }));
    map.set('b-label', m.newElement('text', { id: 'b-label', containerId: 'b', text: 'late' }));
    assert.deepEqual(m.liveElements(map).map((el) => el.id), ['c', 'd', 'b', 'b-label']);
  }
  assert.equal(m.boardTopZ(held), held.get('b').z);
  assert.ok(m.stackOrder(held, ['c'], 'back')[0].z < 0);
});

test('a context menu during an erase takes the erase back', async (t) => {
  const env = await setup(t, twoShapes);
  const { svg, fire, key, saved } = env;
  await key('e');
  await fire('pointerdown', 100, 130);
  assert.equal(svg.querySelector('[data-element="a"]'), null, 'it goes as the eraser passes');
  await rightClick(env, 100, 130);
  assert.ok(svg.querySelector('[data-element="a"]'), 'and comes back, as nothing was saved');
  await fire('pointerup', 100, 130);
  assert.deepEqual(await saved(), []);
});

test('the context menu moves with the arrow keys, runs on Enter and closes on a click away', async (t) => {
  const env = await setup(t, twoShapes);
  const { harness, root, svg } = env;
  await rightClick(env, 700, 500);
  const doc = harness.window.document;
  const rows = [...root.querySelectorAll('.board-context button:not(:disabled)')];
  assert.equal(doc.activeElement, rows[0], 'the first row has the focus');
  await env.key('ArrowDown', doc.activeElement);
  assert.equal(doc.activeElement, rows[1]);
  await env.key('ArrowUp', doc.activeElement);
  await env.key('ArrowUp', doc.activeElement);
  assert.equal(doc.activeElement, rows.at(-1), 'it wraps');
  await env.key('3', doc.activeElement);
  assert.equal(root.querySelector('.board-tool.on').getAttribute('aria-label'), 'Select', 'board keys stay out of it');
  await harness.act(() => svg.dispatchEvent(new harness.window.Event('pointerdown', { bubbles: true, cancelable: true })));
  assert.equal(root.querySelector('.board-context'), null);
});

test('the context menu stays on screen', async (t) => {
  const env = await setup(t, twoShapes);
  const { harness } = env;
  const { BoardContextMenu } = await harness.importDashboardModule('js/board-context-menu.js');
  const proto = harness.window.HTMLElement.prototype;
  const original = proto.getBoundingClientRect;
  proto.getBoundingClientRect = function box() { return this.classList?.contains('board-context') ? { width: 200, height: 300, left: 0, top: 0 } : original.call(this); };
  t.after(() => { proto.getBoundingClientRect = original; });
  const view = await harness.mount(harness.html`<${BoardContextMenu} x=${5000} y=${5000} label="x" items=${[{ label: 'One', run() {} }]} win=${{ innerWidth: 1000, innerHeight: 700 }} onClose=${() => {}} />`);
  const node = view.container.querySelector('.board-context');
  assert.equal(node.style.left, '792px');
  assert.equal(node.style.top, '392px');
});
