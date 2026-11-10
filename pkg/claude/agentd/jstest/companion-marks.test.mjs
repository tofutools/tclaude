// Tests for what decides where a mark goes (tcl-s97jr5). Drawing is checked in
// a real browser; this covers the choices that keep a mark honest.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function load(t) {
  const harness = await createPreactHarness(t);
  return { harness, ...(await harness.importDashboardModule('js/companion-marks.js')) };
}

const emptyDoc = { querySelectorAll: () => [], querySelector: () => null };
const origin = { x: 0, y: 0 };

test('a region follows the page scroll', async (t) => {
  const { regionRect } = await load(t);
  const rect = regionRect({ x: 10, y: 100, width: 50, height: 20 }, { x: 0, y: 40 }, { x: 0, y: 90 });
  assert.equal(rect.top, 50);
  assert.equal(rect.bottom, 70);
});

test('a mark kept to a page shows on it and below it only', async (t) => {
  const { markVisibleHere } = await load(t);
  const mark = { spec: { path: '/config' } };
  assert.equal(markVisibleHere(mark, { pathname: '/config' }), true);
  assert.equal(markVisibleHere(mark, { pathname: '/config/steward' }), true);
  assert.equal(markVisibleHere(mark, { pathname: '/configure' }), false);
  assert.equal(markVisibleHere({ spec: {} }, { pathname: '/anything' }), true);
});

test('a label needs no anchor and a stroke is measured', async (t) => {
  const { resolveMark, strokeBounds, strokePath } = await load(t);
  assert.deepEqual(resolveMark({ kind: 'label', spec: {} }, emptyDoc, origin, origin), {});
  assert.deepEqual(strokeBounds([[1, 2], [5, 9]]), { x: 1, y: 2, width: 4, height: 7 });
  assert.equal(strokePath([[1, 2], [3, 4]], 1, -1), 'M2.0,1.0 L4.0,3.0');
});

test('a missing target says so instead of drawing', async (t) => {
  const { resolveMark } = await load(t);
  assert.deepEqual(resolveMark({ kind: 'highlight', spec: { ref: '#gone' } }, emptyDoc, origin, origin), { why: 'target not on this view' });
  const broken = { querySelector: () => { throw new Error('bad selector'); }, querySelectorAll: () => [] };
  assert.deepEqual(resolveMark({ kind: 'box', spec: { ref: ':::' } }, broken, origin, origin), { why: 'target not on this view' });
});

function button(text, rect) {
  return {
    textContent: text, getAttribute: () => null, closest: () => null, matches: () => true,
    getBoundingClientRect: () => rect,
  };
}

test('a mark never lands on an approval control', async (t) => {
  const { resolveMark } = await load(t);
  const approve = button('Approve', { left: 0, top: 0, right: 100, bottom: 40, width: 100, height: 40 });
  const doc = { querySelector: () => approve, querySelectorAll: () => [approve] };
  assert.deepEqual(resolveMark({ kind: 'highlight', spec: { ref: '#a' } }, doc, origin, origin), { why: 'target is an approval control' });
  const region = { kind: 'box', spec: { region: { x: 50, y: 10, width: 10, height: 10 } } };
  assert.deepEqual(resolveMark(region, doc, origin, origin), { why: 'covers an approval control' });
  const card = { getAttribute: () => null, closest: () => null, matches: () => false, textContent: 'Deploy?', getBoundingClientRect: () => ({ left: 0, top: 0, right: 300, bottom: 200, width: 300, height: 200 }) };
  const wrapper = { querySelector: () => card, querySelectorAll: () => [approve] };
  assert.deepEqual(resolveMark({ kind: 'highlight', spec: { ref: '.card' } }, wrapper, origin, origin), { why: 'covers an approval control' }, 'nor on what holds one');
  const clear = { kind: 'box', spec: { region: { x: 500, y: 10, width: 10, height: 10 } } };
  assert.ok(resolveMark(clear, doc, origin, origin).rect);
});

