import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';
import {
  SANDBOX_RULES_NOT_EVALUATED_NOTE,
  sandboxRuleBuckets,
  sandboxTargetRefusal,
} from '../dashboard/js/sandbox-profiles-data.js';

/* TCL-915 — OPTION C: a refused target LISTS its rules without judging them.

   The operator chose C over the alternative that put the rules in a "Blocked"
   group. That alternative asserts each rule was judged and each was blocked;
   evaluation never got that far. So the whole ticket rests on one distinction —
   LISTED is not JUDGED — and every assertion here exists to hold it.

   Two shapes are easy to confuse and must never render alike:

     refused              -> unjudged bucket, outcome 'not_evaluated', no verdict
     old daemon, no axes  -> notApplied bucket, outcome 'not_enforced'  (a verdict)

   The second is TCL-885's missing-axis fallback. If a refusal ever reached it,
   the preview would say "unsupported" about rules nobody looked at.

   ASSERTIONS ARE POSITIVE PLACEMENTS — which bucket a rule IS in — never "the
   bad bucket is empty". An empty bucket is also what you get when nothing
   rendered at all. */

const REFUSAL = {
  kind: 'unsupported_sandbox_profile_network_allowlist',
  // Production-shaped: cause and remedies fused into ONE sentence, remedies in
  // the trailing clause. This is what the wire actually carries, and its shape
  // is why the renderer must not try to split it.
  message: 'missing capability proxy_engine_name_authority: the Proxy filter engine decides host '
    + 'and domain rules on the name the sandbox asks for, and the authored filesystem grant '
    + '/run/systemd/resolve binds the system resolver socket at '
    + '/run/systemd/resolve/io.systemd.Resolve into the sandbox, which leaves those rules with no '
    + 'name to decide; narrow that grant so it does not cover '
    + '/run/systemd/resolve/io.systemd.Resolve, deny that path explicitly, or select the Packet '
    + 'filter engine, whose DNS broker holds name authority with a resolver socket present',
};

const CONTEXT = {
  filesystem: [{ path: '/run/systemd/resolve', access: 'read' }],
  network: { mode: 'list', allow: [{ domain: 'example.com', ports: [443] }] },
};

const FS_RULE = 'Read-only: /run/systemd/resolve';
const NET_RULE = 'Allow network: domain example.com · port 443';

const REFUSED_TARGET = {
  target: { implementation: 'tclaude-layer', harness: 'claude', platform: 'linux' },
  predicted: false,
  axes: {},
  refusal: REFUSAL,
};

// The scoping control: same request, same shape, no refusal. Its verdict buckets
// must be untouched by this ticket.
const CLEAN_TARGET = {
  target: { implementation: 'tclaude-layer', harness: 'codex', platform: 'linux' },
  predicted: true,
  axes: {
    filesystem: { outcome: 'enforced', tier: 'full', detail: '' },
    network: { outcome: 'enforced', tier: 'list', detail: '' },
    unix_sockets: { outcome: 'enforced', tier: 'list', detail: '' },
  },
};

/* Absence, asserted WITHOUT handing a live DOM node to the diff formatter.

   `assert.equal(container.querySelector(sel), null)` looks like the obvious
   spelling and is a trap: when it FAILS, node's formatter walks the linkedom
   element's circular parent/child graph to build a diff and the process is
   OOM-killed. The test file dies with SIGKILL after ~30s, printing no test names
   at all — so a wrong assertion here fails as a HANG, which reads as slowness
   rather than as a defect. That cost real time on this ticket, twice: once on a
   TCL-885 assertion Option C invalidated, and once on this suite's own scoping
   control under mutation 7.

   Reducing to a short string first keeps the failure a one-line diff AND names
   what was found, which `assert.ok(!el)` would not. */
function assertAbsent(container, selector, message) {
  const found = container.querySelector(selector);
  assert.equal(
    found && `${found.localName}.${found.getAttribute('class') || ''}`, null, message,
  );
}

