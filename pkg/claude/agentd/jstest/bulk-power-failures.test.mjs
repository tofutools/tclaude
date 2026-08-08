import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// The daemon has always sent `outcome` + `detail` per agent for a bulk
// shutdown / power-on; the dashboard rendered only the counts. That is what
// made a real, repeating resume failure ("duplicate session: 019fde64") read to
// the operator as "it failed with no error at all".
test('a bulk power failure shows the daemon per-agent reason, not just a count', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ shellState }, ops] = await Promise.all([
    harness.importDashboardModule('js/shell-state.js'),
    harness.importDashboardModule('js/bulk-power-report.js'),
  ]);

  const shown = ops.reportBulkPowerFailures('Power on', {
    targeted: 3,
    resumed: 2,
    failed: 1,
    agents: [
      { conv_id: 'a', title: 'alpha', outcome: 'resumed' },
      { conv_id: 'b', title: 'bravo', outcome: 'resumed' },
      {
        conv_id: '019fde64-f405-7d33-ad08-f30b295786c6',
        title: 'charlie',
        outcome: 'failed',
        detail: 'resume was launched but the agent did not come online within 30s',
      },
    ],
  });

  const model = shellState.confirmation.value;
  assert.ok(model, 'a failure must put something on screen');
  assert.match(model.title, /1 agent/);
  assert.match(model.body, /charlie/, 'the operator must be told WHICH agent');
  assert.match(model.body, /did not come online within 30s/, 'and the daemon reason it already sent');
  assert.doesNotMatch(model.body, /alpha|bravo/, 'agents that succeeded are not failures');
  assert.equal(model.informational, true, 'nothing to confirm — this is a report');
  assert.equal(model.preformatted, true, 'one agent per line stays readable');

  shellState.resolveConfirmation(true);
  assert.equal(await shown, true);
  assert.equal(shellState.confirmation.value, null);
});

// A clean run must stay quiet: a modal after every successful bulk op would be
// noise, and the summary toast already says what happened.
test('a bulk power op with no failures shows no modal', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ shellState }, ops] = await Promise.all([
    harness.importDashboardModule('js/shell-state.js'),
    harness.importDashboardModule('js/bulk-power-report.js'),
  ]);

  assert.equal(await ops.reportBulkPowerFailures('Shutdown', {
    targeted: 2,
    exited_gracefully: 2,
    failed: 0,
    agents: [
      { conv_id: 'a', title: 'alpha', outcome: 'exited_gracefully' },
      { conv_id: 'b', title: 'bravo', outcome: 'already_offline' },
    ],
  }), false);
  assert.equal(shellState.confirmation.value, null, 'a clean run must not interrupt the operator');
});

// An outcome the dashboard does not recognize must still be reported. The
// daemon's own counters fold anything unrecognized into "failed", so a filter
// keyed on the failure NAME would let a renamed or new outcome make the toast
// say "1 failed" while this modal listed nothing — the original bug again.
test('an unrecognized outcome is reported rather than swallowed', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ shellState }, ops] = await Promise.all([
    harness.importDashboardModule('js/shell-state.js'),
    harness.importDashboardModule('js/bulk-power-report.js'),
  ]);

  const shown = ops.reportBulkPowerFailures('Power on', {
    targeted: 2, resumed: 1, failed: 1,
    agents: [
      { conv_id: 'a', title: 'alpha', outcome: 'resumed' },
      { conv_id: 'b', title: 'bravo', outcome: 'error:some_future_outcome', detail: 'a reason' },
    ],
  });
  assert.match(shellState.confirmation.value.body, /bravo — error:some_future_outcome/);
  assert.match(shellState.confirmation.value.body, /a reason/);
  shellState.resolveConfirmation(true);
  assert.equal(await shown, true);
});

// A long failure list is capped. .modal has no height bound and .modal-overlay
// centres without scrolling, so dumping 40 agents pushes the Close button past
// the viewport — the list this modal exists to show becomes unreachable.
test('a long failure list is capped with a count of the rest', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ shellState }, ops] = await Promise.all([
    harness.importDashboardModule('js/shell-state.js'),
    harness.importDashboardModule('js/bulk-power-report.js'),
  ]);

  const agents = Array.from({ length: 40 }, (_, i) => ({
    conv_id: `c${i}`, title: `agent-${i}`, outcome: 'failed', detail: 'did not come online',
  }));
  const shown = ops.reportBulkPowerFailures('Power on', { targeted: 40, failed: 40, agents });

  const body = shellState.confirmation.value.body;
  assert.match(shellState.confirmation.value.title, /40 agents/, 'the true total is still stated');
  assert.match(body, /agent-0 —/, 'the first failures are listed');
  assert.doesNotMatch(body, /agent-39 —/, 'the tail is not dumped');
  assert.match(body, /and 25 more/, 'and the operator is told how many were withheld');
  shellState.resolveConfirmation(true);
  assert.equal(await shown, true);
});
