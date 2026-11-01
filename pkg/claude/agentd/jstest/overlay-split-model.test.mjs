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

// Every shape a split can take, with an unlocked line among them.
const SHAPES = {
  pair: pair('x', 'a', 'b'),
  offCentre: pair('y', 'a', 'b', 0.3),
  twoPlusOne: { axis: 'x', at: 0.5, a: pair('y', 'a', 'b'), b: { id: 'c' } },
  quad: quad(0.4, 0.6),
  nested: { axis: 'x', at: 0.4, a: { id: 'a' }, b: { axis: 'y', at: 0.5, a: { id: 'b' }, b: pair('x', 'c', 'd') } },
  unlockedPair: { axis: 'x', at: 0.4, free: 0.6, a: { id: 'a' }, b: { id: 'b' } },
  unlockedQuad: { x: 0.4, y: 0.5, freeX: 0.6, cells: ['a', 'b', 'c', 'd'].map((id) => ({ id })) },
  unlockedNested: { axis: 'x', at: 0.5, a: { axis: 'y', at: 0.4, free: 0.6, a: { id: 'a' }, b: { id: 'b' } }, b: { id: 'c' } },
};
const subsets = (ids) => ids.reduce((all, id) => [...all, ...all.map((some) => [...some, id])], [[]]).filter((some) => some.length);

test('a window away keeps its cell in every shape, and nothing else moves', async (t) => {
  const { keepingWindows, leavesOf, parseSplit, splitLayout } = await model(t);
  for (const [name, tree] of Object.entries(SHAPES)) {
    const whole = splitLayout(tree, AREA);
    const ids = leavesOf(tree);
    for (const away of subsets(ids).filter((some) => some.length < ids.length)) {
      const here = new Set(ids.filter((id) => !away.includes(id)));
      const showing = splitLayout(keepingWindows(tree, here), AREA);
      for (const id of here) assert.deepEqual(showing.rects[id], whole.rects[id], `${name}: ${id} with ${away} away`);
      assert.deepEqual(showing.gaps.map((cell) => cell.held).sort(), [...away].sort(), `${name}: each keeps its cell`);
      for (const cell of showing.gaps) assert.deepEqual(cell.rect, whole.rects[cell.held], `${name}: the cell is the window's`);
      assert.deepEqual(showing.lines.map(({ axis, path }) => [axis, path]), whole.lines.map(({ axis, path }) => [axis, path]), `${name}: the lines stay, at the same paths`);
    }
    assert.deepEqual(splitLayout(keepingWindows(tree, new Set(ids)), AREA).rects, whole.rects);
    assert.equal(keepingWindows(tree, new Set()), null, 'nobody showing is no split');
    assert.deepEqual(parseSplit(JSON.stringify(tree)), tree, 'the saved tree is not touched, so a reload changes nothing');
  }
  assert.equal(keepingWindows(null, new Set(['a'])), null);
});

test('a window that closes leaves its cell empty, in every shape and order', async (t) => {
  const { leavesOf, parseSplit, splitLayout, withoutWindow } = await model(t);
  for (const [name, tree] of Object.entries(SHAPES)) {
    const whole = splitLayout(tree, AREA);
    const ids = leavesOf(tree);
    for (const gone of subsets(ids).filter((some) => some.length < ids.length)) {
      const left = gone.reduce(withoutWindow, tree);
      const laid = splitLayout(left, AREA);
      for (const id of ids.filter((other) => !gone.includes(other))) assert.deepEqual(laid.rects[id], whole.rects[id], `${name}: ${id} after ${gone}`);
      for (const id of gone) assert.ok(laid.gaps.some(({ rect }) => rect.x <= whole.rects[id].x && rect.y <= whole.rects[id].y && rect.x + rect.w >= whole.rects[id].x + whole.rects[id].w && rect.y + rect.h >= whole.rects[id].y + whole.rects[id].h), `${name}: ${id}'s cell is a place to drop`);
      assert.deepEqual(parseSplit(JSON.stringify(left)), left, `${name}: a reload keeps it`);
    }
    assert.equal(ids.reduce(withoutWindow, tree), null, 'the last one ends the split');
  }
  const tree = SHAPES.pair;
  assert.equal(withoutWindow(tree, 'nobody'), tree);
});

