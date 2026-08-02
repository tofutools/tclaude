import test from 'node:test';
import assert from 'node:assert/strict';
import { assertSameNode } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';

const attachment = {
  id: 4,
  filename: 'plan.md',
  content_type: 'text/markdown; charset=utf-8',
  size_bytes: 640,
  markdown: true,
  url: '/api/human-messages/42/attachments/4',
};

const document = [
  '# Plan',
  '',
  'Ship it. <script>alert(1)</script>',
  '',
  '- [docs](https://example.invalid/d)',
].join('\n');

// settle lets the on-demand markdown-it import and the fetch resolve.
async function settle(harness, times = 3) {
  for (let i = 0; i < times; i += 1) {
    await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  }
}

function stubFetch(t, respond) {
  const saved = globalThis.fetch;
  const requests = [];
  globalThis.fetch = async (url, options) => {
    requests.push({ url, options });
    return respond(url, options);
  };
  t.after(() => { globalThis.fetch = saved; });
  return requests;
}

function okText(body) {
  return { ok: true, status: 200, text: async () => body };
}

test('a Markdown attachment opens an accessible viewer and renders the document', async (t) => {
  const harness = await createPreactHarness(t);
  const { MarkdownAttachmentPreview } = await harness.importDashboardModule(
    'js/markdown-preview-overlay.js',
  );
  const requests = stubFetch(t, () => okText(document));

  const mounted = await harness.mount(harness.html`
    <${MarkdownAttachmentPreview} messageID=${42} attachment=${attachment} surface="messages" />
  `);
  const trigger = mounted.container.querySelector('.human-attachment-markdown-trigger');
  assert.ok(trigger, 'a Markdown attachment exposes a read button');
  assert.equal(trigger.getAttribute('aria-label'), 'Read plan.md');
  assert.equal(mounted.container.querySelector('.markdown-preview-overlay'), null);

  await harness.act(() => {
    trigger.focus();
    trigger.click();
  });
  await settle(harness);

  const overlay = mounted.container.querySelector('.markdown-preview-overlay');
  assert.ok(overlay, 'clicking the button opens the modal overlay');
  const dialog = overlay.querySelector('[role="dialog"]');
  assert.equal(dialog.getAttribute('aria-modal'), 'true');
  assert.equal(dialog.querySelector('h2').textContent, 'plan.md');
  assert.equal(dialog.querySelector('.markdown-preview-download').getAttribute('href'),
    '/api/human-messages/42/attachments/4');
  assert.equal(requests.length, 1);
  assert.equal(requests[0].url, '/api/human-messages/42/attachments/4');
  assertSameNode(harness.document.activeElement, dialog.querySelector('.markdown-preview-close'),
    'the viewer focuses its close control');

  const rendered = dialog.querySelector('.markdown-document');
  assert.ok(rendered, 'the document is rendered, not shown as source');
  assert.equal(rendered.querySelector('h1').textContent, 'Plan');
  const link = rendered.querySelector('a');
  assert.equal(link.getAttribute('href'), 'https://example.invalid/d');
  assert.equal(link.getAttribute('rel'), 'noopener noreferrer');

  // The document's raw <script> is content, and must reach the DOM as
  // characters rather than as an element.
  assert.equal(rendered.querySelector('script'), null, 'no script element is built');
  assert.match(rendered.textContent, /<script>alert\(1\)<\/script>/);

  await harness.act(() => {
    harness.fireEvent(harness.document, 'keydown', { key: 'Escape' });
  });
  assert.equal(mounted.container.querySelector('.markdown-preview-overlay'), null,
    'Escape closes the topmost viewer');
  assertSameNode(harness.document.activeElement, trigger,
    'closing restores focus to the invoker');
  await mounted.unmount();
});

