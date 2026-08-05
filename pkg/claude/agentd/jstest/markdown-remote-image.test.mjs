import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// These mount the real document renderer over the real parser, because the
// property under test is not "the model produced a node" — markdown-model's own
// suite covers that — but what the browser is actually asked to fetch. An <img>
// in the container with a remote src IS the request; a placeholder is not.
async function render(t, source, attachments) {
  const harness = await createPreactHarness(t);
  const { MarkdownDocument } = await harness.importDashboardModule('js/markdown-document.js');
  const mounted = await harness.mount(
    harness.html`<${MarkdownDocument} source=${source} attachments=${attachments} />`,
  );
  // The parser is imported on demand, so the first paint is the loading state.
  for (let i = 0; i < 3; i += 1) {
    await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  }
  return { harness, ...mounted };
}

const REMOTE = 'https://images.invalid/diagram.png';

test('a remote image is described, not fetched, until the operator asks', async (t) => {
  const { harness, container } = await render(t, `![the diagram](${REMOTE})`);

  assert.equal(container.querySelector('img'), null,
    'nothing in the document may reach the network before the operator decides');
  const placeholder = container.querySelector('.markdown-remote-image');
  assert.ok(placeholder, 'the image is represented by a placeholder');
  assert.match(placeholder.textContent, /the diagram/, 'the alt text says what is being offered');
  assert.match(placeholder.textContent, /images\.invalid/,
    'the host is shown, so the decision is an informed one');

  const load = placeholder.querySelector('.markdown-remote-image-load');
  assert.match(load.getAttribute('aria-label'), /load the external image/i);
  await harness.act(() => load.click());

  const img = container.querySelector('img');
  assert.ok(img, 'the operator’s click is what makes the request');
  assert.equal(img.getAttribute('src'), REMOTE);
  assert.equal(img.getAttribute('alt'), 'the diagram');
  assert.equal(img.getAttribute('referrerpolicy'), 'no-referrer',
    'a loaded image still tells the host as little as it can');
  assert.equal(container.querySelector('.markdown-remote-image'), null);
});

test('an image that fails to load offers another try instead of a broken icon', async (t) => {
  const { harness, container } = await render(t, `![the diagram](${REMOTE})`);
  await harness.act(() => container.querySelector('.markdown-remote-image-load').click());

  const img = container.querySelector('img');
  await harness.act(() => img.dispatchEvent(new container.ownerDocument.defaultView.Event('error')));

  const placeholder = container.querySelector('.markdown-remote-image');
  assert.ok(placeholder, 'the failure is reported in place');
  assert.equal(placeholder.getAttribute('data-failed'), 'true');
  assert.match(placeholder.textContent, /could not be loaded/i);
  const retry = placeholder.querySelector('.markdown-remote-image-load');
  assert.equal(retry.textContent.trim(), 'Try again');
  await harness.act(() => retry.click());
  assert.ok(container.querySelector('img'), 'retrying asks for the image again');
});

test('a document holding several remote images offers them as one decision', async (t) => {
  const { harness, container } = await render(t, [
    '![one](https://a.invalid/1.png)',
    '![two](https://b.invalid/2.png)',
    '![three](https://c.invalid/3.png)',
  ].join('\n\n'));

  const notice = container.querySelector('.markdown-remote-image-notice');
  assert.ok(notice, 'the operator is told once, not per image');
  assert.match(notice.textContent, /3 images/);
  assert.equal(container.querySelectorAll('.markdown-remote-image').length, 3);

  await harness.act(() => notice.querySelector('.markdown-remote-image-notice-load').click());
  assert.deepEqual(
    [...container.querySelectorAll('img')].map((img) => img.getAttribute('src')),
    ['https://a.invalid/1.png', 'https://b.invalid/2.png', 'https://c.invalid/3.png'],
  );
  assert.equal(container.querySelector('.markdown-remote-image-notice'), null,
    'with nothing left held back the notice has nothing to say');
});

// One held-back image needs no banner: its own button is already the shortest
// path, and a notice above every illustrated document would be noise.
test('a single remote image gets no document-level notice', async (t) => {
  const { container } = await render(t, `![only](${REMOTE})`);
  assert.equal(container.querySelector('.markdown-remote-image-notice'), null);
  assert.ok(container.querySelector('.markdown-remote-image'));
});

// The other half of the feature: a report published with its own illustrations.
// These bytes are on the operator's own daemon, so there is no third party to
// decide about and no click to make.
test('an image published with the document renders with no decision to make', async (t) => {
  const { container } = await render(t, '# Report\n\n![the chart](chart.png)\n', [
    { id: 2, filename: 'chart.png', url: '/api/human-messages/9/attachments/2', previewable: true },
  ]);

  assert.equal(container.querySelector('.markdown-remote-image'), null);
  assert.equal(container.querySelector('.markdown-remote-image-notice'), null);
  const img = container.querySelector('img');
  assert.ok(img, 'the attached image is part of the document');
  assert.equal(img.getAttribute('src'), '/api/human-messages/9/attachments/2');
  assert.equal(img.getAttribute('alt'), 'the chart');
  assert.equal(img.getAttribute('loading'), 'lazy');
});

// A surface with no attachment list to hand over must still render documents;
// a reference it cannot resolve falls back to the words the author wrote.
test('a document rendered without its attachments keeps its alt text', async (t) => {
  const { container } = await render(t, '![the chart](chart.png)\n');
  assert.equal(container.querySelector('img'), null);
  assert.match(container.textContent, /the chart/);
});
