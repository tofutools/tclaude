import test from 'node:test';
import assert from 'node:assert/strict';

import {
  PASS_OUTCOMES, UNNAMED_OUTCOME, outcomeCarriesInformation,
} from '../dashboard/js/process-outcome-vocabulary.js';

test('a lone generic outcome is not worth drawing; a branch always is', () => {
  assert.equal(outcomeCarriesInformation(UNNAMED_OUTCOME, 1), false);
  // The same edge gains a sibling: the key now picks which way the run goes.
  assert.equal(outcomeCarriesInformation(UNNAMED_OUTCOME, 2), true);
  assert.equal(outcomeCarriesInformation('', 1), false);
});

test('templates predating unnamed connectors declutter too', () => {
  // Existing YAML spells the lone-exit case 'pass' (and 'done'/'success').
  // Hiding only the new default would leave every already-authored process as
  // noisy as before.
  for (const generic of ['pass', 'done', 'success']) {
    assert.equal(outcomeCarriesInformation(generic, 1), false, generic);
    assert.equal(outcomeCarriesInformation(generic, 2), true, generic);
  }
});

test('a deliberately chosen name survives even on a lone connector', () => {
  // Outside the generic vocabulary the label is documentation, not filler.
  assert.equal(outcomeCarriesInformation('approved', 1), true);
  assert.equal(outcomeCarriesInformation('escalated', 1), true);
});

test('a decision label is never hidden, even as the only way out', () => {
  // plan.DecisionEdge matches a verdict EXACTLY and has no lone-edge fallback,
  // unlike ResolvePassEdge. Hiding a decision's only outcome would leave the
  // author blind to the verdict the run has to produce, and
  // validateChoiceOutcomes does not cross-check choices against next keys.
  assert.equal(outcomeCarriesInformation('pass', 1, 'decision'), true);
  assert.equal(outcomeCarriesInformation('next', 1, 'decision'), true);
  // Every other node type still declutters its lone generic exit.
  for (const type of ['task', 'wait', 'start', 'parallel', '']) {
    assert.equal(outcomeCarriesInformation('pass', 1, type), false, type);
  }
});

test('UNNAMED_OUTCOME is the precedence-winning pass alias', () => {
  // If it were 'next' -- the last alias -- a later 'pass' sibling would take
  // the pass routing away from the connector drawn first.
  assert.equal(UNNAMED_OUTCOME, PASS_OUTCOMES[0]);
  assert.equal(UNNAMED_OUTCOME, 'pass');
});
