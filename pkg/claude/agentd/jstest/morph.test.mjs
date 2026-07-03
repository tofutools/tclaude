// Unit tests for the keyed DOM reconciler (dashboard/js/morph.js), run with
// Node's built-in test runner (`node --test`, asserting via `node:assert`).
// Node has no DOM, so this file ships a small, faithful mock DOM (just the node
// operations the reconciler uses) and drives morphChildren / morphNode / nodeKey
// against it. The Go wrapper `palette_score_node_test.go` (TestPaletteScore_JS)
// globs `jstest/*.test.mjs`, so this runs under `go test ./...` with no new
// wrapper and skips when node is absent. Lives OUTSIDE dashboard/ so
// `//go:embed dashboard` doesn't ship the test inside the agentd binary.
//
// The property that matters most for the copy-paste fix is NODE IDENTITY: a
// steady-state re-render must reuse the very same node objects (so a live text
// selection / focus anchored in them survives). The mock preserves object
// identity across moves the same way the real DOM does, so `===` assertions on
// kept nodes verify exactly that.

import test from 'node:test';
import assert from 'node:assert/strict';
import { nodeKey, morphNode, morphChildren, morphAttributes } from '../dashboard/js/morph.js';

// ---- Minimal mock DOM ----------------------------------------------------
// Node types match the real constants (1 element, 3 text, 8 comment).

class MockNode {
  constructor(nodeType) {
    this.nodeType = nodeType;
    this.parentNode = null;
    this.childNodes = [];
  }
  get firstChild() { return this.childNodes[0] || null; }
  get nextSibling() {
    const p = this.parentNode;
    if (!p) return null;
    const i = p.childNodes.indexOf(this);
    return p.childNodes[i + 1] || null;
  }
  _detach(child) {
    const i = this.childNodes.indexOf(child);
    if (i >= 0) this.childNodes.splice(i, 1);
    child.parentNode = null;
  }
  insertBefore(node, ref) {
    if (node.parentNode) node.parentNode._detach(node); // moves detach first
    if (ref == null) { this.childNodes.push(node); }
    else {
      const i = this.childNodes.indexOf(ref);
      this.childNodes.splice(i, 0, node);
    }
    node.parentNode = this;
    return node;
  }
  appendChild(node) { return this.insertBefore(node, null); }
  removeChild(node) { this._detach(node); return node; }
  replaceChild(newNode, oldNode) {
    if (newNode.parentNode) newNode.parentNode._detach(newNode);
    const i = this.childNodes.indexOf(oldNode);
    this.childNodes[i] = newNode;
    newNode.parentNode = this;
    oldNode.parentNode = null;
    return oldNode;
  }
}

class MockText extends MockNode {
  constructor(value, nodeType = 3) { super(nodeType); this.nodeValue = value; }
  get nodeName() { return this.nodeType === 8 ? '#comment' : '#text'; }
  cloneNode() { return new MockText(this.nodeValue, this.nodeType); }
  isEqualNode(o) { return o.nodeType === this.nodeType && o.nodeValue === this.nodeValue; }
}

class MockElement extends MockNode {
  constructor(tag) {
    super(1);
    this.tagName = tag.toUpperCase();
    this._attrs = new Map();
  }
  get nodeName() { return this.tagName; }
  get id() { return this._attrs.get('id') || ''; }
  get attributes() {
    return [...this._attrs.entries()].map(([name, value]) => ({ name, value }));
  }
  getAttribute(n) { return this._attrs.has(n) ? this._attrs.get(n) : null; }
  setAttribute(n, v) { this._attrs.set(n, String(v)); return this; }
  hasAttribute(n) { return this._attrs.has(n); }
  removeAttribute(n) { this._attrs.delete(n); }
  cloneNode(deep) {
    const c = new MockElement(this.tagName);
    for (const [k, v] of this._attrs) c._attrs.set(k, v);
    if (deep) for (const ch of this.childNodes) c.appendChild(ch.cloneNode(true));
    return c;
  }
  isEqualNode(o) {
    if (!o || o.nodeType !== 1 || o.tagName !== this.tagName) return false;
    if (this._attrs.size !== o._attrs.size) return false;
    for (const [k, v] of this._attrs) if (o._attrs.get(k) !== v) return false;
    if (this.childNodes.length !== o.childNodes.length) return false;
    for (let i = 0; i < this.childNodes.length; i++) {
      if (!this.childNodes[i].isEqualNode(o.childNodes[i])) return false;
    }
    return true;
  }
}