test('a window comes back into the cell it left, and the rest stay', async (t) => {
  const { leavesOf, splitLayout, withinGap, withoutWindow } = await model(t);
  for (const [name, tree] of Object.entries(SHAPES)) {
    const whole = splitLayout(tree, AREA);
    for (const id of leavesOf(tree)) {
      const left = withoutWindow(tree, id);
      const cell = splitLayout(left, AREA).gaps.find(({ rect }) => rect.x <= whole.rects[id].x + 1 && rect.y <= whole.rects[id].y + 1 && rect.x + rect.w >= whole.rects[id].x + whole.rects[id].w);
      const { x, y, w, h } = cell.rect;
      const back = splitLayout(withinGap(left, cell, { x: x + w / 2, y: y + h / 2 }, 'z'), AREA);
      assert.deepEqual(back.rects.z, whole.rects[id], `${name}: ${id}`);
      for (const other of leavesOf(tree).filter((one) => one !== id)) assert.deepEqual(back.rects[other], whole.rects[other], `${name}: ${other} stays`);
    }
  }
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

test('a line moves with a window away, and that window keeps its place', async (t) => {
  const { keepingWindows, movedLine, leavesOf, splitLayout } = await model(t);
  const saved = { axis: 'x', at: 0.5, a: { id: 'a' }, b: { axis: 'y', at: 0.5, a: { id: 'b' }, b: pair('x', 'c', 'd') } };
  const live = keepingWindows(saved, new Set(['a', 'b', 'c']));
  const inner = splitLayout(live, AREA).lines.find((line) => line.path.length === 2);
  const moved = movedLine(saved, inner.path, inner.axis, { x: 1200, y: 240 }, inner.area);
  assert.equal(moved.b.b.at, 0.5);
  assert.deepEqual(leavesOf(moved), ['a', 'b', 'c', 'd']);
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

const gap = { gap: true };
const room = { x: 0, y: 100, w: 1000, h: 800 };

test('windows snapped alone keep their kind of snap, and what nobody took stays a gap', async (t) => {
  const { roomTree, splitLayout } = await model(t);
  assert.deepEqual(roomTree([['a', 'left'], ['b', 'right']]), pair('x', 'a', 'b'));
  const half = roomTree([['a', 'left'], ['b', 'top-right']]);
  assert.deepEqual(half, { axis: 'x', at: 0.5, a: { id: 'a' }, b: { axis: 'y', at: 0.5, a: { id: 'b' }, b: gap } });
  assert.deepEqual(splitLayout(half, room).gaps.map((g) => g.rect), [{ x: 500, y: 500, w: 500, h: 400 }]);
  const corner = roomTree([['a', 'top-left'], ['b', 'right']]);
  assert.deepEqual(corner, { axis: 'x', at: 0.5, a: { axis: 'y', at: 0.5, a: { id: 'a' }, b: gap }, b: { id: 'b' } });
  assert.deepEqual(splitLayout(corner, room).rects.a, { x: 0, y: 100, w: 500, h: 400 }, 'the corner stays a corner');
  assert.deepEqual(roomTree([['a', 'top-left'], ['b', 'top-right']]), { x: 0.5, y: 0.5, cells: [{ id: 'a' }, { id: 'b' }, gap, gap] });
  assert.deepEqual(roomTree([['a', 'top-left'], ['b', 'bottom-right']]), { x: 0.5, y: 0.5, cells: [{ id: 'a' }, gap, gap, { id: 'b' }] });
  assert.deepEqual(roomTree([['a', 'bottom'], ['b', 'top-right']]), { axis: 'y', at: 0.5, a: { axis: 'x', at: 0.5, a: gap, b: { id: 'b' } }, b: { id: 'a' } });
  assert.equal(roomTree([['a', 'left'], ['b', 'bottom']]), null, 'two want one cell');
  assert.equal(roomTree([['a', 'left'], ['b', 'top-left']]), null);
});

test('a window fills a gap, both gaps of a side, or a corner of an empty half', async (t) => {
  const { roomTree, splitLayout, withinGap, leavesOf, rectZone } = await model(t);
  const half = roomTree([['a', 'left'], ['b', 'top-right']]);
  const [hole] = splitLayout(half, room).gaps;
  const filled = withinGap(half, hole, { x: 900, y: 850 }, 'c', 'bottom-right');
  assert.deepEqual(filled.b.b, { id: 'c' });
  assert.deepEqual(leavesOf(filled), ['a', 'b', 'c']);

  const row = roomTree([['a', 'top-left'], ['b', 'top-right']]);
  const [low] = splitLayout(row, room).gaps;
  assert.deepEqual(withinGap(row, low, { x: 400, y: 880 }, 'c', 'bottom'), { axis: 'y', at: 0.5, a: pair('x', 'a', 'b'), b: { id: 'c' } });
  assert.deepEqual(withinGap(row, low, { x: 100, y: 880 }, 'c', 'bottom-left').cells, [{ id: 'a' }, { id: 'b' }, { id: 'c' }, gap]);

  const emptyHalf = { axis: 'y', at: 0.5, a: pair('x', 'a', 'b'), b: gap };
  const [wide] = splitLayout(emptyHalf, room).gaps;
  assert.deepEqual(withinGap(emptyHalf, wide, { x: 950, y: 880 }, 'c', 'bottom-right'), { x: 0.5, y: 0.5, cells: [{ id: 'a' }, { id: 'b' }, gap, { id: 'c' }] });
  assert.deepEqual(withinGap(emptyHalf, wide, { x: 500, y: 700 }, 'c').b, { id: 'c' });

  assert.equal(rectZone({ x: 500, y: 500, w: 500, h: 400 }, room), 'bottom-right');
  assert.equal(rectZone({ x: 0, y: 100, w: 500, h: 800 }, room), 'left');
  assert.equal(rectZone({ x: 0, y: 500, w: 1000, h: 400 }, room), 'bottom');
});

test('empty cells fold into one where a whole part is empty, and survive a reload', async (t) => {
  const { roomTree, withoutWindow, parseSplit } = await model(t);
  const half = roomTree([['a', 'left'], ['b', 'top-right']]);
  assert.deepEqual(withoutWindow(half, 'b'), { ...half, b: gap });
  assert.equal(withoutWindow(withoutWindow(half, 'b'), 'a'), null);
  const three = { x: 0.5, y: 0.5, cells: [{ id: 'a' }, { id: 'b' }, { id: 'c' }, gap] };
  assert.deepEqual(parseSplit(JSON.stringify(three)), three, 'a gap survives a reload');
  assert.deepEqual(parseSplit(JSON.stringify(half.b)), half.b, 'one window can keep its cell');
  assert.equal(parseSplit(JSON.stringify({ axis: 'x', at: 0.5, a: gap, b: gap })), null, 'no window is no split');
});

test('windows stay put when a pushed line unlocks', async (t) => {
  const { splitLayout, unlockedLine, movedEdge, withoutWindow } = await model(t);
  const pushed = pair('x', 'a', 'b', 0.05);
  const shown = splitLayout(pushed, room).rects;
  assert.deepEqual(splitLayout(unlockedLine(pushed, [], 'x', room), room).rects, shown, 'a line pushed past a minimum');

  const quad = { x: 0.5, y: 0.5, cells: [{ id: 'a' }, { id: 'b' }, { id: 'c' }, { id: 'd' }] };
  const open = unlockedLine(quad, [], 'x', room);
  const live = splitLayout(withoutWindow(open, 'd'), room);
  const moved = movedEdge(open, [], live.edges.a.e, { x: 400, y: 200 });
  assert.equal(moved.x, 0.4, 'an edge moves the quad with a cell empty');
  assert.equal(moved.at, undefined);
});

test('an edge brought back locks its line where the other side stands, with the other lines as they are', async (t) => {
  const { edgeMeets, lockedEdge, movedEdge, parseSplit, splitLayout, unlockedLine } = await model(t);
  const two = unlockedLine(pair('x', 'a', 'b'), [], 'x', room);
  const edges = splitLayout(two, room).edges;
  const spaced = movedEdge(movedEdge(two, [], edges.b.w, { x: 600, y: 300 }), [], edges.a.e, { x: 400, y: 300 });
  const { a: { e }, b: { w } } = splitLayout(spaced, room).edges;
  assert.equal(e.partner, 600);
  assert.equal(w.partner, 400);
  assert.ok(edgeMeets(e, { x: 560, y: 300 }, 40) && edgeMeets(e, { x: 640, y: 300 }, 40), 'short of it or past it');
  assert.ok(!edgeMeets(e, { x: 559, y: 300 }, 40));

  const relocked = lockedEdge(spaced, [], e);
  assert.deepEqual(relocked, pair('x', 'a', 'b', 0.6), 'the dragged edge goes to the other side');
  assert.equal(splitLayout(relocked, room).lines.length, 1);
  assert.deepEqual(lockedEdge(spaced, [], w), pair('x', 'a', 'b', 0.4));
  assert.deepEqual(parseSplit(JSON.stringify(relocked)), relocked, 'it survives a reload');

  const open = unlockedLine(unlockedLine(quad(), [], 'x', room), [], 'y', room);
  const low = movedEdge(open, [], splitLayout(open, room).edges.c.n, { x: 100, y: 650 });
  const { c, a } = splitLayout(low, room).edges;
  assert.equal(a.s.partner, 650);
  const crossed = lockedEdge(low, [], c.e);
  assert.equal(crossed.freeX, undefined);
  assert.equal(crossed.freeY, low.freeY, 'the quad\u2019s other line stays unlocked');
  assert.deepEqual(splitLayout(crossed, room).lines.map((line) => line.axis), ['x']);
  assert.deepEqual(splitLayout(lockedEdge(crossed, [], a.s), room).crossings.length, 1, 'both locked is a crossing again');

  const nested = { axis: 'x', at: 0.5, a: pair('y', 'a', 'b'), b: { id: 'c' } };
  const inner = unlockedLine(unlockedLine(nested, [0], 'y', { ...room, w: 500 }), [], 'x', room);
  const deep = splitLayout(inner, room).edges;
  assert.deepEqual(lockedEdge(inner, deep.a.s.path, deep.a.s).a, pair('y', 'a', 'b'), 'a nested line locks alone');
  assert.equal(lockedEdge(inner, deep.a.s.path, deep.a.s).free, inner.free);
  assert.deepEqual(lockedEdge(inner, deep.a.e.path, deep.a.e).a, inner.a, 'as does the line around it');
});

test('detach unlocks one line: the windows stay put, and each side has its own edge', async (t) => {
  const { splitEdges, splitLayout, movedEdge, unlockedLine, parseSplit, withoutWindow } = await model(t);
  const two = unlockedLine(pair('x', 'a', 'b'), [], 'x', room);
  const before = splitLayout(two, room);
  assert.deepEqual(before.rects, splitLayout(pair('x', 'a', 'b'), room).rects, 'nothing moves');
  assert.deepEqual(before.lines, [], 'and no line is left');
  assert.deepEqual(splitEdges(before.edges.a), ['e']);
  assert.deepEqual(splitEdges(before.edges.b), ['w']);

  const spaced = movedEdge(two, [], before.edges.a.e, { x: 400, y: 300 });
  assert.deepEqual(splitLayout(spaced, room).rects, { a: { x: 0, y: 100, w: 400, h: 800 }, b: { x: 500, y: 100, w: 500, h: 800 } }, 'a space opens');
  const overlapped = movedEdge(two, [], before.edges.b.w, { x: 400, y: 300 });
  assert.equal(splitLayout(overlapped, room).rects.b.x, 400);
  assert.equal(splitLayout(overlapped, room).rects.a.w, 500, 'an overlap, and the other side stays');
  assert.deepEqual(parseSplit(JSON.stringify(spaced)), spaced, 'it survives a reload');

  const nested = unlockedLine({ axis: 'x', at: 0.5, a: { id: 'a' }, b: pair('y', 'b', 'c') }, [], 'x', room);
  const layout = splitLayout(nested, room);
  assert.deepEqual(layout.lines.map((line) => line.axis), ['y'], 'the nested line stays shared');
  assert.deepEqual(splitEdges(layout.edges.b), ['w']);

  const quad = { x: 0.5, y: 0.5, cells: [{ id: 'a' }, { id: 'b' }, { id: 'c' }, { id: 'd' }] };
  const one = splitLayout(unlockedLine(quad, [], 'x', room), room);
  assert.deepEqual(one.lines.map((line) => line.axis), ['y']);
  assert.deepEqual(one.crossings, [], 'no line crosses it now');
  assert.deepEqual(splitEdges(one.edges.a), ['e']);
  assert.deepEqual(splitEdges(one.edges.d), ['w']);
  const both = splitLayout(unlockedLine(unlockedLine(quad, [], 'x', room), [], 'y', room), room);
  assert.deepEqual(splitEdges(both.edges.a), ['s', 'e', 'se']);
  const after = withoutWindow(unlockedLine(quad, [], 'x', room), 'd');
  assert.equal(after.freeX, 0.5, 'a cell left empty keeps a line unlocked');
});
