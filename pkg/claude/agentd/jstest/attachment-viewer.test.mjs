// The attachment viewer (attachment-viewer.js) on the task board: a task's
// files, a comment's and the List's 📎 open the Messages tab's viewer, the
// files read through the board's api (attachment-source.js taskFileSource).
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// A 1×1 PNG, so the image viewer has real bytes to show.
const PNG = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==';
const b64 = (text) => Buffer.from(text, 'utf8').toString('base64');

const files = [
  { id: 1, name: 'chart.png', size: 68, content_type: 'image/png', sha256: 'a'.repeat(64), image: true, previewable: true, added_by: 'human:operator' },
  { id: 2, name: 'plan.md', size: 20, content_type: 'text/markdown', sha256: 'b'.repeat(64), markdown: true, added_by: 'human:operator' },
  { id: 3, name: 'log.txt', size: 5, content_type: 'text/plain', sha256: 'c'.repeat(64), added_by: 'human:operator' },
  { id: 4, name: 'chart-2.png', size: 68, content_type: 'image/png', sha256: 'd'.repeat(64), image: true, previewable: true, added_by: 'human:operator' },
];

function fakeApi({ gone = [] } = {}) {
  const reads = [];
  return {
    reads,
    async fileData(taskId, id) {
      reads.push([taskId, id]);
      if (gone.includes(id)) throw Object.assign(new Error('the stored file is missing'), { status: 410 });
      const file = files.find((f) => f.id === id);
      const data = file.name.endsWith('.png') ? PNG : file.name.endsWith('.md') ? b64('# Plan\n\nShip **it**.\n') : b64('a log');
      return { name: file.name, content_type: file.content_type, data };
    },
  };
}

const settle = async (harness) => {
  for (let i = 0; i < 5; i += 1) await harness.act(() => new Promise((r) => setTimeout(r, 0)));
};

async function mountTaskFiles(t, { api = fakeApi(), taskId = 'tk_00000001', attachments = files } = {}) {
  const harness = await createPreactHarness(t);
  const { TaskFiles } = await harness.importDashboardModule('js/groups-taskboard-files.js');
  const intake = { add() {}, remove() {} };
  const task = { id: taskId, attachments };
  const mounted = await harness.mount(harness.html`<${TaskFiles} api=${api} task=${task} removed=${[]} onToggle=${() => {}}
    intake=${intake} pending=${[]} busy=${false} />`);
  await settle(harness);
  return { harness, c: mounted.container, api };
}

const tile = (c, id) => c.querySelector(`[data-file="${id}"] .tb-file-open`);

test('a task\'s image opens in the Messages image viewer, steps through the task\'s images, and closes back to its tile', async (t) => {
  const { harness, c, api } = await mountTaskFiles(t, { taskId: 'tk_view_img' });
  await harness.act(() => { tile(c, 1).focus(); tile(c, 1).click(); });
  await settle(harness);
  const dialog = c.querySelector('.image-preview-overlay [role="dialog"]');
  assert.ok(dialog, 'the image viewer opened');
  assert.equal(dialog.getAttribute('aria-modal'), 'true');
  assert.equal(dialog.querySelector('h2').textContent, 'chart.png');
  const image = dialog.querySelector('.image-preview-image');
  assert.ok(image, 'the image is drawn');
  assert.match(image.getAttribute('src'), /^blob:/, 'from the bytes the board\'s api read');
  assert.ok(api.reads.some(([task, id]) => task === 'tk_view_img' && id === 1));
  assert.match(dialog.querySelector('.image-preview-download').getAttribute('href'), /^blob:/, 'the download is the same bytes');
  // ‹ › step through the task's images only: the document and the text
  // file have no media view.
  assert.equal(dialog.querySelector('.attachment-preview-position').textContent, '1 of 2');
  await harness.act(() => dialog.querySelector('[aria-label="Next attachment"]').click());
  await settle(harness);
  assert.equal(c.querySelector('.image-preview-overlay h2').textContent, 'chart-2.png');

  await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }));
  assert.equal(c.querySelector('.image-preview-overlay'), null, 'Escape closes it');
  assert.equal(harness.document.activeElement, tile(c, 1), 'focus goes back to the tile');
});

