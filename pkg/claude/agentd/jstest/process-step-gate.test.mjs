// Tests for agreement gates in the ☰ Steps list (tk_bd629fb5): every built-in
// way of working, the flagship's gate included, goes graph → steps → graph
// unchanged, and a gate's deciders, agree rule, outcomes and rounds edit in
// the list as the graph inspector edits them.
//
// process-builtin-views.json is the built-ins as the editor receives them
// (TestProcessBuiltinViewsFixture keeps it current). The saved YAML depends
// only on the nodes and edges compared here, not on their order
// (TestProcessBuiltinViewsSaveOrderFree), so an equal round trip saves the
// identical YAML.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import {
  deciderTexts, moveStep, setStepChoices, setStepLoop, setDeciderText, setStepText, stepListParams, stepListToView, stepText, viewToStepList,
} from '../dashboard/js/process-step-list.js';
import {
  addStepDecider, agreeChoices, canAddStepDecider, isGateStep, removeStepDecider, setStepAgree, setStepDecider,
} from '../dashboard/js/process-step-gate.js';

const { builtins } = JSON.parse(readFileSync(new URL('./process-builtin-views.json', import.meta.url), 'utf8'));

// normalized is what the saved YAML is made of: the template without the
// nodes' derived next, and the edges as a set.
function normalized(view) {
  const nodes = {};
  for (const id of Object.keys(view.template.nodes).sort()) {
    const { next, ...rest } = view.template.nodes[id];
    nodes[id] = rest;
  }
  const { nodes: _, start, ...top } = view.template;
  return {
    top, start, nodes,
    edges: view.edges.map((e) => `${e.from}|${e.outcome}|${e.to}`).sort(),
    layout: view.layout || { nodes: {} },
  };
}

const flagship = () => structuredClone(builtins['plan-vet-build']);
const listOf = (view) => {
  const converted = viewToStepList(view);
  assert.equal(converted.ok, true, converted.reason);
  return converted.list;
};
const stepOf = (list, id) => list.steps.find((step) => step.id === id);
const replace = (list, step) => ({ ...list, steps: list.steps.map((s) => (s.id === step.id ? step : s)) });

test('every built-in the list can show goes graph → steps → graph unchanged', () => {
  assert.ok(Object.keys(builtins).length >= 4);
  for (const [id, view] of Object.entries(builtins)) {
    const converted = viewToStepList(structuredClone(view));
    // A fan-out runs parts side by side, which only the graph shows.
    if (!converted.ok) {
      assert.match(converted.reason, /parallel node/, id);
      continue;
    }
    const list = converted.list;
    assert.deepEqual(normalized(stepListToView(list)), normalized(view), id);
  }
});

test('the flagship shows in steps with its review gate', () => {
  const list = listOf(flagship());
  assert.deepEqual(list.steps.map((step) => step.id), ['brief', 'plan', 'vet', 'build', 'review', 'merge']);
  const review = stepOf(list, 'review');
  assert.equal(isGateStep(review), true);
  assert.equal(isGateStep(stepOf(list, 'vet')), false);
  assert.equal(review.node.agree, 'approve');
  assert.equal(review.node.deciders.length, 2);
  assert.deepEqual(agreeChoices(review, ['approve', 'changes', 'escalate']), ['approve', 'escalate']);
  // Each reviewer is asked their own text; params read from either count.
  const texts = deciderTexts(review);
  assert.match(texts[0], /against the plan/);
  assert.match(texts[1], /correctness and tests/);
  assert.equal(stepText(review), texts[0]);
  assert.ok(stepListParams(list).includes('task'));
});

