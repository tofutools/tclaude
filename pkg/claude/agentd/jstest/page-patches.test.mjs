// Tests for live patches (tcl-14z3fq): the probe in the frame takes commands
// only from its parent window with the nonce of its current load, and applies
// a patch with stage_patch.js; the parent admits the probe's answers by the
// schema, and keeps a load in step with the daemon's journal.
//
// The probe runs as the stage inlines it (the applier and the probe in one
// scope), in a vm context standing in for the frame: a linkedom document, a
// parent window that records what it is sent, and sibling windows.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';
import { parseHTML } from './third_party/linkedom.mjs';
import { createPreactHarness } from './preact-harness.mjs';

const NONCE = '0123456789abcdef0123456789abcdef';
const OTHER = 'fedcba9876543210fedcba9876543210';
const wait = (ms = 0) => new Promise((r) => setTimeout(r, ms));

async function probeSource() {
  const read = (name) => readFile(new URL(`../${name}`, import.meta.url), 'utf8');
  // As stage.go builds stageProbeTag.
  return `(function(){\n${await read('stage_patch.js')}\n${await read('stage_probe.js')}\n})();`;
}

// frame runs the probe in a fresh frame and returns its parts: send(data,
// source) delivers a message event, sent holds what the probe posted to its
// parent.
async function frame(body = '<h1>Pay</h1><button class="pay">Pay</button>') {
  const { document } = parseHTML(`<!doctype html><html><head></head><body>${body}</body></html>`);
  const sent = [];
  const parent = { postMessage: (data) => sent.push(JSON.parse(JSON.stringify(data))) };
  const listeners = {};
  const g = {
    document, parent, location: { pathname: '/', search: '' }, scrollX: 0, scrollY: 0, innerWidth: 800, innerHeight: 600,
    setTimeout, clearTimeout, console,
    addEventListener: (type, fn) => { (listeners[type] ||= []).push(fn); },
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
  };
  g.window = g;
  vm.createContext(g);
  vm.runInContext(await probeSource(), g);
  const send = (data, source = parent) => {
    for (const fn of listeners.message || []) fn({ source, data });
  };
  return { document, sent, send, g };
}

const fromParent = (rest) => ({ type: 'tclaude-parent', v: 1, nonce: NONCE, ...rest });

test('the probe takes a patch only from its parent, with the nonce of its load', async () => {
  const f = await frame();
  assert.deepEqual(f.sent.map((m) => m.kind), ['ready']);
  const patch = (id, rest = {}) => fromParent({ kind: 'patch', id, patch: { kind: 'dom', n: 1, selector: 'h1', op: 'set-text', value: 'Pay now' }, ...rest });

  f.send(patch('r1'));
  assert.equal(f.document.querySelector('h1').textContent, 'Pay', 'no hello yet: nothing is taken');

  f.send(fromParent({ kind: 'hello' }), {});
  f.send(fromParent({ kind: 'hello' }), f.g);
  assert.equal(f.sent.filter((m) => m.kind === 'hello').length, 0, 'a hello from a sibling frame, or from the page itself, is ignored');

  f.send(fromParent({ kind: 'hello' }));
  assert.equal(f.sent.filter((m) => m.kind === 'hello').length, 1);

  const sibling = { postMessage() {} };
  f.send(patch('r2'), sibling);
  assert.equal(f.document.querySelector('h1').textContent, 'Pay', 'a sibling frame cannot drive the probe');
  f.send(patch('r3', { nonce: OTHER }));
  assert.equal(f.document.querySelector('h1').textContent, 'Pay', 'a wrong nonce is ignored');
  f.send(patch('r4', { nonce: 'not-hex' }));
  f.send(patch('<bad>'));
  assert.equal(f.document.querySelector('h1').textContent, 'Pay');
  assert.equal(f.sent.filter((m) => m.kind === 'result').length, 0, 'none of them is answered');

  f.send(patch('r5'));
  assert.equal(f.document.querySelector('h1').textContent, 'Pay now');
  assert.deepEqual(f.sent.at(-1), { kind: 'result', id: 'r5', applied: 1, type: 'tclaude-probe', v: 1, nonce: NONCE });
});

test('the applier runs each kind of patch, and says when it cannot', async () => {
  const f = await frame('<ul><li>a</li><li>b</li></ul><p id="x">x</p>');
  f.send(fromParent({ kind: 'hello' }));
  const result = () => f.sent.at(-1);
  const run = (id, patch) => { f.send(fromParent({ kind: 'patch', id, patch })); return result(); };

  assert.equal(run('c1', { kind: 'css', n: 1, css: 'li { color: blue }' }).applied, 1);
  const style = f.document.querySelector('style[data-tclaude-patch="1"]');
  assert.equal(style.textContent, 'li { color: blue }');
  assert.equal(style.parentNode, f.document.documentElement, 'after everything else, so it wins');

  assert.equal(run('d1', { kind: 'dom', n: 2, selector: 'li', op: 'set-attr', name: 'data-x', value: 'y' }).applied, 2);
  assert.equal(f.document.querySelectorAll('li[data-x="y"]').length, 2);
  assert.equal(run('d2', { kind: 'dom', n: 3, selector: 'ul', op: 'insert-html', value: '<li>c</li>' }).applied, 1);
  assert.equal(f.document.querySelectorAll('li').length, 3);
  assert.equal(run('d3', { kind: 'dom', n: 4, selector: '#x', op: 'remove' }).applied, 1);
  assert.equal(f.document.querySelector('#x'), null);
  assert.equal(run('d4', { kind: 'dom', n: 5, selector: '#none', op: 'remove' }).applied, 0);

  const bad = run('d5', { kind: 'dom', n: 6, selector: 'li', op: 'outer' });
  assert.equal(bad.applied, 0);
  assert.ok(bad.error, 'an unknown op is an error, not a silent no-op');
  assert.ok(run('d6', { kind: 'script', n: 7 }).error);
});

