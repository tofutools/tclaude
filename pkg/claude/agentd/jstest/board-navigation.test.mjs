// Tests for finding your way around a board: the number keys that pick a tool
// (tcl-3nn1up), the minimap (tcl-9tax7h), its own zoom (tcl-h3e5p8) and the
// paper's grid (tcl-1jai7s).

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const wait = (ms = 0) => new Promise((r) => setTimeout(r, ms));

function ok(body, status = 200) {
  return { ok: true, status, json: async () => body };
}

const rect = (x, y, width, height) => ({ x, y, width, height, type: 'rectangle', version: 1, versionNonce: 1 });

function boardFetch(state) {
  return async (url, init = {}) => {
    if (String(url).startsWith('/api/boards/') && !init.method) {
      return ok({ id: 'plan', title: 'Plan', version: state.version, participants: [], elements: state.elements });
    }
    return ok({ accepted: 1 });
  };
}

async function setup(t) {
  const harness = await createPreactHarness(t);
  const [island, minimap, { dashPrefs }] = await Promise.all([
    harness.importDashboardModule('js/board-island.js'),
    harness.importDashboardModule('js/board-minimap.js'),
    harness.importDashboardModule('js/prefs.js'),
  ]);
  dashPrefs.removeItem(minimap.BOARD_MINIMAP_COLLAPSED_PREF);
  dashPrefs.removeItem(minimap.BOARD_MINIMAP_ZOOM_PREF);
  const section = harness.window.document.createElement('section');
  harness.window.document.body.appendChild(section);
  const snapshot = harness.signals.signal({ groups: [] });
  const mount = async (fetchImpl, label = 'plan') => {
    const view = await harness.mount(harness.html`<${island.BoardEditor} boardId=${label} snapshot=${snapshot} fetchImpl=${fetchImpl} win=${harness.window} rasterize=${null} />`);
    await harness.act(() => wait());
    return view.container;
  };
  return { harness, island, minimap, dashPrefs, mount };
}

const toolOn = (root) => root.querySelector('.board-tool.on')?.getAttribute('aria-label');
const press = (harness, target, key, init = {}) => harness.act(() => harness.fireEvent(target, 'keydown', { key, ...init }));
const down = (harness, node) => harness.act(() => node.dispatchEvent(new harness.window.Event('pointerdown', { bubbles: true })));

function sized(node, left, top, width, height) {
  node.getBoundingClientRect = () => ({ left, top, width, height, right: left + width, bottom: top + height });
}

// The test DOM's SVG lacks these handlers, so Preact listens under the JSX name.
const JSX_CASE = { pointerdown: 'PointerDown', pointermove: 'PointerMove', pointerup: 'PointerUp', wheel: 'Wheel', dblclick: 'DblClick' };
function fire(harness, target, type, init) {
  return harness.act(() => harness.fireEvent(target, `on${type}` in target ? type : JSX_CASE[type], init));
}

function pointer(harness, target, type, x, y) {
  return fire(harness, target, type, { button: 0, pointerId: 1, clientX: x, clientY: y });
}

// Gives the editor a size, which a test DOM lacks, and makes it re-place the map.
async function layOut(harness, root) {
  sized(root.querySelector('svg.board-canvas'), 0, 0, 1000, 600);
  sized(root.querySelector('.board-minimap-frame'), 800, 400, 200, 120);
  await fire(harness, root.querySelector('svg.board-canvas'), 'wheel', { deltaX: 40, deltaY: 0, clientX: 0, clientY: 0 });
}

test('Ctrl+wheel zooms the board over its inputs and controls, not the page', async (t) => {
  const { harness, mount } = await setup(t);
  const root = await mount(boardFetch({ version: 1, elements: [] }));
  const svg = root.querySelector('svg.board-canvas');
  sized(svg, 0, 0, 1000, 600);
  const zoom = () => root.querySelector('.board-zoom').textContent;
  const editor = root.querySelector('.board-editor');
  const wheel = (target, init) => {
    let event;
    return harness.act(() => { event = harness.fireEvent(target, 'onwheel' in editor ? 'wheel' : 'Wheel', { clientX: 100, clientY: 100, deltaX: 0, ...init }); }).then(() => event);
  };
  const twice = { deltaY: -Math.log(2) / 0.002, ctrlKey: true };

  await fire(harness, svg, 'dblclick', { clientX: 200, clientY: 200 });
  const input = root.querySelector('.board-text-input');
  assert.ok(input, 'the editor is open');
  assert.ok((await wheel(input, twice)).defaultPrevented, 'the browser leaves the page alone');
  assert.equal(zoom(), '200%');

  assert.ok((await wheel(root.querySelector('select.board-group'), { ...twice, deltaY: Math.log(2) / 0.002 })).defaultPrevented);
  assert.equal(zoom(), '100%', 'a control outside the canvas too');

  assert.equal((await wheel(root.querySelector('select.board-group'), { deltaY: 40 })).defaultPrevented, false, 'a plain scroll out there is the page\'s');
});