test('the viewer can show the original Markdown source', async (t) => {
  const harness = await createPreactHarness(t);
  const { MarkdownAttachmentPreview } = await harness.importDashboardModule(
    'js/markdown-preview-overlay.js',
  );
  stubFetch(t, () => okText('# Plan\n'));

  const mounted = await harness.mount(harness.html`
    <${MarkdownAttachmentPreview} messageID=${42} attachment=${attachment} />
  `);
  await harness.act(() => mounted.container.querySelector('button').click());
  await settle(harness);

  const toggle = mounted.container.querySelector('.markdown-preview-toggle');
  assert.equal(toggle.getAttribute('aria-pressed'), 'false');
  await harness.act(() => toggle.click());

  const source = mounted.container.querySelector('.markdown-preview-source');
  assert.ok(source, 'the source view shows the unrendered text');
  assert.equal(source.textContent, '# Plan\n');
  assert.equal(mounted.container.querySelector('.markdown-document'), null);

  await harness.act(() => mounted.container.querySelector('.markdown-preview-toggle').click());
  await settle(harness);
  assert.ok(mounted.container.querySelector('.markdown-document'), 'the toggle goes both ways');
  await mounted.unmount();
});

test('a removed attachment gets an explicit gone state', async (t) => {
  const harness = await createPreactHarness(t);
  const { MarkdownAttachmentPreview } = await harness.importDashboardModule(
    'js/markdown-preview-overlay.js',
  );
  stubFetch(t, () => ({ ok: false, status: 410 }));

  const mounted = await harness.mount(harness.html`
    <${MarkdownAttachmentPreview} messageID=${7} attachment=${attachment} />
  `);
  await harness.act(() => mounted.container.querySelector('button').click());
  await settle(harness);

  const state = mounted.container.querySelector('.markdown-preview-state');
  assert.ok(state);
  assert.match(state.textContent, /no longer available/i);
  assert.equal(mounted.container.querySelector('.markdown-preview-download'), null,
    'a gone attachment does not offer a link that will only fail again');
  await mounted.unmount();
});

test('a failed read leaves the download as the way through', async (t) => {
  const harness = await createPreactHarness(t);
  const { MarkdownAttachmentPreview } = await harness.importDashboardModule(
    'js/markdown-preview-overlay.js',
  );
  stubFetch(t, async () => { throw new Error('offline'); });

  const mounted = await harness.mount(harness.html`
    <${MarkdownAttachmentPreview} messageID=${7} attachment=${attachment} />
  `);
  await harness.act(() => mounted.container.querySelector('button').click());
  await settle(harness);

  assert.match(mounted.container.querySelector('.markdown-preview-state').textContent,
    /could not be read/i);
  assert.ok(mounted.container.querySelector('.markdown-preview-download'));
  await mounted.unmount();
});

test('attachments the daemon did not call Markdown keep the download-only surface', async (t) => {
  const harness = await createPreactHarness(t);
  const { MarkdownAttachmentPreview } = await harness.importDashboardModule(
    'js/markdown-preview-overlay.js',
  );
  for (const variant of [{ ...attachment, markdown: false }, { ...attachment, markdown: undefined }]) {
    const mounted = await harness.mount(harness.html`
      <${MarkdownAttachmentPreview} messageID=${9} attachment=${variant} />
    `);
    assert.equal(mounted.container.querySelector('.human-attachment-markdown-trigger'), null);
    await mounted.unmount();
  }
});

test('the legacy single-attachment route still backs a viewer', async (t) => {
  const harness = await createPreactHarness(t);
  const { MarkdownAttachmentPreview } = await harness.importDashboardModule(
    'js/markdown-preview-overlay.js',
  );
  const requests = stubFetch(t, () => okText('# Plan\n'));
  const legacy = { ...attachment, url: undefined };

  const mounted = await harness.mount(harness.html`
    <${MarkdownAttachmentPreview} messageID=${42} attachment=${legacy} />
  `);
  await harness.act(() => mounted.container.querySelector('button').click());
  await settle(harness);

  assert.equal(requests[0].url, '/api/human-messages/42/attachment');
  await mounted.unmount();
});
