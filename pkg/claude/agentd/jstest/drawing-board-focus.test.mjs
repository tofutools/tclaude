// A task's drawing opens its drawing board on the part it shows
// (tk_413bdf5d): openDrawingBoardAt asks, the editor fits its view to the
// named elements once it has loaded and is laid out, and selects them.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const wait = (ms = 0) => new Promise((r) => setTimeout(r, ms));
const ok = (body) => ({ ok: true, status: 200, json: async () => body });
const rect = (id, x, y) => ({ id, x, y, width: 100, height: 50, type: 'rectangle', version: 1, versionNonce: 1 });

test('a focus request fits the view to its elements and selects them', async (t) => {
  const harness = await createPreactHarness(t);
  const [island, overlay] = await Promise.all([
    harness.importDashboardModule('js/drawing-board-island.js'),
    harness.importDashboardModule('js/drawing-board-overlay-state.js'),
  ]);
  const elements = [rect('near', 0, 0), rect('far', 3000, 2000), rect('farther', 3200, 2000)];
  const fetchImpl = async (url, init = {}) => (String(url).startsWith('/api/drawing-boards/') && !init.method
    ? ok({ id: 'plan', title: 'Plan', version: 3, participants: [], elements }) : ok({ accepted: 1 }));
  overlay.drawingBoardFocus.value = { drawingBoard: 'plan', elements: ['far', 'farther', 'gone'], box: { x: 0, y: 0, width: 10, height: 10 } };
  const snapshot = harness.signals.signal({ groups: [] });
  const view = await harness.mount(harness.html`<${island.DrawingBoardEditor} drawingBoardId="plan" snapshot=${snapshot} fetchImpl=${fetchImpl} win=${harness.window} rasterize=${null} />`);
  const root = view.container;
  await harness.act(() => wait());
  assert.ok(overlay.drawingBoardFocus.value, 'not laid out yet: the request waits');

  const svg = root.querySelector('svg');
  svg.getBoundingClientRect = () => ({ left: 0, top: 0, width: 800, height: 600, right: 800, bottom: 600 });
  await harness.act(() => wait(150));
  assert.equal(overlay.drawingBoardFocus.value, null, 'the request is taken');
  const transform = root.querySelector('[data-drawing-board-viewport]').getAttribute('transform');
  const [, tx, ty] = transform.match(/translate\((-?[\d.]+)[ ,]+(-?[\d.]+)\)/) || [];
  assert.ok(Number(tx) < -2000 && Number(ty) < -1500, `the view moved to the far elements: ${transform}`);
  assert.equal(root.querySelectorAll('.drawing-board-selection').length, 2, 'the elements still on the board are selected');
});

test('a task drawing names its board only when the reference is well formed', async (t) => {
  const harness = await createPreactHarness(t);
  const { drawingLink } = await harness.importDashboardModule('js/groups-taskboard-files.js');
  assert.equal(drawingLink({ name: 'a.png' }), null);
  assert.equal(drawingLink({ drawing: { board: '../etc', version: 1 } }), null);
  assert.deepEqual(drawingLink({ drawing: { board: 'plan', version: 4, elements: ['a', 'b c', 7], crop: { x: 1, y: 2, width: 3, height: 4 } } }),
    { board: 'plan', version: 4, elements: ['a'], box: { x: 1, y: 2, width: 3, height: 4 } });
  assert.equal(drawingLink({ drawing: { board: 'plan', crop: { x: 1, y: 2, width: -3, height: 4 } } }).box, null);
});

test("a drawing's tile offers its drawing board on this node's board only", async (t) => {
  const harness = await createPreactHarness(t);
  const { StoredTile } = await harness.importDashboardModule('js/groups-taskboard-files.js');
  const attachment = { id: 3, name: 'plan.png', size: 1200, drawing: { board: 'plan', version: 2, elements: ['a'] } };
  const tile = async (api) => (await harness.mount(harness.html`<${StoredTile} api=${api} taskId="tk_1" attachment=${attachment} onOpen=${() => {}} />`)).container;
  const here = await tile({ local: true });
  const button = here.querySelector('.tb-file-drawing');
  assert.ok(button, 'a drawing on this node offers its board');
  assert.match(button.textContent, /Edit in drawing board/);
  assert.match(button.getAttribute('title'), /plan .*version 2/);
  assert.equal((await tile({ local: false })).querySelector('.tb-file-drawing'), null, "a linked node's drawing does not");
  assert.equal((await tile({ local: true })).querySelectorAll('.tb-file-drawing').length, 1);
});