test('an eval runs once and answers JSON, awaiting a promise', async () => {
  const f = await frame('<h1>Pay</h1>');
  f.send(fromParent({ kind: 'hello' }));
  f.send(fromParent({ kind: 'eval', id: 'e1', js: 'document.querySelector("h1").textContent' }));
  assert.equal(f.sent.at(-1).json, '"Pay"');
  f.send(fromParent({ kind: 'eval', id: 'e2', js: 'Promise.resolve({a: 1})' }));
  await wait();
  assert.equal(f.sent.at(-1).json, '{"a":1}');
  f.send(fromParent({ kind: 'eval', id: 'e3', js: 'throw new Error("nope")' }));
  assert.equal(f.sent.at(-1).error, 'nope');
  f.send(fromParent({ kind: 'eval', id: 'e4', js: 'var o = {}; o.o = o; o' }));
  assert.match(f.sent.at(-1).error, /no JSON form/);
  f.send(fromParent({ kind: 'eval', id: 'e5', js: '"x".repeat(20000)' }));
  assert.match(f.sent.at(-1).error, /the most is 16384/);
  f.send(fromParent({ kind: 'eval', id: 'e5b', js: '"é".repeat(9000)' }));
  assert.match(f.sent.at(-1).error, /bytes of JSON/, 'the cap counts UTF-8 bytes, as the daemon does');
  f.send(fromParent({ kind: 'eval', id: 'e6', js: 'undefined' }));
  assert.equal(f.sent.at(-1).json, 'null');
  f.send(fromParent({ kind: 'eval', id: 'e7', js: 'window.__ran = (window.__ran || 0) + 1' }), {});
  assert.equal(f.g.__ran, undefined, 'a sibling frame cannot run a script');
});

test('the parent admits patch and eval answers by the schema, and plans the replay', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/page-probe-model.js');
  const win = {};
  const msg = (rest) => ({ source: win, data: { type: 'tclaude-probe', v: 1, nonce: NONCE, kind: 'result', id: 'r1', ...rest } });

  assert.deepEqual(m.acceptProbeMessage(msg({ applied: 2 }), win, NONCE), { kind: 'result', id: 'r1', applied: 2 });
  assert.deepEqual(m.acceptProbeMessage(msg({ applied: 0, error: 'bad\nselector' }), win, NONCE),
    { kind: 'result', id: 'r1', applied: 0, error: 'bad selector' });
  assert.equal(m.acceptProbeMessage(msg({ applied: '2' }), win, NONCE), null);
  assert.equal(m.acceptProbeMessage(msg({ applied: -1 }), win, NONCE), null);
  assert.deepEqual(m.acceptProbeMessage(msg({ json: '{"a":1}' }), win, NONCE), { kind: 'result', id: 'r1', json: '{"a":1}' });
  assert.equal(m.acceptProbeMessage(msg({ json: 'x'.repeat(16 * 1024 + 1) }), win, NONCE), null, 'the eval cap');
  assert.equal(m.acceptProbeMessage(msg({ json: 1 }), win, NONCE), null);
  assert.deepEqual(m.acceptProbeMessage(msg({ error: 'nope' }), win, NONCE), { kind: 'result', id: 'r1', error: 'nope' });
  assert.equal(m.acceptProbeMessage(msg({ applied: 1 }), {}, NONCE), null, 'another window');
  assert.equal(m.acceptProbeMessage(msg({ applied: 1, nonce: OTHER }), win, NONCE), null, 'a stale load');

  const j = (...ns) => ns.map((n) => ({ n, kind: 'css', css: `/* ${n} */` }));
  assert.deepEqual(m.planPatchSync([], j(1, 2)), { reload: false, apply: j(1, 2) }, 'a fresh load replays the journal');
  assert.deepEqual(m.planPatchSync([1], j(1, 2)), { reload: false, apply: j(2) }, 'a new patch is applied alone');
  assert.deepEqual(m.planPatchSync([1, 2], j(2)), { reload: true, apply: [] }, 'a dropped patch reloads');
  assert.deepEqual(m.planPatchSync([], []), { reload: false, apply: [] });

  assert.equal(m.patchChip(null), null, 'no patch yet: no word');
  assert.deepEqual(m.patchChip({ seq: 3, patches: j(1, 3) }), { live: true, text: '⚡ live patch #3 · not in source' });
  assert.deepEqual(m.patchChip({ seq: 3, patches: [] }), { live: false, text: 'No live patches · source' });

  const relayed = m.probePatch(NONCE, 'r9', { n: 1, kind: 'css', css: 'a{}', at: 'x', replay: true, js: 'alert(1)' });
  assert.deepEqual(relayed.patch, { n: 1, kind: 'css', css: 'a{}' }, 'only a patch\'s own fields are relayed');
});
