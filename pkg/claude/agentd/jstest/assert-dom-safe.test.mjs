import test from 'node:test';
import assert from 'node:assert/strict';
import looseAssert from 'node:assert';
import { parseHTML } from './third_party/linkedom.mjs';
import { describeValue } from './assert-dom-safe.mjs';

const rows = Array.from({ length: 40 }, (_, i) => `<div class="w"><div class="mux-tab t${i}"><span class="mux-tab-label">t${i}</span><button>x</button></div></div>`).join('');
const { document } = parseHTML(`<!doctype html><html><body><div id="host">${rows}</div></body></html>`);
const node = document.querySelector('.mux-tab');

test('a failing equality on a DOM node names it, quickly, instead of walking the document', () => {
  const started = Date.now();
  assert.throws(() => assert.equal(node, null), (error) => {
    assert.equal(error.message, '<div.mux-tab.t0> should equal null');
    return true;
  });
  assert.throws(() => looseAssert.notStrictEqual(node, node), /should not equal/);
  assert.throws(() => assert.equal(node, null, 'the tab is gone'), { message: 'the tab is gone' });
  assert.ok(Date.now() - started < 1000, 'failures come back at once');
});

test('equalities that hold, and those without a node, behave as before', () => {
  assert.equal(node, node);
  assert.notEqual(node, null);
  assert.equal(document.querySelector('.missing'), null);
  looseAssert.equal(document.querySelector('.missing'), undefined, 'loose equality still treats null as undefined');
  assert.throws(() => assert.equal(1, 2), assert.AssertionError);
  assert.throws(() => assert.notEqual(null, null), assert.AssertionError);
  assert.equal(describeValue(document.getElementById('host')), '<div#host>');
});
