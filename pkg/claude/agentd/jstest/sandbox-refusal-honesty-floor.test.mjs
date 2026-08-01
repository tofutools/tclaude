import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

/* TCL-885 HONESTY FLOOR.

   This suite guards the one property that must hold no matter which per-target
   refusal ROW TYPE the operator eventually chooses, and it should outlive that
   decision.

   A refused target arrives from the daemon with NO axes. sandboxRuleBuckets
   substitutes {outcome:'not_enforced', detail:'No enforcement verdict was
   returned.'} for a missing axis so an OLD daemon degrades gracefully. If a
   refusal ever reaches that path, the preview says "unsupported, no verdict
   returned" with launchRefused FALSE and shows no alert — the operator is told
   nothing is wrong while the launch is in fact blocked. That is strictly worse
   than the plain 400 this ticket replaced, so it is a correctness floor rather
   than a design choice.

   The renderer branch under test is deliberately minimal and commits to none of
   the mocked options; what it renders may change, that it renders must not. */

/* Absence without handing a live DOM node to node's diff formatter. On FAILURE,
   assert.equal(el, null) walks the linkedom element's circular parent/child
   graph and the process is OOM-killed: the file dies with SIGKILL after ~30s and
   prints no test names, so a wrong assertion fails as a HANG rather than a diff.
   TCL-915 hit exactly that on the assertion below. Reducing to a short string
   first keeps the failure one line AND names what was found. */
function assertAbsent(root, selector, message) {
  const found = root.querySelector(selector);
  assert.equal(found && `${found.localName}.${found.getAttribute('class') || ''}`, null, message);
}

const REFUSAL = {
  kind: 'unsupported_sandbox_profile_network_allowlist',
  message: 'missing capability proxy_engine_name_authority: the Proxy filter engine '
    + 'loses name authority; narrow that grant, deny the resolver socket, or select '
    + 'the Packet filter engine',
};

const CONTEXT = {
  filesystem: [{ path: '/run/systemd/resolve', access: 'read' }],
  network: { mode: 'list', allow: [{ domain: 'example.com', ports: [443] }] },
  agentd_socket: 'always reachable',
};

const REFUSED_TARGET = {
  target: { implementation: 'tclaude-layer', harness: 'claude', platform: 'linux' },
  predicted: false,
  axes: {},
  refusal: REFUSAL,
};

// Same shape, same absent axes, but no refusal — an OLD daemon that simply
// omitted the axes. The two must not render alike.
const OLD_DAEMON_TARGET = {
  target: { implementation: 'tclaude-layer', harness: 'claude', platform: 'linux' },
  predicted: true,
  axes: {},
};

async function renderTarget(harness, target) {
  const { SandboxPolicyResult } = await harness.importDashboardModule('js/management-island.js');
  const mounted = await harness.mount(harness.html`
    <${SandboxPolicyResult} target=${target} context=${CONTEXT} contextIndex=${0}/>`);
  return mounted.container;
}

test('a refused target renders as launch-blocked, carrying the capability and its remedy', async (t) => {
  const harness = await createPreactHarness(t);
  const container = await renderTarget(harness, REFUSED_TARGET);

  const alert = container.querySelector('.sbx-launch-blocked');
  assert.ok(alert, 'a refused target must render the launch-blocked alert');
  assert.equal(alert.getAttribute('role'), 'alert',
    'the refusal must be announced, not merely displayed');
  assert.match(alert.textContent, /launch is refused/);
  // Discriminating: the specific missing capability and the named remedy, not a
  // generic "blocked".
  assert.match(alert.textContent, /proxy_engine_name_authority/);
  assert.match(alert.textContent, /Packet filter engine/);

  // And specifically NOT the missing-axis fallback's wording or bucketing.
  assert.doesNotMatch(container.textContent, /No enforcement verdict was returned/,
    'a refusal must never reach the missing-axis fallback');
  /* TCL-915 narrowed this. It used to read
     `querySelector('.sbx-rule-bucket') === null` — no bucket AT ALL — which was
     true of the TCL-885 minimum and is false by design under Option C, which
     renders one bucket that carries no verdict and says so.

     THE EXACT SET, not an enumeration of known-bad classes. A first attempt
     asserted the three verdict classes were absent by name and was caught by
     cold review as GENUINELY WEAKER: re-introducing Option B verbatim — a fifth
     "Blocked rules" bucket, one row per rule, outcome 'refused', on the refused
     target — passed the whole suite green. That is the ticket's single
     prohibited outcome shipping undetected.

     The justification written alongside that attempt was backwards. It claimed
     per-class assertions stop a bucket "added later" that a count would miss;
     the truth is the reverse — a closed set is what catches an unknown class,
     and naming three known ones is precisely the form that cannot. Recorded
     rather than quietly corrected, because the wrong reasoning is what made the
     weaker assertion look like the stronger one. */
  const bucketClasses = [...container.querySelectorAll('.sbx-rule-bucket')]
    .map((bucket) => [...bucket.classList].find((name) => name.startsWith('sbx-rule-bucket-')));
  assert.deepEqual(bucketClasses, ['sbx-rule-bucket-unjudged'],
    'a refused target renders EXACTLY the unjudged bucket — any other bucket, '
    + 'including a new class, claims a verdict that was never reached');

  const unjudged = container.querySelector('.sbx-rule-bucket-unjudged');
  assert.match(unjudged.textContent, /never judged|none carries a verdict/,
    'listing rules without a verdict is only honest if it SAYS no verdict was reached');
});

