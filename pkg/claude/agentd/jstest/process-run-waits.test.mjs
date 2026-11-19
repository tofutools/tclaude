// Unit tests for process-run-wait-words.js: a waiting wait step in words for
// the run views, run graphs and the phone's run cards.

import test from 'node:test';
import assert from 'node:assert/strict';
import { atWords, leftWords, waitLine, waitShort, waitingCommands } from '../dashboard/js/process-run-wait-words.js';

const now = Date.parse('2026-11-19T10:00:00Z');
const at = (minutes) => new Date(now + minutes * 60000).toISOString();

test('leftWords keeps two units', () => {
  assert.equal(leftWords(3 * 86400000 + 5 * 3600000), '3d 5h');
  assert.equal(leftWords(2 * 3600000 + 13 * 60000), '2h 13m');
  assert.equal(leftWords(4 * 60000 + 5000), '4m 05s');
  assert.equal(leftWords(12000), '12s');
  assert.equal(leftWords(-1), '0s');
});

test('a wait for a time says how long it has left, then finishing', () => {
  const command = { kind: 'wait', nodeId: 'cool', wait: { dueAt: at(133) } };
  assert.match(waitLine(command, now).text, /^2h 13m left · until /);
  assert.equal(waitShort(command, now), '2h 13m left');
  assert.equal(waitLine({ ...command, wait: { dueAt: at(-1) } }, now).text, 'finishing…');
  assert.equal(waitLine({ kind: 'wait', wait: {} }, now).text, 'starting');
  assert.equal(waitLine({ kind: 'wait', wait: { failure: 'params.at is not a time' } }, now).failing, true);
});

test('a wait for a condition says its check, the last answer and its limit', () => {
  const waiting = { kind: 'wait', nodeId: 'ready', wait: { dueAt: at(4), check: { checks: 2, last: 'no' }, maxChecks: 20, givesUpAt: at(60 * 24), timesOut: true } };
  const line = waitLine(waiting, now);
  assert.equal(line.check, true);
  assert.match(line.text, /^check 3 of 20 in 4m 00s · last: not yet · times out /);
  assert.equal(waitShort(waiting, now), 'check 3/20, next in 4m 00s');
  const checking = { kind: 'program', nodeId: 'ready', wait: { checking: true, check: { checks: 3, failed: 1, last: 'failed: exit status 2' }, maxChecks: 20, maxFailedChecks: 3 } };
  assert.match(waitLine(checking, now).text, /^checking now \(check 4 of 20\) · last: failed: exit status 2 · 1 failed in a row \(fails at 3\)$/);
  assert.equal(waitShort(checking, now), 'checking (4/20)');
  assert.deepEqual(waitingCommands({ commands: [waiting, checking, { kind: 'agent', nodeId: 'x' }] }).map((c) => c.nodeId), ['ready', 'ready']);
});

test('atWords is the time today, the weekday this week, else the date', () => {
  const today = new Date(now);
  assert.doesNotMatch(atWords(at(60), today), /\s\S+\s/);
  assert.ok(atWords(at(60 * 24 * 3), today).split(' ').length >= 2);
  assert.equal(atWords('nonsense', today), '');
});
