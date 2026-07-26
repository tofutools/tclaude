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
// The badge is now a bare glyph on the harness line rather than a framed chip
// on a line of its own, so the mode/verdict LABEL moved into the tooltip. That
// makes the tooltip assertions load-bearing: they are the only place an
// operator can still read which mode was requested and what decided it.
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
    glyph: '🔒', danger: false,
    // The deciding file is the operator's answer to "why is this on?", and with
    // the on-screen label gone the tooltip is the only place it can be read.
    title: [/^Sandbox: on —/, /inherited from your Claude Code settings/, /~\/\.claude\/settings\.json/],
  },
  {
    name: 'an explicitly forced-on sandbox says so rather than claiming inheritance',
    state: {
      harness: 'claude', sandbox_mode: 'on',
      os_sandbox_state: 'on', os_sandbox_source: 'this launch (sandbox `on`)',
    },
    glyph: '🔒', danger: false,
    title: [/^Sandbox: on —/, /forced ON for this launch/], titleNot: [/inherited/],
  },
  {
    name: 'a forced-off sandbox is flagged as a danger, not dressed up with a padlock',
    state: {
      harness: 'claude', sandbox_mode: 'off',
      os_sandbox_state: 'off', os_sandbox_source: 'this launch (sandbox `off`)',
    },
    glyph: '⚠', danger: true, title: [/^Sandbox: off —/, /runs unconfined/],
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
    glyph: '⚠', danger: true,
    // Opens with the posture that WON: an operator reading "on" first would take
    // away the opposite of the truth, which is the whole point of this case.
    title: [/^Sandbox: off — this launch asked for the OS sandbox to be ON/, /managed-settings\.json/],
    titleNot: [/^Sandbox: on/],
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
    glyph: '🔒', danger: false,
    title: [/^Sandbox: on —/, /forced ON by/, /overriding this launch's `off`/],
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
    glyph: '⚠', danger: true,
    // The `on?` the chip used to print survives as the opening hedge.
    title: [/^Sandbox: on \(unverified\) —/, /Unverified: tclaude could not read a settings file that outranks this/],
    titleNot: [/Bash is confined/],
  },
  {
    name: 'a verified on keeps the plain padlock and the confinement claim',
    state: {
      harness: 'claude', sandbox_mode: 'inherit', os_sandbox_state: 'on',
      os_sandbox_source: '~/.claude/settings.json', os_sandbox_unverified: false,
    },
    glyph: '🔒', danger: false, title: [/^Sandbox: on —/, /Bash is confined/],
    titleNot: [/Unverified/],
  },
  {
    // A pre-verdict Claude row: `off` is Claude-only and means the sandbox is
    // disabled outright, so the padlock was wrong there for the same reason.
    name: 'a legacy Claude off row with no verdict is a danger badge too',
    state: { harness: 'claude', sandbox_mode: 'off' },
    glyph: '⚠', danger: true, title: [/^Sandbox: off —/],
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
    // Deliberate asymmetry: the hedge rides an ON verdict, where a padlock would
    // over-claim. An unconfigured/off verdict already renders nothing, and the
    // no-badge reading is the SAFE direction — so an unreadable higher tier adds
    // no badge here rather than inventing a warning about an agent tclaude
    // already reports as unconfined. Pinned so a later refactor does not surface
    // one by accident.
    name: 'an unverified unconfigured verdict still renders nothing',
    state: {
      harness: 'claude', sandbox_mode: 'inherit',
      os_sandbox_state: 'unconfigured', os_sandbox_unverified: true,
    },
    absent: true,
  },
  {
    name: 'a legacy Claude off row does not borrow Codex\'s "full access" wording',
    state: { harness: 'claude', sandbox_mode: 'off' },
    glyph: '⚠', danger: true,
    title: [/the OS sandbox is disabled for this launch/],
    titleNot: [/full access/],
  },
  {
    name: 'a pre-column Claude row with no verdict and no explicit mode renders nothing',
    state: { harness: 'claude', sandbox_mode: '' },
    absent: true,
  },
  {
    name: 'a Codex row keeps its mode-driven badge',
    state: { harness: 'codex', sandbox_mode: 'workspace-write' },
    glyph: '🔒', danger: false,
    // The mode itself is no longer printed, so the tooltip has to name it.
    title: [/^Sandbox: workspace-write —/],
  },
  {
    name: 'a Codex full-access row keeps its mode-driven danger badge',
    state: { harness: 'codex', sandbox_mode: 'danger-full-access' },
    glyph: '⚠', danger: true, title: [/^Sandbox: danger-full-access —/],
  },
  {
    // The tooltip named the settings file that ENABLED the sandbox and stopped
    // there, which reads as the whole configuration — while the rules the agent
    // actually runs under came from a profile it never mentioned.
    name: 'an applied sandbox profile is named, with the tier it came from',
    state: {
      harness: 'claude', sandbox_mode: 'inherit',
      os_sandbox_state: 'on', os_sandbox_source: '~/.claude/settings.json',
      sandbox_profiles: [{ scope: 'global', name: 'tclaude-agent' }],
      sandbox_profiles_recorded: true,
    },
    glyph: '🔒', danger: false,
    title: [/^Sandbox: on —/, /Rules: tclaude sandbox profile “tclaude-agent” \(global default\)/],
  },
  {
    name: 'every applied tier is named, in resolution order',
    state: {
      harness: 'claude', sandbox_mode: 'on',
      os_sandbox_state: 'on', os_sandbox_source: 'this launch (sandbox `on`)',
      sandbox_profiles: [
        { scope: 'global', name: 'tclaude-agent' },
        { scope: 'group', name: 'squad-tight' },
      ],
      sandbox_profiles_recorded: true,
    },
    glyph: '🔒', danger: false,
    title: [/“tclaude-agent” \(global default\) \+ “squad-tight” \(group default\)/],
  },
  {
    // The profile is orthogonal to the state: for Claude Code its filesystem
    // grants ride the harness's own sandbox settings, so they bite only while
    // the sandbox is enabled, while its environment entries are plain env vars
    // that apply either way. Saying "rules: X" over an OFF sandbox would claim
    // containment that nothing enforces.
    name: 'a profile over a disabled sandbox says which half still applies',
    state: {
      harness: 'claude', sandbox_mode: 'off',
      os_sandbox_state: 'off', os_sandbox_source: 'this launch (sandbox `off`)',
      sandbox_profiles: [{ scope: 'global', name: 'tclaude-agent' }],
      sandbox_profiles_recorded: true,
    },
    glyph: '⚠', danger: true,
    title: [/still sets this agent's environment/, /not enforced while the sandbox is off/],
    titleNot: [/^Rules:/, / Rules: /],
  },
  {
    name: 'a launch that resolved to no profile reports the absence',
    state: {
      harness: 'claude', sandbox_mode: 'inherit',
      os_sandbox_state: 'on', os_sandbox_source: '~/.claude/settings.json',
      sandbox_profiles_recorded: true,
    },
    glyph: '🔒', danger: false,
    title: [/No tclaude sandbox profile applied\./],
  },
  {
    // A row older than the policy snapshot never observed an absence. Reporting
    // one would be a fresh piece of misinformation in place of the old one.
    name: 'a row with no recorded policy stays silent about profiles',
    state: {
      harness: 'claude', sandbox_mode: 'inherit',
      os_sandbox_state: 'on', os_sandbox_source: '~/.claude/settings.json',
    },
    glyph: '🔒', danger: false,
    titleNot: [/sandbox profile/],
  },
  {
    // The shape claim describes the block tclaude emits for `on`. Under
    // `inherit` the rules are the operator's settings plus whatever profile
    // applied, so asserting it there describes a block this launch never used.
    name: 'the confinement shape is claimed only for the block tclaude itself emits',
    state: {
      harness: 'claude', sandbox_mode: 'inherit',
      os_sandbox_state: 'on', os_sandbox_source: '~/.claude/settings.json',
    },
    glyph: '🔒', danger: false,
    title: [/Bash is confined\./], titleNot: [/working dir writable/],
  },
  {
    name: 'a mode-driven row names its profile too',
    state: {
      harness: 'codex', sandbox_mode: 'workspace-write',
      sandbox_profiles: [{ scope: 'explicit', name: 'tight' }],
      sandbox_profiles_recorded: true,
    },
    glyph: '🔒', danger: false,
    title: [/Rules: tclaude sandbox profile “tight” \(chosen for this agent\)/],
  },
  {
    name: 'a full-access mode-driven row does not claim its profile is enforced',
    state: {
      harness: 'codex', sandbox_mode: 'danger-full-access',
      sandbox_profiles: [{ scope: 'explicit', name: 'tight' }],
      sandbox_profiles_recorded: true,
    },
    glyph: '⚠', danger: true,
    title: [/not enforced while the sandbox is off/], titleNot: [/Rules:/],
  },
  {
    name: 'an offline agent labels its verdict as last-used',
    online: false,
    state: {
      harness: 'claude', sandbox_mode: 'inherit',
      os_sandbox_state: 'on', os_sandbox_source: '~/.claude/settings.json',
    },
    glyph: '🔒', danger: false, offlineClass: true, title: [/^Last used sandbox: on —/],
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
        // Glyph ONLY: the mode/verdict label the chip used to print is now the
        // tooltip's job, so a row costs one glyph of width and no extra line.
        assert.equal(el.textContent.trim(), row.glyph);
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

// Placement: the sandbox glyph rides on the harness line itself, packed with
// the other trailing indicators, instead of the framed chip that used to own a
// line under the control cell. A row that spends a second line per agent to
// say "🔒" does not survive a group of ten.
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
    sandbox_mode: 'inherit', os_sandbox_state: 'on', os_sandbox_source: '~/.claude/settings.json',
  };

  await t.test('it sits inside the harness line, after the effort token', async () => {
    const mounted = await mount({ conv_id: 'c1', online: true, state: confined });
    try {
      const line = mounted.container.querySelector('.agent-harness');
      assert.ok(line, 'expected a harness line');
      assert.ok(line.querySelector('.sandbox-badge'), 'the glyph belongs to the harness line');
      // No stray second line under the cell — the glyph is the whole surface.
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
    // The no-model branches are per-harness, so each has to carry the glyph on
    // its own now that it rides the line rather than the cell. A fresh
    // danger-full-access Codex agent must warn before its first tick, not after.
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
    // The verdict is recorded at LAUNCH, so it is known before the first
    // statusline hook reports a model. Folding the glyph into the harness line
    // must not make it wait for one.
    const state = {
      harness: 'claude', sandbox_mode: 'off',
      os_sandbox_state: 'off', os_sandbox_source: 'this launch (sandbox `off`)',
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
    const state = { harness: 'claude', sandbox_mode: 'inherit', os_sandbox_state: 'unconfigured' };
    const mounted = await mount({ conv_id: 'c1', online: true, state });
    try {
      assert.equal(mounted.container.querySelector('.agent-harness'), null);
    } finally {
      await mounted.unmount();
    }
  });
});
