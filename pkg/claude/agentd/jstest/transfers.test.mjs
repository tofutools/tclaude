import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// FakeXHR stands in for XMLHttpRequest: the test drives upload progress and
// the reply by hand.
function fakeXHR() {
  const sent = [];
  class FakeXHR {
    constructor() { this.upload = {}; sent.push(this); }
    open(method, url) { this.method = method; this.url = url; }
    send(body) { this.body = body; }
    abort() { this.onabort?.(); }
    getAllResponseHeaders() { return 'content-type: application/json\r\n'; }
    progress(loaded, total) { this.upload.onprogress?.({ lengthComputable: true, loaded, total }); }
    reply(status, body) { this.status = status; this.responseText = JSON.stringify(body); this.onload?.(); }
  }
  return { FakeXHR, sent };
}

function manualTimers() {
  const timers = [];
  return {
    setTimeoutImpl: (fn) => { timers.push(fn); return timers.length; },
    clearTimeoutImpl: (id) => { timers[id - 1] = null; },
    runAll: () => { for (const fn of timers.splice(0)) fn?.(); },
  };
}

test('per-file progress follows the body order of the files', async (t) => {
  const harness = await createPreactHarness(t);
  const { fileProgress } = await harness.importDashboardModule('js/transfers.js');
  const files = [{ size: 100 }, { size: 300 }];
  assert.deepEqual(fileProgress(files, 0), [0, 0]);
  assert.deepEqual(fileProgress(files, 50), [0.5, 0]);
  assert.deepEqual(fileProgress(files, 250), [1, 0.5]);
  assert.deepEqual(fileProgress(files, 400), [1, 1]);
});

test('a slow upload shows a per-file card that can be minimised and cancelled', async (t) => {
  const harness = await createPreactHarness(t);
  const { trackedUpload } = await harness.importDashboardModule('js/transfers.js');
  const { TransferCards } = await harness.importDashboardModule('js/transfer-card.js');
  const { FakeXHR, sent } = fakeXHR();
  const timers = manualTimers();
  const mounted = await harness.mount(harness.html`<${TransferCards} scope="/api/open-window-ws/agt_1" />`);
  const q = (s) => mounted.container.querySelector(s);
  const files = [{ name: 'notes.md', size: 100 }, { name: 'data.bin', size: 300 }];
  let upload;
  await harness.act(() => {
    upload = trackedUpload('/api/terminal-attachments', { body: 'form', files, scope: '/api/open-window-ws/agt_1', XHR: FakeXHR, ...timers });
  });
  assert.equal(q('.transfer-card'), null, 'a quick upload never flashes a card');
  await harness.act(() => { timers.runAll(); sent[0].progress(250, 400); });
  assert.match(q('.transfer-title').textContent, /Uploading 2 files/);
  const rows = [...mounted.container.querySelectorAll('.transfer-file')].map((row) => row.textContent);
  assert.match(rows[0], /notes\.md.*done/);
  assert.match(rows[1], /data\.bin.*50%/);
  await harness.act(() => q('.transfer-min').click());
  assert.match(q('.transfer-pill').textContent, /62%/);
  await harness.act(() => q('.transfer-pill').click());
  await harness.act(() => q('.transfer-cancel').click());
  await assert.rejects(upload, (error) => error.name === 'AbortError');
  assert.equal(q('.transfer-card'), null, 'a cancelled upload leaves');
});

test('a finished upload resolves to a Response, lingers as done, then leaves', async (t) => {
  const harness = await createPreactHarness(t);
  const { trackedUpload, subscribeTransfers } = await harness.importDashboardModule('js/transfers.js');
  const { FakeXHR, sent } = fakeXHR();
  const timers = manualTimers();
  let list = [];
  const stop = subscribeTransfers((items) => { list = items; });
  t.after(stop);
  const upload = trackedUpload('/api/spawn-attachments', { body: 'form', files: [{ name: 'a.png', size: 10 }], XHR: FakeXHR, ...timers });
  timers.runAll();
  sent[0].progress(10, 10);
  assert.equal(list[0].phase, 'finishing', 'the wait after the last byte has its own phase');
  sent[0].reply(200, { files: [{ path: '/tmp/a.png' }] });
  const response = await upload;
  assert.deepEqual(await response.json(), { files: [{ path: '/tmp/a.png' }] });
  assert.equal(list[0].phase, 'done');
  assert.equal(list[0].scope, 'dialog');
  timers.runAll();
  assert.equal(list.length, 0);
});
