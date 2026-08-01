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
  assert.equal(container.querySelector('.sbx-rule-bucket'), null,
    'no verdict was reached, so no bucket may claim one');
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
  assert.equal(container.querySelector('.sbx-launch-blocked'), null);
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
  assert.equal(clean.container.querySelector('.sbx-launch-blocked'), null,
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
  assert.doesNotMatch(other.textContent, /Assignment 2/);
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
  assert.equal(mounted.container.querySelector('.sbx-other-assignments'), null);
  assert.equal(mounted.container.querySelector('.sbx-launch-blocked'), null);
  assert.equal(sandboxPolicyNeedsAttention(target, CONTEXT, 0), false);
});
