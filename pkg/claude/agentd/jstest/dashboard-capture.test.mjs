// Tests for the dashboard capture cycle (tcl-7i8b1t). Rendering itself needs a
// real browser; what can run here is what picks the context and answers the
// daemon.

import test from 'node:test';
import assert from 'node:assert/strict';
import { captureContext, cssUrls, describeElement, rewriteCss, startCapturePresence } from '../dashboard/js/dashboard-capture.js';

function element(tag, { attrs = {}, text = '', parent = null, classes = [] } = {}) {
  return {
    tagName: tag.toUpperCase(), id: attrs.id || '', innerText: text, textContent: text, parentElement: parent,
    classList: { contains: (name) => classes.includes(name) },
    getAttribute: (name) => attrs[name] ?? null,
    closest(selector) { return selector.includes(tag) ? this : (selector === '[data-element]' && attrs['data-element'] ? this : null); },
  };
}

test('a field is named but its value never is', () => {
  const field = element('input', { attrs: { 'aria-label': 'Token' }, text: 'ignored' });
  assert.deepEqual(describeElement(field), { tag: 'input', role: '', label: 'Token', text: '', id: '', ref: '', board_element: '' });
});

test('a board element under the cursor is named', () => {
  const shape = element('rect', { attrs: { 'data-element': 'abc' } });
  assert.equal(describeElement(shape).board_element, 'abc');
});

test('the context carries the cursor, focus and viewport', () => {
  const button = element('button', { text: 'Save  draft' });
  const win = {
    innerWidth: 1000, innerHeight: 700, devicePixelRatio: 2, location: { pathname: '/config', search: '?q=1' },
    getSelection: () => ({ toString: () => 'picked' }),
    document: {
      title: 'tclaude', body: {}, documentElement: {}, activeElement: button, hasFocus: () => true,
      elementFromPoint: () => button, querySelector: () => null,
    },
  };
  const context = captureContext({ win, tab: 'config', pointer: { x: 5.4, y: 6.6, inside: true, at: 900 }, now: 1000 });
  assert.equal(context.path, '/config?q=1');
  assert.equal(context.focused, true);
  assert.deepEqual(context.cursor, { x: 5, y: 7, inside: true, age_ms: 100, under: describeElement(button) });
  assert.equal(context.focus.text, 'Save draft');
  assert.equal(context.selection, 'picked');
  assert.equal(context.board, undefined);
});

test('a cursor that left the page is not looked up', () => {
  const win = {
    innerWidth: 1, innerHeight: 1, location: {},
    document: { title: '', querySelector: () => null, elementFromPoint: () => assert.fail('no lookup') },
  };
  const context = captureContext({ win, tab: 'groups', pointer: { x: 1, y: 2, inside: false, at: 0 }, now: 5 });
  assert.equal(context.cursor.inside, false);
  assert.equal(context.cursor.under, undefined);
});

test('css helpers find what needs inlining and move :root rules to the clone', () => {
  assert.deepEqual(cssUrls('a{background:url(sprites/a.png)} b{background:url("data:x")} c{fill:url(#g)} d{background:url(\'/b.png\')}'), ['sprites/a.png', '/b.png']);
  assert.equal(rewriteCss(':root { --x: 1 } :root.dark a { color: red }'), 'html { --x: 1 } html.dark a { color: red }');
});

function presenceHarness(responses) {
  const listeners = new Map();
  const target = () => ({
    addEventListener: (name, fn) => listeners.set(name, fn), removeEventListener: (name) => listeners.delete(name),
  });
  const calls = [];
  const timers = [];
  const doc = { ...target(), visibilityState: 'visible', hasFocus: () => true, title: 't', querySelector: () => null, documentElement: target() };
  const win = {
    ...target(), document: doc, innerWidth: 10, innerHeight: 10, location: { pathname: '/' }, btoa: (s) => globalThis.btoa(s),
  };
  const fetchImpl = async (url, options) => {
    calls.push({ url, body: JSON.parse(options.body) });
    return { ok: true, json: async () => responses.shift() || {} };
  };
  return { win, doc, calls, timers, listeners, fetchImpl };
}

const settle = () => new Promise((resolve) => setTimeout(resolve, 5));

test('a tab reports presence, then shows the cue and hands in its render', async () => {
  const h = presenceHarness([{ capture: { id: 'c1', agent_name: 'steward' } }]);
  const notices = [];
  const stop = startCapturePresence({
    win: h.win, fetchImpl: h.fetchImpl, tab: () => 'groups', onCapture: (r) => notices.push(r.agent_name), randomId: () => 'tab-1',
    render: async () => new Blob(['png']),
    setTimeoutImpl: (fn, ms) => { if (ms < 100) fn(); else h.timers.push(ms); return 1; }, clearTimeoutImpl: () => {},
  });
  await settle();
  assert.deepEqual(h.calls[0], { url: '/api/companion/presence', body: { client: 'tab-1', focused: true, visible: true } });
  assert.deepEqual(notices, ['steward'], 'the operator is told every time');
  assert.equal(h.calls[1].url, '/api/companion/capture/c1');
  assert.equal(h.calls[1].body.png, globalThis.btoa('png'));
  assert.equal(h.calls[1].body.context.tab, 'groups');
  assert.deepEqual(h.timers, [1500], 'and goes back to pinging');
  stop();
  assert.equal(h.listeners.size, 0, 'stopping removes every listener');
});

test('a failed render is reported, not left to time out', async () => {
  const h = presenceHarness([{ capture: { id: 'c2' } }]);
  const stop = startCapturePresence({
    win: h.win, fetchImpl: h.fetchImpl, tab: () => '', randomId: () => 'tab-2',
    render: async () => { throw new Error('no canvas'); },
    setTimeoutImpl: (fn, ms) => { if (ms < 100) fn(); return 1; }, clearTimeoutImpl: () => {},
  });
  await settle();
  assert.deepEqual(h.calls[1].body, { error: 'no canvas' });
  stop();
});

test('a hidden tab says so and pings slowly', async () => {
  const h = presenceHarness([]);
  h.doc.visibilityState = 'hidden';
  h.doc.hasFocus = () => false;
  const stop = startCapturePresence({
    win: h.win, fetchImpl: h.fetchImpl, tab: () => '', randomId: () => 'tab-3',
    setTimeoutImpl: (fn, ms) => { h.timers.push(ms); return 1; }, clearTimeoutImpl: () => {},
  });
  await settle();
  assert.deepEqual(h.calls[0].body, { client: 'tab-3', focused: false, visible: false });
  assert.deepEqual(h.timers, [10000]);
  stop();
});
