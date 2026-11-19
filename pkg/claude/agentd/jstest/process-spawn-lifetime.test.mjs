import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import * as m from '../dashboard/js/process-mini-edit.js';
import { setStepWho } from '../dashboard/js/process-step-list.js';
import { setStepFallback, setStepLifetime } from '../dashboard/js/process-quickstart.js';
import { addReviewer, setDeciderFallback, setDeciderLifetime } from '../dashboard/js/process-gate-deciders.js';
import { setFallbackField } from '../dashboard/js/process-node-form.js';
import {
  DEFAULT_DECIDER_LIFETIME, DEFAULT_LIFETIME, effectiveLifetime, keepLifetime, lifetimeChoices, withLifetime,
} from '../dashboard/js/process-spawn-lifetime.js';

const steps = JSON.parse(readFileSync(new URL('./process-quickstart-starters.json', import.meta.url), 'utf8')).starters;
const ship = steps['build-review-ship'];
const step = (view, id) => ({ id, node: view.template.nodes[id] });

test('the defaults match the engine: run for a step, step for a gate decider', () => {
  assert.equal(DEFAULT_LIFETIME, 'run');
  assert.equal(DEFAULT_DECIDER_LIFETIME, 'step');
  assert.equal(effectiveLifetime({ action: 'spawn' }), 'run');
  assert.equal(effectiveLifetime({ action: 'spawn' }, { decider: true }), 'step');
  assert.match(lifetimeChoices().find((c) => c.value === 'run').label, /\(default\)$/);
  assert.match(lifetimeChoices({ decider: true }).find((c) => c.value === 'step').label, /\(default\)$/);
});

test('a lifetime is stored only when it is not the default, and survives a new profile or fallback', () => {
  assert.deepEqual(withLifetime({ action: 'spawn', profile: 'p' }, 'run'), { action: 'spawn', profile: 'p' });
  assert.deepEqual(withLifetime({ action: 'spawn' }, 'step'), { action: 'spawn', lifetime: 'step' });
  assert.deepEqual(withLifetime({ action: 'spawn', lifetime: 'step' }, 'step', { decider: true }), { action: 'spawn' });
  assert.deepEqual(withLifetime({ action: 'wait' }, 'step'), { action: 'wait' });
  assert.deepEqual(keepLifetime({ action: 'spawn', lifetime: 'keep' }, { action: 'spawn', profile: 'q' }), { action: 'spawn', profile: 'q', lifetime: 'keep' });
  assert.deepEqual(keepLifetime({ action: 'spawn', lifetime: 'keep' }, { action: 'wait' }), { action: 'wait' });

  // A new agent step: a new profile keeps its lifetime.
  const spawned = setStepWho(step(ship, 'implement'), { mode: 'spawn', profile: 'a' });
  const kept = setStepFallback(spawned, withLifetime(spawned.node.performer.fallback, 'step'));
  assert.equal(kept.node.performer.fallback.lifetime, 'step');
  const reprofiled = setStepWho(kept, { mode: 'spawn', profile: 'b' });
  assert.deepEqual(reprofiled.node.performer.fallback, { action: 'spawn', profile: 'b', lifetime: 'step' });
});

test('a gate decider can be a new agent with a profile, and keeps its lifetime', () => {
  const tracked = m.addReviewerTrack(ship, 'implement');
  const gate = addReviewer(tracked.view, tracked.id).view;
  const fresh = m.setDecider(gate, tracked.id, 1, { mode: 'spawn', profile: 'opus-review' });
  const decider = fresh.view.template.nodes[tracked.id].deciders[1];
  assert.deepEqual(decider.fallback, { action: 'spawn', profile: 'opus-review' });
  assert.deepEqual(m.deciderWho(decider), { mode: 'spawn', profile: 'opus-review' });
  const lived = setDeciderFallback(fresh.view, tracked.id, 1, withLifetime(decider.fallback, 'run', { decider: true }));
  assert.equal(lived.view.template.nodes[tracked.id].deciders[1].fallback.lifetime, 'run');
  const reprofiled = m.setDecider(lived.view, tracked.id, 1, { mode: 'spawn', profile: 'sonnet' });
  assert.deepEqual(reprofiled.view.template.nodes[tracked.id].deciders[1].fallback, { action: 'spawn', profile: 'sonnet', lifetime: 'run' });
});

