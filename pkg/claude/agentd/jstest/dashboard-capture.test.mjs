// Tests for the dashboard capture cycle (tcl-7i8b1t). Rendering itself needs a
// real browser; what can run here is what picks the context and answers the
// daemon.

import test from 'node:test';
import assert from 'node:assert/strict';
import { snapshotPage } from '../dashboard/js/capture-snapshot.js';
import { buildNode, captureContext, cssUrls, describeElement, rewriteCss, startCapturePresence } from '../dashboard/js/dashboard-capture.js';
import { parseHTML } from './third_party/linkedom.mjs';

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

function maskedPage(body) {
  const { window } = parseHTML(`<!doctype html><html><body>${body}</body></html>`);
  window.getComputedStyle = () => ({ display: 'block', position: 'static' });
  return window;
}

function capturedHtml(win) {
  const clone = buildNode(snapshotPage(win).root, { doc: win.document, inline: async () => null, tasks: [] });
  return clone.outerHTML;
}

const SECRET = 'sk-live-s3cret-value';

test('a capture never holds what a masked field or display holds', () => {
  const win = maskedPage(`
    <input id="plain" data-capture-mask type="text" title="${SECRET}">
    <input id="shown" data-capture-mask type="password">
    <input id="password" type="password">
    <textarea id="bearer" data-capture-mask readonly>${SECRET}</textarea>
    <code id="once" data-capture-mask>${SECRET}<span>${SECRET}</span></code>
    <input id="empty" data-capture-mask type="text">
    <input id="open" type="text">`);
  const set = (id, value) => { win.document.getElementById(id).value = value; };
  set('plain', SECRET);
  set('shown', SECRET);
  set('password', SECRET);
  set('open', 'not a secret');
  // Toggled to visible by a show button: the marker is what keeps it masked.
  win.document.getElementById('shown').type = 'text';

  const html = capturedHtml(win);
  assert.ok(!html.includes(SECRET), html);
  assert.equal((html.match(/••••••••/g) || []).length, 5, 'each one that holds something says so');
  const page = maskedPage(html.replace(/^<body[^>]*>|<\/body>$/g, '')).document;
  assert.equal(page.getElementById('empty').getAttribute('value'), '', 'an empty field stays empty');
  assert.equal(page.getElementById('open').getAttribute('value'), 'not a secret', 'an ordinary field keeps its value');
  assert.equal(page.getElementById('password').getAttribute('type'), 'text');
});