test('the grid scrolls and zooms with the board', async (t) => {
  const { harness, mount } = await setup(t);
  const m = await harness.importDashboardModule('js/board-model.js');
  const root = await mount(boardFetch({ version: 1, elements: [] }));
  const svg = root.querySelector('svg.board-canvas');
  const wrap = root.querySelector('.board-canvas-wrap');
  sized(svg, 0, 0, 1000, 600);
  const paper = () => [wrap.style.backgroundSize, wrap.style.backgroundPosition];

  // The view starts at (-40, -40), so the board's origin is 40px in, and the
  // tile's middle, where its dot is, lands there.
  assert.deepEqual(paper(), ['20px 20px', '30px 30px']);
  await fire(harness, svg, 'wheel', { deltaX: 15, deltaY: 7, clientX: 0, clientY: 0 });
  assert.deepEqual(paper(), ['20px 20px', '15px 23px'], 'a scroll moves the dots as far as the shapes');
  await fire(harness, svg, 'wheel', { deltaX: 0, deltaY: -Math.log(2) / 0.002, clientX: 0, clientY: 0, ctrlKey: true });
  assert.equal(wrap.style.backgroundSize, '40px 40px', 'a zoom scales them');
  assert.equal(parseFloat(wrap.style.backgroundPosition), 30, 'keeping the board origin under its dot');

  // Zoomed out a dot stays on a board multiple of the grid.
  const far = m.boardGrid({ x: 100, y: 50, zoom: 0.2 });
  assert.equal(far.size, 16);
  assert.deepEqual([far.x, far.y], [-20, -10]);
});

test('the zoom controls and keys step, reset and fit', async (t) => {
  const { harness, mount } = await setup(t);
  const m = await harness.importDashboardModule('js/board-model.js');
  const root = await mount(boardFetch({ version: 1, elements: [m.newElement('rectangle', { id: 'wide', x: 0, y: 0, width: 4000, height: 100 })] }));
  sized(root.querySelector('svg.board-canvas'), 0, 0, 1000, 600);
  const zoom = () => root.querySelector('.board-zoom').textContent;
  const button = (name) => root.querySelector(`[aria-label="${name}"]`);
  const click = (name) => harness.act(() => button(name).click());
  const body = root.querySelector('svg.board-canvas');

  assert.equal(button('Reset zoom to 100%').disabled, true);
  await click('Zoom in');
  assert.equal(zoom(), '125%');
  await click('Reset zoom to 100%');
  assert.equal(zoom(), '100%');
  await click('Zoom out');
  assert.equal(zoom(), '80%');
  await click('Fit everything in view');
  assert.equal(zoom(), '25%');

  await press(harness, body, 'Whatever', { code: 'Digit0', shiftKey: true });
  assert.equal(zoom(), '100%');
  await press(harness, body, '=', { code: 'Digit0', shiftKey: true }); // Shift+0 on a Nordic layout
  assert.equal(zoom(), '100%', 'the physical key decides, not what it types');
  await press(harness, body, '+');
  assert.equal(zoom(), '125%');
  await press(harness, body, '-');
  assert.equal(zoom(), '100%');
  await press(harness, body, '!', { code: 'Digit1', shiftKey: true });
  assert.equal(zoom(), '25%');
  await press(harness, body, '-', { ctrlKey: true });
  assert.equal(zoom(), '25%', 'with Ctrl held the key stays the browser\'s');

  for (let i = 0; i < 20; i += 1) await press(harness, body, '+');
  assert.equal(zoom(), '400%');
  assert.equal(button('Zoom in').disabled, true);
});