test('the full editor sets a spawn lifetime, and drops it with the spawn', () => {
  const holder = {};
  setFallbackField(holder, 'action', 'spawn');
  setFallbackField(holder, 'lifetime', 'keep');
  assert.deepEqual(holder.fallback, { action: 'spawn', lifetime: 'keep' });
  assert.throws(() => setFallbackField(holder, 'lifetime', 'forever'), /invalid lifetime/);
  setFallbackField(holder, 'lifetime', '');
  assert.deepEqual(holder.fallback, { action: 'spawn' });
  setFallbackField(holder, 'lifetime', 'step');
  setFallbackField(holder, 'action', 'wait');
  assert.deepEqual(holder.fallback, { action: 'wait' });
});

test('picking the default lifetime again clears the one set before', () => {
  const spawned = setStepWho(step(ship, 'implement'), { mode: 'spawn' });
  const set = setStepLifetime(spawned, 'step');
  assert.equal(set.node.performer.fallback.lifetime, 'step');
  assert.deepEqual(setStepLifetime(set, 'run').node.performer.fallback, { action: 'spawn' }, 'back to the default');

  const tracked = m.addReviewerTrack(ship, 'implement');
  const gate = m.setDecider(addReviewer(tracked.view, tracked.id).view, tracked.id, 1, { mode: 'spawn' }).view;
  const kept = setDeciderLifetime(gate, tracked.id, 1, 'keep').view;
  assert.equal(kept.template.nodes[tracked.id].deciders[1].fallback.lifetime, 'keep');
  assert.deepEqual(setDeciderLifetime(kept, tracked.id, 1, 'step').view.template.nodes[tracked.id].deciders[1].fallback, { action: 'spawn' });
});

test('a decider made a new agent drops what named another agent', () => {
  const tracked = m.addReviewerTrack(ship, 'implement');
  const gate = addReviewer(tracked.view, tracked.id).view;
  const named = setDeciderFallback(gate, tracked.id, 1, null).view;
  named.template.nodes[tracked.id].deciders[1] = { ...named.template.nodes[tracked.id].deciders[1], sameAs: 'implement', profile: 'legacy-role' };
  const fresh = m.setDecider(named, tracked.id, 1, { mode: 'spawn', profile: 'p' }).view.template.nodes[tracked.id].deciders[1];
  assert.equal(fresh.sameAs, undefined);
  assert.equal(fresh.differentFrom, undefined);
  assert.equal(fresh.profile, undefined);
  assert.deepEqual(m.deciderWho(fresh), { mode: 'spawn', profile: 'p' });
});

test('a new agent keeps what its lifetime meant when a decision becomes a gate and back', () => {
  const decide = JSON.parse(JSON.stringify(ship));
  decide.template.nodes.approve.performer = { kind: 'agent', prompt: 'Decide.', fallback: { action: 'spawn' } };
  const gate = m.addDecider(decide, 'approve', { mode: 'anyone' }).view;
  assert.equal(gate.template.nodes.approve.deciders[0].fallback.lifetime, 'run', 'it meant run as the one performer');
  const vote = setDeciderFallback(gate, 'approve', 0, { action: 'spawn' }).view;
  assert.equal(vote.template.nodes.approve.deciders[0].fallback.lifetime, 'run', 'keepLifetime carries it');
  const plain = m.removeDecider(setDeciderLifetime(gate, 'approve', 0, 'step').view, 'approve', 1).view;
  assert.equal(plain.template.nodes.approve.performer.fallback.lifetime, 'step', 'it meant step as a vote');
});
