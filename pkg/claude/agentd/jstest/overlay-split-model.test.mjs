import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const AREA = { x: 0, y: 40, w: 1600, h: 800 };
const pair = (axis, a, b, at = 0.5) => ({ axis, at, a: { id: a }, b: { id: b } });
const quad = (x = 0.5, y = 0.5) => ({ x, y, cells: ['a', 'b', 'c', 'd'].map((id) => ({ id })) });

async function model(t) {
  const harness = await createPreactHarness(t);
  return harness.importDashboardModule('js/overlay-split-model.js');
}

test('two windows, 2+1 and four are one tree', async (t) => {
  const { splitLayout } = await model(t);
  const two = splitLayout(pair('x', 'a', 'b'), AREA);
  assert.deepEqual(two.rects.a, { x: 0, y: 40, w: 800, h: 800 });
  assert.deepEqual(two.rects.b, { x: 800, y: 40, w: 800, h: 800 });
  assert.equal(two.lines.length, 1);
  const twoPlusOne = splitLayout({ axis: 'x', at: 0.5, a: { id: 'a' }, b: pair('y', 'b', 'c') }, AREA);
  assert.deepEqual(twoPlusOne.rects.c, { x: 800, y: 440, w: 800, h: 400 });
  assert.equal(twoPlusOne.lines[1].w, 800, 'the inner line spans only its own area');
  const four = splitLayout(quad(0.4, 0.6), AREA);
  assert.deepEqual(four.rects.d, { x: 640, y: 520, w: 960, h: 320 });
  assert.deepEqual(four.crossings.map(({ x, y }) => [x, y]), [[640, 520]]);
  assert.equal(four.lines[0].h, 800, 'a quad line runs the full height');
  assert.equal(four.lines[1].w, 1600, 'and the full width');
});

test('a child of either node can be either node', async (t) => {
  const { leavesOf, splitLayout } = await model(t);
  const tree = { x: 0.5, y: 0.5, cells: [pair('x', 'a', 'b'), { id: 'c' }, { id: 'd' }, { axis: 'y', at: 0.5, a: { id: 'e' }, b: { id: 'f' } }] };
  assert.deepEqual(leavesOf(tree), ['a', 'b', 'c', 'd', 'e', 'f']);
  const { rects, lines } = splitLayout({ axis: 'y', at: 0.5, a: { id: 'top' }, b: quad() }, AREA);
  assert.deepEqual(rects.a, { x: 0, y: 400, w: 800, h: 220 }, 'the quad below keeps its minimum, so its line gives way');
  assert.equal(lines.length, 3);
});

test('a line moves only its own node, and a crossing moves both of a quad', async (t) => {
  const { movedLine, splitLayout } = await model(t);
  const tree = { axis: 'x', at: 0.5, a: { id: 'a' }, b: pair('y', 'b', 'c') };
  const { lines } = splitLayout(tree, AREA);
  const moved = movedLine(tree, lines[1].path, 'y', { x: 1200, y: 240 }, lines[1].area);
  assert.equal(moved.at, 0.5);
  assert.equal(moved.b.at, 0.25);
  const [line] = splitLayout(quad(), AREA).lines;
  let both = movedLine(quad(), line.path, 'x', { x: 400, y: 240 }, line.area);
  both = movedLine(both, line.path, 'y', { x: 400, y: 240 }, line.area);
  assert.deepEqual([both.x, both.y], [0.25, 0.25]);
});

test('a line stops where a window would go below its minimum, however deep', async (t) => {
  const { splitLayout } = await model(t);
  const { rects } = splitLayout({ axis: 'x', at: 0.05, a: { id: 'a' }, b: pair('x', 'b', 'c') }, AREA);
  assert.equal(rects.a.w, 360);
  const deep = splitLayout({ axis: 'x', at: 0.95, a: { id: 'a' }, b: pair('x', 'b', 'c') }, AREA);
  assert.equal(deep.rects.b.w, 360);
  assert.equal(deep.rects.c.w, 360);
});

