import test from 'node:test';
import assert from 'node:assert/strict';
import { assertSameNode } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';

const attachment = {
  id: 7,
  url: '/api/human-messages/42/attachments/7',
  filename: 'demo.mp4',
  content_type: 'video/mp4',
  size_bytes: 4 * 1024 * 1024,
  video: true,
};

test('video attachment opens a native player in the shared overlay', async (t) => {
  const harness = await createPreactHarness(t);
  const { VideoAttachmentPreview } = await harness.importDashboardModule(
    'js/video-preview-overlay.js',
  );
  const savedFetch = globalThis.fetch;
  const requests = [];
  globalThis.fetch = async (url, options) => {
    requests.push({ url, options });
    return { ok: true, status: 200 };
  };
  t.after(() => { globalThis.fetch = savedFetch; });

  const mounted = await harness.mount(harness.html`
    <${VideoAttachmentPreview} messageID=${42} attachment=${attachment} surface="messages" />
  `);
  const trigger = mounted.container.querySelector('.video-preview-trigger');
  assert.ok(trigger, 'video attachments expose a poster-frame button');
  assert.equal(trigger.getAttribute('aria-label'), 'Play demo.mp4');
  assert.equal(trigger.querySelector('video').getAttribute('preload'), 'metadata',
    'the thumbnail only fetches metadata');

  await harness.act(() => {
    trigger.focus();
    trigger.click();
  });
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });

  const dialog = mounted.container.querySelector('.video-preview-overlay [role="dialog"]');
  assert.ok(dialog, 'clicking the thumbnail opens the player');
  assert.equal(requests[0].url, attachment.url);
  assert.equal(requests[0].options.method, 'HEAD');
  const player = dialog.querySelector('video.video-preview-video');
  assert.ok(player, 'the player is a native <video>');
  assert.equal(player.getAttribute('src'), attachment.url);
  assert.ok(player.hasAttribute('controls'));
  assert.equal(dialog.querySelector('.image-preview-download').getAttribute('href'), attachment.url);

  await harness.act(() => { harness.fireEvent(player, 'error'); });
  assert.match(dialog.querySelector('.image-preview-state').textContent, /can't play/,
    'an undecodable codec falls back to a download hint');

  await harness.act(() => {
    harness.fireEvent(harness.document, 'keydown', { key: 'Escape' });
  });
  assert.equal(mounted.container.querySelector('.video-preview-overlay'), null);
  assertSameNode(harness.document.activeElement, trigger,
    'closing restores focus to the thumbnail invoker');
  await mounted.unmount();
});

test('non-video attachments render nothing', async (t) => {
  const harness = await createPreactHarness(t);
  const { VideoAttachmentPreview } = await harness.importDashboardModule(
    'js/video-preview-overlay.js',
  );
  const mounted = await harness.mount(harness.html`
    <${VideoAttachmentPreview} messageID=${1} attachment=${{ ...attachment, video: false }} />
  `);
  assert.equal(mounted.container.innerHTML, '');
  await mounted.unmount();
});
