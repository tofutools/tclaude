import test from 'node:test';
import assert from 'node:assert/strict';

import { edgePinPlacement, edgePinTitle } from '../dashboard/js/process-edge-hint.js';

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

// HALF is half the rendered button (22px in dashboard.css), so a placement is a
// centre and the button spans centre +/- HALF.
const HALF = 11;
const box = { left: 100, top: 50, width: 40, height: 14 };

// overlaps is the property that actually matters: the button must not sit on the
// text it controls, whatever the geometry.
function overlaps(placement, rect) {
  const pin = {
    left: placement.left - HALF, right: placement.left + HALF,
    top: placement.top - HALF, bottom: placement.top + HALF,
  };
  return pin.left < rect.left + rect.width && pin.right > rect.left
    && pin.top < rect.top + rect.height && pin.bottom > rect.top;
}

test('the pin never overlaps the label it controls', () => {
  for (const orientation of ['horizontal', 'vertical']) {
    const placement = edgePinPlacement({ anchor: { left: 0, top: 0 }, box, orientation });
    assert.equal(overlaps(placement, box), false, orientation);
  }
});

test('a horizontal edge puts the pin under the label, a vertical one beside it', () => {
  const below = edgePinPlacement({ anchor: { left: 0, top: 0 }, box, orientation: 'horizontal' });
  assert.equal(below.left, box.left + box.width / 2, 'centred on the label');
  assert.ok(below.top > box.top + box.height, 'and clear underneath');

  const beside = edgePinPlacement({ anchor: { left: 0, top: 0 }, box, orientation: 'vertical' });
  assert.ok(beside.left > box.left + box.width, 'clear to the right');
  assert.equal(beside.top, box.top + box.height / 2, 'vertically centred on the label');
});

test('placement follows the measured box, not the layout anchor', () => {
  // The anchor is a point on the edge geometry, not the rendered text extent.
  // A long label can still reach the decoration placed from that point alone.
  const backEdgeBox = { left: 80, top: 50, width: 40, height: 14 };
  const anchor = { left: 100, top: 50 };
  const placement = edgePinPlacement({ anchor, box: backEdgeBox, orientation: 'vertical' });
  assert.equal(overlaps(placement, backEdgeBox), false);
  assert.ok(placement.left > anchor.left,
    'the pin tracks the measured text extent beyond its midpoint anchor');
});

test('a wider label pushes the pin further out', () => {
  const narrow = edgePinPlacement({ anchor: { left: 0, top: 0 }, box, orientation: 'vertical' });
  const wide = edgePinPlacement({
    anchor: { left: 0, top: 0 }, box: { ...box, width: 200 }, orientation: 'vertical',
  });
  assert.ok(wide.left > narrow.left);
  assert.equal(overlaps(wide, { ...box, width: 200 }), false);
});

test('an unmeasured label falls back to an estimate that still clears it', () => {
  // Defensive only: the label is rendered before the pin publishes. The estimate
  // must scale with zoom, since an unmeasured label is larger when zoomed in.
  const anchor = { left: 0, top: 0 };
  const near = edgePinPlacement({ anchor, outcome: 'pass', orientation: 'vertical', zoom: 1 });
  const far = edgePinPlacement({ anchor, outcome: 'pass', orientation: 'vertical', zoom: 3 });
  assert.ok(far.left > near.left, 'a zoomed label is wider, so the pin moves further out');

  const longKey = edgePinPlacement({
    anchor, outcome: 'needs-escalation-review', orientation: 'vertical', zoom: 1,
  });
  assert.ok(longKey.left > near.left, 'a longer key needs more clearance');

  const below = edgePinPlacement({ anchor, outcome: 'pass', orientation: 'horizontal' });
  assert.equal(below.left, anchor.left);
  assert.ok(below.top > anchor.top);
});
