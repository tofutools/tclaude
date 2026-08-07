import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// The attachment viewers open at one fixed size and remember the size the
// operator dragged them to. The pref store is stubbed so the suite can assert
// what was written without a daemon.
const PREFS_STUB = `
  const store = new Map();
  export const writes = [];
  export const dashPrefs = {
    getItem: (key) => (store.has(key) ? store.get(key) : null),
    setItem: (key, value) => { store.set(key, String(value)); writes.push([key, String(value)]); },
    removeItem: (key) => { store.delete(key); writes.push([key, null]); },
    syncItem: (key, value) => { if (value == null) store.delete(key); else store.set(key, String(value)); },
  };
  export const initDashPrefs = async () => {};
  export const seed = (key, value) => store.set(key, value);
`;

const markdownAttachment = {
  id: 4,
  filename: 'plan.md',
  content_type: 'text/markdown; charset=utf-8',
  size_bytes: 640,
  markdown: true,
  url: '/api/human-messages/42/attachments/4',
};

const imageAttachment = {
  id: 5,
  filename: 'shot.png',
  content_type: 'image/png',
  size_bytes: 2048,
  previewable: true,
  url: '/api/human-messages/42/attachments/5',
};

async function settle(harness, times = 3) {
  for (let i = 0; i < times; i += 1) {
    await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  }
}

function stubFetch(t, respond) {
  const saved = globalThis.fetch;
  globalThis.fetch = async (...args) => respond(...args);
  t.after(() => { globalThis.fetch = saved; });
}

// The grip measures the live dialog box, which linkedom has no layout to
// provide. Pin a starting size the way mail-resize.test.mjs pins pane widths.
function pinSize(dialog, w, h) {
  dialog.getBoundingClientRect = () => ({ width: w, height: h });
}

async function setup(t, { seedPref } = {}) {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/prefs.js', PREFS_STUB);
  // The ceiling is the viewport; linkedom reports none, so give it one.
  Object.defineProperty(harness.window, 'innerWidth', { configurable: true, value: 1600 });
  Object.defineProperty(harness.window, 'innerHeight', { configurable: true, value: 1200 });
  const prefs = await harness.importDashboardModule('js/prefs.js');
  if (seedPref) prefs.seed(seedPref[0], seedPref[1]);
  return { harness, prefs };
}

async function openMarkdownViewer(t, options) {
  const { harness, prefs } = await setup(t, options);
  const { MarkdownAttachment } = await harness.importDashboardModule('js/markdown-attachment.js');
  stubFetch(t, async () => ({ ok: true, status: 200, text: async () => '# Plan\n' }));
  const mounted = await harness.mount(harness.html`
    <${MarkdownAttachment} messageID=${42} attachment=${markdownAttachment} surface="messages" />
  `);
  await settle(harness);
  const open = [...mounted.container.querySelectorAll('.human-attachment-markdown-trigger')]
    .find((button) => /Open/.test(button.textContent));
  await harness.act(() => { open.click(); });
  await settle(harness);
  return { harness, prefs, mounted, dialog: mounted.container.querySelector('.markdown-preview-dialog') };
}

// A drag has to grow the dialog by twice the pointer's travel: the dialog is
// centred in its overlay, so it grows away from the pointer as much as toward
// it. Getting this wrong makes the grip drift out from under the cursor.
test('dragging the grip resizes the viewer from both edges and stores the size', async (t) => {
  const { harness, prefs, mounted, dialog } = await openMarkdownViewer(t);
  const grip = dialog.querySelector('.dialog-resizer');
  assert.ok(grip, 'the viewer offers a resize grip');
  pinSize(dialog, 900, 860);

  await harness.act(() => {
    harness.fireEvent(grip, 'pointerdown', { button: 0, pointerId: 3, clientX: 500, clientY: 400 });
    harness.fireEvent(grip, 'pointermove', { pointerId: 3, clientX: 550, clientY: 380 });
  });
  assert.equal(dialog.style.getPropertyValue('--dialog-w'), '1000px', '+50px of pointer = +100px of width');
  assert.equal(dialog.style.getPropertyValue('--dialog-h'), '820px', '-20px of pointer = -40px of height');
  assert.deepEqual(prefs.writes, [], 'nothing is stored mid-gesture');

  await harness.act(() => { harness.fireEvent(grip, 'pointerup', { pointerId: 3 }); });
  assert.deepEqual(prefs.writes, [
    ['tclaude.dash.attachmentViewer.markdown.size', JSON.stringify({ w: 1000, h: 820 })],
  ], 'releasing stores the size the operator settled on');
  await mounted.unmount();
});

