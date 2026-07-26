// Unit tests for the Groups tab's brokered-refusal surfaces (TCL-761):
// the per-agent 🚫 on the harness line (groups-member-table.js) and the
// machine-level notice above the group tree (groups-island.js).
//
// What is being defended is not the glyph, it is what the glyph replaces.
// When agentd refuses an agent's brokered callbacks, that agent's row keeps
// rendering — status, model, cost, context, directory — from the last values
// it managed to write, possibly hours ago. Without a marker the row reads as
// a healthy idle agent, which is exactly wrong. So the tests below check the
// two things that make it useful: it appears in the case where the rest of
// the row has nothing to show (no model yet), and it says the row is frozen
// rather than merely that something failed.
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

test('the refusal badge marks an agent whose telemetry agentd is dropping', async (t) => {
  const { harness, mod } = await memberTable(t);
  const { HarnessLine } = mod;
  const mount = async (state, online = true) =>
    harness.mount(harness.html`<${HarnessLine} member=${{ conv_id: 'c1', online, state }} />`);

  await t.test('a refused agent is badged on its harness line', async () => {
    const mounted = await mount({
      harness: 'claude', model: 'Opus 4.8 (1M context)', effort_level: 'high',
      broker_refusals: 12, broker_refusal_detail: 'hook: claimed session id disagrees',
      broker_refusal_since: new Date(Date.now() - 20 * 60_000).toISOString(),
    });
    try {
      const el = mounted.container.querySelector('.agent-harness .broker-refusal-badge');
      assert.ok(el, 'expected the refusal badge');
      assert.equal(el.textContent.trim(), '🚫');
      // The count and the elapsed time are how an operator tells a blip from a
      // permanently starved agent; both belong in the one place this surface has.
      assert.match(el.title, /refused 12 brokered callbacks/);
      assert.match(el.title, /starting/);
      assert.match(el.title, /claimed session id disagrees/);
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('it says the rest of the row is frozen, not merely that something failed', async () => {
    // The failure is invisible on its own terms — the agent keeps working.
    // What the operator needs told is that the numbers next to the badge have
    // stopped meaning anything, which no other element on the row can say.
    const mounted = await mount({
      harness: 'claude', model: 'Opus 4.8 (1M context)', cost_usd: 1.5, broker_refusals: 3,
    });
    try {
      const el = mounted.container.querySelector('.broker-refusal-badge');
      assert.match(el.title, /FROZEN at its last accepted value/);
      // And what to actually go look at: the stale-row-sharing-a-pid case is
      // the one this exists for, and it is not guessable from a warning glyph.
      assert.match(el.title, /same pid/);
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('the tooltip is the accessible name, not a hover-only secret', async () => {
    const mounted = await mount({ harness: 'claude', model: 'Opus 4.8', broker_refusals: 1 });
    try {
      const el = mounted.container.querySelector('.broker-refusal-badge');
      assert.equal(el.getAttribute('aria-label'), el.title);
      assert.equal(el.getAttribute('role'), 'note');
      // Singular: "1 brokered callbacks" is the kind of detail that makes an
      // operator trust the surface less than they should.
      assert.match(el.title, /refused 1 brokered callback,|refused 1 brokered callback\b/);
      assert.doesNotMatch(el.title, /1 brokered callbacks/);
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('a pre-tick row with no model still shows it', async () => {
    // This is the case that matters most and the one a naive placement loses:
    // the model is stamped by the status line, which is itself brokered. An
    // agent whose callbacks are ALL being refused never gets one, so it only
    // ever renders through the no-model branch. A badge that waited for a
    // model would be invisible in precisely the total-failure case.
    const mounted = await mount({ harness: 'claude', broker_refusals: 40 });
    try {
      const line = mounted.container.querySelector('.agent-harness');
      assert.ok(line, 'expected a harness line to exist for the badge to ride');
      assert.ok(line.querySelector('.broker-refusal-badge'), 'expected the badge on a pre-tick row');
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('a healthy agent carries no badge and gains no line', async () => {
    const mounted = await mount({ harness: 'claude' });
    try {
      assert.equal(mounted.container.querySelector('.broker-refusal-badge'), null);
      assert.equal(mounted.container.querySelector('.agent-harness'), null,
        'an unremarkable pre-tick row must not grow a line just to say nothing');
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('an expired run leaves no badge behind', async () => {
    // The daemon drops the record once refusals stop, so the count arrives as
    // 0/absent. An operator who fixed the cause must see the badge go.
    for (const state of [{ harness: 'claude', model: 'Opus 4.8', broker_refusals: 0 },
      { harness: 'claude', model: 'Opus 4.8' }]) {
      const mounted = await mount(state);
      try {
        assert.equal(mounted.container.querySelector('.broker-refusal-badge'), null);
      } finally {
        await mounted.unmount();
      }
    }
  });

  await t.test('it trails the sandbox and remote glyphs rather than displacing them', async () => {
    const mounted = await mount({
      harness: 'claude', model: 'Opus 4.8', remote_control: true,
      sandbox_mode: 'inherit', os_sandbox_state: 'on', os_sandbox_source: '~/.claude/settings.json',
      broker_refusals: 2,
    });
    try {
      const glyphs = [...mounted.container.querySelectorAll(
        '.sandbox-badge, .remote-badge, .broker-refusal-badge')];
      assert.deepEqual(glyphs.map((el) => el.className.split(' ')[0]),
        ['sandbox-badge', 'remote-badge', 'broker-refusal-badge']);
    } finally {
      await mounted.unmount();
    }
  });
});

test('the unplaceable notice reports the refusals that have no row to land on', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/dashboard.js', `
    export const lastSnapshot = { groups: [], ungrouped: [] };
    export function setLastSnapshot() {}
  `);
  const { BrokerRefusalNotice } = await harness.importDashboardModule('js/groups-island.js');
  const mount = async (snapshot) =>
    harness.mount(harness.html`<${BrokerRefusalNotice} snapshot=${snapshot} />`);

  await t.test('it appears once the daemon has refused an unplaceable caller', async () => {
    const mounted = await mount({ broker_refusals_unplaceable: 7 });
    try {
      const el = mounted.container.querySelector('.broker-refusal-notice');
      assert.ok(el, 'expected the notice');
      assert.match(el.textContent, /refused 7 brokered callbacks/);
      assert.match(el.textContent, /could not place/);
      // The counter alone is not actionable; the daemon log is where the
      // caller pid is, and that pointer is the whole value of the notice.
      assert.match(el.textContent, /daemon log/);
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('it never prints an identifier the refused caller supplied', async () => {
    // The refusal happened precisely because the caller could not be
    // identified. Echoing the session id it claimed would put an
    // unauthenticated string from that caller onto the operator's screen —
    // the same reason it is not allowed to decide which row gets a badge.
    const mounted = await mount({
      broker_refusals_unplaceable: 1,
      // A hostile claim, present in the snapshot's sibling fields only in
      // spirit: the component takes exactly one number and nothing else.
    });
    try {
      const el = mounted.container.querySelector('.broker-refusal-notice');
      assert.match(el.textContent, /refused 1 brokered callback\b/);
      assert.doesNotMatch(el.textContent, /1 brokered callbacks/);
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('a quiet daemon shows nothing', async () => {
    for (const snapshot of [{ broker_refusals_unplaceable: 0 }, {}, null, undefined]) {
      const mounted = await mount(snapshot);
      try {
        assert.equal(mounted.container.querySelector('.broker-refusal-notice'), null);
      } finally {
        await mounted.unmount();
      }
    }
  });
});
