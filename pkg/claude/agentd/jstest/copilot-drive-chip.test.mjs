// Unit tests for the API/app-server drive detail in the harness tooltip.
//
// The drive detail distinguishes "launched for the API drive" from "tclaude
// holds a connection". That pair is not enough: `copilot_api && !connected` is
// equally true of an agent whose bootstrap is still running, of every already-
// running agent between an agentd restart and the reconcile sweep reaching it,
// and of an agent whose channel failed and will never come up. The first two are
// the system working normally. The third is an agent that is spawned, whose
// pane is alive and perfectly typeable, and which will never receive another
// message.
//
// So what is defended here is the DISTINCTION, not a visible indicator: the
// pending state must not read as trouble, and the failed state must not read as
// pending, while both remain discoverable in the general harness tooltip.
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

test('the general harness tooltip tells a starting Copilot agent from a deaf one', async (t) => {
  const { harness, mod } = await memberTable(t);
  const { HarnessLine } = mod;
  const mount = async (state) =>
    harness.mount(harness.html`
      <${HarnessLine} member=${{ conv_id: 'c1', online: true, state }} />`);
  const base = { harness: 'copilot', model: 'GPT-5.6', copilot_api: true };

  await t.test('a connected agent discloses the API drive without a visible indicator', async () => {
    const mounted = await mount({ ...base, copilot_api_connected: true });
    try {
      const line = mounted.container.querySelector('.agent-harness');
      assert.equal(line.querySelector('.harness-drive'), null);
      assert.doesNotMatch(line.textContent, /\bapi\b/i);
      assert.match(line.title, /Drive: Copilot embedded JSON-RPC API/);
      assert.match(line.title, /not tmux send-keys/);
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
      const line = mounted.container.querySelector('.agent-harness');
      assert.equal(line.querySelector('.harness-drive'), null);
      assert.match(line.title, /not connected yet/);
      assert.match(line.title, /still starting up/,
        'no channel yet is described as the normal condition of a starting agent');
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('an observed failure outranks pending and says the mail is held', async () => {
    const mounted = await mount({
      ...base, copilot_api_connected: false, copilot_api_channel_failed: true,
    });
    try {
      const line = mounted.container.querySelector('.agent-harness');
      assert.equal(line.querySelector('.harness-drive'), null);
      // The operator's actual question is "what do I do", and the answer is not
      // guessable from a coloured chip: the agent looks alive and typeable, so
      // nothing else on the row suggests its mail is going nowhere.
      assert.match(line.title, /HELD/);
      assert.match(line.title, /relaunched/);
      assert.match(line.querySelector('.runtime-meta').getAttribute('aria-label'), /messages are being HELD/);
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('a send-keys Copilot agent has no drive detail', async () => {
    const mounted = await mount({
      harness: 'copilot', model: 'GPT-5.6', copilot_api_channel_failed: true,
    });
    try {
      const line = mounted.container.querySelector('.agent-harness');
      assert.equal(line.querySelector('.harness-drive'), null);
      assert.doesNotMatch(line.title, /Drive:/,
        'an agent that never took the drive cannot have drive health detail');
    } finally {
      await mounted.unmount();
    }
  });
});

test('the general harness tooltip discloses Codex observer ownership and quarantine detail', async (t) => {
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
	  codex_app_server_health: 'ready',
	  codex_app_server_source: 'explicit',
      codex_app_server_version: '0.147.0',
      codex_observer_updated: '2026-08-09T12:34:56Z',
    });
    try {
      const line = mounted.container.querySelector('.agent-harness');
      assert.equal(line.querySelector('.harness-drive'), null);
      assert.doesNotMatch(line.textContent, /\bapp\b/i);
      assert.match(line.title, /Drive: Codex app-server ready/);
      assert.match(line.title, /TUI owns approvals/);
      assert.match(line.title, /non-subscribing/);
      assert.match(line.title, /connects only after TUI thread binding/);
      assert.match(line.title, /context remains rollout-derived/);
      assert.match(line.title, /2026-08-09T12:34:56Z/);
	  assert.match(line.title, /via explicit/);
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('quarantine shows the unexpected request method', async () => {
    const mounted = await mount({
      ...base,
      codex_app_server_state: 'unavailable',
	  codex_app_server_health: 'disconnected',
      codex_app_server_detail: 'unexpected server request delivered to non-subscribing observer: item/permissions/requestApproval',
    });
    try {
      const line = mounted.container.querySelector('.agent-harness');
      assert.equal(line.querySelector('.harness-drive'), null);
      assert.match(line.title, /item\/permissions\/requestApproval/);
      assert.match(line.title, /did not fall back to send-keys/);
	  assert.match(line.title, /resume <agent> --send-keys/);
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('degraded is distinct from disconnected and remains fail-closed', async () => {
	const mounted = await mount({
	  ...base,
	  codex_app_server_state: 'ready',
	  codex_app_server_health: 'degraded',
	  codex_app_server_detail: 'status snapshots are stale',
	});
	try {
	  const line = mounted.container.querySelector('.agent-harness');
	  assert.equal(line.querySelector('.harness-drive'), null);
	  assert.match(line.title, /degraded/);
	  assert.match(line.title, /fail closed/);
	  assert.match(line.title, /status snapshots are stale/);
	} finally {
	  await mounted.unmount();
	}
  });
});
