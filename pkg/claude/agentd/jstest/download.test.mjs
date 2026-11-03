// download.js is the dashboard's one download path: a short-lived
// <a download> clicked once, with in-memory data behind an object URL that
// is revoked afterwards.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function captureClicks(t, document) {
  const clicked = [];
  const onClick = (event) => {
    clicked.push({ href: event.target.getAttribute('href'), download: event.target.getAttribute('download') });
    event.preventDefault();
  };
  document.addEventListener('click', onClick);
  t.after(() => document.removeEventListener('click', onClick));
  return clicked;
}

test('downloadURL clicks a transient anchor that the endpoint names', async (t) => {
  const harness = await createPreactHarness(t);
  const { downloadURL } = await harness.importDashboardModule('js/download.js');
  const clicked = captureClicks(t, harness.document);
  downloadURL('/api/export-jobs/7/artifact', { documentRef: harness.document });
  downloadURL('/api/templates/x/export', { filename: 'x.task-force.json', documentRef: harness.document });
  assert.deepEqual(clicked, [
    { href: '/api/export-jobs/7/artifact', download: '' },
    { href: '/api/templates/x/export', download: 'x.task-force.json' },
  ]);
  assert.equal(harness.document.querySelector('a'), null, 'no anchor outlives its click');
});

test('downloadJSON saves pretty JSON through an object URL it revokes', async (t) => {
  const harness = await createPreactHarness(t);
  const { downloadJSON } = await harness.importDashboardModule('js/download.js');
  const clicked = captureClicks(t, harness.document);
  const made = [];
  const revoked = [];
  const windowRef = {
    Blob,
    URL: { createObjectURL: (blob) => { made.push(blob); return 'blob:one'; }, revokeObjectURL: (url) => revoked.push(url) },
  };
  t.mock.timers.enable({ apis: ['setTimeout'] });
  downloadJSON('bundle.json', { a: 1 }, { documentRef: harness.document, windowRef });
  assert.deepEqual(clicked, [{ href: 'blob:one', download: 'bundle.json' }]);
  assert.equal(await made[0].text(), '{\n  "a": 1\n}\n');
  assert.equal(made[0].type, 'application/json');
  assert.deepEqual(revoked, [], 'the URL outlives the click');
  t.mock.timers.tick(1000);
  assert.deepEqual(revoked, ['blob:one']);
});
