// Tests for the quick-start's pure pieces (tcl-7p6bgt): the step-list
// fixtures TestQuickStartStartersAreRunnable proves executable round-trip,
// and the Who picker / who-now guess read the roster the way the step list
// shows it.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import {
  fallbackChoices, fallbackFromValue, fallbackNote, fallbackValue, firstSpawn, groupRoles, groupRoster, profileChoices, setStepFallback,
  stepFallbackValue, whoFromValue, whoNow, whoOptions, whoValue,
} from '../dashboard/js/process-quickstart.js';
import { setStepWho, stepListToView, stepWho, viewToStepList } from '../dashboard/js/process-step-list.js';

const fixture = JSON.parse(readFileSync(new URL('./process-quickstart-starters.json', import.meta.url), 'utf8'));

test('the step-list fixtures round-trip; the fan-out is a graph the step list cannot show', () => {
  for (const [key, view] of Object.entries(fixture.starters)) {
    const converted = viewToStepList(view);
    assert.equal(converted.ok, key !== 'plan-fan-out-merge', key);
    if (converted.ok) assert.deepEqual(stepListToView(converted.list).edges, view.edges, key);
  }
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
  assert.deepEqual(options.map((o) => o.value), ['agent:checkout', 'agent:search', 'agent:review-bot', 'role:dev', 'role:reviewer', 'anyone', 'lead', 'spawn', 'you', 'command']);
  assert.equal(whoOptions(roster, { spawn: false }).some((o) => o.value === 'spawn'), false, 'a decider is not a new agent');
  assert.ok(!options.find((o) => o.value === 'anyone').disabled, 'anyone free is a real selector');
  assert.deepEqual(whoOptions(roster, { roles: [{ role: 'qa', count: 1 }] }).filter((o) => o.group === 'role').map((o) => o.value),
    ['role:qa', 'anyone', 'lead'], 'the process context\'s roles win over the roster\'s');
  assert.deepEqual(options.slice(0, 3).map((o) => o.detail), ['dev · idle', 'dev · working', 'reviewer · offline']);
  for (const value of ['agent:checkout', 'role:dev', 'anyone', 'lead', 'spawn', 'you', 'command']) assert.equal(whoValue(whoFromValue(value)), value);
});

test('who-now guesses the performer the way the resolver ranks them', () => {
  const roster = [
    { name: 'checkout', id: 'a1', role: 'dev', online: true, busy: true },
    { name: 'search', id: 'a2', role: 'dev', online: true, busy: false },
    { name: 'gone', id: 'a3', role: 'qa', online: false, busy: false },
  ];
  const step = (performer) => ({ node: { type: 'task', performer } });
  assert.deepEqual(whoNow(step({ kind: 'agent', lead: true, prompt: 'x' }), roster).label, "the group's lead", 'no member has the role lead: an owner');
  assert.equal(whoNow(step({ kind: 'agent', lead: true, prompt: 'x' }), [...roster, { name: 'lena', id: 'a4', role: 'Lead', online: true, busy: true }]).label, 'lena');
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
  assert.equal(whoNow(step({ kind: 'agent', role: 'qa', profile: 'qa-bot', spawn: 'allowed', prompt: 'x' }), roster, { fallback: { action: 'fail' } }).spawns,
    'qa-bot', 'a legacy spawn opt-in is the step\'s own choice, before the process fallback');
  assert.equal(whoNow(step({ kind: 'agent', role: 'qa', spawn: 'allowed', prompt: 'x' }), roster, { fallback: { action: 'fail' } }).fails, undefined,
    'without a profile it waits, as the runtime does');
  assert.equal(whoNow(step({ kind: 'agent', anyone: true, prompt: 'x' }), roster).label, 'search');
});

test('a fallback picker value round-trips, and a named profile is distinct from the group default', () => {
  for (const fallback of [{ action: 'wait' }, { action: 'fail' }, { action: 'spawn' }, { action: 'spawn', profile: 'opus-dev' }]) {
    assert.deepEqual(fallbackFromValue(fallbackValue(fallback)), fallback);
  }
  assert.equal(fallbackValue(null), '');
  assert.equal(fallbackFromValue(''), null);
});

test('a step override replaces the legacy spawn opt-in, which the validator refuses alongside it', () => {
  const legacy = { id: 'a', node: { type: 'task', performer: { kind: 'agent', profile: 'dev', spawn: 'allowed' } } };
  assert.equal(stepFallbackValue(legacy), 'spawn:dev');
  const set = setStepFallback(legacy, { action: 'fail' });
  assert.deepEqual(set.node.performer, { kind: 'agent', profile: 'dev', fallback: { action: 'fail' } });
  assert.equal(legacy.node.performer.spawn, 'allowed', 'the original step is not touched');
  const cleared = setStepFallback(set, null);
  assert.deepEqual(cleared.node.performer, { kind: 'agent', profile: 'dev' });
  assert.equal(stepFallbackValue(cleared), '');
});

