import test from 'node:test';
import assert from 'node:assert/strict';
import { assertSameNode } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';

const attachment = {
  filename: 'screen.png',
  content_type: 'image/png',
  size_bytes: 2048,
  previewable: true,
};

test('image attachment opens the shared accessible overlay and restores focus', async (t) => {
  const harness = await createPreactHarness(t);
  const { ImageAttachmentPreview } = await harness.importDashboardModule(
    'js/image-preview-overlay.js',
  );
  const savedFetch = globalThis.fetch;
  const requests = [];
  globalThis.fetch = async (url, options) => {
    requests.push({ url, options });
    return { ok: true, status: 200 };
  };
  t.after(() => { globalThis.fetch = savedFetch; });

  const mounted = await harness.mount(harness.html`
    <${ImageAttachmentPreview} messageID=${42} attachment=${attachment} surface="messages" />
  `);
  const trigger = mounted.container.querySelector('.human-attachment-preview-trigger');
  assert.ok(trigger, 'previewable attachments expose a thumbnail button');
  assert.equal(trigger.getAttribute('aria-label'), 'Preview screen.png');
  assert.equal(mounted.container.querySelector('.image-preview-overlay'), null);

  await harness.act(() => {
    trigger.focus();
    trigger.click();
  });
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });

  const overlay = mounted.container.querySelector('.image-preview-overlay');
  assert.ok(overlay, 'clicking the thumbnail opens the modal overlay');
  const dialog = overlay.querySelector('[role="dialog"]');
  assert.equal(dialog.getAttribute('aria-modal'), 'true');
  assert.equal(dialog.querySelector('h2').textContent, 'screen.png');
  assert.equal(dialog.querySelector('.image-preview-download').getAttribute('href'),
    '/api/human-messages/42/attachment');
  assert.equal(requests[0].url, '/api/human-messages/42/attachment');
  assert.equal(requests[0].options.method, 'HEAD');
  assertSameNode(harness.document.activeElement, dialog.querySelector('.image-preview-close'),
    'the overlay focuses its close control');

  await harness.act(() => {
    harness.fireEvent(harness.document, 'keydown', { key: 'Escape' });
  });
  assert.equal(mounted.container.querySelector('.image-preview-overlay'), null,
    'Escape closes the topmost image overlay');
  assertSameNode(harness.document.activeElement, trigger,
    'closing restores focus to the thumbnail invoker');
  await mounted.unmount();
});

test('a missing attachment gets an explicit gone state', async (t) => {
  const harness = await createPreactHarness(t);
  const { ImageAttachmentPreview } = await harness.importDashboardModule(
    'js/image-preview-overlay.js',
  );
  const savedFetch = globalThis.fetch;
  globalThis.fetch = async () => ({ ok: false, status: 410 });
  t.after(() => { globalThis.fetch = savedFetch; });

  const mounted = await harness.mount(harness.html`
    <${ImageAttachmentPreview} messageID=${7} attachment=${attachment} />
  `);
  await harness.act(() => mounted.container.querySelector('button').click());
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });

  const state = mounted.container.querySelector('.image-preview-state');
  assert.ok(state);
  assert.match(state.textContent, /no longer available/i);
  assert.equal(mounted.container.querySelector('.image-preview-download'), null,
    'a gone attachment does not offer a link that will only fail again');
  await mounted.unmount();
});

test('non-previewable attachments keep the ordinary download-only surface', async (t) => {
  const harness = await createPreactHarness(t);
  const { ImageAttachmentPreview } = await harness.importDashboardModule(
    'js/image-preview-overlay.js',
  );
  const mounted = await harness.mount(harness.html`
    <${ImageAttachmentPreview} messageID=${9}
      attachment=${{ ...attachment, previewable: false, content_type: 'image/svg+xml' }} />
  `);
  assert.equal(mounted.container.querySelector('.human-attachment-preview-trigger'), null);
  await mounted.unmount();
});

test('a published file is previewed through its own per-file URL', async (t) => {
  const harness = await createPreactHarness(t);
  const { ImageAttachmentPreview } = await harness.importDashboardModule(
    'js/image-preview-overlay.js',
  );
  const savedFetch = globalThis.fetch;
  const requests = [];
  globalThis.fetch = async (url, options) => {
    requests.push({ url, options });
    return { ok: true, status: 200 };
  };
  t.after(() => { globalThis.fetch = savedFetch; });

  // One notification can publish several files, so the item carries the URL
  // that identifies it — the legacy route names only the first.
  const second = { ...attachment, id: 9, filename: 'guild.png', url: '/api/human-messages/42/attachments/9' };
  const mounted = await harness.mount(harness.html`
    <${ImageAttachmentPreview} messageID=${42} attachment=${second} surface="messages" />
  `);
  await harness.act(() => {
    mounted.container.querySelector('.human-attachment-preview-trigger').click();
  });
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });

  const dialog = mounted.container.querySelector('[role="dialog"]');
  assert.equal(dialog.querySelector('.image-preview-download').getAttribute('href'),
    '/api/human-messages/42/attachments/9');
  assert.equal(requests[0].url, '/api/human-messages/42/attachments/9');
  await mounted.unmount();
});

