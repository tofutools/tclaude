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
    title: [/^Sandbox: on —/, /forced ON by this launch/], titleNot: [/inherited/],
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
      sandbox_implementation: 'harness-builtin',
    },
    glyph: '⚠', danger: true,
    // The `on?` the chip used to print survives as the opening hedge.
    title: [/^Sandbox: on \(unverified\) —/, /Unverified: tclaude could not read a settings file that outranks this/],
    titleNot: [/Bash is confined/],
  },
  {
    name: 'the Darwin tclaude layer exposes its Seatbelt-specific partial fidelity',
    state: {
      harness: 'claude', sandbox_mode: 'off', os_sandbox_state: 'on',
      sandbox_implementation: 'tclaude-layer',
      os_sandbox_source: 'tclaude-layer (Seatbelt/sandbox-exec; filesystem policy enforced; host network and ambient Unix sockets reachable; no mount namespace; hidden paths remain enumerable)',
      os_sandbox_unverified: true,
    },
    glyph: '🔒', danger: false,
    title: [/^Sandbox: on \(unverified\) —/, /Partial fidelity: Seatbelt enforces filesystem operations/,
      /hidden paths remain enumerable/, /host network plus ambient Unix sockets remain reachable/],
    titleNot: [/filesystem mounts are enforced/, /could not read a settings file/],
  },
  {
    name: 'the Darwin isolated tclaude layer reports its platform deltas without claiming host network access',
    state: {
      harness: 'claude', sandbox_mode: 'off', os_sandbox_state: 'on',
      sandbox_implementation: 'tclaude-layer',
      os_sandbox_source: 'tclaude-layer (Seatbelt/sandbox-exec; filesystem policy enforced; isolated network; host loopback/IDE bridge unavailable; agentd socket allowlisted; no PID isolation; no constructed root; hidden paths remain enumerable)',
      os_sandbox_unverified: true,
    },
    glyph: '🔒', danger: false,
    title: [/^Sandbox: on \(unverified\) —/, /Partial fidelity: Seatbelt enforces filesystem and network operations/,
      /no PID isolation or constructed root/, /hidden paths remain enumerable/],
    titleNot: [/host network plus ambient Unix sockets remain reachable/, /could not read a settings file/],
  },
  {
    name: 'the OpenCode executor layer reports the split server and attach boundary',
    state: {
      harness: 'opencode', sandbox_mode: 'tclaude-layer', os_sandbox_state: 'on',
      os_sandbox_source: 'tclaude-layer (bubblewrap; OpenCode tool-executing server confined; attach pane outside the boundary; loopback control plane reachable; host network and ambient host Unix sockets reachable)',
      os_sandbox_unverified: true,
    },
    glyph: '⚠', danger: true,
    title: [/^Sandbox: on \(unverified\) —/, /tool-executing server/,
      /attach pane stays outside/, /loopback control plane remains reachable/,
      /host networking plus ambient host Unix sockets remain available/],
    titleNot: [/could not read a settings file/],
  },
  {
    name: 'the experimental tclaude layer exposes its partial socket fidelity',
    state: {
      harness: 'claude', sandbox_mode: 'off', os_sandbox_state: 'on',
      sandbox_implementation: 'tclaude-layer',
      os_sandbox_source: 'tclaude-layer (bubblewrap; ambient host Unix sockets reachable)',
      os_sandbox_unverified: true,
      sandbox_profiles: [{ scope: 'global', name: 'tclaude-agent' }],
      sandbox_profiles_recorded: true,
    },
    glyph: '🔒', danger: false,
    title: [/^Sandbox: on \(unverified\) —/, /Partial fidelity: filesystem mounts are enforced/,
      /ambient host Unix sockets remain connectable/,
      /Its filesystem rules are enforced as OS mounts by the tclaude layer \(the inner harness sandbox is off by design\)/,
      /any environment entries it defines also apply/],
    titleNot: [/Bash is confined/, /could not read a settings file/, /not in force/],
  },
  {
    name: 'a stacked or unknown implementation does not inherit the tclaude-layer lock',
    state: {
      harness: 'claude', sandbox_mode: 'off', os_sandbox_state: 'on',
      sandbox_implementation: 'stacked',
      os_sandbox_source: 'stacked sandbox implementation',
    },
    glyph: '⚠', danger: true,
    title: [/^Sandbox: on —/],
  },
  {
    name: 'an unavailable tclaude layer does not claim its profile mounts are enforced',
    state: {
      harness: 'claude', sandbox_mode: 'off', os_sandbox_state: 'off',
      sandbox_implementation: 'tclaude-layer',
      os_sandbox_source: 'tclaude-layer unavailable',
      sandbox_profiles: [{ scope: 'global', name: 'tclaude-agent' }],
      sandbox_profiles_recorded: true,
    },
    glyph: '⚠', danger: true,
    title: [
      /^Sandbox: off —/,
      /Its filesystem rules are not in force \(the tclaude layer is not active\)/,
    ],
    titleNot: [/enforced as OS mounts/, /environment entries/],
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
    state: {
      harness: 'claude', sandbox_mode: 'off',
      sandbox_implementation: 'harness-builtin',
    },
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
    title: [/^Sandbox: on —/, /Customized by tclaude sandbox profile “tclaude-agent” \(global default\)\./],
    // The rules ARE in force here, so no withheld-reason is appended.
    titleNot: [/not in force/],
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
    title: [
      /Customized by tclaude sandbox profile “tclaude-agent” \(global default\)\./,
      /Its filesystem rules are not in force \(this launch requested sandbox `off`/,
      // Deliberately conditional: the snapshot carries profile NAMES only, so
      // the browser cannot know whether this profile defines any environment
      // entries. Asserting that it "sets this agent's environment" would be the
      // same unfounded claim, pointed the other way.
      /any environment entries it defines still apply/,
    ],
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
    title: [/Customized by tclaude sandbox profile “tight” \(chosen for this agent\)\./],
    titleNot: [/not in force/],
  },
  {
    // Pins the JS branch rather than a reachable production row: a Codex
    // danger-full-access spawn suppresses the profile tiers outright (see the
    // case below), so it cannot in practice arrive carrying one. The branch
    // still has to be right for any mode-driven row that does.
    name: 'a full-access mode-driven row does not claim its profile is enforced',
    state: {
      harness: 'codex', sandbox_mode: 'danger-full-access',
      sandbox_profiles: [{ scope: 'explicit', name: 'tight' }],
      sandbox_profiles_recorded: true,
    },
    glyph: '⚠', danger: true,
    title: [/Its filesystem rules are not in force \(the sandbox is off\)/],
  },
  {
    // What that row ACTUALLY records. "No profile applied" would be true but
    // would read as "nobody configured one", when the launch mode is what threw
    // them away — a Codex raw --sandbox opt-out cannot carry the managed
    // permission profile that renders filesystem rules.
    name: 'a launch mode that suppresses the profile tiers says so',
    state: {
      harness: 'codex', sandbox_mode: 'danger-full-access',
      sandbox_profiles_recorded: true, sandbox_profiles_omitted: true,
    },
    glyph: '⚠', danger: true,
    title: [/tclaude sandbox profiles do not apply under this launch mode\./],
    titleNot: [/No tclaude sandbox profile applied/],
  },
  {
    // The OTHER producer of the same flag, and the one that is far more common:
    // the operator picked sandbox profile "none" in the spawn dialog (or passed
    // --omit-sandbox-profiles). Blaming the launch MODE there tells them their
    // mode overrode a choice they made themselves — under `on`, a mode that
    // supports profiles perfectly well.
    name: 'a caller who omitted the profiles is not told the mode did it',
    state: {
      harness: 'claude', sandbox_mode: 'on', os_sandbox_state: 'on',
      os_sandbox_source: 'this launch (sandbox `on`)',
      sandbox_profiles_recorded: true, sandbox_profiles_omitted: true,
    },
    glyph: '🔒', danger: false,
    title: [/No tclaude sandbox profile — this launch omitted them\./],
    titleNot: [/under this launch mode/],
  },
  {
    // Two applied tiers, rules withheld: "Its filesystem rules" reads as one
    // profile's when the clause just named two.
    name: 'two withheld profiles read as plural',
    state: {
      harness: 'claude', sandbox_mode: 'off',
      os_sandbox_state: 'off', os_sandbox_source: 'this launch (sandbox `off`)',
      sandbox_profiles: [
        { scope: 'global', name: 'tclaude-agent' },
        { scope: 'group', name: 'squad-tight' },
      ],
      sandbox_profiles_recorded: true,
    },
    glyph: '⚠', danger: true,
    title: [/Their filesystem rules are not in force/, /any environment entries they define still apply/],
    titleNot: [/Its filesystem rules/],
  },
  {
    name: 'two tclaude-layer profiles report their outer-mount enforcement in the plural',
    state: {
      harness: 'claude', sandbox_mode: 'off',
      sandbox_implementation: 'tclaude-layer',
      os_sandbox_state: 'on',
      os_sandbox_source: 'tclaude-layer (bubblewrap; ambient host Unix sockets reachable)',
      os_sandbox_unverified: true,
      sandbox_profiles: [
        { scope: 'global', name: 'tclaude-agent' },
        { scope: 'group', name: 'squad-tight' },
      ],
      sandbox_profiles_recorded: true,
    },
    glyph: '🔒', danger: false,
    title: [
      /Their filesystem rules are enforced as OS mounts by the tclaude layer/,
      /any environment entries they define also apply/,
    ],
    titleNot: [/Its filesystem rules/, /not in force/],
  },
  {
    // The inverted failure: managed policy forces the sandbox ON over a launch
    // that asked for `off`. The sandbox IS on — but tclaude emitted
    // {"sandbox":{"enabled":false}} for that launch and, with it, none of the
    // profile's filesystem keys (claudeSettingsJSON skips every one of them for
    // an `off` mode). What confines this agent is the managed/operator settings,
    // NOT the profile, so claiming the profile's rules are in force would be the
    // same false account of the configuration this whole surface exists to fix.
    name: 'a profile on an off-launch forced ON by policy does not claim its rules',
    state: {
      harness: 'claude', sandbox_mode: 'off', os_sandbox_state: 'on',
      os_sandbox_source: '/etc/claude-code/managed-settings.json (managed policy)',
      sandbox_profiles: [{ scope: 'global', name: 'tclaude-agent' }],
      sandbox_profiles_recorded: true,
    },
    glyph: '🔒', danger: false,
    title: [
      /Customized by tclaude sandbox profile “tclaude-agent” \(global default\)\./,
      /Its filesystem rules are not in force \(this launch requested sandbox `off`/,
    ],
  },
  {
    // The hedge two lines up already drops "Bash is confined" because the
    // posture is unproven. A rules-in-force claim under the same doubt would
    // re-assert exactly what the caveat says tclaude could not establish.
    name: 'an unverified verdict names its profile without claiming its rules are in force',
    state: {
      harness: 'claude', sandbox_mode: 'inherit', os_sandbox_state: 'on',
      os_sandbox_source: '~/.claude/settings.json', os_sandbox_unverified: true,
      sandbox_profiles: [{ scope: 'global', name: 'tclaude-agent' }],
      sandbox_profiles_recorded: true,
    },
    glyph: '⚠', danger: true,
    title: [/^Sandbox: on \(unverified\)/, /Customized by tclaude sandbox profile “tclaude-agent”/],
    // No claim either way: there is no established reason to report, and
    // asserting enforcement is what the hedge exists to prevent.
    titleNot: [/not in force/, /Bash is confined/],
  },
  {
    // The provenance half. `sandbox: on` reaches a launch either because
    // someone typed it or because a spawn profile they never opened carried it;
    // the verdict is identical, so the recorded source is the only thing that
    // can tell them apart. Attributing a default profile's choice to "this
    // launch" credits the operator with a decision they did not make.
    name: 'a sandbox forced on by a default spawn profile names that profile',
    state: {
      harness: 'claude', sandbox_mode: 'on', os_sandbox_state: 'on',
      os_sandbox_source: 'global default profile "agents" (sandbox `on`)',
    },
    glyph: '🔒', danger: false,
    title: [/forced ON by global default profile "agents" \(sandbox `on`\)/],
    titleNot: [/forced ON by this launch/],
  },
  {
    // The mirror image, and the one that matters more: a default profile can
    // opt an agent OUT of containment just as silently. "Explicit opt-in" told
    // the operator a human had decided that.
    name: 'a sandbox forced off by a default spawn profile names that profile',
    state: {
      harness: 'claude', sandbox_mode: 'off', os_sandbox_state: 'off',
      os_sandbox_source: 'group default profile "loose" (sandbox `off`)',
    },
    glyph: '⚠', danger: true,
    title: [/forced OFF by group default profile "loose" \(sandbox `off`\)/],
    titleNot: [/Explicit opt-in/],
  },
  {
    // A harness whose MODE is its posture records no verdict, so there is no
    // os_sandbox_source to fold the chooser into — sandbox_mode_source is the
    // only place its attribution can come from.
    name: 'a mode-driven row names the tier that chose its mode',
    state: {
      harness: 'codex', sandbox_mode: 'danger-full-access',
      sandbox_mode_source: 'global default profile "wide-open"',
    },
    glyph: '⚠', danger: true,
    title: [/Chosen by global default profile "wide-open"\./],
    titleNot: [/Explicit opt-in/],
  },
  {
    // Nothing recorded stays silent rather than crediting anyone.
    name: 'a mode-driven row with no recorded chooser credits nobody',
    state: { harness: 'codex', sandbox_mode: 'danger-full-access' },
    glyph: '⚠', danger: true,
    titleNot: [/Chosen by/, /Explicit opt-in/],
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

test('SandboxBadge shortcuts only valid temporary sandbox transitions', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/dashboard.js', `
    export const lastSnapshot = { groups: [], ungrouped: [] };
    export function setLastSnapshot() {}
  `);
  const { SandboxBadge } = await harness.importDashboardModule('js/groups-member-table.js');
  const mount = async (state, online = true) => harness.mount(harness.html`<${SandboxBadge} member=${{
    agent_id: 'agt_shortcut', conv_id: 'conv-shortcut', title: 'worker', online, state,
  }} />`);

  await t.test('a live lock offers the temporary unlock action', async () => {
    const mounted = await mount({
      harness: 'claude', sandbox_mode: 'inherit',
      os_sandbox_state: 'on', os_sandbox_source: '~/.claude/settings.json',
    });
    try {
      const badge = mounted.container.querySelector('.sandbox-badge');
      assert.equal(badge.getAttribute('role'), 'button');
      assert.equal(badge.getAttribute('tabindex'), '0');
      assert.equal(badge.dataset.act, 'sandbox-restart');
      assert.equal(badge.dataset.action, 'unlock');
      assert.equal(badge.dataset.agent, 'agt_shortcut');
      assert.match(badge.title, /Click to stop and restart this agent with its sandbox temporarily disabled/);
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('the temporary warning offers restoration', async () => {
    const mounted = await mount({
      harness: 'claude', sandbox_mode: 'off',
      os_sandbox_state: 'off', os_sandbox_source: 'temporary sandbox override',
      temporary_sandbox_mode: 'off',
    });
    try {
      const badge = mounted.container.querySelector('.sandbox-badge');
      assert.equal(badge.textContent.trim(), '⚠');
      assert.equal(badge.dataset.action, 'restore');
      assert.match(badge.title, /preserved normal sandbox configuration/);
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('a temporary override kept on by managed policy is still a restore lock', async () => {
    const mounted = await mount({
      harness: 'claude', sandbox_mode: 'off',
      os_sandbox_state: 'on',
      os_sandbox_source: '/etc/claude-code/managed-settings.json (managed policy)',
      temporary_sandbox_mode: 'off',
    });
    try {
      const badge = mounted.container.querySelector('.sandbox-badge');
      assert.equal(badge.textContent.trim(), '🔒');
      assert.equal(badge.dataset.action, 'restore');
      assert.match(badge.title, /preserved normal sandbox configuration/);
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('an ordinary unconfined warning is information, not an unlock action', async () => {
    const mounted = await mount({
      harness: 'codex', sandbox_mode: 'danger-full-access',
    });
    try {
      const badge = mounted.container.querySelector('.sandbox-badge');
      assert.equal(badge.getAttribute('role'), 'note');
      assert.equal(badge.hasAttribute('data-act'), false);
      assert.equal(badge.hasAttribute('tabindex'), false);
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('an offline lock remains non-actionable', async () => {
    const mounted = await mount({
      harness: 'claude', sandbox_mode: 'on',
      os_sandbox_state: 'on', os_sandbox_source: 'this launch',
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
