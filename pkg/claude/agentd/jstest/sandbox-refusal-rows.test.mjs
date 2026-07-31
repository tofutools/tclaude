import test from 'node:test';
import assert from 'node:assert/strict';
import {
  sandboxOtherContextRefusals,
  sandboxPredictionWarnings,
  sandboxRuleBuckets,
  sandboxTargetRefusal,
} from '../dashboard/js/sandbox-profiles-data.js';

// TCL-885. The daemon's multi-target preview used to fail the WHOLE prediction
// on a capability conflict; it now carries a per-target refusal. The refusal is
// its own field rather than an axis outcome, and this suite is what pins that
// choice: sandboxRuleBuckets already substitutes an axis for one an OLD daemon
// omitted, and a refusal that arrived through the axis path would be
// indistinguishable from that fallback.

const REFUSAL = {
  kind: 'unsupported_sandbox_profile_network_allowlist',
  message: 'missing capability proxy_engine_name_authority: … select the Packet filter engine',
};

// The effective policy the editor is previewing. Shared by both the refused and
// the old-daemon case so the ONLY difference between them is how the daemon
// reported the verdict.
const CONTEXT = {
  filesystem: [{ path: '/run/systemd/resolve', access: 'read' }],
  network: { mode: 'list', allow: [{ domain: 'example.com', ports: [443] }] },
};

test('a refusal and an old daemon\'s missing axes produce different renderings', () => {
  // Old daemon: no `refusal` field anywhere, and no axes either. Every rule
  // falls through to the documented fallback verdict.
  const oldDaemon = { predicted: true, axes: {} };
  assert.equal(sandboxTargetRefusal(oldDaemon), null,
    'a missing axis must never be mistaken for a refusal');
  const fallbackBuckets = sandboxRuleBuckets(
    oldDaemon.axes, CONTEXT, [], sandboxTargetRefusal(oldDaemon));
  assert.equal(fallbackBuckets.refusal, null);
  assert.equal(fallbackBuckets.launchRefused, false,
    'an absent verdict is NOT a refusal — claiming it were would over-state what is blocked');
  assert.ok(fallbackBuckets.notApplied.rules.length > 0,
    'the fallback still lists the rules, marked unsupported');
  assert.ok(
    fallbackBuckets.notApplied.items.every((item) => item.outcome === 'not_enforced'),
    'the fallback verdict is not_enforced');

  // Refused target: the dedicated field, and NO axes at all.
  const refused = { predicted: false, axes: {}, refusal: REFUSAL };
  const refusal = sandboxTargetRefusal(refused);
  assert.deepEqual(refusal, REFUSAL);
  const refusedBuckets = sandboxRuleBuckets(refused.axes, CONTEXT, [], refusal);
  assert.deepEqual(refusedBuckets.refusal, REFUSAL);
  assert.equal(refusedBuckets.launchRefused, true);

  // The two renderings must DIFFER. This is the assertion that fails if the
  // refusal is ever folded back into the axis path: it would then arrive as
  // {outcome:'not_enforced'} and produce byte-identical buckets to the fallback.
  assert.notDeepEqual(
    { ...refusedBuckets, refusal: null }, { ...fallbackBuckets, refusal: null },
    'a refusal must not render identically to a missing-axis fallback');
  assert.equal(refusedBuckets.notApplied.rules.length, 0,
    'a refused target produces no buckets: an empty "supported" bucket is itself a verdict, and none was reached');
  assert.notEqual(refusedBuckets.launchRefused, fallbackBuckets.launchRefused);
});

test('a refused target surfaces its capability warning instead of reading as fully enforced', () => {
  // Without the refusal branch, iterating a refused target's (absent) axes finds
  // nothing non-enforced and reports ZERO warnings — the most dangerous possible
  // summary of a target that cannot run the policy at all.
  const warnings = sandboxPredictionWarnings({
    targets: [{ predicted: false, axes: {}, refusal: REFUSAL }],
  });
  assert.deepEqual(warnings.capability, [REFUSAL.message]);
});

test('an unaffected target in the same response keeps its ordinary verdicts', () => {
  const warnings = sandboxPredictionWarnings({
    targets: [
      { predicted: false, axes: {}, refusal: REFUSAL },
      {
        predicted: true,
        axes: {
          filesystem: { outcome: 'enforced', detail: 'fine' },
          network: { outcome: 'enforced_partial', detail: 'partly enforced' },
        },
      },
    ],
  });
  // Both, in one response: the ticket's whole point is that one target's
  // conflict no longer erases the other target's rows.
  assert.deepEqual(warnings.capability, [REFUSAL.message, 'partly enforced']);
});

test('a per-context refusal wins over the target-wide one for the selected context', () => {
  const target = {
    predicted: true,
    axes: { network: { outcome: 'enforced', detail: 'aggregate' } },
    context_axes: [{ network: { outcome: 'enforced', detail: 'clean' } }, {}],
    context_refusals: [null, REFUSAL],
  };
  assert.equal(sandboxTargetRefusal(target, 0), null,
    'the clean context keeps its ordinary rows');
  assert.deepEqual(sandboxTargetRefusal(target, 1), REFUSAL);

  // The clean context must bucket normally — a sibling context's refusal cannot
  // darken it. That is the property the daemon-side aggregate also preserves.
  const clean = sandboxRuleBuckets(
    target.context_axes[0], CONTEXT, [], sandboxTargetRefusal(target, 0));
  assert.equal(clean.launchRefused, false);
  assert.equal(clean.refusal, null);
  assert.ok(clean.applied.rules.length > 0);
});

test('a refusal in an unselected context stays visible', () => {
  const target = { context_refusals: [null, REFUSAL, null] };
  assert.deepEqual(sandboxOtherContextRefusals(target, 0), [{ index: 1, refusal: REFUSAL }]);
  // Selecting the refused context itself leaves no OTHER one to report; the
  // selected-context path carries it instead.
  assert.deepEqual(sandboxOtherContextRefusals(target, 1), []);
});

test('sandboxRuleBuckets is unchanged for every response that carries no refusal', () => {
  // The new parameter is optional and defaults to null, so an older caller that
  // passes three arguments keeps its exact previous behaviour.
  const withArg = sandboxRuleBuckets({}, CONTEXT, [], null);
  const withoutArg = sandboxRuleBuckets({}, CONTEXT, []);
  assert.deepEqual(withoutArg, withArg);
  assert.equal(withoutArg.launchRefused, false);
});