test('a click with no drag leaves the stored size alone', async (t) => {
  const { harness, prefs, mounted, dialog } = await openMarkdownViewer(t);
  const grip = dialog.querySelector('.dialog-resizer');
  pinSize(dialog, 900, 860);

  await harness.act(() => {
    harness.fireEvent(grip, 'pointerdown', { button: 0, pointerId: 4, clientX: 500, clientY: 400 });
    harness.fireEvent(grip, 'pointerup', { pointerId: 4 });
  });
  assert.deepEqual(prefs.writes, []);
  assert.ok(!dialog.style.getPropertyValue('--dialog-w'));
  await mounted.unmount();
});

// Applied as custom properties, not as an inline width/height: the stylesheet
// keeps the last word, so the viewport clamp and the full-screen rules for a
// narrow window still win over a stored size.
test('a stored size reopens the viewer at that size, as custom properties', async (t) => {
  const { mounted, dialog } = await openMarkdownViewer(t, {
    seedPref: ['tclaude.dash.attachmentViewer.markdown.size', JSON.stringify({ w: 1240, h: 700 })],
  });
  assert.equal(dialog.style.getPropertyValue('--dialog-w'), '1240px');
  assert.equal(dialog.style.getPropertyValue('--dialog-h'), '700px');
  assert.ok(!dialog.style.width, 'the size never becomes an inline width the CSS cannot beat');
  assert.ok(!dialog.style.height);
  await mounted.unmount();
});

test('a corrupt or absurd stored size falls back to the stylesheet default', async (t) => {
  for (const value of ['not json', JSON.stringify({ w: 0, h: 0 }), JSON.stringify({ w: -5, h: 'wide' })]) {
    const { mounted, dialog } = await openMarkdownViewer(t, {
      seedPref: ['tclaude.dash.attachmentViewer.markdown.size', value],
    });
    assert.ok(!dialog.style.getPropertyValue('--dialog-w'), `stored ${value} is ignored`);
    await mounted.unmount();
  }
});

test('double-clicking the grip restores the default size and drops the pref', async (t) => {
  const { harness, prefs, mounted, dialog } = await openMarkdownViewer(t, {
    seedPref: ['tclaude.dash.attachmentViewer.markdown.size', JSON.stringify({ w: 1240, h: 700 })],
  });
  const grip = dialog.querySelector('.dialog-resizer');
  await harness.act(() => { harness.fireEvent(grip, 'dblclick'); });

  assert.ok(!dialog.style.getPropertyValue('--dialog-w'), 'the stylesheet default is back');
  assert.deepEqual(prefs.writes, [['tclaude.dash.attachmentViewer.markdown.size', null]]);
  await mounted.unmount();
});

// The grip is a real control, so the size must be reachable without a pointer.
test('the grip resizes from the keyboard and restores with Home', async (t) => {
  const { harness, prefs, mounted, dialog } = await openMarkdownViewer(t);
  const grip = dialog.querySelector('.dialog-resizer');
  pinSize(dialog, 900, 860);

  await harness.act(() => { harness.fireEvent(grip, 'keydown', { key: 'ArrowRight' }); });
  assert.equal(dialog.style.getPropertyValue('--dialog-w'), '948px');
  assert.deepEqual(prefs.writes.at(-1),
    ['tclaude.dash.attachmentViewer.markdown.size', JSON.stringify({ w: 948, h: 860 })]);

  await harness.act(() => { harness.fireEvent(grip, 'keydown', { key: 'Home' }); });
  assert.ok(!dialog.style.getPropertyValue('--dialog-w'));
  assert.equal(prefs.writes.at(-1)[1], null);
  await mounted.unmount();
});

