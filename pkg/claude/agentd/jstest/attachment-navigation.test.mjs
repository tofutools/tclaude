import test from 'node:test';
import assert from 'node:assert/strict';
import { assertSameNode } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';

const image = { id: 1, filename: 'first.png', url: '/first', previewable: true };
const video = { id: 2, filename: 'movie.mp4', url: '/movie', video: true };
const last = { id: 3, filename: 'last.png', url: '/last', previewable: true };
const file = { id: 4, filename: 'archive.zip', url: '/archive' };
const settle = (harness) => harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });

for (const startVideo of [false, true]) {
  test(`mixed-media browsing from ${startVideo ? 'video' : 'image'} preserves the dialog and display mode`, async (t) => {
    const harness = await createPreactHarness(t);
    const { ImageAttachmentPreview } = await harness.importDashboardModule('js/image-preview-overlay.js');
    const { VideoAttachmentPreview } = await harness.importDashboardModule('js/video-preview-overlay.js');
    const savedFetch = globalThis.fetch;
    globalThis.fetch = async () => ({ ok: true, status: 200 });
    t.after(() => { globalThis.fetch = savedFetch; });
    const Component = startVideo ? VideoAttachmentPreview : ImageAttachmentPreview;
    const launcher = startVideo ? video : image;
    const mounted = await harness.mount(harness.html`<${Component} messageID=${42}
      attachment=${launcher} siblings=${[image, file, video, last]} />`);
    const trigger = mounted.container.querySelector('button');
    await harness.act(() => { trigger.focus(); trigger.click(); });
    await settle(harness);
    const dialog = mounted.container.querySelector('[role="dialog"]');
    const key = async (key, extra = {}) => {
      await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key, ...extra }));
      await settle(harness);
    };
    const name = () => dialog.querySelector('h2').textContent;
    await key('t');
    assert.equal(dialog.getAttribute('data-display'), 'maximized');
    if (!startVideo) {
      await key('ArrowRight');
      assert.equal(name(), 'movie.mp4');
    }
    const player = dialog.querySelector('video.video-preview-video');
    assert.ok(player);
    assert.equal(dialog.querySelector('.attachment-preview-position').textContent, '2 of 3');
    await key('ArrowRight');
    assert.equal(name(), 'movie.mp4', 'plain video arrows remain available for seeking');
    await key('ArrowRight', { shiftKey: true });
    assert.equal(name(), 'last.png');
    assert.equal(dialog.querySelector('video.video-preview-video'), null, 'old playback is removed');
    assertSameNode(mounted.container.querySelector('[role="dialog"]'), dialog);
    assert.equal(dialog.getAttribute('data-display'), 'maximized');
    await key('ArrowRight');
    assert.equal(name(), 'first.png', 'navigation wraps');
    await harness.act(() => dialog.querySelector('[aria-label="Previous attachment"]').click());
    await settle(harness);
    assert.equal(name(), 'last.png');
    await key('ArrowLeft', { ctrlKey: true });
    assert.equal(name(), 'last.png', 'modified browser shortcuts are left alone');
    await key('T');
    assert.equal(dialog.getAttribute('data-display'), 'normal');
    await key('Escape');
    assertSameNode(harness.document.activeElement, trigger);
    await harness.act(() => trigger.click());
    await settle(harness);
    assert.equal(mounted.container.querySelector('h2').textContent, launcher.filename, 'reopening starts at the clicked attachment');
    await mounted.unmount();
  });
}

test('navigation ignores editing and composition; single media hides controls', async (t) => {
  const harness = await createPreactHarness(t);
  const { attachmentNavigationStep } = await harness.importDashboardModule('js/attachment-navigation.js');
  const input = harness.document.createElement('input');
  assert.equal(attachmentNavigationStep({ key: 'ArrowRight', target: input }, false), 0);
  assert.equal(attachmentNavigationStep({ key: 'ArrowRight', isComposing: true }, false), 0);
  const { ImageAttachmentPreview } = await harness.importDashboardModule('js/image-preview-overlay.js');
  const savedFetch = globalThis.fetch;
  globalThis.fetch = async () => ({ ok: true, status: 200 });
  t.after(() => { globalThis.fetch = savedFetch; });
  const mounted = await harness.mount(harness.html`<${ImageAttachmentPreview} messageID=${42}
    attachment=${image} siblings=${[image, file]} />`);
  await harness.act(() => mounted.container.querySelector('button').click());
  await settle(harness);
  assert.equal(mounted.container.querySelector('.attachment-preview-nav'), null);
  await mounted.unmount();
});
