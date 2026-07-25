// Unit tests for the Groups tab's per-agent sandbox badge
// (dashboard/js/groups-member-table.js SandboxBadge), run with Node's built-in
// test runner via the shared Preact harness. The Go wrapper
// (jstest/dashboard_node_test.go) globs this package's `*.test.mjs`, so this
// runs under `go test ./...` and skips when node is absent.
//
// Scope: TCL-729 — the badge must describe whether the agent is ACTUALLY
// confined, not merely which sandbox mode was requested. Claude Code's default
// mode is `inherit` ("whatever settings.json says"), so a badge driven off the
// mode alone showed nothing for a sandboxed agent and nothing for an
// unsandboxed one either. The launch boundary now records a verdict
// (state.os_sandbox_state / os_sandbox_source) and the badge reads it, while a
// row without one — a pre-column session, or Codex, whose --sandbox mode IS its
// posture — keeps rendering exactly as before.
//
// One harness serves every case: materializing the dashboard module tree costs
// far more than any single assertion here.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const CASES = [
  {
    name: 'an inherit launch confined by the operator settings is badged, and names the source',
    state: {
      harness: 'claude', sandbox_mode: 'inherit',
      os_sandbox_state: 'on', os_sandbox_source: '~/.claude/settings.json',
    },
    // Rendered nothing at all before TCL-729 — the reported bug.
    text: /🔒\s*on/, danger: false,
    // The deciding file is the operator's answer to "why is this on?".
    title: [/inherited from your Claude Code settings/, /~\/\.claude\/settings\.json/],
  },
  {
    name: 'an explicitly forced-on sandbox says so rather than claiming inheritance',
    state: {
      harness: 'claude', sandbox_mode: 'on',
      os_sandbox_state: 'on', os_sandbox_source: 'this launch (sandbox `on`)',
    },
    text: /🔒\s*on/, danger: false,
    title: [/forced ON for this launch/], titleNot: [/inherited/],
  },
  {
    name: 'a forced-off sandbox is flagged as a danger, not dressed up with a padlock',
    state: {
      harness: 'claude', sandbox_mode: 'off',
      os_sandbox_state: 'off', os_sandbox_source: 'this launch (sandbox `off`)',
    },
    text: /⚠\s*off/, danger: true, title: [/runs unconfined/],
  },
  {
    // Only enterprise managed policy outranks the launch's own --settings
    // block. Believing this agent is confined is the dangerous state, so the
    // override is surfaced rather than echoing the request.
    name: 'a launch that asked for on but was overridden says the sandbox is NOT active',
    state: {
      harness: 'claude', sandbox_mode: 'on', os_sandbox_state: 'off',
      os_sandbox_source: '/etc/claude-code/managed-settings.json (managed policy)',
    },
    text: /⚠/, danger: true,
    title: [/asked for the OS sandbox to be ON/, /managed-settings\.json/],
  },
  {
    // Managed policy is the one tier that outranks the launch's own --settings
    // block, so it can force the sandbox ON over an explicit `off`. Calling that
    // "inherited from your Claude Code settings" would name an enterprise policy
    // file as the operator's own config, and "not chosen at launch" reads as
    // indifference when they actively chose the opposite.
    name: 'a managed policy that forces on over an explicit off says so',
    state: {
      harness: 'claude', sandbox_mode: 'off', os_sandbox_state: 'on',
      os_sandbox_source: '/etc/claude-code/managed-settings.json (managed policy)',
    },
    text: /🔒\s*on/, danger: false,
    title: [/forced ON by/, /overriding this launch's `off`/],
    titleNot: [/your Claude Code settings/, /not chosen at launch/],
  },
  {
    // The verdict tclaude could not prove. A padlock here would assert
    // containment that a policy file it never read may contradict.
    name: 'an unverified on hedges instead of asserting containment',
    state: {
      harness: 'claude', sandbox_mode: 'on', os_sandbox_state: 'on',
      os_sandbox_source: 'this launch (sandbox `on`)', os_sandbox_unverified: true,
    },
    text: /⚠\s*on\?/, danger: true,
    title: [/Unverified: tclaude could not read a settings file that outranks this/],
    titleNot: [/Bash is confined/],
  },
  {
    name: 'a verified on keeps the plain padlock and the confinement claim',
    state: {
      harness: 'claude', sandbox_mode: 'inherit', os_sandbox_state: 'on',
      os_sandbox_source: '~/.claude/settings.json', os_sandbox_unverified: false,
    },
    text: /🔒\s*on/, danger: false, title: [/Bash is confined/],
    titleNot: [/Unverified/],
  },
  {
    // A pre-verdict Claude row: `off` is Claude-only and means the sandbox is
    // disabled outright, so the padlock was wrong there for the same reason.
    name: 'a legacy Claude off row with no verdict is a danger badge too',
    state: { harness: 'claude', sandbox_mode: 'off' },
    text: /⚠\s*off/, danger: true,
  },
  {
    name: 'an inherit launch that nothing configures still renders no badge',
    state: { harness: 'claude', sandbox_mode: 'inherit', os_sandbox_state: 'unconfigured' },
    absent: true,
  },
  {
    name: 'an inherit launch a settings file turns off still renders no badge',
    state: { harness: 'claude', sandbox_mode: 'inherit', os_sandbox_state: 'off' },
    absent: true,
  },
  {
    name: 'a pre-column Claude row with no verdict and no explicit mode renders nothing',
    state: { harness: 'claude', sandbox_mode: '' },
    absent: true,
  },
  {
    name: 'a Codex row keeps its mode-driven badge',
    state: { harness: 'codex', sandbox_mode: 'workspace-write' },
    text: /🔒\s*workspace-write/, danger: false,
  },
  {
    name: 'a Codex full-access row keeps its mode-driven danger badge',
    state: { harness: 'codex', sandbox_mode: 'danger-full-access' },
    text: /⚠\s*danger-full-access/, danger: true,
  },
  {
    name: 'an offline agent labels its verdict as last-used',
    online: false,
    state: {
      harness: 'claude', sandbox_mode: 'inherit',
      os_sandbox_state: 'on', os_sandbox_source: '~/.claude/settings.json',
    },
    text: /🔒/, danger: false, offlineClass: true, title: [/^Last used sandbox:/],
  },
];

