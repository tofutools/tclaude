// Unit tests for the copy-paste fix helpers in dashboard/js/helpers.js:
//   - relTimeHTML     — builds a stable rel-time span with the coarse "Ns ago"
//                       text deliberately OMITTED (filled by the post-pass),
//   - refreshRelTimes — the post-pass that fills/advances those spans,
//   - setHTMLIfChanged — skip-if-unchanged innerHTML assignment.
//
// Run with Node's built-in test runner (`node --test`, asserting via
// `node:assert`) — no bundler/framework, importing the same raw ES module the
// browser loads, exactly like the sibling suites. The Go wrapper
// (palette_score_node_test.go) globs jstest/*.test.mjs, so this runs under
// `go test ./...` and skips when node is absent. Lives OUTSIDE dashboard/ so
// `//go:embed dashboard` doesn't ship it inside the agentd binary.
//
// These helpers reach for `document` only when called with no explicit root;
// every test passes an explicit element/scope, so no DOM globals are needed —
// tiny hand-rolled doubles stand in for the one or two element APIs used.

import test from 'node:test';
import assert from 'node:assert/strict';
import { relTime, relTimeHTML, refreshRelTimes, setHTMLIfChanged } from '../dashboard/js/helpers.js';

const ISO = '2020-01-01T00:00:00Z'; // safely in the past → a nonzero "…ago"

test('relTimeHTML keeps the coarse time OUT of the markup (stable across ticks)', () => {
  // The whole point: the "Ns ago" string never appears in the returned HTML,
  // so a section built from it stays byte-identical from one poll to the next.
  const h = relTimeHTML(ISO, 'last-hook');
  assert.match(h, /class="rel-time last-hook"/);
  assert.match(h, /data-ts="2020-01-01T00:00:00Z"/);
  assert.doesNotMatch(h, /ago/); // no coarse time baked in
  assert.doesNotMatch(h, /\d+[smhd]</);
});

test('relTimeHTML renders an empty span (no data-ts) for a missing timestamp', () => {
  // Matches relTime('') === '' — a missing timestamp stays blank, not "0s ago".
  assert.equal(relTimeHTML(''), '<span class="rel-time"></span>');
  assert.equal(relTimeHTML(null, 'muted'), '<span class="rel-time muted"></span>');
});

test('relTimeHTML adds an optional static title and escapes ts + title', () => {
  assert.match(relTimeHTML(ISO, 'last-hook', ISO), /title="2020-01-01T00:00:00Z"/);
  const h = relTimeHTML('a"><script>', 'c', 'x"y');
  assert.doesNotMatch(h, /<script>/); // ts is HTML-escaped, no breakout
  assert.match(h, /title="x&quot;y"/);
});

// --- refreshRelTimes -------------------------------------------------------

// A minimal span double: exposes getAttribute('data-ts') + a textContent
// setter that counts writes, so we can prove the post-pass mutates the text
// node ONLY when the coarse string actually changed (selection-preserving).
function fakeSpan(iso, text = '') {
  let tc = text, writes = 0;
  const el = { getAttribute: (a) => (a === 'data-ts' ? iso : null) };
  Object.defineProperty(el, 'textContent', {
    get: () => tc,
    set: (v) => { tc = v; writes++; },
  });
  Object.defineProperty(el, 'writes', { get: () => writes });
  return el;
}
const scopeOf = (spans) => ({ querySelectorAll: () => spans });

test('refreshRelTimes fills an empty span with the current relTime string', () => {
  const span = fakeSpan(ISO, '');
  refreshRelTimes(scopeOf([span]));
  assert.equal(span.textContent, relTime(ISO));
  assert.equal(span.writes, 1);
});

test('refreshRelTimes leaves an already-correct span untouched (no text-node churn)', () => {
  const span = fakeSpan(ISO, relTime(ISO)); // already current
  refreshRelTimes(scopeOf([span]));
  assert.equal(span.writes, 0); // no write → a selection inside would survive
});

// --- setHTMLIfChanged ------------------------------------------------------

// A minimal element double: an innerHTML setter counting assignments, plus a
// querySelectorAll so the internal refreshRelTimes call is a harmless no-op.
function fakeEl() {
  const el = { _html: null, assigns: 0, querySelectorAll: () => [] };
  Object.defineProperty(el, 'innerHTML', {
    get: () => el._html,
    set: (v) => { el._html = v; el.assigns++; },
  });
  return el;
}

test('setHTMLIfChanged assigns once, skips an identical string, re-assigns on change', () => {
  const el = fakeEl();
  assert.equal(setHTMLIfChanged(el, '<b>a</b>'), true);
  assert.equal(el.innerHTML, '<b>a</b>');
  assert.equal(el.assigns, 1);

  assert.equal(setHTMLIfChanged(el, '<b>a</b>'), false); // identical → skip
  assert.equal(el.assigns, 1); // no DOM touched → selection survives

  assert.equal(setHTMLIfChanged(el, '<b>b</b>'), true); // changed → assign
  assert.equal(el.assigns, 2);
});

test('setHTMLIfChanged caches per element, not globally', () => {
  const a = fakeEl(), b = fakeEl();
  setHTMLIfChanged(a, '<i>x</i>');
  // b has never seen this string, so it must still assign (independent cache).
  assert.equal(setHTMLIfChanged(b, '<i>x</i>'), true);
  assert.equal(b.assigns, 1);
});

test('setHTMLIfChanged tolerates a missing element', () => {
  assert.equal(setHTMLIfChanged(null, '<b>x</b>'), false);
});