// ---- Tiny tree builders --------------------------------------------------
// el('tr', {'data-key': 'a'}, [text('hi')])
const el = (tag, attrs = {}, kids = []) => {
  const e = new MockElement(tag);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
  for (const k of kids) e.appendChild(k);
  return e;
};
const text = (v) => new MockText(v);
const parent = (kids) => el('div', {}, kids);
const keys = (p) => p.childNodes.map(nodeKey);
const tags = (p) => p.childNodes.map((c) => c.nodeType === 1 ? c.tagName : `#${c.nodeValue}`);

// ---- nodeKey -------------------------------------------------------------

test('nodeKey prefers id, then data-group-key, then data-key; else empty', () => {
  assert.equal(nodeKey(el('div', { id: 'x', 'data-key': 'y' })), 'x');
  assert.equal(nodeKey(el('details', { 'data-group-key': 'g' })), 'g');
  assert.equal(nodeKey(el('tr', { 'data-key': 'k' })), 'k');
  assert.equal(nodeKey(el('tr', {})), '');
  assert.equal(nodeKey(text('hi')), '');
});

// ---- steady-state text update preserves node identity --------------------

test('unchanged subtree is skipped entirely (same node objects, no rewrite)', () => {
  const from = parent([el('tr', { 'data-key': 'a' }, [el('td', {}, [text('30s ago')])])]);
  const to = parent([el('tr', { 'data-key': 'a' }, [el('td', {}, [text('30s ago')])])]);
  const keptTr = from.childNodes[0];
  const keptTextNode = keptTr.childNodes[0].childNodes[0];
  morphChildren(from, to);
  assert.equal(from.childNodes[0], keptTr, 'row node identity preserved');
  assert.equal(keptTr.childNodes[0].childNodes[0], keptTextNode, 'text node identity preserved');
  assert.equal(keptTextNode.nodeValue, '30s ago');
});

test('changed text is rewritten in place on the SAME text node (relTime churn)', () => {
  const from = parent([el('tr', { 'data-key': 'a' }, [el('td', {}, [text('30s ago')])])]);
  const to = parent([el('tr', { 'data-key': 'a' }, [el('td', {}, [text('32s ago')])])]);
  const keptTextNode = from.childNodes[0].childNodes[0].childNodes[0];
  morphChildren(from, to);
  assert.equal(from.childNodes[0].childNodes[0].childNodes[0], keptTextNode,
    'the "Ns ago" text node is the same object — selection over it survives');
  assert.equal(keptTextNode.nodeValue, '32s ago', 'value updated in place');
});

// ---- keyed reordering ----------------------------------------------------

test('keyed rows reorder by moving nodes (identity preserved), not rebuilding', () => {
  const rowA = el('tr', { 'data-key': 'a' }, [text('A')]);
  const rowB = el('tr', { 'data-key': 'b' }, [text('B')]);
  const from = parent([rowA, rowB]);
  const to = parent([
    el('tr', { 'data-key': 'b' }, [text('B')]),
    el('tr', { 'data-key': 'a' }, [text('A')]),
  ]);
  morphChildren(from, to);
  assert.deepEqual(keys(from), ['b', 'a'], 'order swapped');
  assert.equal(from.childNodes[0], rowB, 'row B is the SAME node, just moved');
  assert.equal(from.childNodes[1], rowA, 'row A is the SAME node, just moved');
});

test('keyed reorder survives interspersed whitespace text nodes', () => {
  // Renderers .join('') template literals, so real containers carry whitespace
  // text nodes between keyed siblings. The morph must reorder around them.
  const dA = el('details', { 'data-group-key': 'a' }, [text('A')]);
  const dB = el('details', { 'data-group-key': 'b' }, [text('B')]);
  const from = parent([text('\n  '), dA, text('\n  '), dB, text('\n  ')]);
  const to = parent([
    text('\n  '),
    el('details', { 'data-group-key': 'b' }, [text('B')]),
    text('\n  '),
    el('details', { 'data-group-key': 'a' }, [text('A')]),
    text('\n  '),
  ]);
  morphChildren(from, to);
  assert.deepEqual(keys(from), ['', 'b', '', 'a', ''], 'groups reordered around whitespace');
  assert.equal(from.childNodes[1], dB, 'group B kept its node identity');
  assert.equal(from.childNodes[3], dA, 'group A kept its node identity');
});