test('each tool has its number key, shown in the menu, in keyboard order', async (t) => {
  const { harness, mount } = await setup(t);
  const { BOARD_TOOLS, boardToolDigit } = await harness.importDashboardModule('js/board-model.js');
  const root = await mount(boardFetch({ version: 1, elements: [] }));
  const body = harness.window.document.body;
  await down(harness, root.querySelector('svg.board-canvas'));
  const buttons = [...root.querySelectorAll('.board-toolbar .board-tool')].slice(0, BOARD_TOOLS.length);
  assert.deepEqual(buttons.map((b) => b.querySelector('.board-tool-digit')?.textContent), ['1', '2', '3', '4', '5', '6', '7', '8', '9', '0']);
  assert.equal(buttons[2].title, 'Rectangle (3 or R)');
  for (const [index, tool] of BOARD_TOOLS.entries()) {
    await press(harness, body, boardToolDigit(index));
    assert.equal(toolOn(root), tool.label);
  }
  await press(harness, body, 'r');
  assert.equal(toolOn(root), 'Rectangle', 'the letters still work');
  await press(harness, body, '1', { ctrlKey: true });
  await press(harness, body, '!', { shiftKey: true });
  assert.equal(toolOn(root), 'Rectangle', 'Ctrl+1 and Shift+1 belong to the browser');
  assert.equal(boardToolDigit(10), '', 'an eleventh tool has no key');
});

test('number keys leave typing alone', async (t) => {
  const { harness, mount } = await setup(t);
  const root = await mount(boardFetch({ version: 1, elements: [] }));
  await down(harness, root.querySelector('svg.board-canvas'));
  await press(harness, harness.window.document.body, '3');
  assert.equal(toolOn(root), 'Rectangle');
  const select = root.querySelector('select.board-group');
  await press(harness, select, '6');
  assert.equal(toolOn(root), 'Rectangle', 'a select in the editor keeps its type-ahead');
  const doc = harness.window.document;
  const editable = doc.createElement('div');
  editable.setAttribute('contenteditable', '');
  root.appendChild(editable);
  await press(harness, editable, '6');
  assert.equal(toolOn(root), 'Rectangle', 'an editable element in the editor keeps its keys');
  // A terminal: xterm types into a textarea of its own, outside the board.
  const terminal = doc.createElement('textarea');
  doc.body.appendChild(terminal);
  await down(harness, terminal);
  await press(harness, terminal, '6');
  assert.equal(toolOn(root), 'Rectangle');
  // With the terminal pressed, the board's claim on page keys has ended too.
  await press(harness, doc.body, '6');
  assert.equal(toolOn(root), 'Rectangle');
  // The text being edited on the board.
  await down(harness, root.querySelector('svg.board-canvas'));
  await press(harness, doc.body, 't');
  await pointer(harness, root.querySelector('svg.board-canvas'), 'pointerdown', 50, 50);
  const input = root.querySelector('textarea.board-text-input');
  assert.ok(input);
  await press(harness, input, '3');
  assert.equal(toolOn(root), 'Text');
});

test('with two editors open a number key goes to the last one used', async (t) => {
  const { harness, mount } = await setup(t);
  const first = await mount(boardFetch({ version: 1, elements: [] }), 'one');
  const second = await mount(boardFetch({ version: 1, elements: [] }), 'two');
  const body = harness.window.document.body;
  await down(harness, first.querySelector('svg.board-canvas'));
  await press(harness, body, '3');
  assert.deepEqual([toolOn(first), toolOn(second)], ['Rectangle', 'Select']);
  await down(harness, second.querySelector('svg.board-canvas'));
  await press(harness, body, '6');
  assert.deepEqual([toolOn(first), toolOn(second)], ['Rectangle', 'Arrow']);
  // Focus on a control of one editor sends keys there, whichever was used last.
  await press(harness, first.querySelector('.board-toolbar .board-tool'), '4');
  assert.deepEqual([toolOn(first), toolOn(second)], ['Ellipse', 'Arrow']);
});

test('the map holds the board and the view, and keeps its window inside them', async (t) => {
  const { minimap: m } = await setup(t);
  const elements = new Map([
    ['a', rect(0, 0, 100, 100)],
    ['b', rect(900, 400, 100, 100)],
    ['gone', { ...rect(-5000, 0, 10, 10), isDeleted: true }],
  ]);
  const content = m.minimapContent(elements);
  assert.deepEqual(content, { x: 0, y: 0, width: 1000, height: 500 });
  assert.equal(m.minimapContent(new Map()), null);
  const view = { x: 2000, y: 0, width: 500, height: 300 };
  const world = m.minimapWorld(content, view);
  assert.ok(world.x < 0 && world.x + world.width > 2500, 'a view off the content is still on the map');
  assert.deepEqual(m.minimapWorld(null, view).x < view.x, true, 'an empty board still has a world');

  const frame = { width: 200, height: 100 };
  const whole = m.minimapWindow(world, frame);
  assert.ok(whole.x <= world.x && whole.x + whole.width >= world.x + world.width);
  assert.ok(whole.y <= world.y && whole.y + whole.height >= world.y + world.height);
  const zoomed = m.minimapWindow(world, frame, 4, { x: -1e6, y: -1e6 });
  assert.ok(Math.abs(zoomed.x - world.x) < 1e-6, 'a window does not leave the board');
  assert.ok(Math.abs(zoomed.width - whole.width / 4) < 1e-6);
});