async function openLoadedPreview(t, harness, natural = { w: 1600, h: 900 }) {
  const { ImageAttachmentPreview } = await harness.importDashboardModule(
    'js/image-preview-overlay.js',
  );
  const savedFetch = globalThis.fetch;
  globalThis.fetch = async () => ({ ok: true, status: 200 });
  t.after(() => { globalThis.fetch = savedFetch; });
  const mounted = await harness.mount(harness.html`
    <${ImageAttachmentPreview} messageID=${3} attachment=${attachment} />
  `);
  await harness.act(() => mounted.container.querySelector('.human-attachment-preview-trigger').click());
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  const image = mounted.container.querySelector('.image-preview-image');
  Object.defineProperty(image, 'naturalWidth', { value: natural.w });
  Object.defineProperty(image, 'naturalHeight', { value: natural.h });
  await harness.act(() => { harness.fireEvent(image, 'load'); });
  return { mounted, dialog: mounted.container.querySelector('[role="dialog"]') };
}

const zoomLabel = (dialog) => dialog.querySelector('.image-preview-zoom-value').textContent;

test('Ctrl+wheel and a trackpad pinch zoom the image and cancel page zoom', async (t) => {
  const harness = await createPreactHarness(t);
  const { mounted, dialog } = await openLoadedPreview(t, harness);
  // linkedom has no layout, so "fit" falls back to actual size.
  assert.equal(zoomLabel(dialog), '100%');

  await harness.act(() => { harness.fireEvent(dialog, 'wheel', { deltaY: -30, deltaMode: 0 }); });
  assert.equal(zoomLabel(dialog), '100%', 'a plain wheel scrolls rather than zooms');

  let event;
  await harness.act(() => {
    event = harness.fireEvent(dialog, 'wheel', { deltaY: -30, deltaMode: 0, ctrlKey: true });
  });
  assert.equal(event.defaultPrevented, true, 'the browser does not also zoom the page');
  assert.equal(zoomLabel(dialog), '135%');
  const image = dialog.querySelector('.image-preview-image');
  assert.equal(image.style.width, `${1600 * Math.exp(0.3)}px`, 'zoom resizes the image in layout');

  await harness.act(() => {
    harness.fireEvent(dialog, 'gesturestart', { scale: 1 });
    harness.fireEvent(dialog, 'gesturechange', { scale: 0.5 });
  });
  assert.equal(zoomLabel(dialog), '67%', 'Safari pinch applies its scale ratio');
  await mounted.unmount();
});

test('zoom keys, Fit and 1:1 drive the image scale', async (t) => {
  const harness = await createPreactHarness(t);
  const { mounted, dialog } = await openLoadedPreview(t, harness);

  let event;
  await harness.act(() => { event = harness.fireEvent(dialog, 'keydown', { key: '=', ctrlKey: true }); });
  assert.equal(event.defaultPrevented, true);
  assert.equal(zoomLabel(dialog), '125%');
  await harness.act(() => { harness.fireEvent(dialog, 'keydown', { key: '-' }); });
  await harness.act(() => { harness.fireEvent(dialog, 'keydown', { key: '-' }); });
  assert.equal(zoomLabel(dialog), '80%');

  const fit = [...dialog.querySelectorAll('.image-preview-footer button')].find((b) => b.textContent === 'Fit');
  assert.equal(fit.getAttribute('aria-pressed'), 'false');
  await harness.act(() => { harness.fireEvent(dialog, 'keydown', { key: '0' }); });
  assert.equal(fit.getAttribute('aria-pressed'), 'true', '0 returns to fit');

  const actual = dialog.querySelector('[aria-label="Actual size"]');
  await harness.act(() => { harness.fireEvent(dialog, 'keydown', { key: '+' }); });
  await harness.act(() => actual.click());
  assert.equal(zoomLabel(dialog), '100%');
  assert.equal(actual.getAttribute('aria-pressed'), 'true');
  await mounted.unmount();
});

test('the viewer can be maximized to the window', async (t) => {
  const harness = await createPreactHarness(t);
  const { mounted, dialog } = await openLoadedPreview(t, harness);
  assert.equal(dialog.getAttribute('data-display'), 'normal');
  const maximize = dialog.querySelector('.viewer-maximize');
  await harness.act(() => maximize.click());
  assert.equal(dialog.getAttribute('data-display'), 'maximized');
  assert.equal(maximize.getAttribute('aria-pressed'), 'true');
  await harness.act(() => maximize.click());
  assert.equal(dialog.getAttribute('data-display'), 'normal');
  await mounted.unmount();
});
