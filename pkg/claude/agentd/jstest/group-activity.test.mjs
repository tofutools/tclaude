// Unit tests for the group activity indicator's pure aggregation logic
// (dashboard/js/group-activity.js), run with Node's BUILT-IN test runner
// (`node --test`, asserting via `node:assert`). No bundler/framework: the
// test imports the same raw ES module the browser loads. The Go wrapper
// `dashboard_node_test.go` (TestDashboardJS) globs the package's
// `*.test.mjs`, so this suite runs under `go test ./...` with no new
// wrapper and skips when node is absent. Lives OUTSIDE dashboard/ so
// `//go:embed dashboard` doesn't ship the test inside the agentd binary.

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  memberVariant, activitySummary, activityBotView, activityModeViews,
  aggregateActivity, VARIANT_ORDER,
  variantLabel, themedSummaryText,
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
  assert.deepEqual(activityModeViews(s), []);
});

test('summaryText reads as a human breakdown', () => {
  const s = activitySummary([on('error'), on('working'), on('working'), on('idle')]);
  assert.equal(s.summaryText, '1 error · 2 working · 1 idle');
});

test('variantLabel: plain nouns by default, arcane verbs in the wizard theme', () => {
  // Regular / blank / unknown theme → the honest noun.
  assert.equal(variantLabel('working', 2), '2 working');
  assert.equal(variantLabel('idle', 1, ''), '1 idle');
  assert.equal(variantLabel('working', 3, 'slop'), '3 working'); // slop keeps honest nouns
  // Wizard → "N familiar(s) <verb>", pluralised on the count.
  assert.equal(variantLabel('working', 2, 'wizard'), '2 familiars channeling');
  assert.equal(variantLabel('idle', 1, 'wizard'), '1 familiar meditating');
  assert.equal(variantLabel('asking', 1, 'wizard'), '1 familiar awaiting a decree');
  assert.equal(variantLabel('crashed', 1, 'wizard'), '1 familiar slain by a grue');
  assert.equal(variantLabel('offline', 3, 'wizard'), '3 familiars departed');
});

test('themedSummaryText re-flavours the breakdown for wizard, plain otherwise', () => {
  const s = activitySummary([on('working'), on('working'), on('idle')]);
  assert.equal(themedSummaryText(s), '2 working · 1 idle');           // default = regular
  assert.equal(themedSummaryText(s, ''), '2 working · 1 idle');       // blank = regular
  assert.equal(themedSummaryText(s, 'wizard'), '2 familiars channeling · 1 familiar meditating');
  assert.equal(themedSummaryText(activitySummary([]), 'wizard'), ''); // nothing present
});

test('activity mode models preserve exact classes, keys, counts and themed titles', () => {
  const summary = activitySummary([on('working'), on('working'), on('idle')]);
  const modes = activityModeViews(summary, { regular: 'emoji', slop: 'sprites', wizard: 'emoji' });
  assert.deepEqual(modes.map((mode) => mode.key), ['regular', 'slop', 'wizard']);
  assert.deepEqual(modes.map((mode) => mode.className), ['ga-regular', 'ga-slop', 'ga-wizard']);
  assert.ok(modes.every((mode) => mode.level === 'working'));
  assert.equal(modes[0].title, '2 working · 1 idle');
  assert.equal(modes[2].title, '2 familiars channeling · 1 familiar meditating');
  assert.deepEqual(modes[0].bots.map((bot) => bot.key), ['working', 'idle']);
  assert.equal(modes[0].bots[0].count, 2);
  assert.equal(modes[0].bots[1].count, 1);
  assert.equal(modes[0].bots[0].className, 'actbot actbot-working');
  assert.equal(modes[1].bots[0].className, 'actbot actbot-sprite actbot-working');
  assert.equal(modes[1].bots[0].faceClassName, 'actbot-spr spr-dance');
  assert.equal(modes[2].bots[0].face, '🧙');
});

test('activity bot models preserve emoji tags, sprite poses and wizard vocabulary', () => {
  const asking = activityBotView('asking', 1, 'emoji');
  assert.equal(asking.tag, '❓');
  assert.equal(asking.face, '🤖');
  assert.equal(asking.title, '1 awaiting');

  const sprite = activityBotView('crashed', 1, 'sprites');
  assert.equal(sprite.tag, '');
  assert.equal(sprite.faceClassName, 'actbot-spr spr-static');
  assert.match(sprite.className, /actbot-sprite actbot-crashed/);

  const wizard = activityBotView('working', 2, 'emoji', true);
  assert.equal(wizard.face, '🧙');
  assert.equal(wizard.title, '2 familiars channeling');
  assert.equal(wizard.tag, '');

  const wizardSprite = activityBotView('asking', 1, 'sprites', true);
  assert.match(wizardSprite.className, /actbot-sprite actbot-wiz actbot-asking/);
  assert.equal(wizardSprite.faceClassName, 'actbot-spr spr-wiz-ask');
  assert.equal(wizardSprite.title, '1 familiar awaiting a decree');
});

test('activity mode models omit disabled modes and never carry status detail strings', () => {
  const summary = activitySummary([on('working', '<img src=x onerror=alert(1)>')]);
  const regular = activityModeViews(summary, { regular: 'emoji', slop: 'off', wizard: 'off' });
  assert.deepEqual(regular.map((mode) => mode.key), ['regular']);
  assert.doesNotMatch(JSON.stringify(regular), /img|onerror/);
  assert.deepEqual(activityModeViews(summary, { regular: 'off', slop: 'off', wizard: 'off' }), []);
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