test('the map zooms about the pointer, within its limits, and follows the view', async (t) => {
  const { minimap: m } = await setup(t);
  const world = { x: 0, y: 0, width: 1000, height: 500 };
  const frame = { width: 200, height: 100 };
  const at = m.minimapZoomAt(world, frame, 1, null, 50, 25, 2);
  assert.equal(at.zoom, 2);
  const window = m.minimapWindow(world, frame, at.zoom, at.center);
  assert.ok(Math.abs(window.x + 50 / window.scale - 250) < 1e-6, 'the point under the pointer stays');
  assert.equal(m.minimapZoomAt(world, frame, 30, at.center, 0, 0, 10).zoom, m.BOARD_MINIMAP_ZOOM_MAX);
  assert.equal(m.minimapZoomAt(world, frame, 1.2, at.center, 0, 0, 0.1).zoom, 1);
  assert.equal(m.clampMinimapZoom(Number('junk')), 1);

  const center = { x: 100, y: 100 };
  const box = { x: 0, y: 0, width: 200, height: 200 };
  assert.deepEqual(m.followView(center, box, { x: 90, y: 90, width: 20, height: 20 }), center, 'a view in the window moves nothing');
  assert.deepEqual(m.followView(center, box, { x: 290, y: 90, width: 20, height: 20 }), { x: 200, y: 100 });
});

test('pressing and dragging the map moves the main view, and it updates live', async (t) => {
  const { harness, mount } = await setup(t);
  const state = { version: 1, elements: [{ ...rect(0, 0, 400, 200), id: 'a' }] };
  const root = await mount(boardFetch(state));
  await layOut(harness, root);
  const frame = root.querySelector('.board-minimap-frame');
  const viewport = () => root.querySelector('[data-board-viewport]').getAttribute('transform');
  const marker = root.querySelector('.board-minimap-view');
  assert.notEqual(marker.style.width, '', 'the marker shows the view');

  await pointer(harness, frame, 'pointerdown', 990, 510);
  const moved = viewport();
  assert.match(moved, /scale\(1\)/, 'the map moves the view, not its zoom');
  await pointer(harness, frame, 'pointermove', 810, 410);
  assert.notEqual(viewport(), moved, 'a drag keeps moving it');
  await pointer(harness, frame, 'pointerup', 810, 410);
  const left = viewport();
  await pointer(harness, frame, 'pointermove', 900, 450);
  assert.equal(viewport(), left, 'a released map does nothing');

  // A press on the marker holds it: nothing jumps until it is dragged.
  const x = 800 + Number.parseFloat(marker.style.left) + 4;
  const y = 400 + Number.parseFloat(marker.style.top) + 4;
  await pointer(harness, frame, 'pointerdown', x, y);
  assert.equal(viewport(), left);
  await pointer(harness, frame, 'pointerup', x, y);

  // A remote edit far away grows the map, which the marker shows.
  const before = marker.style.width;
  state.elements = [{ ...rect(9000, 4000, 100, 100), id: 'far', version: 1, versionNonce: 1 }];
  state.version = 2;
  await harness.act(() => wait(1100));
  assert.ok(Number.parseFloat(marker.style.width) < Number.parseFloat(before), 'the map takes in an element an agent added');
});

