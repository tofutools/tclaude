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