test('a tree that the room cannot hold is reported', async (t) => {
  const { fits } = await model(t);
  assert.equal(fits(pair('x', 'a', 'b'), { ...AREA, w: 719 }), false);
  assert.equal(fits(pair('x', 'a', 'b'), { ...AREA, w: 720 }), true);
  assert.equal(fits(quad(), { ...AREA, h: 439 }), false);
});

test('closing a window gives its room to its sibling, and a pair ends', async (t) => {
  const { withoutWindow } = await model(t);
  assert.equal(withoutWindow(pair('x', 'a', 'b'), 'a'), null);
  const tree = { axis: 'x', at: 0.3, a: { id: 'a' }, b: pair('y', 'b', 'c') };
  assert.deepEqual(withoutWindow(tree, 'c'), pair('x', 'a', 'b', 0.3));
  assert.deepEqual(withoutWindow(tree, 'a'), pair('y', 'b', 'c'));
  assert.equal(withoutWindow(tree, 'nobody'), tree);
});

test('closing a cell of a quad gives it to its row, and the horizontal line stays', async (t) => {
  const { splitLayout, withoutWindow } = await model(t);
  const without = withoutWindow(quad(0.4, 0.6), 'd');
  assert.deepEqual(without, { axis: 'y', at: 0.6, a: pair('x', 'a', 'b', 0.4), b: { id: 'c' } });
  assert.equal(splitLayout(without, AREA).rects.c.w, 1600);
  const top = withoutWindow(quad(0.4, 0.6), 'a');
  assert.deepEqual(top.a, { id: 'b' });
  assert.deepEqual(top.b, pair('x', 'c', 'd', 0.4));
  assert.deepEqual(withoutWindow(without, 'c'), pair('x', 'a', 'b', 0.4));
});

test('the showing tree leaves out windows that are away, and the saved one keeps them', async (t) => {
  const { keepingWindows } = await model(t);
  const tree = { axis: 'x', at: 0.5, a: { id: 'a' }, b: pair('y', 'b', 'c') };
  assert.deepEqual(keepingWindows(tree, new Set(['a', 'c'])), pair('x', 'a', 'c'));
  assert.equal(keepingWindows(tree, new Set(['a'])), null);
  assert.equal(keepingWindows(null, new Set(['a'])), null);
  assert.equal(tree.b.a.id, 'b');
});

test('windows facing each other pair up and fill the room', async (t) => {
  const { roomPair } = await model(t);
  assert.deepEqual(roomPair('a', 'right', 'b', 'left'), pair('x', 'a', 'b'));
  assert.deepEqual(roomPair('a', 'left', 'b', 'right'), pair('x', 'b', 'a'));
  assert.deepEqual(roomPair('a', 'top-right', 'b', 'top-left'), pair('x', 'a', 'b'), 'a row of corners stands side by side');
  assert.deepEqual(roomPair('a', 'bottom-right', 'b', 'top-left'), pair('x', 'a', 'b'), 'and so does a diagonal');
  assert.deepEqual(roomPair('a', 'bottom-left', 'b', 'top-left'), pair('y', 'a', 'b'), 'a column of corners stacks');
  assert.deepEqual(roomPair('a', 'top-left', 'b', 'bottom'), pair('y', 'b', 'a'));
  assert.equal(roomPair('a', 'left', 'b', 'left'), null);
  assert.equal(roomPair('a', 'left', 'b', 'bottom'), null);
});

test('a drop on a half splits that window, and the fourth corner makes a quad', async (t) => {
  const { withWindow } = await model(t);
  const two = pair('x', 'a', 'b');
  const twoPlusOne = withWindow(two, 'b', 'bottom', 'c');
  assert.deepEqual(twoPlusOne, { axis: 'x', at: 0.5, a: { id: 'a' }, b: pair('y', 'b', 'c') });
  const asCorner = withWindow(two, 'b', 'bottom-right', 'c');
  assert.deepEqual(asCorner, twoPlusOne);
  const four = withWindow(asCorner, 'a', 'bottom-left', 'd');
  assert.deepEqual(four, { x: 0.5, y: 0.5, cells: [{ id: 'a' }, { id: 'b' }, { id: 'd' }, { id: 'c' }] });
  const nested = withWindow(twoPlusOne, 'a', 'bottom', 'd');
  assert.equal(nested.axis, 'x', 'halves stay binaries with lines of their own');
  assert.deepEqual(withWindow(pair('y', 'a', 'b'), 'a', 'left', 'c').a, pair('x', 'c', 'a'));
});

