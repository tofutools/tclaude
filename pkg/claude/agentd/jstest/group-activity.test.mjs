// Unit tests for the group activity indicator's pure aggregation logic
// (dashboard/js/group-activity.js), run with Node's BUILT-IN test runner
// (`node --test`, asserting via `node:assert`). No bundler/framework: the
// test imports the same raw ES module the browser loads. The existing Go
// wrapper `palette_score_node_test.go` (TestPaletteScore_JS) globs
// `jstest/*.test.mjs`, so this suite runs under `go test ./...` with no new
// wrapper and skips when node is absent. Lives OUTSIDE dashboard/ so
// `//go:embed dashboard` doesn't ship the test inside the agentd binary.

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  memberVariant, activitySummary, activityBotsHTML, groupActivityHTML,
  spriteBotsHTML, styledBotsHTML, aggregateActivity, VARIANT_ORDER,
} from '../dashboard/js/group-activity.js';

// Tiny member factory — online unless overridden, with a status string.
const on = (status, detail) => ({ online: true, state: { status, status_detail: detail || '' } });
const off = (exit_reason) => ({ online: false, state: { status: 'working', exit_reason: exit_reason || '' } });

test('memberVariant maps each status to its bot variant', () => {
  assert.equal(memberVariant(on('working')), 'working');
  assert.equal(memberVariant(on('main_agent_idle')), 'working');
  assert.equal(memberVariant(on('idle')), 'idle');
  assert.equal(memberVariant(on('exited')), 'idle');   // online-but-exited folds to calm
  assert.equal(memberVariant(on('')), 'idle');          // blank online status
  assert.equal(memberVariant(on('awaiting_permission')), 'asking');
  assert.equal(memberVariant(on('awaiting_input')), 'asking');
  assert.equal(memberVariant(on('error')), 'error');
});

test('memberVariant reads offline from online+exit_reason, never frozen status', () => {
  // Both are offline with a stale status:'working' — the indicator must
  // NOT call them working; exit_reason decides crashed vs clean offline.
  assert.equal(memberVariant(off('unexpected')), 'crashed');
  assert.equal(memberVariant(off('')), 'offline');
  assert.equal(memberVariant(off('clean')), 'offline'); // any non-unexpected reason
  assert.equal(memberVariant(null), 'offline');
  assert.equal(memberVariant(undefined), 'offline');
});

test('activitySummary dedups statuses and counts them', () => {
  const s = activitySummary([on('working'), on('working'), on('idle')]);
  assert.equal(s.total, 3);
  assert.equal(s.online, 3);
  assert.equal(s.counts.working, 2);
  assert.equal(s.counts.idle, 1);
  // One bot per DISTINCT status, not one per member.
  assert.deepEqual(s.present, ['working', 'idle']);
});

test('present is ordered loudest-first (error > asking > working > idle)', () => {
  const s = activitySummary([on('idle'), on('working'), on('awaiting_input'), on('error')]);
  assert.deepEqual(s.present, ['error', 'asking', 'working', 'idle']);
  assert.equal(s.level, 'error'); // level = loudest present variant
});

test('crashed shows even amongst working, but does not become the level', () => {
  const s = activitySummary([on('working'), on('working'), off('unexpected')]);
  // crashed is present (notable) ...
  assert.ok(s.present.includes('crashed'));
  // ... but ranked below working, so a busy group stays "working" mood.
  assert.equal(s.level, 'working');
  assert.deepEqual(s.present, ['working', 'crashed']);
});

test('clean offline is suppressed when anything live is present', () => {
  const s = activitySummary([on('working'), off(''), off('')]);
  assert.equal(s.counts.offline, 2);
  assert.deepEqual(s.present, ['working']); // the two asleep bots are hidden
});

test('an all-offline group still shows a single sleeping bot', () => {
  const s = activitySummary([off(''), off('')]);
  assert.deepEqual(s.present, ['offline']);
  assert.equal(s.level, 'offline');
  assert.equal(s.counts.offline, 2);
});

test('empty membership yields nothing to render', () => {
  const s = activitySummary([]);
  assert.deepEqual(s.present, []);
  assert.equal(s.level, 'empty');
  assert.equal(activityBotsHTML(s), '');
  assert.equal(spriteBotsHTML(s), '');
  assert.equal(groupActivityHTML([], 'emoji', 'sprites'), '');
});

test('summaryText reads as a human breakdown', () => {
  const s = activitySummary([on('error'), on('working'), on('working'), on('idle')]);
  assert.equal(s.summaryText, '1 error · 2 working · 1 idle');
});

