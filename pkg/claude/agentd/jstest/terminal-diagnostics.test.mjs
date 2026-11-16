// The terminal Diagnostics overlay's numbers (dashboard/js/terminal-diagnostics.js).

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  createDiagRecorder, droppedFrames, gapsOf, reportLines, serverStats, sparkline, summarize,
} from '../dashboard/js/terminal-diagnostics.js';

test('a summary is the count, p50, p95 and max; empty says nothing', () => {
  assert.deepEqual(summarize([]), { n: 0, p50: null, p95: null, max: null });
  const hundred = Array.from({ length: 100 }, (_, i) => 100 - i);
  assert.deepEqual(summarize(hundred), { n: 100, p50: 50, p95: 95, max: 100 });
  assert.deepEqual(summarize([7]), { n: 1, p50: 7, p95: 7, max: 7 });
  assert.deepEqual(gapsOf([0, 10, 15, 45]), [10, 5, 30]);
});

test('dropped frames are counted against the display\'s own interval, 60 Hz or 120 Hz', () => {
  assert.deepEqual(droppedFrames([16.7, 16.7, 16.7, 50, 16.7, 16.7, 16.7, 16.7, 16.7, 16.7]), { interval: 16.7, dropped: 2 });
  const fast = [8.3, 8.3, 8.3, 8.3, 8.3, 8.3, 8.3, 8.3, 8.3, 33.2];
  assert.equal(droppedFrames(fast).dropped, 3);
  assert.equal(droppedFrames([16]).dropped, 0, 'too few to say');
});

test('a sparkline scales to its tallest gap', () => {
  assert.equal(sparkline([1, 8]), '▁█');
  assert.equal(sparkline([0, 0]), '');
  assert.equal(sparkline(Array.from({ length: 50 }, () => 1), 40).length, 40);
});

test('frame rate, parse and render times, and scroll latency over the window', () => {
  let t = 0;
  const rec = createDiagRecorder({ now: () => t, windowMs: 5000 });
  t = 1000;
  // A scroll step; its frame arrives 120 ms later, parses in 4 ms, draws 30 ms after the step's arrival.
  rec.input(true);
  t = 1120; rec.message(2000); const parsed = rec.wrote();
  t = 1124; parsed();
  t = 1150; rec.rendered();
  // A key is not a scroll step, and a second frame with no step waiting adds no latency.
  t = 1200; rec.input(false);
  t = 1300; rec.message(500); rec.wrote()();
  t = 1316; rec.rendered();
  t = 2000;
  const s = rec.stats();
  assert.equal(s.windowS, 2, 'the window so far');
  assert.equal(s.messages.perS, 1, 'received/s');
  assert.equal(s.renders.perS, 1, 'rendered/s');
  assert.equal(s.messages.bytesPerS, 1250);
  assert.deepEqual(s.gaps, { n: 1, p50: 180, p95: 180, max: 180, spark: '█' });
  assert.equal(s.parse.max, 4);
  assert.deepEqual([s.writeToRender.p50, s.writeToRender.max], [16, 30]);
  assert.equal(s.scroll.stepsPerS, 0.5);
  assert.deepEqual(s.scroll.toArrival, { n: 1, p50: 120, p95: 120, max: 120 }, 'input → arrival');
  assert.deepEqual(s.scroll.toRender, { n: 1, p50: 150, p95: 150, max: 150 }, 'input → render');
  assert.equal(s.keysPerS, 0.5);
  // Past the window everything has rolled off.
  t = 9000;
  assert.equal(rec.stats().messages.perS, 0);
  assert.equal(rec.stats().scroll.toArrival.n, 0);
});

test('the server side: send gaps, write times and how soon output followed input', () => {
  const events = [
    { seq: 1, kind: 'in', at_ms: 1000, bytes: 9 },
    { seq: 2, kind: 'out', at_ms: 1040, bytes: 3000, write_ms: 0.5 },
    { seq: 3, kind: 'out', at_ms: 1090, bytes: 1000, write_ms: 12 },
    { seq: 4, kind: 'in', at_ms: 1100, bytes: 9 },
    { seq: 5, kind: 'out', at_ms: 1130, bytes: 1000, write_ms: 1 },
  ];
  const s = serverStats(events, 2000, 5000);
  assert.equal(s.framesPerS, 3 / 5);
  assert.deepEqual([s.gaps.p50, s.gaps.max], [40, 50]);
  assert.equal(s.writeMs.max, 12, 'a long write is the network pushing back');
  assert.deepEqual([s.turnaround.p50, s.turnaround.max], [30, 40]);
  assert.equal(serverStats(events, 1000000, 5000).framesPerS, 0, 'old events are out of the window');
});

test('the report leads with frame rate and latency, and says what it could not read', () => {
  let t = 0;
  const rec = createDiagRecorder({ now: () => t });
  t = 10; rec.input(true); t = 60; rec.message(10); rec.wrote()(); t = 70; rec.rendered();
  t = 1000;
  const lines = reportLines(rec.stats(), { renderer: 'DOM', dpr: 3, cols: 50, rows: 30, fontSize: 12, layout: 'phone layout', serverNote: 'not read for a remote terminal' });
  assert.match(lines[1], /^Frames: {3}received 1\.0\/s · rendered 1\.0\/s$/);
  assert.match(lines[3], /→ arrival p50 50 · p95 50 · max 50 ms/);
  assert.match(lines[4], /→ render {2}p50 60 · p95 60 · max 60 ms/);
  assert.ok(lines.includes('Server:   not read for a remote terminal'));
  assert.ok(lines.some((line) => /Renderer: DOM · dpr 3\.00 · 50×30 · font 12\.0 px/.test(line)));
});

test('a render before the frame is parsed is not its render, and what never renders is given up', () => {
  let t = 0;
  const rec = createDiagRecorder({ now: () => t });
  rec.input(true);
  t = 50; rec.message(10); const parsed = rec.wrote();
  t = 55; rec.rendered(); // a cursor blink: the frame is not parsed yet
  t = 60; parsed();
  t = 75; rec.rendered();
  assert.deepEqual(rec.stats().scroll.toRender, { n: 1, p50: 75, p95: 75, max: 75 });
  assert.equal(rec.stats().writeToRender.max, 25, 'from the write to the render after its parse');
  // A hidden terminal: parsed writes pile up with no render, and are dropped after a while.
  for (let i = 0; i < 5000; i += 1) { t += 1; rec.message(1); rec.wrote()(); }
  t += 10000; rec.rendered();
  assert.equal(rec.stats().writeToRender.n, 0, 'nothing waited that long');
});
