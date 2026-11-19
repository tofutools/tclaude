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
import { InlineEditor, advanced, advancedTracks, simple } from './process-reference-recipes.mjs';
import { canonical, fullAdvanced, fullSimple } from './process-reference-full.mjs';

const FIXTURE = new URL('./process-reference-recipes.json', import.meta.url);

// BUDGET is how many inline edits each process takes today. A change that
// makes building one harder fails here; one that makes it easier lowers
// the number (and should lower the budget with it).
const BUDGET = { simple: 36, advanced: 78, 'advanced-tracks': 75 };
// FULL_BUDGET is the same for the full editor (process-reference-full.mjs),
// which names a node apart from adding it and has no Who picker.
const FULL_BUDGET = { simple: 56, advanced: 124 };

const built = { simple: simple(), advanced: advanced(), 'advanced-tracks': advancedTracks() };
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

test('all open with the operator and the lead agreeing the idea, and close with ready, then a rule that merges if allowed', () => {
  for (const { view } of Object.values(built)) {
    const { nodes } = view.template;
    assert.equal(to(view, 'start', 'next'), 'discuss-the-idea');
    assert.equal(nodes['discuss-the-idea'].performer.lead, true);
    assert.equal(nodes['idea-decided'].performer.kind, 'human');
    assert.equal(to(view, 'idea-decided', 'not-yet'), 'discuss-the-idea');
    assert.equal(nodes.merge.performer.kind, 'rule');
    assert.equal(nodes.merge.performer.else, 'leave');
    assert.equal(to(view, 'merge', 'merge'), 'merge-the-pr');
    assert.equal(nodes[to(view, 'merge', 'leave')].type, 'end');
    assert.equal(view.template.params.auto_merge.type, 'boolean');
    assert.equal(view.template.params.auto_merge.default, false);
  }
});

// Whether CI is a step is the designer's choice (tk_82f97468): one style each.
test('example 2 keeps CI inside the agents\' steps: no CI step, and Merge reads only auto_merge', () => {
  const { view } = built.simple;
  const { nodes } = view.template;
  assert.equal(nodes.ci, undefined);
  assert.match(nodes['build-the-feature'].performer.prompt, /until CI on the PR passes/);
  assert.match(nodes['ready-the-pr'].performer.prompt, /Once CI on the PR passes/);
  assert.equal(to(view, 'ready-the-pr', 'next'), 'merge');
  assert.deepEqual(nodes.merge.performer.cases, [{ when: 'params.auto_merge == true', then: 'merge' }]);
});

test('example 1 models CI as a step that may fail, and Merge needs it passed and auto_merge on', () => {
  for (const key of ['advanced', 'advanced-tracks']) {
    const { view } = built[key];
    const { nodes } = view.template;
    assert.equal(to(view, 'ready-the-pr', 'next'), 'ci');
    assert.equal(nodes.ci.performer.kind, 'program');
    assert.equal(nodes.ci.mayFail, true, 'CI failing goes on, for the rule to see');
    assert.equal(to(view, 'ci', 'next'), 'merge');
    assert.deepEqual(nodes.merge.performer.cases, [{ when: 'steps.ci.result == succeeded and params.auto_merge == true', then: 'merge' }]);
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
  assert.equal(to(view, 'meets-the-criteria', 'misses'), 'amending-allowed');
  assert.deepEqual(nodes['amending-allowed'].performer, { kind: 'rule', cases: [{ when: 'params.allow_amend == true', then: 'allowed' }], else: 'rework' });
  assert.equal(to(view, 'amending-allowed', 'rework'), 'implement');
  assert.equal(to(view, 'amending-allowed', 'allowed'), 'amend-the-criteria');
  assert.equal(nodes['amend-the-criteria'].performer.lead, true, 'allowed: the lead judges whether the gap is minor');
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
  // auto_merge is read by the Merge rule, not quoted in a step: declared in the dialog.
  assert.deepEqual(Object.keys(view.template.params).sort(), ['idea']);
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
  // A lead's decision after the lead's "Ready the PR": kept off it, the lead
  // could never vote.
  const added = m.insertAfter(built.simple.view, 'ready-the-pr', 'decide');
  const byLead = m.setDecisionWho(added.view, added.id, { mode: 'lead' });
  const lead = addReviewer(byLead.view, added.id).view.template.nodes[added.id];
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
    assert.deepEqual(canonical(e.view()), canonical(built[key].view), 'the same steps, Who, text, answers, rules, may-fail, loops and params as the inline editor builds');
    assert.deepEqual(e.view(), fixture.cases[`${key}-full`]);
  });
}