test('a task\'s Markdown file opens in the document viewer, rendered, with its source a toggle away', async (t) => {
  const { harness, c } = await mountTaskFiles(t, { taskId: 'tk_view_md' });
  await harness.act(() => tile(c, 2).click());
  await settle(harness);
  const dialog = c.querySelector('.markdown-preview-overlay [role="dialog"]');
  assert.ok(dialog, 'the document viewer opened');
  assert.equal(dialog.querySelector('h2').textContent, 'plan.md');
  assert.equal(dialog.querySelector('.markdown-preview-document h1')?.textContent, 'Plan');
  assert.equal(dialog.querySelector('.markdown-preview-document strong')?.textContent, 'it');
  const source = [...dialog.querySelectorAll('.markdown-preview-mode')].find((b) => b.textContent === 'Source');
  await harness.act(() => source.click());
  assert.match(dialog.querySelector('.markdown-preview-source').textContent, /^# Plan/);
});

test('a file the viewer cannot show keeps its download; a removed file says so in the viewer', async (t) => {
  const downloads = [];
  const { harness, c } = await mountTaskFiles(t, { taskId: 'tk_view_misc', api: fakeApi({ gone: [4] }) });
  const anchorClick = harness.window.HTMLAnchorElement.prototype.click;
  harness.window.HTMLAnchorElement.prototype.click = function click() { downloads.push(this.getAttribute('download')); };
  t.after(() => { harness.window.HTMLAnchorElement.prototype.click = anchorClick; });

  await harness.act(() => tile(c, 3).click());
  await settle(harness);
  assert.equal(c.querySelector('.image-preview-overlay, .markdown-preview-overlay'), null, 'a text file opens no viewer');
  assert.deepEqual(downloads, ['log.txt'], 'it downloads, as before');

  await harness.act(() => tile(c, 4).click());
  await settle(harness);
  const state = c.querySelector('.image-preview-overlay .image-preview-state');
  assert.match(state?.textContent || '', /no longer available/i, 'a removed file shows the viewer\'s gone state');
  assert.equal(c.querySelector('.image-preview-download'), null, 'with nothing to download');
});

test('a comment\'s files and the List\'s 📎 count open the same viewer', async (t) => {
  const harness = await createPreactHarness(t);
  const { StoredFiles, FilesCount } = await harness.importDashboardModule('js/groups-taskboard-files.js');
  const api = fakeApi();
  const mounted = await harness.mount(harness.html`<div>
    <div class="comment"><${StoredFiles} api=${api} taskId="tk_view_comment" attachments=${[files[1]]} class="tb-comment-files" surface="comment-1" /></div>
    <table><tbody><tr><td class="tb-files-cell"><${FilesCount} api=${api} task=${{ id: 'tk_view_list', attachments: [files[2], files[0]] }} /></td></tr></tbody></table>
  </div>`);
  const c = mounted.container;
  await settle(harness);

  await harness.act(() => c.querySelector('.comment .tb-file-open').click());
  await settle(harness);
  assert.equal(c.querySelector('.markdown-preview-overlay h2')?.textContent, 'plan.md', 'a comment\'s document opens in the document viewer');
  await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }));

  const count = c.querySelector('.tb-files-count');
  assert.ok(count, 'a task with a viewable file has a 📎 button');
  assert.equal(count.textContent.trim(), '📎 2');
  await harness.act(() => count.click());
  await settle(harness);
  assert.equal(c.querySelector('.image-preview-overlay h2')?.textContent, 'chart.png', 'it opens on the first file with a viewer');
});

test('a 📎 count with no viewable file, or no api to read it, stays plain', async (t) => {
  const harness = await createPreactHarness(t);
  const { FilesCount } = await harness.importDashboardModule('js/groups-taskboard-files.js');
  const mounted = await harness.mount(harness.html`<div>
    <span class="a"><${FilesCount} api=${fakeApi()} task=${{ id: 'tk_plain_1', attachments: [files[2]] }} /></span>
    <span class="b"><${FilesCount} api=${null} task=${{ id: 'tk_plain_2', attachments: [files[0]] }} /></span>
  </div>`);
  const c = mounted.container;
  assert.equal(c.querySelector('.a button'), null);
  assert.equal(c.querySelector('.a .tb-files').textContent.trim(), '📎 1');
  assert.equal(c.querySelector('.b button'), null);
});
