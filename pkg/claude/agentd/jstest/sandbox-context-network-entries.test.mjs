import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';

/* TCL-914. ONE PREDICATE FOR THE PER-CONTEXT NETWORK ENTRIES.

   Two consumers read `context_network_entries[i]`: SandboxPolicyResult, to
   decide what to render, and sandboxPolicyNeedsAttention, to decide whether the
   assignment needs attention. They are the same question about the same
   context, so every test here asserts BOTH — a divergence between them is the
   defect this ticket exists to remove, and asserting only the renderer is how a
   previous attempt shipped with one of the two call sites still unfixed.

   The other half is what the fallback keys on. A daemon that never sends the
   field is old and has no per-context answer, so the target-wide draft-only rows
   stand in. A daemon that sends the list is authoritative for every index in it,
   including a null one: null says "this context produced no entries", which is a
   VERDICT. Filling it from `target.network_entries` attributes the DRAFT-ONLY
   prediction — a different policy, which no launch uses — to this context.

   HOW THESE TESTS DISCRIMINATE. The observable is which bucket the network rule
   lands in, because the bucket is decided by the outcome of whichever entry
   matched the rule's key:

     draft-only row  outcome 'refused'          -> Unsupported bucket, launch blocked
     per-context row outcome 'enforced_partial' -> Partially supported bucket
     no matching row (axes.network 'enforced')  -> Fully supported bucket

   Three distinct, mutually exclusive placements, so "the fixture never reached
   the code" cannot masquerade as "the fix works". The first test below feeds the
   SAME draft-only row through the old-daemon path and proves it does match a
   rendered rule and does block the launch; that is what keeps the null test's
   negative assertions from being satisfied by an inert fixture. A previous
   attempt's fixture omitted `keys`, matched nothing, and passed under mutation. */

const ALLOW_ENTRY = { domain: 'example.com', ports: [443] };

const CONTEXT = {
  network: { mode: 'list', allow: [ALLOW_ENTRY] },
};

// The row label effectiveRuleRows() builds for ALLOW_ENTRY. bucketOfRule throws
// when it matches nothing, so a drift here fails loudly rather than quietly
// weakening every assertion below.
const ALLOW_RULE_LABEL = 'Allow network: domain example.com · port 443';

const DRAFT_ONLY_DETAIL = 'draft-only evaluation refuses this destination list';
const PER_CONTEXT_DETAIL = 'this context enforces the list only partially';

// Only `network` is populated on both, so sandboxOtherAssignmentWarnings has
// nothing to report and cannot supply attention from a source other than the
// entries under test.
const ENFORCED_NETWORK_AXES = {
  network: { outcome: 'enforced', tier: 'list', detail: 'the list is enforced' },
};

/* Both rows carry the production key spellings, computed with the production
   helper rather than transcribed, so they genuinely match the rendered rule. A
   `refused` network-entry outcome is a real daemon shape: it is what
   DescribePredictedNetworkEntries emits for every allow row when the capability
   table sets NetworkListRefusal. It is also the ONLY outcome that reaches
   sandboxPolicyNeedsAttention, which is why the draft-only row carries it — a
   leak of a merely-partial row would be invisible to the second call site and
   the test would prove nothing about it. */
async function fixtures(harness) {
  const { sandboxNetworkModeEntryKey, sandboxNetworkEntryKey } =
    await harness.importDashboardModule('js/sandbox-profiles-data.js');
  const keys = [
    sandboxNetworkModeEntryKey('allow', ALLOW_ENTRY),
    sandboxNetworkEntryKey(ALLOW_ENTRY),
  ];
  return {
    draftOnlyRow: {
      entry: ALLOW_ENTRY, mode: 'allow', keys,
      outcome: 'refused', detail: DRAFT_ONLY_DETAIL,
    },
    perContextRow: {
      entry: ALLOW_ENTRY, mode: 'allow', keys,
      outcome: 'enforced_partial', detail: PER_CONTEXT_DETAIL,
    },
  };
}

// The bucket key holding the allow rule, or a failure naming what was rendered
// instead. Never returns "absent" as a passing answer.
function bucketOfRule(container, label) {
  for (const bucket of container.querySelectorAll('.sbx-rule-bucket')) {
    for (const row of bucket.querySelectorAll('.sbx-rule-row')) {
      if (row.querySelector('span')?.textContent === label) {
        return [...bucket.classList].find((name) => name.startsWith('sbx-rule-bucket-'));
      }
    }
  }
  const rendered = [...container.querySelectorAll('.sbx-rule-row span')]
    .map((span) => span.textContent);
  return assert.fail(
    `the fixture never rendered a rule row labelled ${JSON.stringify(label)}, `
    + `so nothing downstream of it was exercised; rendered rows: ${JSON.stringify(rendered)}`,
  );
}

