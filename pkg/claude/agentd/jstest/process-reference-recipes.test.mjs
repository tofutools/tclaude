// The operator's two reference processes must stay easy to build in the
// inline editor (tk_c324d5a4): each recipe (process-reference-recipes.mjs)
// builds its process with the card's own operations, within a budget of
// edits, into exactly the fixture the Go tests validate and run
// (TestReferenceProcessesAreRunnable, TestReferenceProcessFlows).
//
// Regenerate the fixture after a deliberate change:
//   UPDATE_REFERENCE_RECIPES=1 node --test pkg/claude/agentd/jstest/process-reference-recipes.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, writeFileSync } from 'node:fs';
import * as m from '../dashboard/js/process-mini-edit.js';
import { addReviewer } from '../dashboard/js/process-gate-deciders.js';
import { advanced, simple } from './process-reference-recipes.mjs';
import { canonical, fullAdvanced, fullSimple } from './process-reference-full.mjs';

const FIXTURE = new URL('./process-reference-recipes.json', import.meta.url);

// BUDGET is how many inline edits each process takes today. A change that
// makes building one harder fails here; one that makes it easier lowers
// the number (and should lower the budget with it).
const BUDGET = { simple: 36, advanced: 68 };
// FULL_BUDGET is the same for the full editor (process-reference-full.mjs),
// which names a node apart from adding it and has no Who picker.
const FULL_BUDGET = { simple: 63, advanced: 115 };

const built = { simple: simple(), advanced: advanced() };
const full = { simple: fullSimple(), advanced: fullAdvanced() };

if (process.env.UPDATE_REFERENCE_RECIPES) {
  const cases = Object.fromEntries([
    ...Object.entries(built).map(([key, e]) => [key, e.view]),
    // The full editor's builds are validated and executed by the Go test too.
    ...Object.entries(full).map(([key, e]) => [`${key}-full`, e.view()]),
  ]);
  writeFileSync(FIXTURE, `${JSON.stringify({ cases }, null, 1)}\n`);
}
const fixture = JSON.parse(readFileSync(FIXTURE, 'utf8'));

const to = (view, from, outcome) => view.edges.find((edge) => edge.from === from && edge.outcome === outcome)?.to;
const out = (view, from) => view.edges.filter((edge) => edge.from === from).map((edge) => edge.outcome).sort();

for (const key of Object.keys(BUDGET)) {
  test(`${key}: built inline within ${BUDGET[key]} edits, into the fixture the Go tests run`, () => {
    const e = built[key];
    assert.ok(e.ops.length <= BUDGET[key], `${key} took ${e.ops.length} edits, over its budget of ${BUDGET[key]}:\n${e.ops.join('\n')}`);
    assert.deepEqual(e.view, fixture.cases[key]);
  });
}

test('both open with the operator and the lead agreeing the idea, and close with ready, then merge if allowed', () => {
  for (const { view } of Object.values(built)) {
    const { nodes } = view.template;
    assert.equal(to(view, 'start', 'next'), 'discuss-the-idea');
    assert.equal(nodes['discuss-the-idea'].performer.lead, true);
    assert.equal(nodes['idea-decided'].performer.kind, 'human');
    assert.equal(to(view, 'idea-decided', 'not-yet'), 'discuss-the-idea');
    assert.equal(to(view, 'ready-the-pr', 'next'), 'merge');
    // TODO(tk_c324d5a4): a rule decision on params.auto_merge and CI, once the engine has one.
    assert.equal(nodes.merge.performer.lead, true);
    assert.match(nodes.merge.performer.prompt, /\{\{params\.auto_merge\}\}/);
    assert.equal(to(view, 'merge', 'merge'), 'merge-the-pr');
    assert.equal(nodes[to(view, 'merge', 'leave')].type, 'end');
    assert.equal(view.template.params.auto_merge.type, 'boolean');
    assert.equal(view.template.params.auto_merge.default, false);
  }
});

test('simple: one implementor, one cold reviewer, and the lead decides whether to iterate', () => {
  const { view } = built.simple;
  const { nodes } = view.template;
  assert.equal(nodes['build-the-feature'].performer.role, 'implementor');
  assert.deepEqual(nodes['cold-review'].performer.differentFrom, ['build-the-feature']);
  assert.equal(nodes['iterate-or-move-on'].performer.lead, true);
  assert.equal(to(view, 'iterate-or-move-on', 'iterate'), 'build-the-feature');
  assert.ok(nodes['iterate-or-move-on'].loops.iterate.maxIterations > 0);
  assert.equal(to(view, 'iterate-or-move-on', 'move-on'), 'ready-the-pr');
});

test('advanced: four reviewers vote together, each on its own focus, none of them the implementor', () => {
  const { view } = built.advanced;
  const review = view.template.nodes.review;
  assert.equal(review.deciders.length, 4);
  assert.equal(review.agree, 'approved');
  assert.deepEqual(review.deciders.map((d) => d.prompt.split(' ')[0]), ['Cold', 'Security', 'Product', 'Engineering']);
  for (const decider of review.deciders) {
    assert.equal(decider.anyone, true);
    assert.deepEqual(decider.differentFrom, ['implement']);
  }
  assert.equal(to(view, 'review', 'changes'), 'implement');
  assert.ok(review.loops.changes.onExhausted, 'a gate loop needs its way out');
});