test('rectsOverlap and marksKey', async (t) => {
  const { rectsOverlap, marksKey } = await load(t);
  const a = { left: 0, top: 0, right: 10, bottom: 10 };
  assert.equal(rectsOverlap(a, { left: 5, top: 5, right: 15, bottom: 15 }), true);
  assert.equal(rectsOverlap(a, { left: 10, top: 0, right: 15, bottom: 5 }), false);
  assert.notEqual(marksKey([{ id: 1, expires_at: 'a' }]), marksKey([{ id: 1, expires_at: 'b' }]));
  assert.equal(marksKey(null), '');
});

test('every mark can be ended and answered at the mark, and clicks pass the drawing', async (t) => {
  const { harness, MarksLayer } = await load(t);
  const win = globalThis.window;
  win.requestAnimationFrame = (fn) => setTimeout(fn, 5);
  win.cancelAnimationFrame = (id) => clearTimeout(id);
  const ended = [];
  const replies = [];
  const reports = [];
  const mark = { id: 7, kind: 'label', agent_id: 'agt_1', agent_name: 'steward', spec: {}, tip: 'Look here' };
  const view = await harness.mount(harness.html`<${MarksLayer} marks=${[mark]} win=${win}
    onEnd=${(m, how) => ended.push([m.id, how])} onDismissAll=${() => ended.push('all')}
    onReply=${async (m, text) => { replies.push([m.id, text]); return true; }} onReport=${(r) => reports.push(r)} />`);
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 30)));
  const tip = view.container.querySelector('.companion-mark-tip');
  assert.match(tip.textContent, /Look here/);
  assert.deepEqual(reports.at(-1), [{ id: 7, drawn: true }]);
  await harness.act(() => tip.querySelector('[aria-label="Acknowledge"]').click());
  await harness.act(() => tip.querySelector('[aria-label="Dismiss"]').click());
  await harness.act(() => view.container.querySelector('.companion-marks-clear').click());
  assert.deepEqual(ended, [[7, 'acknowledge'], [7, 'dismiss'], 'all']);
  await harness.act(() => tip.querySelector('.companion-mark-reply').click());
  assert.ok(tip.querySelector('textarea'));
});

test('a tip stays off an approval control', async (t) => {
  const { placeTips, rectsOverlap: overlap, tipHeight } = await load(t);
  const rect = { left: 300, top: 100, right: 400, bottom: 130, width: 100, height: 30 };
  const approve = { left: 280, top: 120, right: 640, bottom: 400 };
  const mark = { id: 1, tip: 'near it' };
  const [place] = placeTips([{ mark, rect }], 1400, 850, [approve]);
  assert.equal(overlap({ left: place.left, right: place.left + 300, top: place.top, bottom: place.top + tipHeight(mark) }, approve), false);
});

test('tips never cover each other', async (t) => {
  const { placeTips, rectsOverlap: overlap, tipHeight } = await load(t);
  const rect = { left: 300, top: 100, right: 400, bottom: 130, width: 100, height: 30 };
  const items = [1, 2, 3].map((id) => ({ mark: { id, tip: 'x'.repeat(100) }, rect }));
  items.push({ mark: { id: 4, tip: 'y' }, rect: null }, { mark: { id: 5, tip: 'y' }, rect: null });
  const boxes = placeTips(items, 1400, 850).map((place, i) => ({
    left: place.left, right: place.left + 300, top: place.top, bottom: place.top + tipHeight(items[i].mark),
  }));
  for (const [i, a] of boxes.entries()) {
    assert.ok(a.top >= 0 && a.bottom <= 850, 'stays on screen');
    for (const b of boxes.slice(i + 1)) assert.equal(overlap(a, b), false);
  }
});

test('a tip stays off the companion\'s floating controls', async (t) => {
  const { placeTips, rectsOverlap: overlap, tipHeight, toolRects } = await load(t);
  const tools = { left: 900, right: 1100, top: 300, bottom: 330, width: 200, height: 30 };
  const doc = { querySelectorAll: (selector) => (/companion-floating/.test(selector) ? [{ getBoundingClientRect: () => tools }] : []) };
  const mark = { id: 'm1', tip: 'look here' };
  const rect = { left: 980, right: 1000, top: 250, bottom: 290, width: 20, height: 40 };
  const [place] = placeTips([{ mark, rect }], 1400, 850, toolRects(doc));
  const box = { left: place.left, right: place.left + 300, top: place.top, bottom: place.top + tipHeight(mark) };
  assert.equal(overlap(box, tools), false);
});

