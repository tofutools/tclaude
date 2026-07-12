import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('Logs model formats records, builds API params, and keys duplicate rows stably', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/logs-model.js');
  assert.equal(model.levelKey('WARNING'), 'raw');
  assert.equal(model.levelKey('WARN'), 'warn');
  assert.equal(model.fieldsText({ nested: { ok: true }, count: 2 }), 'nested={"ok":true}  count=2');
  assert.equal(model.fmtBytes(2048), '2 KB');
  assert.equal(model.pageCount(101, 100), 2);
  const row = { time: 'x', level: 'INFO', msg: 'same' };
  const first = model.keyedLogRows([row]);
  const prepended = model.keyedLogRows([row, row]);
  assert.equal(prepended[1].key, first[0].key, 'existing identical row keeps its reverse-occurrence key');
  assert.notEqual(prepended[0].key, prepended[1].key, 'duplicate records remain unique');
  const params = model.logsParams({ page: 2, pageSize: 50, query: ' boom ', level: 'warn', rangeMs: 1000, includeRotated: true, hideRaw: true }, 5000);
  assert.equal(params.toString(), 'page=2&page_size=50&q=boom&level=warn&from=4000&include_rotated=1&hide_raw=1');
});
