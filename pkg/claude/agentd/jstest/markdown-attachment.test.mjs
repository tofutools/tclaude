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

async function mount(t, respond, item = attachment, messageID = 42) {
  const harness = await createPreactHarness(t);
  const { MarkdownAttachment } = await harness.importDashboardModule('js/markdown-attachment.js');
  const requests = stubFetch(t, respond);
  const mounted = await harness.mount(harness.html`
    <${MarkdownAttachment} messageID=${messageID} attachment=${item} surface="messages" />
  `);
  await settle(harness);
  return { harness, mounted, requests };
}

// The document is the message, so it must be on screen without the operator
// doing anything — no click, and nothing to go looking for.
test('a Markdown attachment renders its document in the message', async (t) => {
  const { mounted, requests } = await mount(t, () => okText(document));

  assert.equal(requests.length, 1, 'the document is fetched when the message is shown');
  assert.equal(requests[0].url, '/api/human-messages/42/attachments/4');

  const rendered = mounted.container.querySelector('.markdown-attachment-document .markdown-document');
  assert.ok(rendered, 'the document renders inline, not behind a control');
  assert.equal(rendered.querySelector('h1').textContent, 'Plan');
  const link = rendered.querySelector('a');
  assert.equal(link.getAttribute('href'), 'https://example.invalid/d');
  assert.equal(link.getAttribute('rel'), 'noopener noreferrer');

  // The document's raw <script> is content, and must reach the DOM as
  // characters rather than as an element.
  assert.equal(rendered.querySelector('script'), null, 'no script element is built');
  assert.match(rendered.textContent, /<script>alert\(1\)<\/script>/);

  // Nothing is opened yet: the overlay is the source view, not the document.
  assert.equal(mounted.container.querySelector('.markdown-preview-overlay'), null);
  await mounted.unmount();
});

test('the control opens the original Markdown source', async (t) => {
  const { harness, mounted } = await mount(t, () => okText(document));

  const trigger = mounted.container.querySelector('.human-attachment-markdown-trigger');
  assert.ok(trigger, 'a Markdown attachment offers a source control');
  assert.match(trigger.textContent, /View source/);
  assert.equal(trigger.getAttribute('aria-label'), 'View the Markdown source of plan.md');
  assert.ok(!trigger.disabled, 'the control is live once the document has loaded');

  await harness.act(() => {
    trigger.focus();
    trigger.click();
  });

  const overlay = mounted.container.querySelector('.markdown-preview-overlay');
  assert.ok(overlay, 'the control opens the modal source view');
  const dialog = overlay.querySelector('[role="dialog"]');
  assert.equal(dialog.getAttribute('aria-modal'), 'true');
  assert.equal(dialog.querySelector('h2').textContent, 'plan.md');
  assert.equal(dialog.querySelector('.markdown-preview-source').textContent, document,
    'the source view shows the unrendered text verbatim');
  assert.equal(dialog.querySelector('.markdown-document'), null,
    'the overlay is the source, not a second rendering');
  assert.equal(dialog.querySelector('.markdown-preview-download').getAttribute('href'),
    '/api/human-messages/42/attachments/4');
  assertSameNode(harness.document.activeElement, dialog.querySelector('.markdown-preview-close'),
    'the source view focuses its close control');

  await harness.act(() => {
    harness.fireEvent(harness.document, 'keydown', { key: 'Escape' });
  });
  assert.equal(mounted.container.querySelector('.markdown-preview-overlay'), null,
    'Escape closes the topmost source view');
  assertSameNode(harness.document.activeElement, trigger,
    'closing restores focus to the invoker');
  await mounted.unmount();
});

// Reading the source of a document that never arrived would show an empty box,
// so the control waits for the fetch rather than lying about being ready.
test('the source control is inert until the document has been read', async (t) => {
  const harness = await createPreactHarness(t);
  const { MarkdownAttachment } = await harness.importDashboardModule('js/markdown-attachment.js');
  let release;
  stubFetch(t, () => new Promise((resolve) => { release = () => resolve(okText('# Plan\n')); }));
  const mounted = await harness.mount(harness.html`
    <${MarkdownAttachment} messageID=${42} attachment=${attachment} />
  `);

  const trigger = mounted.container.querySelector('.human-attachment-markdown-trigger');
  assert.equal(trigger.disabled, true, 'the control is disabled while the fetch is in flight');
  assert.match(mounted.container.querySelector('.markdown-attachment-state').textContent,
    /loading document/i);

  await harness.act(async () => { release(); await new Promise((resolve) => setTimeout(resolve, 0)); });
  await settle(harness);
  assert.equal(mounted.container.querySelector('.human-attachment-markdown-trigger').disabled, false);
  await mounted.unmount();
});

test('a removed attachment gets an explicit gone state in place of the document', async (t) => {
  const { mounted } = await mount(t, () => ({ ok: false, status: 410 }));
  const state = mounted.container.querySelector('.markdown-attachment-state');
  assert.ok(state);
  assert.match(state.textContent, /no longer available/i);
  assert.equal(mounted.container.querySelector('.markdown-document'), null);
  await mounted.unmount();
});

test('a failed read says so where the document would have been', async (t) => {
  const { mounted } = await mount(t, async () => { throw new Error('offline'); });
  assert.match(mounted.container.querySelector('.markdown-attachment-state').textContent,
    /could not be read/i);
  await mounted.unmount();
});

test('attachments the daemon did not call Markdown render nothing and fetch nothing', async (t) => {
  for (const variant of [{ ...attachment, markdown: false }, { ...attachment, markdown: undefined }]) {
    const { mounted, requests } = await mount(t, () => okText(document), variant);
    assert.equal(mounted.container.querySelector('.markdown-attachment-document'), null);
    assert.equal(mounted.container.querySelector('.human-attachment-markdown-trigger'), null);
    assert.equal(requests.length, 0, 'a non-Markdown attachment is never read');
    await mounted.unmount();
  }
});

test('the legacy single-attachment route still backs a document', async (t) => {
  const { requests } = await mount(t, () => okText('# Plan\n'), { ...attachment, url: undefined });
  assert.equal(requests[0].url, '/api/human-messages/42/attachment');
});
