// Tests for the quick-start's pure pieces (tcl-7p6bgt): the starters build
// the fixture TestQuickStartStartersAreRunnable proves executable, fill their
// slots from the group's roster, and the Who picker / who-now guess read the
// roster the way the step list shows it.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import {
  STARTERS, groupRoles, groupRoster, whoFromValue, whoNow, whoOptions, whoValue,
} from '../dashboard/js/process-quickstart.js';
import { stepListToView, stepWho, viewToStepList } from '../dashboard/js/process-step-list.js';

const fixture = JSON.parse(readFileSync(new URL('./process-quickstart-starters.json', import.meta.url), 'utf8'));

function built(starter, roster) {
  const out = starter.build(roster);
  return out.view || stepListToView(out.list);
}

test('starters build exactly the fixture the Go test proves runnable', () => {
  assert.deepEqual(Object.keys(fixture.starters), STARTERS.map((s) => s.key));
  for (const starter of STARTERS) assert.deepEqual(built(starter, fixture.roster), fixture.starters[starter.key], starter.key);
});

test('starters that are step lists round-trip; the fan-out opens in the graph editor', () => {
  for (const starter of STARTERS) {
    const view = fixture.starters[starter.key];
    const converted = viewToStepList(view);
    assert.equal(converted.ok, !starter.graph, starter.key);
    if (converted.ok) assert.deepEqual(stepListToView(converted.list).edges, view.edges, starter.key);
  }
});

test('starters fill roles from the roster, then members, then nobody', () => {
  const [buildReview] = STARTERS;
  const roles = buildReview.build(fixture.roster);
  assert.deepEqual(roles.uses, ['dev', 'reviewer']);
  assert.deepEqual(stepWho(roles.list.steps[0]), { mode: 'role', role: 'dev' });
  const plain = [{ name: 'ann', id: 'a', role: '', online: true }, { name: 'bob', id: 'b', role: '', online: true }];
  const members = buildReview.build(plain);
  assert.deepEqual(members.uses, ['ann', 'bob'], 'distinct members for implement and review');
  assert.deepEqual(stepWho(members.list.steps[1]), { mode: 'agent', agent: 'bob' });
  const empty = buildReview.build([]);
  assert.deepEqual(empty.uses, []);
  assert.deepEqual(stepWho(empty.list.steps[0]), { mode: 'nobody' });
});

test('the roster, its roles and the Who options', () => {
  const roster = groupRoster({ members: [
    { title: 'checkout', agent_id: 'agt_1', role: 'dev', online: true },
    { title: 'search', agent_id: 'agt_2', role: 'dev', online: true },
    { title: 'review-bot', agent_id: 'agt_3', role: 'reviewer', online: false },
    { title: '', conv_id: '' },
  ] }, new Set(['agt_2']));
  assert.deepEqual(roster.map((m) => [m.name, m.busy, m.online]), [['checkout', false, true], ['search', true, true], ['review-bot', false, false]]);
  assert.deepEqual(groupRoles(roster), [{ role: 'dev', count: 2 }, { role: 'reviewer', count: 1 }]);
  const options = whoOptions(roster);
  assert.deepEqual(options.map((o) => o.value), ['agent:checkout', 'agent:search', 'agent:review-bot', 'role:dev', 'role:reviewer', 'anyone', 'you', 'command']);
  assert.ok(!options.find((o) => o.value === 'anyone').disabled, 'anyone free is a real selector');
  assert.deepEqual(whoOptions(roster, { roles: [{ role: 'qa', count: 1 }] }).filter((o) => o.group === 'role').map((o) => o.value),
    ['role:qa', 'anyone'], 'the process context\'s roles win over the roster\'s');
  assert.deepEqual(options.slice(0, 3).map((o) => o.detail), ['dev · idle', 'dev · working', 'reviewer · offline']);
  for (const value of ['agent:checkout', 'role:dev', 'anyone', 'you', 'command']) assert.equal(whoValue(whoFromValue(value)), value);
});

test('who-now guesses the performer the way the resolver ranks them', () => {
  const roster = [
    { name: 'checkout', id: 'a1', role: 'dev', online: true, busy: true },
    { name: 'search', id: 'a2', role: 'dev', online: true, busy: false },
    { name: 'gone', id: 'a3', role: 'qa', online: false, busy: false },
  ];
  const step = (performer) => ({ node: { type: 'task', performer } });
  assert.equal(whoNow(step({ kind: 'agent', profile: 'dev', prompt: 'x' }), roster).label, 'search', 'an idle member first');
  assert.equal(whoNow(step({ kind: 'agent', profile: 'qa', prompt: 'x' }), roster).waits, true, 'an offline member cannot take it');
  assert.equal(whoNow(step({ kind: 'agent', profile: 'qa', spawn: 'allowed', prompt: 'x' }), roster).spawns, 'qa');
  assert.equal(whoNow(step({ kind: 'agent', assignee: 'checkout', prompt: 'x' }), roster).detail, 'working');
  assert.match(whoNow(step({ kind: 'agent', assignee: 'stranger', prompt: 'x' }), roster).detail, /not in this group/);
  assert.equal(whoNow(step({ kind: 'human', ask: 'x' }), roster).label, 'you');
  assert.equal(whoNow(step({ kind: 'program', run: 'make' }), roster).label, 'a command');
  assert.equal(whoNow(step({ kind: 'agent', prompt: 'x' }), roster).label, 'nobody yet');
  const qa = step({ kind: 'agent', role: 'qa', prompt: 'x' });
  assert.equal(whoNow(qa, roster, { fallback: { action: 'spawn' }, defaultSpawnProfile: 'sonnet-dev' }).spawns, 'sonnet-dev',
    'the process fallback spawns the group default');
  assert.equal(whoNow(qa, roster, { fallback: { action: 'spawn' } }).waits, true, 'with no profile to spawn, it waits');
  assert.equal(whoNow(qa, roster, { fallback: { action: 'fail' } }).fails, true);
  assert.equal(whoNow(step({ kind: 'agent', anyone: true, prompt: 'x' }), roster).label, 'search');
});