test('⑂ on an answer puts a new step on that answer, before where it went', () => {
  const { view } = built.simple;
  const out = m.insertOnAnswer(view, 'merge', 'leave', 'you');
  assert.equal(to(out.view, 'merge', 'leave'), out.id);
  assert.equal(to(out.view, out.id, 'next'), 'end', 'your step goes on to where the answer went');
  assert.equal(to(out.view, 'merge', 'merge'), 'merge-the-pr', 'the other answers stay');
  assert.throws(() => m.insertOnAnswer(view, 'iterate-or-move-on', 'iterate', 'agent'), /goes back/);
  assert.throws(() => m.insertOnAnswer(view, 'ready-the-pr', 'next', 'agent'), /decision/);
});

test('advanced with reviewer tracks: four reviews alongside, each loops on its own fix by the implementor, joined before the lead', () => {
  const { view } = built['advanced-tracks'];
  const { nodes } = view.template;
  const fork = to(view, 'implement', 'next');
  assert.equal(nodes[fork].type, 'parallel');
  const reviews = view.edges.filter((edge) => edge.from === fork).map((edge) => edge.to).sort();
  assert.deepEqual(reviews, ['cold-review', 'engineering-review', 'product-review', 'security-review']);
  for (const review of reviews) {
    const node = nodes[review];
    assert.equal(node.type, 'decision');
    assert.equal(node.performer.anyone, true);
    assert.deepEqual(node.performer.differentFrom, ['implement'], `${review} is not the implementor`);
    const fix = to(view, review, 'changes');
    assert.equal(nodes[fix].performer.sameAs, 'implement', `${review}'s fix goes to the implementor`);
    assert.equal(to(view, fix, 'next'), review, 'and back to the same reviewer');
    assert.ok(node.loops.changes.onExhausted, 'a used-up track has a way out');
    assert.equal(to(view, review, 'approved'), 'fix-minors-or-proceed');
  }
  assert.equal(nodes['fix-minors-or-proceed'].join, 'all', 'the lead goes on once every reviewer approved');
  assert.equal(nodes.review, undefined, 'no gate');
});

test('a step that may fail stops being one when it becomes a decision (the server refuses mayFail off a task)', () => {
  const e = new InlineEditor('may fail');
  e.step('after', 'start', 'command', { name: 'CI', text: 'make test' });
  e.failure('ci', 'go-on');
  const { view } = e;
  assert.equal(view.template.nodes.ci.mayFail, true);
  const yours = m.changeKind(view, 'ci', 'you').view.template.nodes.ci;
  assert.equal(yours.type, 'task', 'a step you perform is still a task');
  assert.equal(yours.mayFail, true, 'and may still fail and go on');
  assert.equal(m.changeKind(view, 'ci', 'agent').view.template.nodes.ci.mayFail, true, 'still a task: kept');
  const decided = structuredClone(view);
  decided.template.nodes.ci.type = 'decision';
  decided.template.nodes.ci.performer = { kind: 'agent', prompt: 'Pass?' };
  assert.equal(m.changeKind(decided, 'ci', 'you').view.template.nodes.ci.mayFail, undefined, 'a decision may not fail and go on');
});
