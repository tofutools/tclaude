import test from 'node:test';
import assert from 'node:assert/strict';

import { edgePinOffset, edgePinTitle } from '../dashboard/js/process-edge-hint.js';

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

test('the pin clears the label on the side with room for it', () => {
  // A horizontal run has empty canvas below, so the pin hangs there.
  const below = edgePinOffset('pass', 'horizontal');
  assert.equal(below.dx, 0);
  assert.ok(below.dy > 0, 'horizontal edges keep the pin underneath');

  // On a vertical run, below is where the next node sits, so it goes beside.
  const beside = edgePinOffset('pass', 'vertical');
  assert.equal(beside.dy, 0);
  assert.ok(beside.dx > 0, 'vertical edges push the pin to the right');
});

test('a wider key pushes the pin further right, but never up or down', () => {
  const short = edgePinOffset('ok', 'vertical');
  const long = edgePinOffset('needs-escalation-review', 'vertical');
  assert.ok(long.dx > short.dx, 'the pin must clear the text it sits beside');
  assert.equal(long.dy, 0);
});

test('the beside offset scales with zoom; the below offset is fixed', () => {
  // Both the label and the gap grow with the viewport, so the horizontal shift
  // has to follow. The vertical drop is a constant clearance under the text.
  assert.ok(edgePinOffset('pass', 'vertical', 2).dx > edgePinOffset('pass', 'vertical', 1).dx);
  assert.equal(edgePinOffset('pass', 'horizontal', 2).dy, edgePinOffset('pass', 'horizontal', 1).dy);
});

test('an unknown orientation falls back to below, the historical behaviour', () => {
  assert.equal(edgePinOffset('pass', undefined).dy > 0, true);
  assert.equal(edgePinOffset('pass', '').dx, 0);
});