// Arrow keys on the grip must not reach the dialog behind it, and Escape must
// still close the viewer from the grip.
test('the grip swallows its arrow keys but not Escape', async (t) => {
  const { harness, mounted, dialog } = await openMarkdownViewer(t);
  const grip = dialog.querySelector('.dialog-resizer');
  pinSize(dialog, 900, 860);

  let arrow;
  await harness.act(() => { arrow = harness.fireEvent(grip, 'keydown', { key: 'ArrowUp' }); });
  assert.equal(arrow.defaultPrevented, true, 'the grip claims the key');

  await harness.act(() => { harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }); });
  assert.equal(mounted.container.querySelector('.markdown-preview-overlay'), null,
    'Escape still closes the viewer');
  await mounted.unmount();
});

// Both viewers share the grip; the image one stores its own size, because a
// screenshot and a report do not want the same shape.
test('the image viewer is resizable too, under its own pref key', async (t) => {
  const { harness, prefs } = await setup(t);
  const { ImageAttachmentPreview } = await harness.importDashboardModule('js/image-preview-overlay.js');
  stubFetch(t, async () => ({ ok: true, status: 200 }));
  const mounted = await harness.mount(harness.html`
    <${ImageAttachmentPreview} messageID=${42} attachment=${imageAttachment} surface="messages" />
  `);
  await harness.act(() => { mounted.container.querySelector('.human-attachment-preview-trigger').click(); });
  await settle(harness);

  const dialog = mounted.container.querySelector('.image-preview-dialog');
  const grip = dialog.querySelector('.dialog-resizer');
  assert.ok(grip, 'the image viewer offers the same grip');
  pinSize(dialog, 1100, 860);

  await harness.act(() => {
    harness.fireEvent(grip, 'pointerdown', { button: 0, pointerId: 9, clientX: 0, clientY: 0 });
    harness.fireEvent(grip, 'pointermove', { pointerId: 9, clientX: 30, clientY: 30 });
    harness.fireEvent(grip, 'pointerup', { pointerId: 9 });
  });
  assert.deepEqual(prefs.writes, [
    ['tclaude.dash.attachmentViewer.image.size', JSON.stringify({ w: 1160, h: 920 })],
  ], 'the image viewer keeps a size of its own');
  await mounted.unmount();
});

// The floors keep the header actions from wrapping; the ceiling keeps a stored
// size from being one no window on this machine can show.
test('a resize is bounded by the dialog floor and the viewport', async (t) => {
  const { harness, mounted, dialog } = await openMarkdownViewer(t);
  const grip = dialog.querySelector('.dialog-resizer');
  pinSize(dialog, 900, 860);

  await harness.act(() => {
    harness.fireEvent(grip, 'pointerdown', { button: 0, pointerId: 5, clientX: 0, clientY: 0 });
    harness.fireEvent(grip, 'pointermove', { pointerId: 5, clientX: -4000, clientY: -4000 });
  });
  assert.equal(dialog.style.getPropertyValue('--dialog-w'), '380px', 'the width floor holds');
  assert.equal(dialog.style.getPropertyValue('--dialog-h'), '260px', 'the height floor holds');

  await harness.act(() => { harness.fireEvent(grip, 'pointermove', { pointerId: 5, clientX: 4000, clientY: 4000 }); });
  assert.equal(dialog.style.getPropertyValue('--dialog-w'), '1568px', 'the viewport is the ceiling');
  assert.equal(dialog.style.getPropertyValue('--dialog-h'), '1168px');
  await mounted.unmount();
});
