// terminal-drag-out.test.mjs — pure-logic unit tests for the "drag a terminal
// out of its home region" gesture (TCL-617). The browser cannot report that a
// drag ended over another window, so detach/reattach is inferred from the drag
// end point; these cases pin the conservative rule that keeps a sloppy reorder
// or a cancelled drag from silently moving a terminal.
//
//   node --test pkg/claude/agentd/jstest/terminal-drag-out.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  DRAG_OUT_MARGIN, dragOutPoint, draggedOut, dragLeftRegion,
} from '../dashboard/js/terminal-drag-out.js';

const stripRect = { left: 100, top: 0, right: 400, bottom: 30, width: 300, height: 30 };

test('drag-out geometry only fires well clear of the home region', () => {
  assert.deepEqual(dragOutPoint({ clientX: 12, clientY: 34 }), { x: 12, y: 34 });
  assert.equal(dragOutPoint({}), null, 'a drag end without coordinates is never a gesture');
  assert.equal(dragOutPoint({ clientX: 0, clientY: 0 }), null,
    'the viewport origin is how a cancelled drag reports itself');

  assert.equal(draggedOut({ x: 200, y: 15 }, stripRect), false, 'inside the strip');
  assert.equal(draggedOut({ x: 200, y: 30 + DRAG_OUT_MARGIN }, stripRect), false,
    'exactly at the slop margin is still a near-miss reorder');
  assert.equal(draggedOut({ x: 200, y: 30 + DRAG_OUT_MARGIN + 1 }, stripRect), true, 'below the strip');
  assert.equal(draggedOut({ x: 100 - DRAG_OUT_MARGIN - 1, y: 15 }, stripRect), true, 'left of the strip');
  assert.equal(draggedOut({ x: 400 + DRAG_OUT_MARGIN + 1, y: 15 }, stripRect), true, 'right of the strip');
  assert.equal(draggedOut({ x: 200, y: -DRAG_OUT_MARGIN - 1 }, stripRect), true, 'above the strip');
  assert.equal(draggedOut(null, stripRect), false);
  assert.equal(draggedOut({ x: 900, y: 900 }, null), false, 'an unmeasured region never detaches');
  assert.equal(draggedOut({ x: 900, y: 900 }, { left: 10, width: 100 }), false,
    'a partial rect is treated as unmeasured rather than guessed at');
  assert.equal(draggedOut({ x: 900, y: 900 }, { left: 10, top: 0, width: 100, height: 20 }), true,
    'width/height stand in for a missing right/bottom edge');

  const region = { getBoundingClientRect: () => stripRect };
  assert.equal(dragLeftRegion({ clientX: 200, clientY: 15 }, region), false);
  assert.equal(dragLeftRegion({ clientX: 200, clientY: 400 }, region), true);
  assert.equal(dragLeftRegion({ clientX: 200, clientY: 400 }, null), false,
    'an unmounted region resolves to the explicit buttons, not a detach');
  assert.equal(dragLeftRegion({ clientX: 200, clientY: 400 }, region, 1000), false,
    'the margin is the caller-tunable slop');
});
