import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const withFiles = {
  id: 7, subject: 'Plan', body: 'see files',
  attachments: [
    { filename: 'a.png', size_bytes: 1024 },
    { filename: 'b.md', size_bytes: 1024 },
  ],
};

test('download splits into one-click zip and a menu of variants, including copy as Markdown', async (t) => {
  const harness = await createPreactHarness(t);
  const { MessageDownloadMenu } = await harness.importDashboardModule('js/message-download-menu.js');
  const downloads = [];
  const toasts = [];
  const copied = [];
  const fetched = [];
  const mounted = await harness.mount(harness.html`<${MessageDownloadMenu} message=${withFiles}
    download=${(href) => downloads.push(href)} toast=${(text) => toasts.push(text)}
    fetchImpl=${async (url) => { fetched.push(url); return { ok: true, text: async () => '# Plan\n' }; }}
    clipboard=${{ writeText: async (text) => { copied.push(text); } }} />`);
  const root = mounted.container;

  await harness.act(() => root.querySelector('.mail-download-main').click());
  assert.deepEqual(downloads, ['/api/human-messages/7/export?attachments=1'], 'one click takes the files along');
  assert.equal(root.querySelector('[role="menu"]'), null);

  const caret = root.querySelector('.mail-download-caret');
  await harness.act(() => caret.click());
  assert.equal(caret.getAttribute('aria-expanded'), 'true');
  const items = [...root.querySelectorAll('[role="menuitem"]')];
  assert.deepEqual(items.map((item) => item.dataset.export), ['zip', 'md', 'copy']);
  assert.match(items[0].textContent, /message\.md \+ 2 files · 2\.0 KiB/);

  await harness.act(() => items[1].click());
  assert.equal(downloads.at(-1), '/api/human-messages/7/export', 'message only drops the files');
  assert.equal(root.querySelector('[role="menu"]'), null, 'choosing closes the menu');

  await harness.act(() => caret.click());
  await harness.act(async () => { root.querySelector('[data-export="copy"]').click(); await Promise.resolve(); });
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  assert.deepEqual(fetched, ['/api/human-messages/7/export']);
  assert.deepEqual(copied, ['# Plan\n']);
  assert.deepEqual(toasts, ['Message #7 copied as Markdown']);

  await harness.act(() => caret.click());
  harness.fireEvent(root.querySelector('[role="menu"]'), 'keydown', { key: 'Escape' });
  await harness.act(() => {});
  assert.equal(root.querySelector('[role="menu"]'), null, 'Escape closes the menu');
  await mounted.unmount();
});

test('a message without files downloads Markdown and offers no zip', async (t) => {
  const harness = await createPreactHarness(t);
  const { MessageDownloadMenu } = await harness.importDashboardModule('js/message-download-menu.js');
  const downloads = [];
  const mounted = await harness.mount(harness.html`<${MessageDownloadMenu} message=${{ id: 3, subject: 's', body: 'b' }}
    download=${(href) => downloads.push(href)} />`);
  await harness.act(() => mounted.container.querySelector('.mail-download-main').click());
  assert.deepEqual(downloads, ['/api/human-messages/3/export']);
  await harness.act(() => mounted.container.querySelector('.mail-download-caret').click());
  assert.deepEqual([...mounted.container.querySelectorAll('[role="menuitem"]')].map((item) => item.dataset.export), ['md', 'copy']);
  await mounted.unmount();
});
