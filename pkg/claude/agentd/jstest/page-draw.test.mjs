// Tests for drawing on top of a live page (tcl-2dwbad): the parent's checks on
// what the probe in the frame sends (source, nonce, size, schema), and the
// draw layer — a box drawn over the frame is anchored by the probe at the
// frame's scroll, its anchors stay text, and Send posts the marks and note.
// The frame and its window are faked: the probe protocol is all the layer
// sees of them.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const wait = (ms = 0) => new Promise((r) => setTimeout(r, ms));
const NONCE = '0123456789abcdef0123456789abcdef';
const JSX_CASE = { pointerdown: 'PointerDown', pointermove: 'PointerMove', pointerup: 'PointerUp' };

test('the parent admits only well-formed probe messages from its own frame and load', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/page-probe-model.js');
  const frame = {};
  const msg = (data, source = frame) => ({ source, data });
  const probe = (rest) => ({ type: 'tclaude-probe', v: 1, nonce: NONCE, ...rest });
  const scroll = probe({ kind: 'scroll', x: 0, y: 120, vw: 800, vh: 600, docW: 800, docH: 3000, path: '/a' });

  assert.deepEqual(m.acceptProbeMessage(msg(scroll), frame, NONCE),
    { kind: 'scroll', x: 0, y: 120, vw: 800, vh: 600, docW: 800, docH: 3000, path: '/a' });
  assert.equal(m.acceptProbeMessage(msg(scroll, {}), frame, NONCE), null, 'another window');
  assert.equal(m.acceptProbeMessage(msg(scroll, null), frame, NONCE), null, 'no source');
  assert.equal(m.acceptProbeMessage(msg(scroll), null, NONCE), null, 'no frame');
  assert.equal(m.acceptProbeMessage(msg({ ...scroll, nonce: 'f'.repeat(32) }), frame, NONCE), null, 'a stale load');
  assert.equal(m.acceptProbeMessage(msg(scroll), frame, ''), null, 'before any hello');
  assert.equal(m.acceptProbeMessage(msg({ ...scroll, v: 2 }), frame, NONCE), null, 'another version');
  assert.equal(m.acceptProbeMessage(msg({ ...scroll, y: '120' }), frame, NONCE), null, 'a string for a number');
  assert.equal(m.acceptProbeMessage(msg({ ...scroll, y: Infinity }), frame, NONCE), null);
  assert.deepEqual(m.acceptProbeMessage(msg({ type: 'tclaude-probe', v: 1, kind: 'ready' }), frame, ''), { kind: 'ready' },
    'ready needs no nonce: it only asks for a hello');

  const result = (elements) => probe({ kind: 'result', id: 'r1', elements });
  const ok = m.acceptProbeMessage(msg(result([{ path: 'main > button', text: 'Pay', hits: 3, onclick: 'x' }])), frame, NONCE);
  assert.deepEqual(ok, { kind: 'result', id: 'r1', elements: [{ path: 'main > button', text: 'Pay', hits: 3 }] },
    'only the known fields survive, as fresh values');
  assert.equal(m.acceptProbeMessage(msg(result([{ path: 'a', text: { toString: () => '<b>' }, hits: 1 }])), frame, NONCE), null, 'text is a string');
  assert.equal(m.acceptProbeMessage(msg(result([{ path: 'a'.repeat(301), text: '', hits: 1 }])), frame, NONCE), null, 'path cap');
  assert.equal(m.acceptProbeMessage(msg(result(Array.from({ length: 51 }, () => ({ path: 'a', text: '', hits: 1 })))), frame, NONCE), null, 'element cap');
  assert.equal(m.acceptProbeMessage(msg(probe({ kind: 'result', id: '<x>', elements: [] })), frame, NONCE), null, 'request id charset');
  const big = probe({ kind: 'result', id: 'r2', count: 1, items: [{ path: 'p', rect: { x: 0, y: 0, w: 1, h: 1 }, value: 'x'.repeat(70 * 1024) }] });
  assert.equal(m.acceptProbeMessage(msg(big), frame, NONCE), null, 'the message size cap');
  const cyclic = probe({ kind: 'scroll' });
  cyclic.self = cyclic;
  assert.equal(m.acceptProbeMessage(msg(cyclic), frame, NONCE), null, 'a message without a JSON form');
  assert.equal(m.acceptProbeMessage(msg(probe({ kind: 'eval', code: '1' })), frame, NONCE), null, 'unknown kinds');
});

