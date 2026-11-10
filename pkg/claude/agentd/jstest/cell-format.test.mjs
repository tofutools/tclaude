// cell-format.js holds the formatters dashboard table cells share. Each case
// pins the text a table showed before it shared the formatter, plus the
// guards for times that are missing, unparseable or Go's zero time.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const ZERO = '0001-01-01T00:00:00Z';
const SOME = '2026-06-16T10:30:00Z';

test('errText reads an error message, or stringifies anything else', async (t) => {
  const harness = await createPreactHarness(t);
  const { errText } = await harness.importDashboardModule('js/cell-format.js');
  assert.equal(errText(new Error('boom')), 'boom');
  assert.equal(errText('plain'), 'plain');
  assert.equal(errText(undefined), 'undefined');
});

test('localStamp shows a real time and empties a missing, invalid or zero one', async (t) => {
  const harness = await createPreactHarness(t);
  const { localStamp, timeOf, isZeroTime } = await harness.importDashboardModule('js/cell-format.js');
  assert.equal(localStamp(SOME), new Date(SOME).toLocaleString());
  assert.equal(localStamp(SOME, { dateOnly: true }), new Date(SOME).toLocaleDateString());
  for (const value of [undefined, null, '', 'not a time', ZERO]) {
    assert.equal(localStamp(value), '—', `default empty for ${value}`);
    assert.equal(localStamp(value, { empty: '' }), '', `chosen empty for ${value}`);
    assert.equal(timeOf(value), null);
  }
  assert.equal(isZeroTime(ZERO), true);
  assert.equal(isZeroTime(SOME), false);
  assert.equal(isZeroTime(''), false);
  assert.equal(isZeroTime('nope'), false);
});

test('relTime leaves Go zero time empty instead of a huge age', async (t) => {
  const harness = await createPreactHarness(t);
  const { relTime } = await harness.importDashboardModule('js/helpers.js');
  assert.equal(relTime(ZERO), '');
  assert.equal(relTime(''), '');
  assert.match(relTime(new Date(Date.now() - 90_000).toISOString()), /^1m ago$/);
});

test('binarySize keeps the KiB/MiB text the fleet tables used', async (t) => {
  const harness = await createPreactHarness(t);
  const { binarySize } = await harness.importDashboardModule('js/cell-format.js');
  assert.equal(binarySize(0), '0 B');
  assert.equal(binarySize(1023), '1023 B');
  assert.equal(binarySize(1536), '1.5 KiB');
  assert.equal(binarySize(3 << 20), '3.0 MiB');
  assert.equal(binarySize(-1), '—');
  assert.equal(binarySize(undefined, ''), '');
});

test('groupScopeLabel names the group, or its id when the name is unknown', async (t) => {
  const harness = await createPreactHarness(t);
  const { groupScopeLabel } = await harness.importDashboardModule('js/cell-format.js');
  assert.equal(groupScopeLabel('webshop', 7), 'group:webshop');
  assert.equal(groupScopeLabel('', 7), 'group:#7');
});