test('the map zooms on its own, leaving the main view, and remembers', async (t) => {
  const { harness, mount, minimap, dashPrefs } = await setup(t);
  const root = await mount(boardFetch({ version: 1, elements: [{ ...rect(0, 0, 4000, 2000), id: 'a' }] }));
  await layOut(harness, root);
  const viewport = () => root.querySelector('[data-board-viewport]').getAttribute('transform');
  const before = viewport();
  const marker = root.querySelector('.board-minimap-view');
  const wide = Number.parseFloat(marker.style.width);
  const button = (name) => harness.getByRole(root, 'button', { name });
  await harness.act(() => button('Zoom the map in').click());
  assert.equal(viewport(), before, 'the main view stays');
  assert.ok(Number.parseFloat(marker.style.width) > wide, 'the marker grows with the map');
  assert.equal(root.querySelector('.board-minimap-zoom').textContent, '1.5×');
  assert.equal(dashPrefs.getItem(minimap.BOARD_MINIMAP_ZOOM_PREF), '1.5');
  await harness.act(() => button('Show the whole board').click());
  assert.equal(root.querySelector('.board-minimap-zoom').textContent, 'Fit');
  assert.equal(Number.parseFloat(marker.style.width), wide);
  const frame = root.querySelector('.board-minimap-frame');
  await fire(harness, frame, 'wheel', { deltaY: -200, clientX: 900, clientY: 460 });
  assert.notEqual(root.querySelector('.board-minimap-zoom').textContent, 'Fit');
  assert.equal(viewport(), before);
  const again = await mount(boardFetch({ version: 1, elements: [] }), 'other');
  assert.notEqual(again.querySelector('.board-minimap-zoom').textContent, 'Fit', 'the next board opens at that zoom');
});

test('the minimap collapses, and stays collapsed', async (t) => {
  const { harness, mount, minimap, dashPrefs } = await setup(t);
  const root = await mount(boardFetch({ version: 1, elements: [] }));
  assert.ok(root.querySelector('.board-minimap'));
  await harness.act(() => harness.getByRole(root, 'button', { name: 'Hide the minimap' }).click());
  assert.equal(root.querySelector('.board-minimap'), null);
  assert.equal(dashPrefs.getItem(minimap.BOARD_MINIMAP_COLLAPSED_PREF), '1');
  const next = await mount(boardFetch({ version: 1, elements: [] }), 'other');
  assert.equal(next.querySelector('.board-minimap'), null, 'a board opened later keeps it collapsed');
  await harness.act(() => harness.getByRole(next, 'button', { name: 'Show the minimap' }).click());
  assert.ok(next.querySelector('.board-minimap'));
  assert.equal(dashPrefs.getItem(minimap.BOARD_MINIMAP_COLLAPSED_PREF), '0');
});

test('a tab left mounted picks up the map settings when it comes back', async (t) => {
  const { harness, island, minimap, dashPrefs } = await setup(t);
  const shown = harness.signals.signal(true);
  const snapshot = harness.signals.signal({ groups: [] });
  const Host = () => harness.html`<${island.BoardEditor} boardId="plan" onScreen=${shown.value} snapshot=${snapshot}
    fetchImpl=${boardFetch({ version: 1, elements: [] })} win=${harness.window} rasterize=${null} />`;
  const { container: root } = await harness.mount(harness.html`<${Host} />`);
  await harness.act(() => wait());
  assert.ok(root.querySelector('.board-minimap'));
  await harness.act(() => { shown.value = false; });
  dashPrefs.setItem(minimap.BOARD_MINIMAP_COLLAPSED_PREF, '1');
  await harness.act(() => { shown.value = true; });
  await harness.act(() => wait());
  assert.equal(root.querySelector('.board-minimap'), null);
});