test('an old daemon\'s missing axes render differently from a refusal', async (t) => {
  const harness = await createPreactHarness(t);
  const refused = (await renderTarget(harness, REFUSED_TARGET)).innerHTML;
  const oldDaemon = (await renderTarget(harness, OLD_DAEMON_TARGET)).innerHTML;
  assert.notEqual(refused, oldDaemon,
    'the two states must be visibly distinct, or a refusal reads as "not enforced"');

  const container = await renderTarget(harness, OLD_DAEMON_TARGET);
  // The old-daemon path keeps its documented behaviour: rules listed as
  // unsupported with the fallback reason, and NO launch-blocked alert — an
  // absent verdict is not a refusal, and claiming otherwise would over-state
  // what is blocked.
  assert.ok(container.querySelector('.sbx-rule-bucket'));
  assert.match(container.textContent, /No enforcement verdict was returned/);
  assertAbsent(container, '.sbx-launch-blocked',
    'an absent verdict is not a refusal');
});

test('a refused target is flagged as needing attention', async (t) => {
  const harness = await createPreactHarness(t);
  const { sandboxPolicyNeedsAttention } = await harness.importDashboardModule('js/management-island.js');
  assert.equal(sandboxPolicyNeedsAttention(REFUSED_TARGET, CONTEXT, 0), true,
    'a blocked launch must open its section rather than sit collapsed');
});

test('a refusal in one context leaves the other contexts rendering normally', async (t) => {
  const harness = await createPreactHarness(t);
  const target = {
    target: { implementation: 'tclaude-layer', harness: 'claude', platform: 'linux' },
    predicted: true,
    axes: { filesystem: { outcome: 'enforced', tier: '1 read rule', detail: 'aggregate' } },
    context_axes: [
      {
        filesystem: { outcome: 'enforced', tier: '1 read rule', detail: 'enforced here' },
        network: { outcome: 'enforced', tier: 'list', detail: 'enforced here' },
        unix_sockets: { outcome: 'enforced', tier: 'unset', detail: 'enforced here' },
      },
      {},
    ],
    context_refusals: [null, REFUSAL],
  };
  const { SandboxPolicyResult } = await harness.importDashboardModule('js/management-island.js');

  const clean = await harness.mount(harness.html`
    <${SandboxPolicyResult} target=${target} context=${CONTEXT} contextIndex=${0}/>`);
  assertAbsent(clean.container, '.sbx-launch-blocked',
    'a sibling context\'s refusal must not darken this one');
  assert.ok(clean.container.querySelector('.sbx-rule-bucket-applied'));

  const refused = await harness.mount(harness.html`
    <${SandboxPolicyResult} target=${target} context=${CONTEXT} contextIndex=${1}/>`);
  assert.ok(refused.container.querySelector('.sbx-launch-blocked'));
  assert.match(refused.container.textContent, /proxy_engine_name_authority/);
});

/* The following three cases were added after a cold review found that the
   per-context refusal was PRODUCED by the daemon but never RENDERED, so an
   assignment the operator was not currently looking at could be refused with no
   signal anywhere on screen — a regression from the 400 this ticket replaced,
   one level up from where the original guard sat. */