function fakeFrame() {
  const posted = [];
  const contentWindow = { postMessage: (data, target) => posted.push({ data, target }) };
  return { frameRef: { current: { contentWindow } }, contentWindow, posted };
}

function fakeWindow() {
  const listeners = new Set();
  return {
    addEventListener: (type, fn) => { if (type === 'message') listeners.add(fn); },
    removeEventListener: (type, fn) => listeners.delete(fn),
    say: (source, data) => { for (const fn of listeners) fn({ source, data }); },
  };
}

test('a box drawn over the page is anchored at the frame\'s scroll and sent as text', async (t) => {
  const harness = await createPreactHarness(t);
  const draw = await harness.importDashboardModule('js/page-draw.js');
  const { useState } = harness.hooks;
  const frame = fakeFrame();
  const win = fakeWindow();
  const sent = [];
  const fetchImpl = async (url, init) => {
    sent.push({ url, init });
    return { ok: true, status: 200, json: async () => ({ seq: 1, told_agent: true }) };
  };

  let api = null;
  function Host() {
    const probe = draw.usePageProbe(frame.frameRef, { win });
    const marks = draw.usePageMarks(probe);
    const [tool, setTool] = useState('rectangle');
    const [note, setNote] = useState('');
    api = { probe, marks, note };
    return harness.html`<div>
      <${draw.PageDrawBar} tool=${tool} setTool=${setTool} color="#e03131" setColor=${() => {}} marks=${marks.marks}
        onUndo=${marks.undo} onClear=${marks.clear} note=${note} setNote=${setNote} agentName="ui-worker" sending=${false}
        onSend=${() => draw.sendPageMarks('pg_00112233445566aa', draw.marksPayload(marks.marks, note, probe.scroll), { fetchImpl })} />
      <${draw.PageDrawLayer} active=${true} tool=${tool} color="#e03131" marks=${marks.marks} onMark=${marks.add} scroll=${probe.scroll} />
    </div>`;
  }
  const view = await harness.mount(harness.html`<${Host} />`);
  const say = (data, source = frame.contentWindow) => harness.act(() => win.say(source, data));

  // The probe's ready gets a hello with a fresh nonce, posted to any origin
  // (the frame's is opaque), and the hello's answer connects it.
  await say({ type: 'tclaude-probe', v: 1, kind: 'ready' });
  assert.equal(frame.posted.length, 1);
  const hello = frame.posted[0];
  assert.equal(hello.target, '*');
  assert.equal(hello.data.kind, 'hello');
  assert.match(hello.data.nonce, /^[0-9a-f]{32}$/);
  const nonce = hello.data.nonce;
  await say({ type: 'tclaude-probe', v: 1, kind: 'hello', nonce });
  assert.equal(api.probe.connected, true);

  // Scroll from another window, or with another load's nonce, is ignored.
  await say({ type: 'tclaude-probe', v: 1, nonce, kind: 'scroll', x: 0, y: 999, vw: 1, vh: 1, docW: 1, docH: 1 }, {});
  await say({ type: 'tclaude-probe', v: 1, nonce: 'a'.repeat(32), kind: 'scroll', x: 0, y: 999, vw: 1, vh: 1, docW: 1, docH: 1 });
  assert.equal(api.probe.scroll, null);
  await say({ type: 'tclaude-probe', v: 1, nonce, kind: 'scroll', x: 0, y: 100, vw: 800, vh: 600, docW: 800, docH: 2400, path: '/checkout' });
  assert.equal(api.probe.scroll.y, 100);

  const svg = view.container.querySelector('svg.page-draw-svg');
  const fire = (type, x, y) => harness.act(() => harness.fireEvent(svg, `on${type}` in svg ? type : JSX_CASE[type],
    { button: 0, pointerId: 1, clientX: x, clientY: y }));
  await fire('pointerdown', 20, 30);
  await fire('pointermove', 220, 130);
  await fire('pointerup', 220, 130);

  assert.equal(api.marks.marks.length, 1);
  const el = api.marks.marks[0].el;
  assert.deepEqual([el.type, el.x, el.y, el.width, el.height], ['rectangle', 20, 130, 200, 100], 'kept in page coordinates');
  assert.ok(svg.querySelector('rect'), 'drawn with the boards\' renderer');
  assert.equal(svg.querySelector('g').getAttribute('transform'), 'translate(0 -100)', 'shown at the frame\'s scroll');

  const ask = frame.posted.at(-1).data;
  assert.equal(ask.kind, 'elementsAt');
  assert.equal(ask.nonce, nonce);
  assert.ok(ask.points.every(([x, y]) => x >= 20 && x <= 220 && y >= 30 && y <= 130), 'asked in the frame\'s viewport');

  // An answer for an unknown request is dropped; the real one anchors the mark.
  const evil = '<img src=x onerror="globalThis.__pwned=1">Pay';
  await say({ type: 'tclaude-probe', v: 1, nonce, kind: 'result', id: 'zzz', elements: [{ path: 'x', text: 'no', hits: 1 }] });
  await say({ type: 'tclaude-probe', v: 1, nonce, kind: 'result', id: ask.id, elements: [{ path: 'main > button.pay', text: evil, hits: 12 }] });
  await harness.act(() => wait());
  assert.deepEqual(api.marks.marks[0].anchors, [{ path: 'main > button.pay', text: evil, hits: 12 }]);
  assert.equal(view.container.querySelector('img'), null, 'probe text is never markup');
  assert.equal(globalThis.__pwned, undefined);

  await harness.input(view.container.querySelector('input[aria-label="Note for the agent"]'), 'Make it blue');
  const send = [...view.container.querySelectorAll('button')].find((b) => b.textContent.includes('Send to agent'));
  assert.ok(send && !send.disabled);
  await harness.act(async () => { harness.fireEvent(send, 'click'); await wait(); });
  assert.equal(sent.length, 1);
  assert.equal(sent[0].url, '/api/pages/pg_00112233445566aa/marks');
  const body = JSON.parse(sent[0].init.body);
  assert.equal(body.note, 'Make it blue');
  assert.equal(body.path, '/checkout');
  assert.deepEqual(body.viewport, { w: 800, h: 600 });
  assert.deepEqual(body.scroll, { x: 0, y: 100 });
  assert.deepEqual(body.marks, [{ kind: 'box', color: '#e03131', points: [[20, 130], [220, 230]], anchors: [{ path: 'main > button.pay', text: evil, hits: 12 }] }]);

  // A reload (the probe's ready again) rotates the nonce; the old one stops working.
  await say({ type: 'tclaude-probe', v: 1, kind: 'ready' });
  const fresh = frame.posted.at(-1).data;
  assert.equal(fresh.kind, 'hello');
  assert.notEqual(fresh.nonce, nonce);
  await say({ type: 'tclaude-probe', v: 1, nonce, kind: 'scroll', x: 0, y: 5, vw: 800, vh: 600, docW: 800, docH: 2400 });
  assert.equal(api.probe.scroll.y, 100, 'the old load\'s nonce is refused');
  await view.unmount();
});

test('a pen stroke and a text mark become pen and text marks', async (t) => {
  const harness = await createPreactHarness(t);
  const draw = await harness.importDashboardModule('js/page-draw.js');
  const m = await harness.importDashboardModule('js/board-model.js');
  const pen = m.newElement('freedraw', { x: 10, y: 20, points: [[0, 0], [5, 5], [10, 0]], strokeColor: '#2f9e44' });
  assert.deepEqual(draw.elementToMark(pen), { kind: 'pen', color: '#2f9e44', points: [[10, 20], [15, 25], [20, 20]] });
  const text = m.newElement('text', { x: 3.4, y: 7.6, text: 'here' });
  assert.deepEqual(draw.elementToMark(text), { kind: 'text', color: '#1e1e1e', points: [[3, 8]], text: 'here' });
  assert.deepEqual(draw.PAGE_DRAW_TOOLS.map((tool) => tool.kind), ['pen', 'arrow', 'box', 'text']);
});
