import test from 'node:test';
import assert from 'node:assert/strict';

import {
  UNNAMED_OUTCOME, outcomeCarriesInformation,
} from '../dashboard/js/process-outcome-vocabulary.js';

test('a lone unnamed outcome is not worth drawing; a branch always is', () => {
  assert.equal(outcomeCarriesInformation(UNNAMED_OUTCOME, 1), false);
  // The same edge gains a sibling: the key now picks which way the run goes.
  assert.equal(outcomeCarriesInformation(UNNAMED_OUTCOME, 2), true);
  // A deliberately named lone edge keeps its label — the author chose it.
  assert.equal(outcomeCarriesInformation('approved', 1), true);
  assert.equal(outcomeCarriesInformation('', 1), false);
});
