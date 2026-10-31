// What still works on a dashboard opened over plain http away from localhost.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('randomUUID is built from getRandomValues where crypto.randomUUID is missing', async (t) => {
  const harness = await createPreactHarness(t);
  const { randomUUID } = await harness.importDashboardModule('js/insecure-origin.js');
  assert.equal(randomUUID({ randomUUID: () => 'native' }), 'native');
  assert.match(randomUUID({ getRandomValues: (bytes) => bytes.fill(255) }), /^ffffffff-ffff-4fff-bfff-ffffffffffff$/);
});

test('writeClipboard says whether the text got there', async (t) => {
  const harness = await createPreactHarness(t);
  const { writeClipboard } = await harness.importDashboardModule('js/insecure-origin.js');
  const doc = harness.document;
  doc.defaultView.HTMLElement.prototype.select ||= () => {};
  doc.execCommand = () => true;
  t.after(() => { delete doc.execCommand; });
  assert.equal(await writeClipboard('hello'), true, 'the legacy copy stands in for navigator.clipboard');
  doc.execCommand = () => false;
  assert.equal(await writeClipboard('hello'), false);
});