// Which bucket holds a rule, by label. Fails naming what WAS rendered rather
// than returning "absent", so a fixture that never reached the code cannot pass
// for a working placement.
function bucketOfRule(container, label) {
  for (const bucket of container.querySelectorAll('.sbx-rule-bucket')) {
    for (const row of bucket.querySelectorAll('.sbx-rule-row')) {
      if (row.querySelector('span')?.textContent === label) {
        return [...bucket.classList].find((name) => name.startsWith('sbx-rule-bucket-'));
      }
    }
  }
  const rendered = [...container.querySelectorAll('.sbx-rule-row span')].map((s) => s.textContent);
  return assert.fail(`no rule row labelled ${JSON.stringify(label)} rendered, so nothing `
    + `downstream of it ran; rendered rows: ${JSON.stringify(rendered)}`);
}

async function render(harness, target) {
  const { SandboxPolicyResult } = await harness.importDashboardModule('js/management-island.js');
  const mounted = await harness.mount(harness.html`
    <${SandboxPolicyResult} target=${target} context=${CONTEXT} contextIndex=${0}/>`);
  return mounted.container;
}

test('a refused target places its rules in the unjudged bucket, not a verdict one', () => {
  const buckets = sandboxRuleBuckets({}, CONTEXT, [], sandboxTargetRefusal(REFUSED_TARGET));

  // POSITIVE placement first: the rules are present and they are HERE.
  assert.ok(buckets.unjudged.rules.includes(FS_RULE), 'the authored filesystem rule is listed');
  assert.ok(buckets.unjudged.rules.includes(NET_RULE), 'the authored network rule is listed');
  assert.ok(buckets.unjudged.rules.length >= 2);

  // Then, and only then, that no verdict bucket claims them.
  for (const key of ['applied', 'partial', 'notApplied']) {
    assert.deepEqual(buckets[key].rules, [],
      `no verdict was reached, so the ${key} bucket must claim nothing`);
  }
  assert.equal(buckets.launchRefused, true);
});

test('an unjudged rule carries an outcome distinct from the missing-axis fallback', () => {
  const refused = sandboxRuleBuckets({}, CONTEXT, [], sandboxTargetRefusal(REFUSED_TARGET));
  // An OLD daemon with no axes: same absent axes, but no refusal.
  const oldDaemon = sandboxRuleBuckets({}, CONTEXT, [], null);

  assert.ok(refused.unjudged.items.every((item) => item.outcome === 'not_evaluated'),
    'not judged');
  assert.ok(oldDaemon.notApplied.items.every((item) => item.outcome === 'not_enforced'),
    'judged, and found unsupported');

  /* The whole point, stated as an assertion: these must not be the same string.
     '' would rank equal to 'enforced' and 'not_enforced' claims the rule was
     checked — either would turn "we did not look" into a verdict. */
  const refusedOutcomes = new Set(refused.unjudged.items.map((i) => i.outcome));
  const fallbackOutcomes = new Set(oldDaemon.notApplied.items.map((i) => i.outcome));
  for (const outcome of refusedOutcomes) {
    assert.ok(!fallbackOutcomes.has(outcome),
      `'${outcome}' is used for BOTH not-judged and judged-unsupported rules`);
    assert.notEqual(outcome, '', 'an empty outcome ranks equal to enforced');
  }
});