test('botsHTML emits one bot per present variant, count badge only when >1', () => {
  const s = activitySummary([on('working'), on('working'), on('idle')]);
  const html = activityBotsHTML(s);
  assert.equal((html.match(/class="actbot /g) || []).length, 2); // two bots
  assert.ok(html.includes('actbot-working'));
  assert.ok(html.includes('actbot-idle'));
  assert.ok(html.includes('<span class="actbot-count">2</span>')); // working count
  // idle is a single member → no count badge for it. Only one count total.
  assert.equal((html.match(/actbot-count/g) || []).length, 1);
});

test('asking + error bots carry a corner tag glyph', () => {
  const html = activityBotsHTML(activitySummary([on('awaiting_permission'), on('error')]));
  assert.ok(html.includes('❓'));
  assert.ok(html.includes('💥'));
});

test('spriteBotsHTML emits sprite bots with anim classes, no tag glyphs', () => {
  const html = spriteBotsHTML(activitySummary([on('working'), on('awaiting_permission'), on('idle')]));
  assert.ok(html.includes('actbot-sprite'));
  assert.ok(html.includes('spr-dance'));   // working → dance
  assert.ok(html.includes('spr-asking'));  // awaiting → asking
  assert.ok(html.includes('spr-idle'));    // idle → idle
  assert.ok(!html.includes('❓'));          // pose carries status; no corner glyph
});

test('crashed + offline sprites fall back to the static frame', () => {
  const html = spriteBotsHTML(activitySummary([off('unexpected')])); // crashed
  assert.ok(html.includes('spr-static'));
  assert.ok(html.includes('actbot-crashed'));
});

test('styledBotsHTML switches emoji / sprites / off', () => {
  const s = activitySummary([on('working')]);
  assert.ok(styledBotsHTML(s, 'emoji').includes('actbot-face'));   // emoji bot
  assert.ok(styledBotsHTML(s, 'sprites').includes('actbot-spr'));  // sprite bot
  assert.equal(styledBotsHTML(s, 'off'), '');                      // hidden
});

test('groupActivityHTML emits a per-mode wrapper in each configured style', () => {
  const html = groupActivityHTML([on('awaiting_input'), on('working')], 'emoji', 'sprites');
  // regular wrapper = emoji bots, slop wrapper = sprite bots, both tinted
  // by the loudest level (asking).
  assert.ok(html.includes('class="ga-regular level-asking"'));
  assert.ok(html.includes('class="ga-slop level-asking"'));
  assert.ok(html.includes('actbot-face'));  // emoji in the regular wrapper
  assert.ok(html.includes('actbot-spr'));   // sprites in the slop wrapper
});

test('groupActivityHTML drops a wrapper whose mode is off; empty when both off', () => {
  const onlyRegular = groupActivityHTML([on('working')], 'emoji', 'off');
  assert.ok(onlyRegular.includes('ga-regular'));
  assert.ok(!onlyRegular.includes('ga-slop'));
  assert.equal(groupActivityHTML([on('working')], 'off', 'off'), '');
});

test('groupActivityHTML output is injection-safe (no caller strings)', () => {
  // status_detail is attacker-influenceable; it must never reach the HTML.
  const html = groupActivityHTML([on('working', '<img src=x onerror=alert(1)>')], 'emoji', 'sprites');
  assert.ok(!html.includes('<img'));
  assert.ok(!html.includes('onerror'));
});

test('aggregateActivity flattens several group member lists', () => {
  const g1 = [on('working'), on('idle')];
  const g2 = [on('working'), on('error')];
  const ungrouped = [off('')];
  const s = aggregateActivity([g1, g2, ungrouped]);
  assert.equal(s.total, 5);
  assert.equal(s.counts.working, 2);
  assert.equal(s.counts.error, 1);
  assert.equal(s.counts.idle, 1);
  // one live offline among live agents → suppressed
  assert.deepEqual(s.present, ['error', 'working', 'idle']);
});

test('aggregateActivity dedups by conv_id (an agent in several groups)', () => {
  // Same conv in two groups must count ONCE — the global indicator would
  // otherwise read "2 working" for a single agent in two groups.
  const shared = { online: true, state: { status: 'working' }, conv_id: 'abc' };
  const g1 = [shared, { online: true, state: { status: 'idle' }, conv_id: 'x1' }];
  const g2 = [shared, { online: true, state: { status: 'error' }, conv_id: 'x2' }];
  const s = aggregateActivity([g1, g2]);
  assert.equal(s.total, 3);          // shared counted once, + x1 + x2
  assert.equal(s.counts.working, 1); // NOT 2
  assert.equal(s.counts.idle, 1);
  assert.equal(s.counts.error, 1);
});

test('VARIANT_ORDER is the canonical priority list', () => {
  assert.deepEqual(VARIANT_ORDER, ['error', 'asking', 'working', 'idle', 'crashed', 'offline']);
});
