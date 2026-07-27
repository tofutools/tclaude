// Unit tests for the Groups tab's per-agent sandbox badge
// (dashboard/js/groups-member-table.js SandboxBadge), run with Node's built-in
// test runner via the shared Preact harness.
//
// The glyph decision still uses the resolved launch verdict: Claude Code's
// `inherit` mode alone cannot say whether a sandbox is active. The tooltip is
// intentionally much smaller than that decision tree and exposes only the
// status, implementation, applied profile names, and available click action.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const CASES = [
  {
    name: 'a Claude Code inherit launch confined by operator settings',
    state: {
      harness: 'claude', sandbox_mode: 'inherit',
      os_sandbox_state: 'on', os_sandbox_source: '~/.claude/settings.json',
    },
    glyph: '🔒', danger: false,
    tooltip: 'Status: ON\nImplementation: CC\nProfile: None\nClick to temporarily disable',
  },
  {
    name: 'a forced-off Claude Code sandbox',
    state: {
      harness: 'claude', sandbox_mode: 'off',
      os_sandbox_state: 'off', os_sandbox_source: 'this launch',
    },
    glyph: '⚠', danger: true,
    tooltip: 'Status: OFF\nImplementation: CC\nProfile: None',
  },
  {
    name: 'a launch that requested on but resolved off',
    state: {
      harness: 'claude', sandbox_mode: 'on', os_sandbox_state: 'off',
      os_sandbox_source: '/etc/claude-code/managed-settings.json',
    },
    glyph: '⚠', danger: true,
    tooltip: 'Status: OFF\nImplementation: CC\nProfile: None',
  },
  {
    name: 'a launch that requested off but managed policy kept on',
    state: {
      harness: 'claude', sandbox_mode: 'off', os_sandbox_state: 'on',
      os_sandbox_source: '/etc/claude-code/managed-settings.json',
    },
    glyph: '🔒', danger: false,
    tooltip: 'Status: ON\nImplementation: CC\nProfile: None\nClick to temporarily disable',
  },
  {
    name: 'an unverified harness sandbox fails closed',
    state: {
      harness: 'claude', sandbox_mode: 'on', os_sandbox_state: 'on',
      os_sandbox_unverified: true, sandbox_implementation: 'harness-builtin',
    },
    glyph: '⚠', danger: true,
    tooltip: 'Status: OFF\nImplementation: CC\nProfile: None',
  },
  {
    name: 'a tclaude outer layer earns a lock',
    state: {
      harness: 'claude', sandbox_mode: 'off', os_sandbox_state: 'on',
      os_sandbox_unverified: true, sandbox_implementation: 'tclaude-layer',
      sandbox_profiles: [{ scope: 'global', name: 'tclaude-agent' }],
    },
    glyph: '🔒', danger: false,
    tooltip: 'Status: ON\nImplementation: TClaude\nProfile: tclaude-agent\nClick to temporarily disable',
  },
  {
    name: 'a stacked implementation uses the two-wall lock and tclaude label',
    state: {
      harness: 'claude', sandbox_mode: 'on', os_sandbox_state: 'on',
      os_sandbox_unverified: true, sandbox_implementation: 'stacked',
      sandbox_profiles: [{ scope: 'global', name: 'stacked-agents' }],
    },
    glyph: '🔒²', danger: false,
    tooltip: 'Status: ON\nImplementation: TClaude\nProfile: stacked-agents\nClick to temporarily disable',
  },
  {
    name: 'an unknown implementation earns no lock',
    state: {
      harness: 'claude', sandbox_mode: 'on', os_sandbox_state: 'on',
      sandbox_implementation: 'future-layer',
    },
    glyph: '⚠', danger: true,
    tooltip: 'Status: OFF\nImplementation: Unknown\nProfile: None',
  },
  {
    name: 'an unavailable tclaude layer reports off with its profile',
    state: {
      harness: 'claude', sandbox_mode: 'off', os_sandbox_state: 'off',
      sandbox_implementation: 'tclaude-layer',
      sandbox_profiles: [{ scope: 'global', name: 'tclaude-agent' }],
    },
    glyph: '⚠', danger: true,
    tooltip: 'Status: OFF\nImplementation: TClaude\nProfile: tclaude-agent',
  },
  {
    name: 'a legacy Claude off row remains a warning',
    profileRecorded: false,
    state: {
      harness: 'claude', sandbox_mode: 'off',
      sandbox_implementation: 'harness-builtin',
    },
    glyph: '⚠', danger: true,
    tooltip: 'Status: OFF\nImplementation: CC\nProfile: Not recorded',
  },
  {
    name: 'an unconfigured inherit launch renders no badge',
    state: {
      harness: 'claude', sandbox_mode: 'inherit', os_sandbox_state: 'unconfigured',
    },
    absent: true,
  },
  {
    name: 'a Codex workspace sandbox',
    state: {
      harness: 'codex', sandbox_mode: 'workspace-write',
    },
    glyph: '🔒', danger: false,
    tooltip: 'Status: ON\nImplementation: Codex\nProfile: None\nClick to temporarily disable',
  },
  {
    name: 'a Codex full-access launch',
    state: {
      harness: 'codex', sandbox_mode: 'danger-full-access',
    },
    glyph: '⚠', danger: true,
    tooltip: 'Status: OFF\nImplementation: Codex\nProfile: None',
  },
  {
    name: 'multiple applied profiles retain resolution order without scope detail',
    state: {
      harness: 'codex', sandbox_mode: 'workspace-write',
      sandbox_profiles: [
        { scope: 'global', name: 'base' },
        { scope: 'group', name: 'team' },
        { scope: 'explicit', name: 'tight' },
      ],
    },
    glyph: '🔒', danger: false,
    tooltip: 'Status: ON\nImplementation: Codex\nProfile: base + team + tight\nClick to temporarily disable',
  },
  {
    name: 'an offline lock stays informative but non-actionable',
    online: false,
    state: {
      harness: 'claude', sandbox_mode: 'inherit', os_sandbox_state: 'on',
    },
    glyph: '🔒', danger: false, offlineClass: true,
    tooltip: 'Status: ON\nImplementation: CC\nProfile: None',
  },
];