test('advanced: the lead fixes minors or proceeds, checks the criteria, and amends them only as allowed', () => {
  const { view } = built.advanced;
  const { nodes } = view.template;
  assert.equal(to(view, 'review', 'approved'), 'fix-minors-or-proceed');
  assert.deepEqual(out(view, 'fix-minors-or-proceed'), ['fix-minors', 'proceed']);
  assert.equal(to(view, 'fix-minors-or-proceed', 'fix-minors'), 'fix-the-minors');
  assert.equal(nodes['fix-the-minors'].performer.sameAs, 'implement');
  assert.equal(to(view, 'fix-the-minors', 'next'), 'meets-the-criteria');
  assert.equal(to(view, 'fix-minors-or-proceed', 'proceed'), 'meets-the-criteria');
  assert.deepEqual(out(view, 'meets-the-criteria'), ['meets', 'misses']);
  assert.equal(to(view, 'meets-the-criteria', 'meets'), 'ready-the-pr');
  // TODO(tk_c324d5a4): a rule decision on params.allow_amend, once the engine has one.
  assert.equal(to(view, 'meets-the-criteria', 'misses'), 'amend-the-criteria');
  assert.match(nodes['amend-the-criteria'].performer.prompt, /\{\{params\.allow_amend\}\}/);
  assert.equal(to(view, 'amend-the-criteria', 'amend'), 'amend-criteria-and-design');
  assert.equal(to(view, 'amend-criteria-and-design', 'next'), 'ready-the-pr');
  assert.equal(to(view, 'amend-the-criteria', 'rework'), 'implement');
  assert.equal(view.template.params.allow_amend.default, false);
});

test('a step that uses {{x}} declares x, and the Params dialog sets its type and default', () => {
  let { view } = built.simple;
  view = m.setParams(view, {}).view;
  assert.equal(view.template.params, undefined);
  view = m.declareParams(view);
  assert.deepEqual(Object.keys(view.template.params).sort(), ['auto_merge', 'idea']);
  assert.deepEqual(view.template.params.idea, { type: 'string', required: true });
  // An existing declaration is left as it is.
  assert.equal(m.declareParams(built.simple.view), built.simple.view);
});

test('⫴ on a branch of a fork adds one more branch to that fork: four reviewers side by side', () => {
  const e = { view: built.simple.view };
  const fork = m.runAlongside(e.view, 'cold-review', 'agent');
  let { view } = fork;
  for (let n = 0; n < 2; n += 1) {
    assert.equal(m.canRunAlongside(view, 'cold-review'), '');
    const more = m.runAlongside(view, 'cold-review', 'agent');
    assert.equal(more.fork, fork.fork, 'the same fork, not a fork inside it');
    view = more.view;
  }
  assert.equal(view.edges.filter((edge) => edge.from === fork.fork).length, 4);
  const join = to(view, 'cold-review', 'next');
  assert.equal(view.template.nodes[join].join, 'all');
  assert.equal(view.edges.filter((edge) => edge.to === join).length, 4);
  // A branch two steps long is no single-step branch: ⫴ there forks inside it.
  const longer = m.insertAfter(fork.view, 'cold-review', 'agent');
  const nested = m.runAlongside(longer.view, 'cold-review', 'agent');
  assert.notEqual(nested.fork, fork.fork);
});

test('+ another decider keeps a first decider who names nobody off the reviewed step, never a lead or a role', () => {
  // Merge is the lead's decision after the lead's "Ready the PR": kept off
  // it, the lead could never vote.
  const lead = addReviewer(built.simple.view, 'merge').view.template.nodes.merge;
  assert.equal(lead.deciders[0].lead, true);
  assert.equal(lead.deciders[0].differentFrom, undefined);
  assert.deepEqual(lead.deciders[1].differentFrom, ['ready-the-pr']);
  const open = m.insertAfter(built.simple.view, 'build-the-feature', 'decide');
  const gate = addReviewer(open.view, open.id).view.template.nodes[open.id];
  assert.deepEqual(gate.deciders.map((d) => d.differentFrom), [['build-the-feature'], ['build-the-feature']]);
});

for (const key of Object.keys(FULL_BUDGET)) {
  test(`${key}: the full editor builds the same process within ${FULL_BUDGET[key]} edits`, () => {
    const e = full[key];
    assert.ok(e.ops.length <= FULL_BUDGET[key], `${key} took ${e.ops.length} full-editor edits, over ${FULL_BUDGET[key]}:\n${e.ops.join('\n')}`);
    assert.deepEqual(canonical(e.view()), canonical(built[key].view), 'the same steps, Who, text, answers, loops and params as the inline editor builds');
    assert.deepEqual(e.view(), fixture.cases[`${key}-full`]);
  });
}

test('⑂ on an answer puts a new step on that answer, before where it went', () => {
  const { view } = built.simple;
  const out = m.insertOnAnswer(view, 'merge', 'leave', 'you');
  assert.equal(to(out.view, 'merge', 'leave'), out.id);
  assert.equal(to(out.view, out.id, 'continue'), 'end');
  assert.equal(to(out.view, 'merge', 'merge'), 'merge-the-pr', 'the other answers stay');
  assert.throws(() => m.insertOnAnswer(view, 'iterate-or-move-on', 'iterate', 'agent'), /goes back/);
  assert.throws(() => m.insertOnAnswer(view, 'ready-the-pr', 'next', 'agent'), /decision/);
});