test('a gate edits its deciders, agree rule, outcomes and rounds, and saves back', () => {
  let list = listOf(flagship());
  let review = stepOf(list, 'review');
  assert.equal(canAddStepDecider(list, review), '');

  // A third decider: any qa member, asked what the first is.
  review = addStepDecider(list, review, { mode: 'role', role: 'qa' });
  assert.equal(review.node.deciders.length, 3);
  assert.equal(review.node.deciders[2].role, 'qa');
  assert.equal(review.node.deciders[2].prompt, review.node.deciders[0].prompt);
  // The second becomes a named agent, keeping its text; the constraint goes
  // with a role (an agent leaves no choice to narrow), as in the inspector.
  review = setStepDecider(list, review, 1, { mode: 'agent', agent: 'rita' });
  assert.equal(review.node.deciders[1].assignee, 'rita');
  assert.match(review.node.deciders[1].prompt, /correctness and tests/);
  // Its own text, and one text for all.
  review = setDeciderText(review, 2, 'QA: does it work? {{task}}');
  assert.equal(review.node.deciders[2].prompt, 'QA: does it work? {{params.task}}');
  // Rounds of changes going back, and the agree rule.
  review = setStepLoop(review, 'changes', { maxIterations: 5, onExhausted: 'escalate' });
  review = setStepAgree(list, review, 'escalate');
  assert.equal(review.node.agree, 'escalate');
  review = setStepAgree(list, review, 'approve');

  list = replace(list, review);
  const view = stepListToView(list);
  const node = view.template.nodes.review;
  assert.equal(node.deciders.length, 3);
  assert.equal(node.agree, 'approve');
  assert.equal(node.loops.changes.maxIterations, 5);
  assert.equal(node.performer, undefined);
  assert.deepEqual(view.edges.filter((e) => e.from === 'review').map((e) => `${e.outcome}>${e.to}`).sort(),
    ['approve>merge', 'changes>build', 'escalate>escalated']);

  // One text for all deciders.
  const same = setStepText(stepOf(listOf(view), 'review'), 'Review it.');
  assert.deepEqual(deciderTexts(same), ['Review it.', 'Review it.', 'Review it.']);
});

test('a gate keeps its deciders through outcome edits, and needs two outcomes', () => {
  const list = listOf(flagship());
  const review = stepOf(list, 'review');
  // Renaming the agreed outcome away picks another forward one to agree on.
  const renamed = setStepChoices(review, ['lgtm', 'changes', 'escalate'], 'merge');
  assert.equal(renamed.node.deciders.length, 2);
  assert.equal(renamed.node.performer, undefined);
  assert.equal(renamed.node.agree, 'lgtm');
  // Dropping escalate: the rounds of changes then end in another outcome,
  // since nobody is there to ask again.
  const fewer = setStepChoices(review, ['approve', 'changes'], 'merge');
  assert.equal(fewer.node.loops.changes.onExhausted, 'approve');
  assert.deepEqual(Object.keys(builtins).filter((id) => !viewToStepList(structuredClone(builtins[id])).ok), ['plan-fan-out']);
  // A gate never drops to one outcome.
  assert.equal(setStepChoices(review, ['approve'], 'merge'), review);
});

test('removing deciders down to one makes a plain decision; adding one makes a gate', () => {
  const list = listOf(flagship());
  const single = removeStepDecider(list, stepOf(list, 'review'), 0);
  assert.equal(isGateStep(single), false);
  assert.equal(single.node.deciders, undefined);
  assert.equal(single.node.agree, undefined);
  assert.match(single.node.performer.prompt, /correctness and tests/);
  const vet = stepOf(list, 'vet');
  const gate = addStepDecider(list, vet, { mode: 'you' });
  assert.equal(isGateStep(gate), true);
  assert.equal(gate.node.agree, 'ok');
  assert.equal(gate.node.deciders[1].kind, 'human');
  // Its rounds of rework need an outcome to end in when they run out.
  assert.equal(gate.node.loops.rework.onExhausted, 'escalate');
  // A command decides alone.
  const command = { ...vet, node: { ...vet.node, performer: { kind: 'program', run: 'make' } } };
  assert.match(canAddStepDecider(list, command), /command decides alone/);
});