test('SandboxBadge keeps its posture semantics with the compact tooltip', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/dashboard.js', `
    export const lastSnapshot = { groups: [], ungrouped: [] };
    export function setLastSnapshot() {}
  `);
  const { SandboxBadge } = await harness.importDashboardModule('js/groups-member-table.js');

  for (const row of CASES) {
    await t.test(row.name, async () => {
      const state = row.profileRecorded === false
        ? row.state
        : { sandbox_profiles_recorded: true, ...row.state };
      const member = { conv_id: 'c1', online: row.online !== false, state };
      const mounted = await harness.mount(harness.html`<${SandboxBadge} member=${member} />`);
      try {
        const el = mounted.container.querySelector('.sandbox-badge');
        if (row.absent) {
          assert.equal(el, null, 'expected no badge');
          return;
        }
        assert.ok(el, 'expected a badge');
        assert.equal(el.textContent.trim(), row.glyph);
        assert.equal(el.classList.contains('sandbox-danger'), row.danger,
          `danger styling should be ${row.danger}`);
        assert.equal(el.classList.contains('runtime-meta-offline'), row.offlineClass === true,
          'offline rows are dimmed');
        assert.equal(el.getAttribute('title'), row.tooltip);
        assert.equal(el.getAttribute('aria-label'), row.tooltip,
          'the compact tooltip is also the glyph-only badge accessible name');
      } finally {
        await mounted.unmount();
      }
    });
  }
});

test('SandboxBadge shortcuts only valid temporary sandbox transitions', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/dashboard.js', `
    export const lastSnapshot = { groups: [], ungrouped: [] };
    export function setLastSnapshot() {}
  `);
  const { SandboxBadge } = await harness.importDashboardModule('js/groups-member-table.js');
  const mount = async (state, online = true) => harness.mount(harness.html`<${SandboxBadge} member=${{
    agent_id: 'agt_shortcut', conv_id: 'conv-shortcut', title: 'worker', online,
    state: { sandbox_profiles_recorded: true, ...state },
  }} />`);

  await t.test('a live lock offers the temporary disable action', async () => {
    const mounted = await mount({
      harness: 'claude', sandbox_mode: 'inherit', os_sandbox_state: 'on',
    });
    try {
      const badge = mounted.container.querySelector('.sandbox-badge');
      assert.equal(badge.getAttribute('role'), 'button');
      assert.equal(badge.getAttribute('tabindex'), '0');
      assert.equal(badge.dataset.act, 'sandbox-restart');
      assert.equal(badge.dataset.action, 'unlock');
      assert.equal(badge.dataset.agent, 'agt_shortcut');
      assert.equal(badge.title,
        'Status: ON\nImplementation: CC\nProfile: None\nClick to temporarily disable');
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('a temporary warning offers normal-configuration restoration', async () => {
    const mounted = await mount({
      harness: 'claude', sandbox_mode: 'off', os_sandbox_state: 'off',
      temporary_sandbox_mode: 'off',
    });
    try {
      const badge = mounted.container.querySelector('.sandbox-badge');
      assert.equal(badge.textContent.trim(), '⚠');
      assert.equal(badge.dataset.action, 'restore');
      assert.equal(badge.title,
        'Status: OFF\nImplementation: CC\nProfile: None\nClick to restore normal sandbox');
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('an override kept on by managed policy is still a restore action', async () => {
    const mounted = await mount({
      harness: 'claude', sandbox_mode: 'off', os_sandbox_state: 'on',
      temporary_sandbox_mode: 'off',
    });
    try {
      const badge = mounted.container.querySelector('.sandbox-badge');
      assert.equal(badge.textContent.trim(), '🔒');
      assert.equal(badge.dataset.action, 'restore');
      assert.equal(badge.title,
        'Status: ON\nImplementation: CC\nProfile: None\nClick to restore normal sandbox');
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('an ordinary unconfined warning is information, not an action', async () => {
    const mounted = await mount({
      harness: 'codex', sandbox_mode: 'danger-full-access',
    });
    try {
      const badge = mounted.container.querySelector('.sandbox-badge');
      assert.equal(badge.getAttribute('role'), 'note');
      assert.equal(badge.hasAttribute('data-act'), false);
      assert.equal(badge.hasAttribute('tabindex'), false);
      assert.equal(badge.title, 'Status: OFF\nImplementation: Codex\nProfile: None');
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('an offline lock remains non-actionable', async () => {
    const mounted = await mount({
      harness: 'claude', sandbox_mode: 'on', os_sandbox_state: 'on',
    }, false);
    try {
      const badge = mounted.container.querySelector('.sandbox-badge');
      assert.equal(badge.getAttribute('role'), 'note');
      assert.equal(badge.hasAttribute('data-act'), false);
    } finally {
      await mounted.unmount();
    }
  });
});

test('the sandbox glyph rides the harness line, left of the remote indicator', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/dashboard.js', `
    export const lastSnapshot = { groups: [], ungrouped: [] };
    export function setLastSnapshot() {}
  `);
  const { HarnessLine } = await harness.importDashboardModule('js/groups-member-table.js');

  const mount = async (member) => harness.mount(harness.html`<${HarnessLine} member=${member} />`);
  const confined = {
    harness: 'claude', model: 'Opus 4.8 (1M context)', effort_level: 'high',
    sandbox_mode: 'inherit', os_sandbox_state: 'on',
  };

  await t.test('it sits inside the harness line, after the effort token', async () => {
    const mounted = await mount({ conv_id: 'c1', online: true, state: confined });
    try {
      const line = mounted.container.querySelector('.agent-harness');
      assert.ok(line, 'expected a harness line');
      assert.ok(line.querySelector('.sandbox-badge'), 'the glyph belongs to the harness line');
      assert.equal(mounted.container.querySelectorAll('.sandbox-badge').length, 1);
      const text = line.textContent.replace(/\s+/g, ' ').trim();
      assert.match(text, /high\s*🔒/, 'the glyph trails the effort token');
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('sandbox sits left of the remote indicator when both show', async () => {
    const state = { ...confined, remote_control: true };
    const mounted = await mount({ conv_id: 'c1', online: true, state });
    try {
      const glyphs = [...mounted.container.querySelectorAll('.sandbox-badge, .remote-badge')];
      assert.deepEqual(glyphs.map((el) => el.className.split(' ')[0]),
        ['sandbox-badge', 'remote-badge']);
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('a pre-tick Codex row still shows its unconfined warning', async () => {
    const state = { harness: 'codex', sandbox_mode: 'danger-full-access' };
    const mounted = await mount({ conv_id: 'c1', online: true, state });
    try {
      const el = mounted.container.querySelector('.agent-harness .sandbox-badge');
      assert.ok(el, 'expected the unconfined warning on a pre-tick Codex row');
      assert.equal(el.textContent.trim(), '⚠');
      assert.match(mounted.container.querySelector('.agent-harness').textContent, /Codex\s*⚠/);
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('a pre-tick row with no model still shows its sandbox verdict', async () => {
    const state = {
      harness: 'claude', sandbox_mode: 'off', os_sandbox_state: 'off',
    };
    const mounted = await mount({ conv_id: 'c1', online: true, state });
    try {
      const el = mounted.container.querySelector('.agent-harness .sandbox-badge');
      assert.ok(el, 'expected the unconfined warning on a pre-tick row');
      assert.equal(el.textContent.trim(), '⚠');
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('a quiet pre-tick row still renders no line at all', async () => {
    const state = {
      harness: 'claude', sandbox_mode: 'inherit', os_sandbox_state: 'unconfigured',
    };
    const mounted = await mount({ conv_id: 'c1', online: true, state });
    try {
      assert.equal(mounted.container.querySelector('.agent-harness'), null);
    } finally {
      await mounted.unmount();
    }
  });
});