test('a refusal in an unselected context is surfaced, not silently dropped', async (t) => {
  const harness = await createPreactHarness(t);
  const { SandboxPolicyResult, sandboxPolicyNeedsAttention } =
    await harness.importDashboardModule('js/management-island.js');
  const target = {
    target: { implementation: 'tclaude-layer', harness: 'claude', platform: 'linux' },
    predicted: true,
    // The aggregate summarizes SURVIVING contexts only, so it is clean — which
    // is exactly why the pre-existing axis-based "other assignments" check
    // cannot find this refusal and why it needs its own path.
    axes: { filesystem: { outcome: 'enforced', tier: '1 read rule', detail: 'aggregate' } },
    context_axes: [
      {
        filesystem: { outcome: 'enforced', tier: '1 read rule', detail: 'enforced here' },
        network: { outcome: 'enforced', tier: 'list', detail: 'enforced here' },
        unix_sockets: { outcome: 'enforced', tier: 'unset', detail: 'enforced here' },
      },
      {},
    ],
    context_refusals: [null, REFUSAL],
  };
  // Viewing context 0, which is itself perfectly clean.
  const contexts = [
    { context: { group_name: 'crew-clean' } },
    { context: { group_name: 'crew-conflicted' } },
  ];
  const mounted = await harness.mount(harness.html`
    <${SandboxPolicyResult} target=${target} context=${CONTEXT} contextIndex=${0}
      contexts=${contexts}/>`);
  const other = mounted.container.querySelector('.sbx-other-assignments');
  assert.ok(other, 'the refused sibling assignment must be reported');
  assert.ok(other.classList.contains('refused'));
  assert.equal(other.getAttribute('role'), 'alert');
  assert.match(other.textContent, /proxy_engine_name_authority/);
  // Named with the SAME vocabulary the context selector uses, so the operator
  // can find the offending assignment. An ordinal like "Assignment 2" matches
  // nothing on screen and leaves the warning unactionable.
  assert.match(other.textContent, /group crew-conflicted/);
  // Lowercase: the ordinal fallback emits "assignment 2", so the capitalised
  // form this originally used could never match anything the code can produce.
  assert.doesNotMatch(other.textContent, /assignment 2/);
  assert.equal(sandboxPolicyNeedsAttention(target, CONTEXT, 0), true,
    'a blocked sibling assignment must open the section');
});

test('several omitted refusals each get their own entry', async (t) => {
  // The daemon can emit more than one refusal past the display cap, and each
  // must get its own row rather than being collapsed. That is what this test
  // verifies.
  //
  // It does NOT verify the accompanying list-KEY change. Those entries are keyed
  // by their axis field, and the fix gives each omitted entry a distinct one;
  // but Preact reconciles correctly with duplicate keys here, so reverting that
  // half leaves this test passing — confirmed by running it that way. The key
  // change is a latent-hazard fix with no observable behaviour to assert, and
  // saying so is the point: a test whose title implies coverage it does not have
  // is worse than an absent one.
  const harness = await createPreactHarness(t);
  const { SandboxPolicyResult } = await harness.importDashboardModule('js/management-island.js');
  const second = { ...REFUSAL, message: 'missing capability second_distinct_capability' };
  const target = {
    target: { implementation: 'tclaude-layer', harness: 'claude', platform: 'linux' },
    predicted: true,
    axes: { filesystem: { outcome: 'enforced', tier: '1 read rule', detail: 'aggregate' } },
    context_axes: [{
      filesystem: { outcome: 'enforced', tier: '1 read rule', detail: 'enforced here' },
      network: { outcome: 'enforced', tier: 'list', detail: 'enforced here' },
      unix_sockets: { outcome: 'enforced', tier: 'unset', detail: 'enforced here' },
    }],
    context_refusals: [null],
    omitted_refusals: [REFUSAL, second],
  };
  const mounted = await harness.mount(harness.html`
    <${SandboxPolicyResult} target=${target} context=${CONTEXT} contextIndex=${0}/>`);
  const items = mounted.container.querySelectorAll('.sbx-other-assignments li');
  assert.equal(items.length, 2, 'each omitted refusal needs its own row');
  const text = [...items].map((item) => item.textContent).join('\n');
  assert.match(text, /proxy_engine_name_authority/);
  assert.match(text, /second_distinct_capability/,
    'a second omitted refusal must not be collapsed into the first');
});

