// Unit tests for the Copilot drive chip's three states (TCL-1089).
//
// The chip has always distinguished "launched for the API drive" from "tclaude
// holds a connection". That pair is not enough, and the gap is the bug this
// ticket is about: `copilot_api && !connected` is equally true of an agent whose
// bootstrap is still running, of every already-running agent between an agentd
// restart and the reconcile sweep reaching it, and of an agent whose channel
// failed and will never come up. The first two are the system working normally.
// The third is an agent that is spawned, whose pane is alive and perfectly
// typeable, and which will never receive another message — and until the third
// state existed it rendered identically to an agent that was merely starting.
//
// So what is defended here is the DISTINCTION, not the styling: the pending
// state must not read as trouble, and the failed state must not read as pending.
//
// The Go wrapper (jstest/dashboard_node_test.go) globs this package's
// `*.test.mjs`, so this runs under `go test ./...` and skips when node is
// absent.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function memberTable(t) {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/dashboard.js', `
    export const lastSnapshot = { groups: [], ungrouped: [] };
    export function setLastSnapshot() {}
  `);
  return { harness, mod: await harness.importDashboardModule('js/groups-member-table.js') };
}

test('the Copilot drive chip tells a starting agent from a deaf one', async (t) => {
  const { harness, mod } = await memberTable(t);
  const { HarnessLine } = mod;
  const mount = async (state) =>
    harness.mount(harness.html`
      <${HarnessLine} member=${{ conv_id: 'c1', online: true, state }} />`);
  const base = { harness: 'copilot', model: 'GPT-5.6', copilot_api: true };

  await t.test('a connected agent is the plain chip', async () => {
    const mounted = await mount({ ...base, copilot_api_connected: true });
    try {
      const el = mounted.container.querySelector('.agent-harness .harness-drive');
      assert.ok(el, 'expected the drive chip');
      assert.equal(el.textContent.trim(), 'api');
      assert.ok(!el.classList.contains('harness-drive-pending'));
      assert.ok(!el.classList.contains('harness-drive-failed'));
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('an agent that is merely starting up is pending, NOT failed', async () => {
    // The arm that keeps the surface from crying wolf. Every healthy API spawn
    // spends its whole bootstrap here, and so does every agent waiting for a
    // reconcile after an agentd restart. If this rendered as failure the fleet
    // would look broken at exactly the moments it is working.
    const mounted = await mount({ ...base, copilot_api_connected: false });
    try {
      const el = mounted.container.querySelector('.agent-harness .harness-drive');
      assert.ok(el.classList.contains('harness-drive-pending'));
      assert.ok(!el.classList.contains('harness-drive-failed'),
        'no channel yet is the normal condition of a starting agent, not a fault');
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('an observed failure outranks pending and says the mail is held', async () => {
    const mounted = await mount({
      ...base, copilot_api_connected: false, copilot_api_channel_failed: true,
    });
    try {
      const el = mounted.container.querySelector('.agent-harness .harness-drive');
      assert.ok(el.classList.contains('harness-drive-failed'));
      assert.ok(!el.classList.contains('harness-drive-pending'),
        'the two states must not both apply; dashed means wait and this means act');
      // The operator's actual question is "what do I do", and the answer is not
      // guessable from a coloured chip: the agent looks alive and typeable, so
      // nothing else on the row suggests its mail is going nowhere.
      assert.match(el.title, /HELD/);
      assert.match(el.title, /relaunched/);
      assert.equal(el.getAttribute('aria-label'), 'Copilot API drive failed, messages held');
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('a send-keys Copilot agent has no chip at all', async () => {
    // The chip marks the API drive only; send-keys is what every Copilot agent
    // has always been, so a marker for it would be noise on every row. Asserted
    // because the failed state added a second condition to the same expression.
    const mounted = await mount({
      harness: 'copilot', model: 'GPT-5.6', copilot_api_channel_failed: true,
    });
    try {
      assert.equal(mounted.container.querySelector('.agent-harness .harness-drive'), null,
        'an agent that never took the drive cannot have a failed one');
    } finally {
      await mounted.unmount();
    }
  });
});

test('the Codex app-server chip discloses safe observer ownership and quarantine detail', async (t) => {
  const { harness, mod } = await memberTable(t);
  const { HarnessLine } = mod;
  const mount = async (state) =>
    harness.mount(harness.html`
      <${HarnessLine} member=${{ conv_id: 'codex-1', online: true, state }} />`);
  const base = { harness: 'codex', model: 'gpt-5.6-sol', codex_app_server: true };

  await t.test('ready names the non-subscribing observer and freshness', async () => {
    const mounted = await mount({
      ...base,
      codex_app_server_state: 'ready',
      codex_app_server_version: '0.147.0',
      codex_observer_updated: '2026-08-09T12:34:56Z',
    });
    try {
      const el = mounted.container.querySelector('.agent-harness .harness-drive');
      assert.equal(el.textContent.trim(), 'app');
      assert.match(el.title, /TUI owns approvals/);
      assert.match(el.title, /non-subscribing/);
      assert.match(el.title, /2026-08-09T12:34:56Z/);
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('quarantine shows the unexpected request method', async () => {
    const mounted = await mount({
      ...base,
      codex_app_server_state: 'unavailable',
      codex_app_server_detail: 'unexpected server request delivered to non-subscribing observer: item/permissions/requestApproval',
    });
    try {
      const el = mounted.container.querySelector('.agent-harness .harness-drive');
      assert.ok(el.classList.contains('harness-drive-failed'));
      assert.match(el.title, /item\/permissions\/requestApproval/);
      assert.match(el.title, /did not fall back to send-keys/);
    } finally {
      await mounted.unmount();
    }
  });
});