test('the unjudged bucket says in prose that no verdict was reached', async (t) => {
  /* Pinned as the SENTENCE, not as the bucket's class or its existence. A bucket
     can be present and empty, and a class can be renamed; the operator-visible
     property is that the list denies being a verdict IN WORDS. Colour cannot say
     it and is unavailable to anyone reading without colour. */
  const harness = await createPreactHarness(t);
  const container = await render(harness, REFUSED_TARGET);

  const bucket = container.querySelector('.sbx-rule-bucket-unjudged');
  assert.ok(bucket, 'the refused target lists its rules');
  const note = bucket.querySelector('.sbx-bucket-note');
  assert.ok(note, 'the list carries its disclaimer');
  assert.equal(note.textContent, SANDBOX_RULES_NOT_EVALUATED_NOTE);
  assert.match(note.textContent, /none carries a verdict/);

  // Rules are LISTED — the note is not standing in for missing content.
  assert.equal(bucketOfRule(container, FS_RULE), 'sbx-rule-bucket-unjudged');
  assert.equal(bucketOfRule(container, NET_RULE), 'sbx-rule-bucket-unjudged');

  // Ships collapsed: no verdict was reached, so nothing here needs attention.
  assert.equal(bucket.hasAttribute('open'), false, 'the unjudged bucket ships collapsed');
});

test('a refused rule\'s own help must not claim it was not applied', async (t) => {
  /* The per-rule help chain ends in a bare 'Not applied'. Without an explicit
     not_evaluated branch every rule in the bucket would disclose "Not applied on
     <target>" — asserting, per rule, exactly what the bucket around it denies.
     A fix for an invisibility class committing that class in its own
     diagnostics. */
  const harness = await createPreactHarness(t);
  const container = await render(harness, REFUSED_TARGET);
  const bucket = container.querySelector('.sbx-rule-bucket-unjudged');

  const helps = [...bucket.querySelectorAll('[aria-label], [title], button')]
    .map((el) => `${el.getAttribute('aria-label') || ''} ${el.getAttribute('title') || ''}`)
    .join(' ');
  const everything = `${bucket.textContent} ${helps}`;
  assert.doesNotMatch(everything, /Not applied/,
    'a rule that was never judged must not disclose the "Not applied" verdict');
  assert.match(everything, /Not evaluated|never judged/,
    'and it must say what DID happen, not merely omit the wrong word');
});

test('the refusal banner carries the kind and the message verbatim', async (t) => {
  /* Equality, not a substring match. The message is a single fused sentence
     whose remedies are its trailing clause; there is no structured remedies
     field. Splitting it client-side is a guess dressed as structure, and a
     mis-split silently DROPS a remedy — this ticket's own failure mode in its
     own rendering. Equality is what makes any future split fail loudly. */
  const harness = await createPreactHarness(t);
  const container = await render(harness, REFUSED_TARGET);

  const alert = container.querySelector('.sbx-launch-blocked');
  assert.ok(alert, 'the refusal is announced');
  assert.equal(alert.getAttribute('role'), 'alert');
  assert.equal(alert.querySelector('.sbx-refusal-kind').textContent, REFUSAL.kind,
    'the capability kind is shown as its own token, quotable in a bug report');
  assert.equal(alert.querySelector('.sbx-refusal-detail').textContent, REFUSAL.message,
    'the evaluator\'s text is rendered verbatim, remedies clause included');
  // The remedies specifically, so a truncation that kept the cause still fails.
  assert.match(alert.textContent, /select the Packet filter engine/);
});

test('an unaffected target in the same request keeps its verdict buckets untouched', async (t) => {
  /* The scoping control. Without it, "the refused target renders correctly" is
     compatible with having broken every other target's rendering. */
  const harness = await createPreactHarness(t);
  const container = await render(harness, CLEAN_TARGET);

  assertAbsent(container, '.sbx-rule-bucket-unjudged',
    'a target that WAS evaluated has nothing unjudged');
  assertAbsent(container, '.sbx-launch-blocked', 'and is not blocked');
  assert.equal(bucketOfRule(container, FS_RULE), 'sbx-rule-bucket-applied',
    'its rules keep their real verdicts');
  assert.equal(bucketOfRule(container, NET_RULE), 'sbx-rule-bucket-applied');
  assertAbsent(container, '.sbx-bucket-note',
    'and it carries no not-evaluated disclaimer');
});
