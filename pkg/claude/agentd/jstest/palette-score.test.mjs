// Unit tests for the command palette's ranking logic
// (dashboard/js/palette-score.js), run with Node's BUILT-IN test runner —
// `node --test`, asserting via `node:assert`. No bundler, no framework, no
// package.json, no node_modules: the test imports the same raw ES module
// the browser loads. A Go wrapper (palette_score_js_test.go) runs this
// under `go test ./...` and skips when node is absent.
//
// This file lives OUTSIDE dashboard/ on purpose, so `//go:embed dashboard`
// doesn't ship the test inside the agentd binary.

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  scoreMatch, expandQuery, scoreCommand, rankCommands, SYNONYMS,
} from '../dashboard/js/palette-score.js';

// Fixture mirroring the real palette commands (label + keywords), in the
// SAME build order palette.js pushes them — so the stable-sort
// tie-break-by-build-order is exercised too. Verbs match the unified
// Hide/Focus presentation.
const COMMANDS = [
  { label: 'Hide all windows', keywords: 'hide unfocus all windows declutter detach panic minimize' },
  { label: 'Focus all windows', keywords: 'show all windows raise focus bring up' },
  { label: 'Pick windows to focus / hide…', keywords: 'windows subset choose select modal some' },
  { label: 'Spawn agent…', keywords: 'new agent create spawn launch start' },
  { label: 'Switch to slop theme', keywords: 'toggle switch theme slop regular vegas casino mode appearance' },
  { label: 'Go to Groups', keywords: 'tab navigate go open groups' },
  { label: 'Hide group: alpha', keywords: 'hide unfocus group windows alpha' },
  { label: 'Focus group: alpha', keywords: 'focus show group windows alpha' },
  { label: 'Focus window: worker-7', keywords: 'focus show jump bring up window agent worker-7' },
  { label: 'Hide window: worker-7', keywords: 'hide detach window agent worker-7' },
];

const labels = (q) => rankCommands(COMMANDS, q).map((c) => c.label);
const top = (q) => labels(q)[0];

test('empty / whitespace query returns the whole list in build order', () => {
  assert.deepEqual(labels(''), COMMANDS.map((c) => c.label));
  assert.deepEqual(labels('   '), COMMANDS.map((c) => c.label));
});

test('prefix beats mid-word: "focus all" → Focus all windows (not Unfocus)', () => {
  assert.equal(top('focus all'), 'Focus all windows');
  // Hide all (whose keyword carries "unfocus all") is still listed, lower.
  assert.ok(labels('focus all').includes('Hide all windows'));
});

test('synonym hide→unfocus: "hide all" → Hide all windows on top', () => {
  assert.equal(top('hide all'), 'Hide all windows');
});

test('synonym show→focus: "show all" → Focus all windows on top', () => {
  assert.equal(top('show all'), 'Focus all windows');
});

test('legacy term still works: "unfocus all" → Hide all windows', () => {
  // The label says "Hide" now, but "unfocus" must still find it (via the
  // keyword AND the unfocus→hide synonym), so old muscle memory isn't lost.
  assert.equal(top('unfocus all'), 'Hide all windows');
});

test('bare "hide" surfaces the hide commands', () => {
  const out = labels('hide');
  assert.ok(out.includes('Hide all windows'));
  assert.ok(out.includes('Hide group: alpha'));
  assert.ok(out.includes('Hide window: worker-7'));
});

test('bare "show" surfaces the focus commands via synonym', () => {
  const out = labels('show');
  assert.ok(out.includes('Focus all windows'));
  assert.ok(out.includes('Focus group: alpha'));
});

test('"theme" finds the theme toggle', () => {
  assert.equal(top('theme'), 'Switch to slop theme');
});

test('no match returns an empty list', () => {
  assert.deepEqual(rankCommands(COMMANDS, 'zzzzz'), []);
});

test('expandQuery maps synonyms bidirectionally', () => {
  assert.ok(expandQuery('hide all').includes('unfocus all'));
  assert.ok(expandQuery('show all').includes('focus all'));
  assert.ok(expandQuery('unfocus').includes('hide'));
  assert.ok(expandQuery('focus').includes('show'));
  // The typed query is always a variant itself.
  assert.ok(expandQuery('hide all').includes('hide all'));
});

test('SYNONYMS pairs are bidirectional', () => {
  assert.deepEqual(SYNONYMS.hide, ['unfocus']);
  assert.deepEqual(SYNONYMS.unfocus, ['hide']);
  assert.deepEqual(SYNONYMS.show, ['focus']);
  assert.deepEqual(SYNONYMS.focus, ['show']);
});

test('scoreMatch ladder: exact > prefix > word-start > substring', () => {
  const label = 'focus all windows';
  const hay = label + ' x';
  const exact = scoreMatch('focus all windows', ['focus', 'all', 'windows'], label, hay);
  const prefix = scoreMatch('focus', ['focus'], label, hay);
  const wordStart = scoreMatch('all', ['all'], label, hay); // " all" at word boundary
  const none = scoreMatch('zzz', ['zzz'], label, hay);
  assert.ok(exact > prefix, 'exact beats prefix');
  assert.ok(prefix > wordStart, 'prefix beats word-start');
  assert.ok(wordStart > 0 && none === 0);
});

test('scoreCommand lifts a keyword-phrase hit above scattered tokens', () => {
  // "hide all" is only a contiguous phrase in the keywords here (label is
  // "Unfocus…"), which must beat a command that merely scatters the tokens.
  const phraseHit = scoreCommand('hide all', 'unfocus all windows',
    'unfocus all windows hide all windows detach');
  const scattered = scoreCommand('hide all', 'focus window: hotkey-for-hide-all-windows',
    'focus window: hotkey-for-hide-all-windows jump');
  assert.ok(phraseHit > scattered, `phrase ${phraseHit} should beat scattered ${scattered}`);
});