test('fallback choices are the actions; a spawn\'s profiles list the default, the others, and keep one that is gone', () => {
  const values = (options) => options.map((option) => option.value);
  assert.deepEqual(values(fallbackChoices({ defaultSpawnProfile: 'dev', profiles: ['dev', 'opus'] })), ['wait', 'spawn', 'fail']);
  assert.deepEqual(values(fallbackChoices({ inherit: 'as the process', current: 'spawn:gone' })), ['', 'wait', 'spawn', 'fail']);
  const spawn = (options) => options.find((option) => option.value === 'spawn');
  assert.equal(spawn(fallbackChoices({ profiles: ['opus'] })).disabled, false, 'a named profile can spawn without a group default');
  assert.equal(spawn(fallbackChoices({})).disabled, true, 'no profile at all: nothing to spawn');
  assert.equal(spawn(fallbackChoices({ current: 'spawn' })).disabled, false, 'a spawn already chosen stays pickable');
  assert.deepEqual(profileChoices({ defaultSpawnProfile: 'dev', profiles: ['dev', 'opus', 'opus'] }).map((o) => [o.value, o.label]),
    [['', 'group default (dev)'], ['dev', 'dev'], ['opus', 'opus']]);
  assert.deepEqual(profileChoices({ current: 'gone', profiles: ['opus'] }).map((o) => [o.value, o.label]),
    [['', 'group default (none set)'], ['opus', 'opus'], ['gone', 'gone (not on this node)']]);
  assert.deepEqual(firstSpawn({ defaultSpawnProfile: 'dev', profiles: ['opus'] }), { action: 'spawn' });
  assert.deepEqual(firstSpawn({ profiles: ['opus'] }), { action: 'spawn', profile: 'opus' }, 'no default: a named one, or it would only wait');
});

test('a new agent is a step with no one to match and its own spawn, so every run spawns one', () => {
  const role = { id: 'r', node: { type: 'task', performer: { kind: 'agent', role: 'qa', sameAs: 'x', prompt: 'Review.' } } };
  const fresh = setStepWho(role, { mode: 'spawn', profile: 'opus-review' });
  assert.deepEqual(fresh.node.performer, { kind: 'agent', prompt: 'Review.', fallback: { action: 'spawn', profile: 'opus-review' } });
  assert.deepEqual(stepWho(fresh), { mode: 'spawn', profile: 'opus-review' });
  assert.deepEqual(setStepWho(fresh, { mode: 'spawn', profile: '' }).node.performer.fallback, { action: 'spawn' }, 'the group default');
  const legacy = { id: 'l', node: { type: 'task', performer: { kind: 'agent', profile: 'dev', spawn: 'allowed' } } };
  assert.deepEqual(setStepWho(legacy, { mode: 'spawn', profile: 'opus' }).node.performer, { kind: 'agent', fallback: { action: 'spawn', profile: 'opus' } });
  const roster = [{ name: 'idle', id: 'a1', role: 'qa', online: true, busy: false }];
  assert.equal(whoNow(fresh, roster).spawns, 'opus-review', 'an idle member does not take it');
  assert.equal(fallbackNote(fresh), '', 'the spawn is its who, not a fallback mark');
  const bare = setStepWho(role, { mode: 'spawn', profile: '' });
  assert.equal(whoNow(bare, roster, { defaultSpawnProfile: 'sonnet-dev' }).spawns, 'sonnet-dev');
  assert.equal(whoNow(bare, roster).waits, true, 'no default spawn profile: it waits');
  // With sameAs it goes to that step's performer once it ran: not a new agent.
  const same = { id: 's', node: { type: 'task', performer: { kind: 'agent', sameAs: 'build', fallback: { action: 'spawn' } } } };
  assert.equal(stepWho(same).mode, 'nobody');
  // Back to a role keeps the spawn as the step's own fallback.
  assert.deepEqual(setStepWho(fresh, { mode: 'role', role: 'qa' }).node.performer.fallback, { action: 'spawn', profile: 'opus-review' });
});

test('a node carries its fallback unless it simply waits', () => {
  const step = (performer) => ({ node: { performer } });
  assert.equal(fallbackNote(step({ kind: 'agent', role: 'dev' })), '');
  assert.equal(fallbackNote(step({ kind: 'agent', role: 'dev' }), { fallback: { action: 'spawn' }, defaultSpawnProfile: 'sonnet' }), '↯ spawn sonnet');
  assert.equal(fallbackNote(step({ kind: 'agent', role: 'dev', fallback: { action: 'fail' } }), { fallback: { action: 'spawn' } }), '↯ fail');
  assert.equal(fallbackNote(step({ kind: 'human' }), { fallback: { action: 'spawn' } }), '');
  assert.equal(fallbackNote(step({ kind: 'agent', role: 'dev' }), { fallback: { action: 'spawn' } }), '', 'with no default profile a bare spawn waits');
});
