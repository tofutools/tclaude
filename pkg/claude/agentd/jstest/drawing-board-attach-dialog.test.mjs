// File › Export › Attach to a task… (tk_413bdf5d, the operator's pick A on
// Q33): find a task, see the part drawn, see that attaching updates the
// drawing already there, attach (or comment).

import test from 'node:test';
import assert from 'node:assert/strict';
import { createHash, webcrypto } from 'node:crypto';
import { createPreactHarness } from './preact-harness.mjs';

const wait = (ms = 0) => new Promise((r) => setTimeout(r, ms));
const ok = (body) => ({ ok: true, status: 200, json: async () => body });
const digest = (ids) => createHash('sha256').update([...new Set(ids)].sort().join('\n')).digest('hex').slice(0, 32);

const context = (attachments = []) => ({ groups: [
  { group: 'other', tasks: [{ id: 'tk_other', group: 'other', title: 'API docs elsewhere', stage: 'next', status: 'open' }] },
  { group: 'crew', tasks: [
    { id: 'tk_idea', group: 'crew', title: 'API rate limits', stage: 'idea', status: 'open' },
    { id: 'tk_now', group: 'crew', title: 'API layer rework', stage: 'next', status: 'open', assignee: { agent_id: 'agt_1' }, attachments },
    { id: 'tk_done', group: 'crew', title: 'API done already', stage: 'next', status: 'done' },
  ] },
] });

test('the digest is the daemon\'s: sorted distinct ids, one per line, SHA-256, 16 bytes', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/drawing-board-attach-dialog.js');
  assert.equal(await m.selectionDigest(['b', 'a', 'b'], webcrypto.subtle), digest(['a', 'b']));
  assert.equal(await m.selectionDigest([], webcrypto.subtle), '');
  assert.equal(await m.selectionDigest(['a'], null), null, 'without WebCrypto (plain http) a pick cannot be told');
  assert.equal(await m.selectionDigest([], null), '', 'the whole board needs no hash');
  const whole = { id: 1, name: 'arch.png', drawing: { board: 'arch', version: 1 } };
  assert.equal(m.existingDrawing({ attachments: [whole] }, 'arch', null), null, 'unknown is no match, not the whole board');
});

test('the list puts the board\'s group and the work under way first, open tasks only', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/drawing-board-attach-dialog.js');
  assert.deepEqual(m.taskChoices(context(), 'crew', 'api').map((x) => x.id), ['tk_now', 'tk_idea', 'tk_other']);
  assert.deepEqual(m.taskChoices(context(), 'crew', 'TK_OTH').map((x) => x.id), ['tk_other'], 'by id too, any case');
  const file = { id: 4, name: 'arch.png', drawing: { board: 'arch', version: 3, selection: digest(['a', 'b']) } };
  assert.equal(m.existingDrawing({ attachments: [file] }, 'arch', digest(['b', 'a'])), file);
  assert.equal(m.existingDrawing({ attachments: [file] }, 'arch', ''), null, 'the whole board is another part');
  assert.equal(m.existingDrawing({ attachments: [file] }, 'plan', digest(['a', 'b'])), null);
});

test('the dialog previews the part, says an attach updates the drawing there, and attaches', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/drawing-board-attach-dialog.js');
  const calls = [];
  const existing = { id: 4, name: 'arch.png', drawing: { board: 'arch', version: 3, selection: digest(['a', 'b']) } };
  const fetchImpl = async (url, init = {}) => {
    calls.push([String(url), init.body ? JSON.parse(init.body) : null]);
    if (String(url) === '/v1/taskboard/context') return ok(context([existing]));
    if (String(url).includes('/export')) return ok({ content_type: 'image/png', data: btoa('png') });
    return ok({ id: 'tk_now', replaced: true, attachment: { id: 4, name: 'arch.png' } });
  };
  const notes = [];
  let closed = 0;
  const request = { drawingBoardId: 'arch', title: 'Architecture', group: 'crew', selection: ['a', 'b'], fetchImpl, notify: (n) => notes.push(n), seq: 1 };
  const view = await harness.mount(harness.html`<${m.DrawingBoardAttachDialog} request=${request} close=${() => { closed += 1; }} />`);
  const root = view.container.ownerDocument;
  const settle = async () => { for (let i = 0; i < 6; i += 1) await harness.act(() => wait(5)); };
  await settle();

  const exportCall = calls.find(([url]) => url.includes('/export'));
  assert.deepEqual(exportCall, ['/api/drawing-boards/arch/export?format=json', { format: 'png', scale: 1, elements: ['a', 'b'] }], 'the selection is drawn first');
  assert.ok(root.querySelector('.drawing-board-attach-preview img')?.getAttribute('src').startsWith('data:image/png;base64,'));
  const rows = [...root.querySelectorAll('[data-task]')].map((b) => b.getAttribute('data-task'));
  assert.deepEqual(rows, ['tk_now', 'tk_idea', 'tk_other']);
  const submit = root.querySelector('#drawing-board-attach-submit');
  assert.ok(submit.disabled, 'no task chosen yet');

  await harness.act(() => harness.fireEvent(root.querySelector('[data-task="tk_now"]'), 'click'));
  await settle();
  assert.match(root.querySelector('.drawing-board-attach-existing').textContent, /Already on tk_now as arch\.png \(version 3\): attaching updates it/);
  assert.match(submit.textContent, /Update on tk_now/);

  await harness.act(() => harness.fireEvent(submit, 'click'));
  await settle();
  assert.deepEqual(calls.at(-1), ['/v1/taskboard/tasks/tk_now/drawings', { board: 'arch', elements: ['a', 'b'] }]);
  assert.deepEqual(notes, ['Updated arch.png on tk_now.']);
  assert.equal(closed, 1);
});