test('a quad keeps the line that was already there', async (t) => {
  const { withWindow } = await model(t);
  const dragged = { axis: 'x', at: 0.5, a: { id: 'a' }, b: pair('y', 'b', 'c', 0.3) };
  assert.equal(withWindow(dragged, 'a', 'bottom-left', 'd').y, 0.3);
});

test('a line of the showing tree finds its split in the saved one', async (t) => {
  const { coveringPath, keepingWindows, movedLine, leavesOf, nodeAt, splitLayout } = await model(t);
  const saved = { axis: 'x', at: 0.5, a: { id: 'a' }, b: { axis: 'y', at: 0.5, a: { id: 'b' }, b: pair('x', 'c', 'd') } };
  const live = keepingWindows(saved, new Set(['a', 'b', 'c']));
  const [, inner] = splitLayout(live, AREA).lines;
  const path = coveringPath(saved, leavesOf(nodeAt(live, inner.path)));
  const moved = movedLine(saved, path, inner.axis, { x: 1200, y: 240 }, inner.area);
  assert.equal(moved.b.at, 0.25);
  assert.deepEqual(leavesOf(moved), ['a', 'b', 'c', 'd'], 'the window that is away keeps its place');
  const crossing = { x: 0.5, y: 0.5, cells: ['a', 'b', 'c', 'd'].map((id) => ({ id })) };
  assert.deepEqual(coveringPath(crossing, ['a', 'b']), [], 'a collapsed quad line is the quad');
});

test('a drop counts near an edge or corner of a window, not in its middle', async (t) => {
  const { dropZone } = await model(t);
  const rect = { x: 100, y: 100, w: 100, h: 100 };
  assert.equal(dropZone({ x: 110, y: 150 }, rect), 'left');
  assert.equal(dropZone({ x: 190, y: 150 }, rect), 'right');
  assert.equal(dropZone({ x: 150, y: 110 }, rect), 'top');
  assert.equal(dropZone({ x: 110, y: 190 }, rect), 'bottom-left');
  assert.equal(dropZone({ x: 150, y: 150 }, rect), '');
  assert.equal(dropZone({ x: 50, y: 150 }, rect), '');
});

test('a saved tree is read back, and a bad one is not', async (t) => {
  const { parseSplit } = await model(t);
  const tree = { axis: 'y', at: 0.3, a: quad(0.2, 0.7), b: { id: 'e' } };
  assert.deepEqual(parseSplit(JSON.stringify(tree)), tree);
  for (const bad of ['', 'nope', 'null', '{"id":"a"}', JSON.stringify(pair('z', 'a', 'b')), JSON.stringify(pair('x', 'a', 'a')),
    JSON.stringify({ x: 0.5, y: 0.5, cells: [{ id: 'a' }, { id: 'b' }] }), JSON.stringify({ axis: 'x', at: 'half', a: { id: 'a' }, b: { id: 'b' } })]) {
    assert.equal(parseSplit(bad), null, bad);
  }
});

test('a detach button sits at the line centre, or on a quad at the arm before the crossing', async (t) => {
  const { detachSpot, splitLayout } = await model(t);
  const { lines } = splitLayout(pair('x', 'a', 'b'), AREA);
  assert.deepEqual(detachSpot(lines[0]), { x: 800, y: 440 });
  const four = splitLayout(quad(), AREA);
  assert.deepEqual(detachSpot(four.lines[0], four.crossings[0]), { x: 800, y: 240 });
  assert.deepEqual(detachSpot(four.lines[1], four.crossings[0]), { x: 400, y: 440 });
});