// gateProblems mirrors the server's gate rules (model/gates.go): agree is a
// routed outcome that does not loop, there is another outcome, and every
// loop has a way on once its rounds run out.
function gateProblems(view, id) {
  const node = view.template.nodes[id];
  const outcomes = view.edges.filter((e) => e.from === id).map((e) => e.outcome);
  const out = [];
  if (!outcomes.includes(node.agree)) out.push('agree_outcome');
  if (outcomes.length < 2) out.push('gate_outcomes');
  if (node.loops?.[node.agree]) out.push('agree_loop');
  for (const [outcome, bound] of Object.entries(node.loops || {})) {
    if (!String(bound.onExhausted || '').trim()) out.push(`gate_loop_exhausted:${outcome}`);
    else if (!outcomes.includes(bound.onExhausted) || node.loops[bound.onExhausted]) out.push(`exhausted_target:${outcome}`);
  }
  return out;
}

test('moving a step keeps a gate valid: what all must choose never goes back', () => {
  // Merge above review: approve now goes back to it.
  const list = moveStep(listOf(flagship()), 'merge', -1);
  const view = stepListToView(list);
  const review = view.template.nodes.review;
  assert.ok(review.loops.approve, 'approve now goes back, so it loops');
  assert.notEqual(review.agree, 'approve');
  assert.deepEqual(gateProblems(view, 'review'), []);
});

test('retargeting an outcome back keeps a gate valid', () => {
  const list = listOf(flagship());
  const review = stepOf(list, 'review');
  // As the outcomes editor does: escalate → back to plan, a bounded loop
  // whose way on is filled in (nobody is there to ask again).
  const links = review.links.map((link) => (link.outcome === 'escalate' ? { ...link, to: 'plan' } : link));
  const back = setStepLoop({ ...review, links }, 'escalate', { maxIterations: 3, onExhausted: '' });
  assert.equal(back.node.loops.escalate.onExhausted, 'approve', 'the only outcome that goes on');
  assert.deepEqual(gateProblems(stepListToView(replace(list, back)), 'review'), []);
  // approve back instead: what all must choose moves to the outcome that
  // still goes on, and changes' rounds end there.
  const links2 = review.links.map((link) => (link.outcome === 'approve' ? { ...link, to: 'build' } : link));
  const approveBack = setStepLoop({ ...review, links: links2 }, 'approve', { maxIterations: 3, onExhausted: '' });
  assert.equal(approveBack.node.agree, 'escalate');
  assert.deepEqual(gateProblems(stepListToView(replace(list, approveBack)), 'review'), []);
});

test('a gate of a person, an agent and a legacy role goes graph → steps → graph unchanged', () => {
  const view = {
    template: {
      id: 'mixed', name: 'mixed', start: 'decide',
      nodes: {
        decide: {
          type: 'decision', agree: 'yes',
          deciders: [
            { kind: 'human', ask: 'Ship {{params.what}}?' },
            { kind: 'agent', role: 'qa', prompt: 'Does it work?', fallback: { action: 'spawn' } },
            { kind: 'agent', profile: 'sec', spawn: 'allowed', prompt: 'Is it safe?' },
          ],
          loops: { no: { maxIterations: 2, onExhausted: 'stop' } },
        },
        work: { type: 'task', performer: { kind: 'agent', role: 'dev', prompt: 'Do {{params.what}}' } },
        done: { type: 'end', result: 'success' },
        stopped: { type: 'end', result: 'failed' },
      },
      params: { what: { type: 'string', required: true } },
    },
    edges: [
      { from: '', outcome: 'start', to: 'work' },
      { from: 'work', outcome: 'pass', to: 'decide' },
      { from: 'decide', outcome: 'yes', to: 'done' },
      { from: 'decide', outcome: 'no', to: 'work' },
      { from: 'decide', outcome: 'stop', to: 'stopped' },
    ],
    layout: { nodes: {} },
  };
  view.template.start = 'work';
  const list = listOf(structuredClone(view));
  assert.deepEqual(normalized(stepListToView(list)), normalized(view));
  assert.deepEqual(deciderTexts(stepOf(list, 'decide')), ['Ship {{params.what}}?', 'Does it work?', 'Is it safe?']);
  const asked = setDeciderText(stepOf(list, 'decide'), 0, 'Ship it?');
  assert.equal(asked.node.deciders[0].ask, 'Ship it?');
  assert.equal(asked.node.deciders[0].prompt, undefined);
});
