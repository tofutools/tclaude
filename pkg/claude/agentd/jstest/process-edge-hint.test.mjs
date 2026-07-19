import test from 'node:test';
import assert from 'node:assert/strict';

import { edgePinTitle } from '../dashboard/js/process-edge-hint.js';

test('the pin tooltip names the action it will perform', () => {
  const willUnpin = edgePinTitle('fail', true);
  assert.match(willUnpin, /^Unpin "fail"/);
  assert.match(willUnpin, /hide this label unless the connector is selected/);

  const willPin = edgePinTitle('fail', false);
  assert.match(willPin, /^Pin "fail"/);
  assert.match(willPin, /keep this label visible/);
});

test('the tooltip separates hiding the key from renaming it', () => {
  // The whole hazard: hiding is cosmetic, renaming re-points the run. A tooltip
  // that only said "this is the outcome key" would leave that ambiguous.
  const title = edgePinTitle('pass', true);
  assert.match(title, /hiding it changes nothing/);
  assert.match(title, /renaming it changes where the run goes/);
});