async function render(harness, target) {
  const { SandboxPolicyResult, sandboxPolicyNeedsAttention } =
    await harness.importDashboardModule('js/management-island.js');
  const mounted = await harness.mount(harness.html`
    <${SandboxPolicyResult} target=${target} context=${CONTEXT} contextIndex=${0}/>`);
  return {
    container: mounted.container,
    launchBlocked: mounted.container.querySelector('.sbx-launch-blocked'),
    needsAttention: sandboxPolicyNeedsAttention(target, CONTEXT, 0),
  };
}

test('an old daemon that sends no per-context list still reads the target-wide entries', async (t) => {
  /* THE COMPAT DIRECTION, and the positive control for the two tests after it.
     A response with no context_network_entries at all must behave exactly as it
     did before the field existed: the target-wide rows apply to the context on
     screen.

     This is also what makes the null test discriminating. It proves the very
     same draft-only row matches a rendered rule and drives it to Unsupported +
     launch-blocked + attention. A helper that simply returned [] would pass the
     null test and fail here. */
  const harness = await createPreactHarness(t);
  const { draftOnlyRow } = await fixtures(harness);
  const target = {
    target: { implementation: 'tclaude-layer', harness: 'claude', platform: 'linux' },
    predicted: true,
    axes: ENFORCED_NETWORK_AXES,
    context_axes: [ENFORCED_NETWORK_AXES],
    network_entries: [draftOnlyRow],
    // context_network_entries deliberately absent — that is the whole fixture.
  };
  const { container, launchBlocked, needsAttention } = await render(harness, target);

  assert.equal(bucketOfRule(container, ALLOW_RULE_LABEL), 'sbx-rule-bucket-not-applied',
    'the target-wide refused row must still reach the rule it keys to');
  assert.notEqual(launchBlocked, null, 'a refused rule blocks the launch');
  assert.equal(needsAttention, true,
    'both readers must see the fallback, not just the renderer');
});

test('a populated per-context entry is used instead of the target-wide entries', async (t) => {
  // The ordinary path, pinned so the fallback cannot quietly become the only
  // path: with both lists present the per-context one wins.
  const harness = await createPreactHarness(t);
  const { draftOnlyRow, perContextRow } = await fixtures(harness);
  const target = {
    target: { implementation: 'tclaude-layer', harness: 'claude', platform: 'linux' },
    predicted: true,
    axes: ENFORCED_NETWORK_AXES,
    context_axes: [ENFORCED_NETWORK_AXES],
    context_refusals: [null],
    context_network_entries: [[perContextRow]],
    network_entries: [draftOnlyRow],
  };
  const { container, launchBlocked, needsAttention } = await render(harness, target);

  assert.equal(bucketOfRule(container, ALLOW_RULE_LABEL), 'sbx-rule-bucket-partial',
    'the per-context verdict decides the bucket');
  assertAbsent(launchBlocked, 'the draft-only refusal belongs to a different policy and must not block this one');
  assert.equal(needsAttention, false, 'both readers must agree the context is fine');
});

test('a null per-context entry is a verdict, not a gap to fill from the draft', async (t) => {
  /* THE TICKET. The daemon writes an explicit null at a refused index to keep
     the list index-aligned with context_axes. `??` treats null as nullish and
     substitutes target.network_entries — the draft-only rows — so a different
     policy's verdicts get attributed to this context.

     The fixture DECOUPLES the null entry from a refusal on purpose. "The refusal
     branch returns before the value is read" is a claim about path ordering, and
     the helper exists so correctness does not rest on it; context_refusals is
     explicitly [null] here, so nothing short-circuits and the entries really are
     read. That reachability is not asserted by faith: bucketOfRule proves a rule
     row rendered and was bucketed.

     FALSIFIABILITY — both mutations must fail this test, and they are different
     mutations:
       19. Restore `?? target.network_entries ?? []`.
       20. Restore a PER-INDEX guard that falls back on a miss:
             const e = target.context_network_entries?.[contextIndex];
             return Array.isArray(e) ? e : (target.network_entries ?? []);
           For an explicit null this returns exactly what 19 returns. A guard
           that reproduces the expression it replaced is decoration, and 20
           passing while 19 failed would mean this test cannot tell the two
           apart. Run both against BOTH call sites. */
  const harness = await createPreactHarness(t);
  const { draftOnlyRow } = await fixtures(harness);
  const target = {
    target: { implementation: 'tclaude-layer', harness: 'claude', platform: 'linux' },
    predicted: true,
    axes: ENFORCED_NETWORK_AXES,
    context_axes: [ENFORCED_NETWORK_AXES],
    // Not refused: the guard must hold on its own, without the refusal branch
    // returning first.
    context_refusals: [null],
    context_network_entries: [null],
    network_entries: [draftOnlyRow],
  };
  const { container, launchBlocked, needsAttention } = await render(harness, target);

  assert.equal(bucketOfRule(container, ALLOW_RULE_LABEL), 'sbx-rule-bucket-applied',
    'no entries for this context means the axis verdict stands, not the draft row');
  assertAbsent(launchBlocked, 'a draft-only refusal must not be rendered as a refusal of this context');
  assert.equal(needsAttention, false,
    'the attention check reads the same value through the same helper');
});
