// Taking files into a dialog (attachment-intake.js): the spawn dialog and the
// task board's forms share one paste, drop and picker behaviour.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const file = (name, size = 10, type = 'text/plain') => {
  const blob = new Blob(['x'.repeat(size)], { type });
  if (name) Object.defineProperty(blob, 'name', { value: name });
  return blob;
};

async function mountIntake(t, options = {}) {
  const harness = await createPreactHarness(t);
  const { useAttachmentIntake, AttachmentList } = await harness.importDashboardModule('js/attachment-intake.js');
  const rejected = [];
  let changes = 0;
  let busy = false;
  function Dialog() {
    const [attachments, setAttachments] = harness.hooks.useState([]);
    const intake = useAttachmentIntake({ setAttachments, attachments, busy: () => busy, onChange: () => { changes += 1; },
      onReject: (text) => rejected.push(text), ...options });
    return harness.html`<div class=${intake.dragOver ? 'dialog over' : 'dialog'} ...${intake.handlers}>
      <${AttachmentList} attachments=${attachments} remove=${intake.remove} busy=${false} />
    </div>`;
  }
  const { container } = await harness.mount(harness.html`<${Dialog} />`);
  const dialog = container.querySelector('.dialog');
  // Preact listens for 'paste' only where the element has onpaste, which
  // LinkeDOM's does not: it then listens for 'Paste'.
  const eventName = (type) => (type === 'paste' && !('onpaste' in dialog) ? 'Paste' : type);
  const fire = async (type, init) => {
    let event;
    await harness.act(() => { event = harness.fireEvent(dialog, eventName(type), init); });
    return event;
  };
  const names = () => [...container.querySelectorAll('.att-name')].map((n) => n.textContent);
  return { harness, container, dialog, fire, names, rejected, changes: () => changes, setBusy: (v) => { busy = v; } };
}

test('a paste takes its files and file items once each, and names a nameless screenshot', async (t) => {
  const { fire, names, changes } = await mountIntake(t);
  const notes = file('notes.txt');
  const shot = file('', 20, 'image/png');
  const event = await fire('paste', { clipboardData: {
    files: [notes], items: [{ kind: 'file', getAsFile: () => notes }, { kind: 'file', getAsFile: () => shot }, { kind: 'string' }],
  } });
  assert.equal(event.defaultPrevented, true, 'a paste of files is the dialog\'s, not the field\'s');
  assert.equal(names()[0], 'notes.txt');
  assert.match(names()[1], /^pasted-\d+\.png$/, 'a screenshot has no name: it gets one with its type\'s extension');
  assert.equal(names().length, 2);
  assert.equal(changes(), 1);
  // A text paste is left to the field it lands in.
  const text = await fire('paste', { clipboardData: { files: [], items: [{ kind: 'string' }] } });
  assert.equal(text.defaultPrevented, false);
});

test('the same files pasted again at once are one paste; later they are a second copy', async (t) => {
  const { harness, fire, names } = await mountIntake(t);
  const notes = file('notes.txt');
  const paste = () => fire('paste', { clipboardData: { files: [notes], items: [] } });
  const perf = harness.window.performance || globalThis.performance;
  const realNow = perf.now.bind(perf);
  let now = 5000;
  perf.now = () => now;
  t.after(() => { perf.now = realNow; });
  await paste();
  now += 200;
  await paste();
  assert.deepEqual(names(), ['notes.txt'], 'a repeat within the window is dropped');
  now += 5000;
  await paste();
  assert.deepEqual(names(), ['notes.txt', 'notes.txt'], 'a deliberate second paste adds a copy');
});

test('a drop of files adds them; the drag-over state follows nested enters and leaves', async (t) => {
  const { fire, names, dialog } = await mountIntake(t);
  const files = { types: ['Files'], files: [file('a.txt'), file('b.txt')], dropEffect: '' };
  await fire('dragenter', { dataTransfer: files });
  await fire('dragenter', { dataTransfer: files });
  await fire('dragleave', { dataTransfer: files });
  assert.ok(dialog.classList.contains('over'), 'still over the dialog after leaving a child');
  const over = await fire('dragover', { dataTransfer: files });
  assert.equal(over.defaultPrevented, true);
  assert.equal(files.dropEffect, 'copy');
  await fire('drop', { dataTransfer: files });
  assert.equal(dialog.classList.contains('over'), false);
  assert.deepEqual(names(), ['a.txt', 'b.txt']);
  // A drag that is not files (a row, text) is left alone.
  const row = await fire('dragover', { dataTransfer: { types: ['text/plain'] } });
  assert.equal(row.defaultPrevented, false);
});

test('limits leave out what is too many, too big or of a refused type, and say so', async (t) => {
  const limits = { maxFiles: 2, maxBytes: 50, accept: (f, name) => (name.endsWith('.exe') ? 'type not allowed' : '') };
  const { fire, names, rejected } = await mountIntake(t, { limits });
  await fire('drop', { dataTransfer: { types: ['Files'], files: [file('big.txt', 80), file('tool.exe'), file('a.txt'), file('b.txt'), file('c.txt')] } });
  assert.deepEqual(names(), ['a.txt', 'b.txt']);
  assert.equal(rejected.length, 1);
  assert.match(rejected[0], /big\.txt \(over 50 B\)/);
  assert.match(rejected[0], /tool\.exe \(type not allowed\)/);
  assert.match(rejected[0], /c\.txt \(at most 2 files\)/);
});

test('while the dialog sends, nothing is taken, but a drop still stops the browser opening the file', async (t) => {
  const { fire, names, setBusy, container, harness } = await mountIntake(t);
  await fire('drop', { dataTransfer: { types: ['Files'], files: [file('a.txt')] } });
  setBusy(true);
  const drop = await fire('drop', { dataTransfer: { types: ['Files'], files: [file('b.txt')] } });
  assert.equal(drop.defaultPrevented, true);
  await harness.act(() => harness.fireEvent(container.querySelector('.att-remove'), 'click'));
  assert.deepEqual(names(), ['a.txt'], 'nor removed');
  setBusy(false);
  await harness.act(() => harness.fireEvent(container.querySelector('.att-remove'), 'click'));
  assert.deepEqual(names(), []);
});