test('wheelIsMouse tells a mouse wheel from a touchpad', async (t) => {
  const { harness } = await setup(t);
  const { wheelIsMouse, wheelPan, wheelPixels } = await harness.importDashboardModule('js/board-model.js');
  const mouse = [
    { deltaMode: 0, deltaX: 0, deltaY: 100, wheelDeltaY: -120 }, // Chrome
    { deltaMode: 0, deltaX: 0, deltaY: 133.33, wheelDeltaY: -240 }, // two notches, fractional
    { deltaMode: 1, deltaX: 0, deltaY: 3, wheelDeltaY: -120 }, // Firefox
    { deltaMode: 2, deltaX: 0, deltaY: 1 },
  ];
  const touchpad = [
    { deltaMode: 0, deltaX: 0, deltaY: 4, wheelDeltaY: -12 },
    { deltaMode: 0, deltaX: 0, deltaY: 7.5, wheelDeltaY: -22 },
    { deltaMode: 0, deltaX: 3, deltaY: 40, wheelDeltaY: -120 }, // a notch's value, but it moves sideways
    { deltaMode: 0, deltaX: 40, deltaY: 0, wheelDeltaY: 0 },
    { deltaMode: 0, deltaX: 0, deltaY: 40 }, // no wheelDeltaY: no evidence of a mouse
  ];
  for (const event of mouse) assert.equal(wheelIsMouse(event), true, JSON.stringify(event));
  for (const event of touchpad) assert.equal(wheelIsMouse(event), false, JSON.stringify(event));
  assert.equal(wheelPixels({ deltaMode: 1, deltaY: 3 }), 99, 'a Firefox notch zooms like a Chrome one');
  assert.equal(wheelPixels({ deltaMode: 0, deltaY: 100 }), 100);
  assert.deepEqual(wheelPan({ deltaMode: 1, deltaX: 3, deltaY: 0 }), { x: 99, y: 0 }, 'a tilt wheel moves in lines too');
  assert.deepEqual(wheelPan({ deltaMode: 0, deltaX: 0, deltaY: 4 }), { x: 0, y: 4 });
  assert.deepEqual(wheelPan({ deltaMode: 1, deltaX: 0, deltaY: 3, shiftKey: true }), { x: 99, y: 0 }, 'Firefox leaves Shift+wheel vertical');
  assert.deepEqual(wheelPan({ deltaMode: 0, deltaX: 100, deltaY: 0, shiftKey: true }), { x: 100, y: 0 }, 'Chrome has swapped it already');
});

test('on the canvas a mouse wheel zooms at the pointer and a touchpad pans', async (t) => {
  const { harness, mount } = await setup(t);
  const root = await mount(boardFetch({ version: 1, elements: [{ ...rect(0, 0, 100, 100), id: 'a' }] }));
  const svg = root.querySelector('svg.board-canvas');
  sized(svg, 0, 0, 1000, 600);
  const view = () => root.querySelector('[data-board-viewport]').getAttribute('transform');
  const start = view();
  await fire(harness, svg, 'wheel', { deltaX: 0, deltaY: 4, wheelDeltaY: -12, clientX: 500, clientY: 300 });
  assert.notEqual(view(), start);
  assert.match(view(), /^scale\(1\)/, 'a touchpad pans');
  await new Promise((r) => setTimeout(r, 200));
  const panned = view();
  await fire(harness, svg, 'wheel', { deltaX: 0, deltaY: -Math.log(2) / 0.002, wheelDeltaY: 360, clientX: 500, clientY: 300 });
  assert.match(view(), /^scale\(2\)/, 'a mouse notch zooms');
  assert.notEqual(view(), panned);
});

test('a scroll stays one device from start to end', async (t) => {
  const { harness, mount } = await setup(t);
  const root = await mount(boardFetch({ version: 1, elements: [] }));
  const svg = root.querySelector('svg.board-canvas');
  sized(svg, 0, 0, 1000, 600);
  const zoomOf = () => root.querySelector('[data-board-viewport]').getAttribute('transform').match(/^scale\(([^)]+)\)/)[1];
  await fire(harness, svg, 'wheel', { deltaX: 0, deltaY: 4, wheelDeltaY: -12, timeStamp: 1000 });
  await fire(harness, svg, 'wheel', { deltaX: 0, deltaY: 40, wheelDeltaY: -120, timeStamp: 1050 });
  assert.equal(zoomOf(), '1', 'a touchpad event that looks like a notch does not jump the view');
  await fire(harness, svg, 'wheel', { deltaX: 0, deltaY: -300, wheelDeltaY: 120, timeStamp: 2000 });
  assert.notEqual(zoomOf(), '1', 'a pause starts a new scroll');
  const zoomed = zoomOf();
  await fire(harness, svg, 'wheel', { deltaX: 0, deltaY: 3, deltaMode: 1, wheelDeltaY: -120, shiftKey: true, timeStamp: 2050 });
  assert.equal(zoomOf(), zoomed, 'Shift pans, even in the middle of a zoom');
});

test('a pan does not re-render the shapes', async (t) => {
  const { harness, mount } = await setup(t);
  const root = await mount(boardFetch({ version: 1, elements: [{ ...rect(0, 0, 100, 100), id: 'a' }] }));
  const shape = root.querySelector('rect.board-el');
  await fire(harness, root.querySelector('svg.board-canvas'), 'wheel', { deltaX: 40, deltaY: 0 });
  assert.equal(root.querySelector('[data-board-viewport]').getAttribute('transform'), 'scale(1) translate(0 40)');
  assert.equal(root.querySelector('rect.board-el'), shape);
});