test('the context carries no masked text, selected or under the cursor', () => {
  const win = maskedPage(`
    <textarea id="bearer" data-capture-mask readonly>${SECRET}</textarea>
    <label id="wrap"><code id="once" data-capture-mask>${SECRET}</code></label>
    <button id="ok">Save</button>`);
  const { document } = win;
  const once = document.getElementById('once');
  assert.equal(describeElement(once).text, '');
  const wrap = document.getElementById('wrap');
  wrap.setAttribute('title', SECRET);
  assert.equal(describeElement(once).label, '', 'a title can repeat the value');
  wrap.setAttribute('aria-label', 'Invite');
  assert.equal(describeElement(once).label, 'Invite', 'but a field keeps its name');
  assert.equal(describeElement(document.getElementById('wrap')).text, '', 'nor from around it');
  assert.equal(describeElement(document.getElementById('ok')).text, 'Save', 'ordinary text is still described');

  const selecting = (text, node) => ({ toString: () => text, containsNode: (el) => el === node });
  const context = (selection) => captureContext({
    win: { document, location: { pathname: '/' }, innerWidth: 1, innerHeight: 1, getSelection: () => selection }, tab: 'x',
  });
  assert.equal(context(selecting(SECRET, once)).selection, undefined, 'a selection over a masked display');
  assert.equal(context(selecting('words', null)).selection, 'words', 'ordinary text is still shared');
  Object.defineProperty(document, 'activeElement', { value: document.getElementById('bearer'), configurable: true });
  assert.equal(context(selecting(SECRET, null)).selection, undefined, 'a field that selected itself on focus');
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

const EMPTY_PAGE = { root: { attrs: [], children: [] }, form: null, formTitle: '' };
const settle = () => new Promise((resolve) => setTimeout(resolve, 5));

test('a tab asks rarely while the presence route is absent, and resumes when it answers', async () => {
  const h = presenceHarness([]);
  let status = 404;
  const pings = [];
  const stop = startCapturePresence({
    win: h.win, fetchImpl: async () => ({ ok: status === 200, status, json: async () => ({}) }), tab: () => 'groups', randomId: () => 'tab-1',
    setTimeoutImpl: (fn, ms) => { if (ms >= 1500) pings.push({ fn, ms }); return 1; }, clearTimeoutImpl: () => {},
  });
  await settle();
  assert.equal(pings.at(-1).ms, 60000, 'Collab off on this node: nothing to reach');
  status = 200;
  pings.at(-1).fn();
  await settle();
  assert.equal(pings.at(-1).ms, 1500);
  stop();
});

test('asked for form text, a tab renders it from the snapshot it drew the image from', async () => {
  const h = presenceHarness([{ capture: { id: 'c3', form: true } }]);
  const drawn = [];
  const page = { root: { tag: 'body', attrs: [], children: [{ text: 'hello' }], seen: true, scroll: { x: 0, y: 0 } }, form: null, formTitle: '' };
  const stop = startCapturePresence({
    win: h.win, fetchImpl: h.fetchImpl, tab: () => 'groups', randomId: () => 'tab-3',
    render: async ({ snapshot }) => { drawn.push(snapshot); return new Blob(['png']); }, snapshot: () => page,
    setTimeoutImpl: (fn, ms) => { if (ms < 100) fn(); return 1; }, clearTimeoutImpl: () => {},
  });
  await settle();
  assert.deepEqual(drawn, [page]);
  assert.match(h.calls[1].body.form_text, /hello/);
  stop();
});

test('a tab reports presence, then shows the cue and hands in its render', async () => {
  const h = presenceHarness([{ capture: { id: 'c1', agent_name: 'steward' } }]);
  const notices = [];
  const stop = startCapturePresence({
    win: h.win, fetchImpl: h.fetchImpl, tab: () => 'groups', onCapture: (r) => notices.push(r.agent_name), randomId: () => 'tab-1',
    render: async () => new Blob(['png']), snapshot: () => EMPTY_PAGE,
    setTimeoutImpl: (fn, ms) => { if (ms < 100) fn(); else h.timers.push(ms); return 1; }, clearTimeoutImpl: () => {},
  });
  await settle();
  assert.deepEqual(h.calls[0], { url: '/api/companion/presence', body: { client: 'tab-1', focused: true, visible: true } });
  assert.deepEqual(notices, ['steward'], 'the operator is told every time');
  assert.equal(h.calls[1].url, '/api/companion/capture/c1');
  assert.equal(h.calls[1].body.png, globalThis.btoa('png'));
  assert.equal(h.calls[1].body.context.tab, 'groups');
  assert.equal(h.calls[1].body.form_text, undefined, 'no text unless it was asked for');
  assert.equal(h.timers.at(-1), 1500, 'and goes back to pinging');
  stop();
  assert.equal(h.listeners.size, 0, 'stopping removes every listener');
});

test('a failed render is reported, not left to time out', async () => {
  const h = presenceHarness([{ capture: { id: 'c2' } }]);
  const stop = startCapturePresence({
    win: h.win, fetchImpl: h.fetchImpl, tab: () => '', randomId: () => 'tab-2',
    render: async () => { throw new Error('no canvas'); }, snapshot: () => EMPTY_PAGE,
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
  assert.equal(h.timers.at(-1), 10000);
  stop();
});

test('a tab on a page without crypto.randomUUID still reports presence', async () => {
  const h = presenceHarness([]);
  h.win.crypto = { getRandomValues: (bytes) => bytes.fill(7) };
  const stop = startCapturePresence({
    win: h.win, fetchImpl: h.fetchImpl, tab: () => '', render: async () => new Blob(['png']),
    setTimeoutImpl: () => 1, clearTimeoutImpl: () => {},
  });
  await settle();
  assert.match(h.calls[0].body.client, /^07070707-0707-4707-8707-070707070707$/);
  stop();
});

test('a ping that never answers cannot end presence', async () => {
  const h = presenceHarness([]);
  const pending = [];
  let sent = 0;
  const stop = startCapturePresence({
    win: h.win, tab: () => '', randomId: () => 'tab-4',
    fetchImpl: () => { sent += 1; return new Promise(() => {}); },
    setTimeoutImpl: (fn, ms) => { pending.push({ fn, ms }); return pending.length; }, clearTimeoutImpl: () => {},
  });
  await settle();
  assert.equal(sent, 1);
  pending.find((t) => t.ms === 8000).fn();
  await settle();
  pending.find((t) => t.ms === 1500).fn();
  await settle();
  assert.equal(sent, 2, 'the loop goes on after the timeout');
  stop();
});

test('a render that never finishes cannot end presence', async () => {
  const h = presenceHarness([{ capture: { id: 'c9' } }]);
  const pending = [];
  const stop = startCapturePresence({
    win: h.win, fetchImpl: h.fetchImpl, tab: () => '', randomId: () => 'tab-5', render: () => new Promise(() => {}),
    setTimeoutImpl: (fn, ms) => { if (ms === 60) fn(); else pending.push({ fn, ms }); return 1; }, clearTimeoutImpl: () => {},
  });
  await settle();
  pending.find((t) => t.ms === 20000).fn();
  await settle();
  assert.ok(pending.some((t) => t.ms === 1500), 'it goes back to pinging');
  stop();
});