test('a refusal past the display cap is surfaced via omitted_refusals', async (t) => {
  const harness = await createPreactHarness(t);
  const { SandboxPolicyResult, sandboxPolicyNeedsAttention } =
    await harness.importDashboardModule('js/management-island.js');
  // The daemon caps the per-context lists at 10. A context beyond the cap has no
  // index in context_axes AND contributes nothing to the aggregate, so without
  // omitted_refusals it would be invisible while the editor still tells the
  // operator the omitted assignments "are still included in the overall safety
  // check".
  const target = {
    target: { implementation: 'tclaude-layer', harness: 'claude', platform: 'linux' },
    predicted: true,
    axes: { filesystem: { outcome: 'enforced', tier: '1 read rule', detail: 'aggregate' } },
    context_axes: [{
      filesystem: { outcome: 'enforced', tier: '1 read rule', detail: 'enforced here' },
      network: { outcome: 'enforced', tier: 'list', detail: 'enforced here' },
      unix_sockets: { outcome: 'enforced', tier: 'unset', detail: 'enforced here' },
    }],
    context_refusals: [null],
    omitted_refusals: [REFUSAL],
  };
  const mounted = await harness.mount(harness.html`
    <${SandboxPolicyResult} target=${target} context=${CONTEXT} contextIndex=${0}/>`);
  const other = mounted.container.querySelector('.sbx-other-assignments');
  assert.ok(other, 'an omitted assignment that refuses must still be reported');
  assert.match(other.textContent, /omitted from this selector/);
  assert.match(other.textContent, /proxy_engine_name_authority/);
  assert.equal(sandboxPolicyNeedsAttention(target, CONTEXT, 0), true);
});

test('a target with no refusal anywhere reports no refusal warning', async (t) => {
  // The converse, so the two cases above cannot pass by always warning.
  const harness = await createPreactHarness(t);
  const { SandboxPolicyResult, sandboxPolicyNeedsAttention } =
    await harness.importDashboardModule('js/management-island.js');
  const target = {
    target: { implementation: 'tclaude-layer', harness: 'claude', platform: 'linux' },
    predicted: true,
    axes: { filesystem: { outcome: 'enforced', tier: '1 read rule', detail: 'aggregate' } },
    context_axes: [{
      filesystem: { outcome: 'enforced', tier: '1 read rule', detail: 'enforced here' },
      network: { outcome: 'enforced', tier: 'list', detail: 'enforced here' },
      unix_sockets: { outcome: 'enforced', tier: 'unset', detail: 'enforced here' },
    }],
    context_refusals: [null],
  };
  const mounted = await harness.mount(harness.html`
    <${SandboxPolicyResult} target=${target} context=${CONTEXT} contextIndex=${0}/>`);
  assertAbsent(mounted.container, '.sbx-other-assignments', 'no other assignment warns');
  assertAbsent(mounted.container, '.sbx-launch-blocked', 'and nothing is blocked');
  assert.equal(sandboxPolicyNeedsAttention(target, CONTEXT, 0), false);
});

// Characterization test for the assignment-naming vocabulary, pinned across all
// four of its branches BEFORE TCL-913 extracted the value-taking core out of
// sandboxContextLabel. The extraction moves the strings verbatim; this is what
// makes "no behaviour change" evidence rather than a claim.
//
// Only the group_name branch had coverage before this (the refused-sibling test
// above), so three of the four were resting on inspection. They are exercised
// through the renderer because the helper is module-private, which is also the
// path that actually matters: the operator reads these words.
test('every assignment-naming branch keeps its exact wording', async (t) => {
  const harness = await createPreactHarness(t);
  const { SandboxPolicyResult } = await harness.importDashboardModule('js/management-island.js');
  const axes = {
    filesystem: { outcome: 'enforced', tier: '1 read rule', detail: 'enforced here' },
    network: { outcome: 'enforced', tier: 'list', detail: 'enforced here' },
    unix_sockets: { outcome: 'enforced', tier: 'unset', detail: 'enforced here' },
  };
  const target = {
    target: { implementation: 'tclaude-layer', harness: 'claude', platform: 'linux' },
    predicted: true,
    axes,
    context_axes: [axes, axes],
    context_refusals: [null, REFUSAL],
  };
  for (const [context, expected] of [
    [{ group_name: 'crew-x' }, 'group crew-x'],
    [{ explicit: 'some-profile' }, 'explicit selection'],
    [{ global: 'global-profile' }, 'global assignment'],
    [undefined, 'assignment 2'],
  ]) {
    const contexts = [{ context: { group_name: 'crew-selected' } }, { context }];
    const mounted = await harness.mount(harness.html`
      <${SandboxPolicyResult} target=${target} context=${CONTEXT} contextIndex=${0}
        contexts=${contexts}/>`);
    const other = mounted.container.querySelector('.sbx-other-assignments');
    assert.ok(other, `a refused sibling must be reported for ${JSON.stringify(context)}`);
    assert.match(other.textContent, new RegExp(expected.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')),
      `context ${JSON.stringify(context)} must be named "${expected}"`);
  }
});
