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
  DETACH_WINDOW_CHROME_HEIGHT, DETACH_WINDOW_MIN, DRAG_OUT_MARGIN,
  detachWindowFeatures, dragLeftRegion, dragOutPoint, dragScreenPoint, draggedOut,
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

const screen = { availWidth: 1600, availHeight: 900, availLeft: 0, availTop: 0 };

function features(overrides = {}) {
  return detachWindowFeatures({
    size: { width: 800, height: 500 }, at: { x: 200, y: 100 }, screen, ...overrides,
  });
}

test('a dragged-out terminal asks for a window sized to the pane it is leaving', () => {
  assert.deepEqual(dragScreenPoint({ screenX: 40, screenY: 60 }), { x: 40, y: 60 });
  assert.equal(dragScreenPoint({ clientX: 40, clientY: 60 }), null,
    'a drag with no screen coordinate cannot place a window');

  assert.equal(features(), `popup=yes,width=800,height=${500 + DETACH_WINDOW_CHROME_HEIGHT},left=200,top=100`);
  assert.match(features(), /^popup=yes/,
    'the features string is what makes a browser choose a window over a tab');

  assert.equal(features({ at: null }), `popup=yes,width=800,height=${500 + DETACH_WINDOW_CHROME_HEIGHT},left=0,top=0`,
    'an unplaceable drag still sizes the window');
  assert.equal(features({ size: { width: 4000, height: 3000 } }),
    'popup=yes,width=1600,height=900,left=0,top=0', 'a pane larger than the screen is clamped onto it');
  assert.equal(features({ size: { width: 10, height: 10 } }),
    `popup=yes,width=${DETACH_WINDOW_MIN.width},height=${DETACH_WINDOW_MIN.height},left=200,top=100`,
    'a sliver of a pane still opens a usable window');
  assert.equal(features({ at: { x: 1500, y: 850 } }),
    `popup=yes,width=800,height=540,left=800,top=360`,
    'a release near the screen edge pulls the whole window back on-screen');
  assert.equal(features({ at: { x: -500, y: -500 } }),
    'popup=yes,width=800,height=540,left=0,top=0');
  assert.equal(
    detachWindowFeatures({
      size: { width: 800, height: 500 }, at: { x: 100, y: 100 },
      screen: { availWidth: 1600, availHeight: 900, availLeft: 1600, availTop: 0 },
    }),
    'popup=yes,width=800,height=540,left=1600,top=100',
    'a second monitor is addressed through its own available origin');

  assert.equal(features({ size: null }), '', 'an unmeasured pane asks for a plain tab, not a guess');
  assert.equal(features({ screen: null }), '', 'an unmeasured screen asks for a plain tab');
  assert.equal(features({ size: { width: 0, height: 500 } }), '');
  assert.equal(detachWindowFeatures(), '');
});
