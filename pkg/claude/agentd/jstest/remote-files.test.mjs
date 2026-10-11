import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const TERM = '/api/federation/terminal?peer=inst_forge7&agent=agt_ada1&mode=watch';

function peerFiles(log, { listError = null } = {}) {
  const tree = {
    '.': { entries: [{ path: 'src', kind: 'directory', size: 0 }, { path: 'README.md', kind: 'file', size: 30 }, { path: 'logo.png', kind: 'file', size: 2048 }, { path: 'big.log', kind: 'file', size: 10 << 20 }], truncated: false },
    src: { entries: [{ path: 'src/main.go', kind: 'file', size: 40 }], truncated: true },
  };
  const body = { 'README.md': new TextEncoder().encode('# <b>hello</b>\n'), 'logo.png': new Uint8Array([137, 80, 78, 71, 0, 1, 2]), 'src/main.go': new TextEncoder().encode('package main\n') };
  return async (url, init = {}) => {
    const u = new URL(url, 'https://dash.test');
    const path = u.searchParams.get('path');
    log.push([init.method || 'GET', u.pathname, Object.fromEntries(u.searchParams)]);
    const json = (status, v) => ({ ok: status < 300, status, json: async () => v, arrayBuffer: async () => new ArrayBuffer(0) });
    if (u.searchParams.get('list') === 'true') {
      if (listError) return json(listError.status, { code: listError.code, error: 'refused' });
      return tree[path] ? json(200, tree[path]) : json(404, { code: 'not_found' });
    }
    if (path === 'secret.env') return json(403, { code: 'unsafe_path' });
    if (!body[path]) return json(404, { code: 'not_found' });
    return { ok: true, status: 200, json: async () => { throw new Error('not json'); }, arrayBuffer: async () => body[path].buffer.slice(0) };
  };
}

test('the helpers list one level, preview text, spot binaries and word refusals', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/remote-files.js');
  const log = [];
  const fetchImpl = peerFiles(log);
  const listing = await m.listRemoteDir({ terminal: TERM, viewer: 'env_7k', path: '.', fetchImpl });
  assert.deepEqual(listing.entries.map((e) => e.path), ['src', 'README.md', 'logo.png', 'big.log']);
  assert.deepEqual(log[0], ['GET', '/api/federation/terminal-file', { terminal: TERM, viewer: 'env_7k', path: '.', list: 'true' }]);
  assert.deepEqual(await m.previewRemoteFile({ terminal: TERM, viewer: 'env_7k', path: 'README.md', fetchImpl }), { text: '# <b>hello</b>\n' });
  assert.deepEqual(await m.previewRemoteFile({ terminal: TERM, viewer: 'env_7k', path: 'logo.png', fetchImpl }), { binary: true });
  let cancelled = false; let pulls = 0;
  const growing = async () => ({ ok: true, status: 200, body: { getReader: () => ({
    read: async () => { pulls += 1; return pulls > 100 ? { done: true } : { done: false, value: new Uint8Array(64 << 10) }; },
    cancel: async () => { cancelled = true; },
  }) } });
  assert.deepEqual(await m.previewRemoteFile({ terminal: TERM, viewer: 'env_7k', path: 'grew.log', fetchImpl: growing }), { tooLarge: true });
  assert.ok(cancelled && pulls === 5, 'the transfer stops just past the preview cap');
  await assert.rejects(m.previewRemoteFile({ terminal: TERM, viewer: 'env_7k', path: 'secret.env', fetchImpl }), { message: m.REMOTE_FILE_ERRORS.unsafe_path });
  await assert.rejects(m.listRemoteDir({ terminal: TERM, viewer: 'env_7k', path: '.', fetchImpl: peerFiles([], { listError: { status: 403, code: 'root_too_broad' } }) }), { message: m.REMOTE_FILE_ERRORS.root_too_broad });
  await assert.rejects(m.listRemoteDir({ terminal: TERM, viewer: '', fetchImpl }), { message: m.REMOTE_FILE_ERRORS.not_shared });
  const clicked = [];
  const orig = harness.document.defaultView.HTMLAnchorElement.prototype.click;
  harness.document.defaultView.HTMLAnchorElement.prototype.click = function () { clicked.push(this.getAttribute('href')); };
  t.after(() => { harness.document.defaultView.HTMLAnchorElement.prototype.click = orig; });
  await m.downloadRemoteFile({ terminal: TERM, viewer: 'env_7k', path: 'README.md', fetchImpl, documentRef: harness.document });
  assert.equal(clicked.length, 1);
  assert.match(clicked[0], /^\/api\/federation\/terminal-file\?.*path=README\.md/);
  assert.equal(harness.document.querySelector('a[download]'), null, 'the anchor is removed in the same operation');
  assert.deepEqual(log.filter((l) => l[2].path === 'README.md').map((l) => l[0]), ['GET', 'HEAD'], 'a HEAD preflight before the browser download');
});

test('the panel opens folders, previews small text as text, and shows refusals plainly', async (t) => {
  const harness = await createPreactHarness(t);
  const { RemoteFilesPanel } = await harness.importDashboardModule('js/remote-files-panel.js');
  const log = []; let closed = 0;
  const mounted = await harness.mount(harness.html`<${RemoteFilesPanel} terminal=${TERM} viewer="env_7k" peer="forge" fetchImpl=${peerFiles(log)} documentRef=${harness.document} onClose=${() => { closed += 1; }} />`);
  const q = (s) => mounted.container.querySelector(s);
  const settle = () => harness.act(() => new Promise((r) => setTimeout(r, 10)));
  const click = async (el) => { await harness.act(() => el.click()); await settle(); };
  await settle();
  assert.match(q('#term-files').textContent, /Files on forge/);
  const row = (p) => q(`[data-path="${p}"]`);
  assert.equal(row('big.log').querySelector('[data-files="preview"]'), null, 'large files download only');
  assert.ok(row('big.log').querySelector('[data-files="download"]'));
  await click(row('README.md').querySelector('[data-files="preview"]'));
  assert.equal(q('.term-files-text').textContent, '# <b>hello</b>\n', 'shown as text, never HTML');
  assert.equal(q('.term-files-text b'), null);
  await click(q('#term-files-preview-back'));
  await click(row('logo.png').querySelector('[data-files="preview"]'));
  assert.match(q('#term-files-preview').textContent, /binary file; download it/);
  await click(q('#term-files-preview-back'));
  await click(row('src').querySelector('[data-files="open"]'));
  assert.equal(q('#term-files-dir').textContent, 'src');
  assert.ok(row('src/main.go'));
  assert.match(q('#term-files-truncated').textContent, /first 100 entries/);
  await click(q('#term-files-up'));
  assert.equal(q('#term-files-dir').textContent, '.');
  await click(q('#term-files-close'));
  assert.equal(closed, 1);
});

test('a refused listing says why', async (t) => {
  const harness = await createPreactHarness(t);
  const { RemoteFilesPanel } = await harness.importDashboardModule('js/remote-files-panel.js');
  const { REMOTE_FILE_ERRORS } = await harness.importDashboardModule('js/remote-files.js');
  const mounted = await harness.mount(harness.html`<${RemoteFilesPanel} terminal=${TERM} viewer="env_7k" fetchImpl=${peerFiles([], { listError: { status: 403, code: 'viewer_closed' } })} onClose=${() => {}} />`);
  await harness.act(() => new Promise((r) => setTimeout(r, 10)));
  assert.equal(mounted.container.querySelector('#term-files-error').textContent, REMOTE_FILE_ERRORS.viewer_closed);
});
