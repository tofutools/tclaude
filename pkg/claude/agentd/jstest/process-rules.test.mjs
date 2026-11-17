// process-rules.js: a rule decision's conditions in plain words and as the
// editor's rows. The grammar mirrors pkg/claude/process/model/rule.go.
import test from 'node:test';
import assert from 'node:assert/strict';
import * as r from '../dashboard/js/process-rules.js';

const names = { ci: 'CI', build: 'Build' };
const stepName = (id) => names[id] || id;

test('a condition reads in plain words', () => {
  for (const [when, said] of [
    ['params.merge == yes and steps.ci.result == succeeded', 'merge is yes and CI succeeded'],
    ['steps.ci.result == failed', 'CI failed'],
    ['steps.ci.result != failed', 'CI did not fail'],
    ['steps.build.result != skipped', 'Build was not skipped'],
    ['steps.build.result == skipped', 'Build was skipped'],
    ['steps.ci.exitCode >= 2', 'CI exit code is at least 2'],
    ['params.auto_merge and not params.hold', 'auto_merge and not hold'],
    ['params.a == 1 or params.b == 2 and outputs.risk != high', 'a is 1 or (b is 2 and risk is not high)'],
    ['(params.a == 1 or params.b == 2) and params.c == "two words"', '(a is 1 or b is 2) and c is two words'],
  ]) assert.equal(r.conditionWords(when, stepName), said, when);
  assert.equal(r.conditionWords('params.merge ==', stepName), 'params.merge ==', 'what does not parse is shown as written');
});

test('a rule and its choice read in plain words, with the values read', () => {
  const rule = {
    cases: [{ when: 'steps.ci.result == failed', then: 'fix' }, { when: 'params.merge == yes and steps.ci.result == succeeded', then: 'merge' }],
    else: 'hold',
  };
  assert.equal(r.ruleWords(rule, stepName), 'CI failed → fix · merge is yes and CI succeeded → merge · otherwise → hold');
  assert.equal(r.choiceWords(rule, stepName), null, 'it has not decided yet');
  const chose = r.choiceWords({ ...rule, choice: { case: 1, outcome: 'merge', read: { 'steps.ci.result': 'succeeded', 'params.merge': 'yes' } } }, stepName);
  assert.deepEqual(chose, { said: 'merge is yes and CI succeeded → merge', read: 'CI = succeeded · merge = yes' });
  assert.equal(r.choiceWords({ ...rule, choice: { case: -1, outcome: 'hold' } }, stepName).said, 'otherwise → hold');
});

test('a condition of comparisons joined by and becomes rows and back', () => {
  const when = 'params.merge == yes and steps.ci.result == succeeded and outputs.note != "two words"';
  const rows = r.toRows(when);
  assert.deepEqual(rows, [
    { operand: 'params.merge', cmp: '==', value: 'yes' },
    { operand: 'steps.ci.result', cmp: '==', value: 'succeeded' },
    { operand: 'outputs.note', cmp: '!=', value: 'two words' },
  ]);
  assert.equal(r.fromRows(rows), when);
  assert.deepEqual(r.toRows('params.auto_merge'), [{ operand: 'params.auto_merge', cmp: '==', value: 'true' }], 'a lone boolean is == true');
  for (const shown of ['params.a == 1 or params.b == 2', 'not params.a', 'params.a ==']) assert.equal(r.toRows(shown), null, shown);
  assert.equal(r.fromRows([{ operand: 'params.x', cmp: '==', value: 'and' }]), 'params.x == "and"', 'a keyword value is quoted');
});

test('what a rule cannot read is refused, as the engine does', () => {
  for (const when of ['', 'env.HOME == x', 'steps.ci == ok', 'params.a == yes yes', '(params.a == 1', 'params.a == "x']) {
    assert.throws(() => r.parseCondition(when), when);
  }
});

test('the row edits leave a condition the rows cannot show alone', () => {
  const rule = { cases: [{ when: 'steps.a.result == failed or params.x == y', then: 'fix' }], else: 'hold' };
  const operands = [{ value: 'steps.a.result', values: r.STEP_RESULTS }];
  assert.deepEqual(r.withAnd(rule, operands, 0), rule);
  assert.deepEqual(r.withRow(rule, operands, 0, 0, { cmp: '!=' }), rule);
  assert.deepEqual(r.withoutRow(rule, 0, 0), rule);
});
