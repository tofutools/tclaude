// The drawing board's File › Export (tk_413bdf5d): PNG and SVG, drawn by the
// daemon, of the selection when there is one; as JSON, so a peer's page gets
// it through the peer view the same way.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('Export PNG and SVG ask the daemon for the selection, or the whole board, and save what it drew', async (t) => {
  const harness = await createPreactHarness(t);
  const island = await harness.importDashboardModule('js/drawing-board-island.js');
  const calls = [];
  const fetchImpl = async (url, init) => {
    calls.push([String(url), JSON.parse(init.body)]);
    return { ok: true, status: 200, json: async () => ({ content_type: 'image/png', filename: 'plan.png', data: btoa('img') }) };
  };
  const notes = [];
  const items = island.drawingBoardFileItems({ drawingBoardId: 'plan', selection: () => ['a', 'b'], fetchImpl, notify: (m) => notes.push(m) });
  const keys = items.filter((i) => i.key).map((i) => i.key);
  assert.deepEqual(keys.slice(0, 3), ['export-png', 'export-svg', 'download'], 'the images lead the Export section');
  assert.match(items.find((i) => i.key === 'export-png').title, /2 selected elements/);

  const saved = [];
  await island.exportDrawingBoardImage({ drawingBoardId: 'plan', format: 'png', selection: ['a'], fetchImpl,
    save: async (blob, name) => saved.push([name, blob.type, await blob.text()]) });
  assert.deepEqual(calls.at(-1), ['/api/drawing-boards/plan/export?format=json', { format: 'png', elements: ['a'] }]);
  assert.deepEqual(saved, [['plan.png', 'image/png', 'img']]);

  await island.exportDrawingBoardImage({ drawingBoardId: 'plan', format: 'svg', fetchImpl, save: async () => {} });
  assert.deepEqual(calls.at(-1)[1], { format: 'svg' }, 'no selection: the whole board');

  const whole = island.drawingBoardFileItems({ drawingBoardId: 'plan' });
  assert.match(whole.find((i) => i.key === 'export-svg').title, /whole drawing board/);

  const refused = async () => ({ ok: false, status: 422, json: async () => ({ error: 'nothing to export' }) });
  await island.exportDrawingBoardImage({ drawingBoardId: 'plan', format: 'png', fetchImpl: refused, notify: (m) => notes.push(m), save: async () => assert.fail('nothing saved') });
  assert.match(notes.at(-1), /Could not export the drawing board as PNG: .*nothing to export/);
});