// ---- keyed insert / remove ----------------------------------------------

test('a new keyed row is inserted at its slot; kept rows keep identity', () => {
  const rowA = el('tr', { 'data-key': 'a' }, [text('A')]);
  const rowC = el('tr', { 'data-key': 'c' }, [text('C')]);
  const from = parent([rowA, rowC]);
  const to = parent([
    el('tr', { 'data-key': 'a' }, [text('A')]),
    el('tr', { 'data-key': 'b' }, [text('B')]),
    el('tr', { 'data-key': 'c' }, [text('C')]),
  ]);
  morphChildren(from, to);
  assert.deepEqual(keys(from), ['a', 'b', 'c']);
  assert.equal(from.childNodes[0], rowA);
  assert.equal(from.childNodes[2], rowC, 'C kept identity despite B inserted before it');
});

test('a removed keyed row is dropped; the rest keep identity', () => {
  const rowA = el('tr', { 'data-key': 'a' }, [text('A')]);
  const rowB = el('tr', { 'data-key': 'b' }, [text('B')]);
  const rowC = el('tr', { 'data-key': 'c' }, [text('C')]);
  const from = parent([rowA, rowB, rowC]);
  const to = parent([
    el('tr', { 'data-key': 'a' }, [text('A')]),
    el('tr', { 'data-key': 'c' }, [text('C')]),
  ]);
  morphChildren(from, to);
  assert.deepEqual(keys(from), ['a', 'c']);
  assert.equal(from.childNodes[0], rowA);
  assert.equal(from.childNodes[1], rowC);
});

// ---- unkeyed positional path --------------------------------------------

test('unkeyed children morph positionally and update in place', () => {
  const h = el('h3', {}, [text('Defaults')]);
  const from = parent([h, el('div', {}, [text('old')])]);
  const to = parent([el('h3', {}, [text('Defaults')]), el('div', {}, [text('new')])]);
  morphChildren(from, to);
  assert.equal(from.childNodes[0], h, 'unchanged <h3> kept its node');
  assert.equal(from.childNodes[1].childNodes[0].nodeValue, 'new');
});

test('an incompatible unkeyed node (different tag) is replaced', () => {
  const from = parent([el('span', {}, [text('x')])]);
  const to = parent([el('button', {}, [text('x')])]);
  morphChildren(from, to);
  assert.deepEqual(tags(from), ['BUTTON']);
});

// ---- <details open> preservation -----------------------------------------

test('morphAttributes preserves live <details open> against the fresh HTML', () => {
  // User opened the group live; the fresh render (rebuilt from the same
  // dashPref) also says open — but even if it disagreed, the live node wins.
  const liveOpen = el('details', { 'data-group-key': 'g', open: '' });
  const freshClosed = el('details', { 'data-group-key': 'g' }); // no open attr
  morphAttributes(liveOpen, freshClosed);
  assert.ok(liveOpen.hasAttribute('open'), 'live open is not stripped by the morph');
});

test('morphAttributes syncs non-open attributes (add, update, remove)', () => {
  const from = el('tr', { class: 'old', 'data-x': '1' });
  const to = el('tr', { class: 'new', 'data-y': '2' });
  morphAttributes(from, to);
  assert.equal(from.getAttribute('class'), 'new', 'updated');
  assert.equal(from.getAttribute('data-y'), '2', 'added');
  assert.equal(from.hasAttribute('data-x'), false, 'removed');
});

// ---- morphNode fast path -------------------------------------------------

test('morphNode leaves an isEqualNode subtree completely untouched', () => {
  const from = el('div', { class: 'c' }, [el('span', {}, [text('same')])]);
  const to = el('div', { class: 'c' }, [el('span', {}, [text('same')])]);
  const innerSpan = from.childNodes[0];
  morphNode(from, to);
  assert.equal(from.childNodes[0], innerSpan, 'no child was recreated');
});
