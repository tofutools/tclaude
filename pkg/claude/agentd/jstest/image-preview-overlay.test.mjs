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