test('SandboxBadge describes what actually confines the agent', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/dashboard.js', `
    export const lastSnapshot = { groups: [], ungrouped: [] };
    export function setLastSnapshot() {}
  `);
  const { SandboxBadge } = await harness.importDashboardModule('js/groups-member-table.js');

  for (const row of CASES) {
    await t.test(row.name, async () => {
      const member = { conv_id: 'c1', online: row.online !== false, state: row.state };
      const mounted = await harness.mount(harness.html`<${SandboxBadge} member=${member} />`);
      try {
        const el = mounted.container.querySelector('.sandbox-badge');
        if (row.absent) {
          assert.equal(el, null, 'expected no badge');
          return;
        }
        assert.ok(el, 'expected a badge');
        assert.match(el.textContent, row.text);
        assert.equal(el.classList.contains('sandbox-danger'), row.danger,
          `danger styling should be ${row.danger}`);
        assert.equal(el.classList.contains('runtime-meta-offline'), row.offlineClass === true,
          'offline rows are dimmed');
        const title = el.getAttribute('title');
        // The tooltip is the accessible name too — a pointer-only badge would
        // strand the whole verdict behind a hover.
        assert.equal(el.getAttribute('aria-label'), title);
        for (const want of row.title || []) assert.match(title, want);
        for (const unwanted of row.titleNot || []) assert.doesNotMatch(title, unwanted);
      } finally {
        await mounted.unmount();
      }
    });
  }
});
