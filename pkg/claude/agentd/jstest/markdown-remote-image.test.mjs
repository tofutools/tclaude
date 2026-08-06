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

// A fetch that failed leaves the operator exactly where they started for that
// image, so the document has to count it among what it is still holding back —
// otherwise the count above the document disagrees with the placeholders under
// it, and the bulk control silently skips the one image that needs a retry.
test('an image that fails after Load all is counted and offered again', async (t) => {
  const { harness, container } = await render(t, [
    '![one](https://a.invalid/1.png)', '![two](https://b.invalid/2.png)',
    '![three](https://c.invalid/3.png)',
  ].join('\n\n'));
  const W = container.ownerDocument.defaultView;
  const fail = (img) => harness.act(() => img.dispatchEvent(new W.Event('error')));
  const placeholders = () => container.querySelectorAll('.markdown-remote-image');
  const notice = () => container.querySelector('.markdown-remote-image-notice');

  await harness.act(() => notice().querySelector('.markdown-remote-image-notice-load').click());
  assert.equal(container.querySelectorAll('img').length, 3);

  const [first, second] = container.querySelectorAll('img');
  await fail(first);
  await fail(second);

  assert.equal(placeholders().length, 2, 'both failures are back to placeholders');
  assert.equal(container.querySelectorAll('img').length, 1, 'the one that loaded stays');
  assert.match(notice().textContent, /2 images/,
    'the count names what is on screen, failures included');
  for (const placeholder of placeholders()) {
    assert.equal(placeholder.getAttribute('data-failed'), 'true');
    assert.equal(placeholder.querySelector('.markdown-remote-image-load').textContent.trim(),
      'Try again');
  }

  // Load all is the retry for every one of them, not just the untried ones.
  await harness.act(() => notice().querySelector('.markdown-remote-image-notice-load').click());
  assert.deepEqual(
    [...container.querySelectorAll('img')].map((img) => img.getAttribute('src')),
    ['https://a.invalid/1.png', 'https://b.invalid/2.png', 'https://c.invalid/3.png']);
  assert.equal(placeholders().length, 0);
  assert.equal(notice(), null, 'with nothing held back the banner has nothing to say');

  // And a failure after the retry is still reported, rather than latching.
  await fail(container.querySelectorAll('img')[0]);
  assert.equal(placeholders().length, 1);
  assert.equal(placeholders()[0].getAttribute('data-failed'), 'true');
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

// The notice counts what is on screen. A document showing the same URL twice
// shows two placeholders, and a line above it saying "1" would be describing
// something the operator cannot see. Loading is still by URL, so the pair
// resolves together — one decision, two pictures.
test('a repeated remote URL is counted per placeholder and loaded as one', async (t) => {
  const { harness, container } = await render(t, [
    `![one](${REMOTE})`, `![again](${REMOTE})`, '![other](https://b.invalid/2.png)',
  ].join('\n\n'));

  assert.equal(container.querySelectorAll('.markdown-remote-image').length, 3);
  assert.match(container.querySelector('.markdown-remote-image-notice').textContent, /3 images/,
    'the count names the placeholders, not the distinct hosts');

  const first = container.querySelectorAll('.markdown-remote-image-load')[0];
  await harness.act(() => first.click());
  assert.deepEqual(
    [...container.querySelectorAll('img')].map((img) => img.getAttribute('src')),
    [REMOTE, REMOTE], 'one decision covers every placeholder for that URL');
  assert.equal(container.querySelectorAll('.markdown-remote-image').length, 1,
    'only the unrelated image is still held back');
  assert.equal(container.querySelector('.markdown-remote-image-notice'), null,
    'and one of those needs no banner');
});

// A document may wrap its image in a link: `[![alt](img)](target)`. The button
// then sits inside an anchor the author chose the destination of, and loading
// the image must not also be a navigation.
test('loading an image inside a link does not follow the link', async (t) => {
  const { harness, container } = await render(t,
    `[![the diagram](${REMOTE})](https://elsewhere.invalid/)`);

  const anchor = container.querySelector('a');
  assert.ok(anchor?.querySelector('.markdown-remote-image'), 'the placeholder is inside the link');

  let navigated = false;
  anchor.addEventListener('click', (event) => { if (!event.defaultPrevented) navigated = true; });
  await harness.act(() => container.querySelector('.markdown-remote-image-load').click());

  assert.equal(navigated, false, 'the click loaded the image instead of opening the link');
  assert.equal(container.querySelector('img')?.getAttribute('src'), REMOTE);
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

// What the operator agreed to fetch belongs to the document they were reading.
// The clearing has to happen in the same commit that paints the new document:
// an effect would run one commit late, and by then the browser has already been
// handed an <img> the operator never approved for THIS document.
test('a new document does not inherit the last one’s loaded images', async (t) => {
  const harness = await createPreactHarness(t);
  const { MarkdownDocument } = await harness.importDashboardModule('js/markdown-document.js');
  const mounted = await harness.mount(
    harness.html`<${MarkdownDocument} source=${`![d](${REMOTE})`} />`,
  );
  for (let i = 0; i < 3; i += 1) {
    await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  }
  await harness.act(() => mounted.container.querySelector('.markdown-remote-image-load').click());
  assert.equal(mounted.container.querySelector('img').getAttribute('src'), REMOTE);

  // The same URL in a different document: nothing about the first decision
  // says anything about this one. Rendered deliberately WITHOUT act, so the
  // assertion sees the commit the browser sees — effects have not run yet, and
  // an <img> present here is a request already in flight.
  harness.preact.render(
    harness.html`<${MarkdownDocument} source=${`# Other\n\n![d](${REMOTE})`} />`,
    mounted.container,
  );
  // Read the commit, then let effects run before asserting: throwing inside the
  // unflushed window leaves the renderer mid-update and the failure surfaces as
  // a hung suite instead of a message.
  const committed = {
    img: mounted.container.querySelector('img'),
    placeholder: mounted.container.querySelector('.markdown-remote-image'),
  };
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });

  assert.equal(committed.img, null,
    'the second document must not be fetching on the strength of the first');
  assert.ok(committed.placeholder, 'it offers the same image as a fresh decision');
  assert.equal(mounted.container.querySelector('img'), null, 'and it stays that way');
  await mounted.unmount();
});

// A surface with no attachment list to hand over must still render documents;
// a reference it cannot resolve falls back to the words the author wrote.
test('a document rendered without its attachments keeps its alt text', async (t) => {
  const { container } = await render(t, '![the chart](chart.png)\n');
  assert.equal(container.querySelector('img'), null);
  assert.match(container.textContent, /the chart/);
});
