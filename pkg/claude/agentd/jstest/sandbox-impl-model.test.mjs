import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createPreactHarness } from './preact-harness.mjs';

// The sandbox-IMPLEMENTATION row (TCL-769) has one job the other launch rows do
// not: it must DISCLOSE that this host cannot run the experimental layer without
// ever DECIDING that the operator may not pick it. The launch-time refusal is
// the authority; a dialog that removed the option would have replaced it, and
// would also make it impossible to author a profile for a machine where bwrap is
// not installed yet.

const sandboxImpl = {
  options: [
    { value: 'harness-builtin', label: 'Harness built-in', descr: 'current behavior' },
    { value: 'tclaude-layer', label: 'tclaude layer (experimental)', experimental: true, descr: 'Linux only' },
  ],
  default: 'harness-builtin',
  host_available: true,
  server_host_available: true,
};

const modeHelp = JSON.parse(readFileSync(new URL('./mode-help-fixture.json', import.meta.url), 'utf8'));

const harnesses = [
  { name: 'claude', display_name: 'Claude Code', models: ['sonnet'], can_tclaude_layer: true },
  {
    name: 'opencode', display_name: 'OpenCode', models: [], can_tclaude_layer: true,
    tclaude_layer_server_boundary: true,
  },
  { name: 'unsupported', display_name: 'Unsupported Harness', models: [], can_tclaude_layer: false },
];

test('sandbox mode help tells the truth for the selected implementation', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const off = modeHelp['claude/sandbox/off'];

  const inherited = model.sandboxModeHelpForImplementation(off, '', 'claude');
  assert.match(inherited, /inherited from the profile chain at launch/);
  assert.match(inherited, /effect is not known yet/);
  assert.doesNotMatch(inherited, /runs unconfined|rules as OS mounts/);
  assert.equal(
    model.sandboxModeHelpForImplementation(off, 'harness-builtin', 'claude'),
    off,
    'the harness-builtin branch keeps the real mode-help fixture, including its unconfined warning',
  );
  assert.match(
    model.sandboxModeHelpForImplementation(off, 'tclaude-layer', 'claude'),
    /filesystem rules as OS mounts/,
  );
  assert.match(
    model.sandboxModeHelpForImplementation(off, 'tclaude-layer', 'codex'),
    /harness's own sandbox is off by design/,
  );
  assert.equal(
    model.sandboxModeHelpForImplementation(
      modeHelp['opencode/sandbox/off'], 'tclaude-layer', 'opencode',
    ),
    modeHelp['opencode/sandbox/off'],
    'OpenCode keeps its dedicated mode-help branch because its soft rules stay on',
  );
  assert.equal(
    model.sandboxModeHelpForImplementation(off, 'stacked', 'claude'),
    off,
    'other implementations fail closed to their own mode help',
  );
});

test('sandbox-implementation view gates on the harness, discloses on the host', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');

  // The HARNESS half decides whether there is a choice to render at all.
  const claude = model.spawnCapabilityView({ harness: 'claude' }, { harnesses, sandboxImpl });
  assert.equal(claude.showSandboxImpl, true);
  assert.equal(claude.sandboxImplDefault, 'harness-builtin');
  assert.deepEqual(claude.sandboxImplOptions.map((o) => o.value), ['harness-builtin', 'tclaude-layer']);

  // Read from the capability flag, not the harness NAME — so a later workstream
  // that teaches the layer a new topology needs no dashboard edit.
  const opencode = model.spawnCapabilityView({ harness: 'opencode' }, { harnesses, sandboxImpl });
  assert.equal(opencode.showSandboxImpl, true);
  assert.equal(opencode.sandboxImplHostAvailable, true);
});

test('sandbox-implementation hint stays silent for the default and warns honestly', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = { harnesses, sandboxImpl };
  const view = model.spawnCapabilityView({ harness: 'claude' }, context);

  // An untouched row and an explicit harness-builtin both say nothing: a hint
  // on the legacy path would be noise on every spawn.
  assert.equal(model.sandboxImplHintFor({ sandboxImpl: '' }, view), null);
  assert.equal(model.sandboxImplHintFor({ sandboxImpl: 'harness-builtin' }, view), null);

  // Selecting the layer on a capable host explains what it DOES — never what it
  // guarantees (epic requirement 12).
  const ok = model.sandboxImplHintFor({ sandboxImpl: 'tclaude-layer' }, view);
  assert.equal(ok.warn, false);
  assert.match(ok.text, /Experimental/);

  // OpenCode has a row backed by the relay-free server capability.
  const oc = model.spawnCapabilityView({ harness: 'opencode' }, context);
  assert.equal(model.sandboxImplHintFor({ sandboxImpl: 'tclaude-layer' }, oc).warn, false);
});

test('an unavailable host warns with the concrete reason and the real consequence', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = {
    harnesses,
    sandboxImpl: {
      ...sandboxImpl,
      host_available: false,
      host_unavailable_reason: 'tclaude-layer requires Linux and bubblewrap',
    },
  };
  const view = model.spawnCapabilityView({ harness: 'claude' }, context);

  // Still selectable — the row is rendered and the option list is intact.
  assert.equal(view.showSandboxImpl, true);
  assert.equal(view.sandboxImplHostAvailable, false);
  assert.equal(view.sandboxImplOptions.length, 2);

  const hint = model.sandboxImplHintFor({ sandboxImpl: 'tclaude-layer' }, view);
  assert.equal(hint.warn, true);
  assert.match(hint.text, /requires Linux and bubblewrap/, 'names the concrete missing capability');
  assert.match(hint.text, /refuse the launch, not fall back/, 'states the real consequence');
});

test('server topology does not inherit an interactive-only pidfd refusal', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = {
    harnesses,
    sandboxImpl: {
      ...sandboxImpl,
      host_available: false,
      host_unavailable_reason: 'pidfd unavailable',
      server_host_available: true,
    },
  };

  const interactive = model.spawnCapabilityView({ harness: 'claude' }, context);
  assert.equal(interactive.sandboxImplHostAvailable, false);
  assert.match(
    model.sandboxImplHintFor({ sandboxImpl: 'tclaude-layer' }, interactive).text,
    /pidfd unavailable/,
  );

  const server = model.spawnCapabilityView({ harness: 'opencode' }, context);
  assert.equal(server.sandboxImplHostAvailable, true);
  assert.equal(model.sandboxImplHintFor({ sandboxImpl: 'tclaude-layer' }, server).warn, false);
});

test('the spawn request omits an untouched row and sends an explicit one', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = { harnesses, sandboxImpl, groups: [{ name: 'crew' }] };
  const base = { group: 'crew', harness: 'claude', name: 'w', cwd: '/repo', wtRepo: '/repo' };

  // Default-off on the wire: an untouched row sends no key at all, so the
  // daemon's profile tier stack still speaks and nothing is pinned.
  const plain = model.buildSpawnRequest({ ...base, sandboxImpl: '' }, context, null);
  assert.equal('sandbox_implementation' in plain.body, false);

  // An explicit harness-builtin IS sent: pinning the legacy layer against a
  // group default that would have flipped it is a real intent, not a no-op.
  const pinned = model.buildSpawnRequest({ ...base, sandboxImpl: 'harness-builtin' }, context, null);
  assert.equal(pinned.body.sandbox_implementation, 'harness-builtin');

  const layered = model.buildSpawnRequest({ ...base, sandboxImpl: 'tclaude-layer' }, context, null);
  assert.equal(layered.body.sandbox_implementation, 'tclaude-layer');

  // OpenCode supports the layer through its managed server topology.
  const oc = model.buildSpawnRequest(
    { ...base, harness: 'opencode', sandboxImpl: 'tclaude-layer' }, context, null,
  );
  assert.equal(oc.body.sandbox_implementation, 'tclaude-layer');

  // A harness that cannot host the layer never sends a stale draft value.
  const unsupported = model.buildSpawnRequest(
    { ...base, harness: 'unsupported', sandboxImpl: 'tclaude-layer' }, context, null,
  );
  assert.equal('sandbox_implementation' in unsupported.body, false);
});

test('switching to a harness without the layer clears the selection', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = { harnesses, sandboxImpl, groups: [{ name: 'crew' }] };

  const switched = model.selectSpawnHarness(
    { group: 'crew', harness: 'claude', sandboxImpl: 'tclaude-layer' }, 'unsupported', context,
  );
  assert.equal(switched.sandboxImpl, '');
});

// A harness switch that discards a sandbox-implementation selection ALSO hides
// the row that held it — so the one control that could show the loss is gone at
// the instant there is something to show. Left silent, that is the dialog
// deciding by erasure: the server's loud refusal for an explicitly incompatible
// request is unreachable precisely because the value never reaches it.
//
// These three pin the notice that closes that hole.

test('the notice appears when a harness switch clears a selection', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = { harnesses, sandboxImpl, groups: [{ name: 'crew' }] };

  const before = { group: 'crew', harness: 'claude', sandboxImpl: 'tclaude-layer' };
  assert.equal(model.sandboxImplClearedNoticeFor(before), null, 'nothing to disclose yet');

  const after = model.selectSpawnHarness(before, 'unsupported', context);
  assert.equal(after.sandboxImpl, '', 'the incompatible value is still dropped');

  // The row is gone, which is exactly why the notice has to exist.
  const view = model.spawnCapabilityView(after, context);
  assert.equal(view.showSandboxImpl, false);

  const notice = model.sandboxImplClearedNoticeFor(after);
  assert.ok(notice, 'the loss must be disclosed even though the row is hidden');
  assert.equal(notice.warn, true);
});

test('the notice names the implementation that was dropped and the harness that dropped it', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = { harnesses, sandboxImpl, groups: [{ name: 'crew' }] };

  const after = model.selectSpawnHarness(
    { group: 'crew', harness: 'claude', sandboxImpl: 'tclaude-layer' }, 'unsupported', context,
  );
  const notice = model.sandboxImplClearedNoticeFor(after);

  assert.match(notice.text, /tclaude-layer/, 'names the implementation that was dropped');
  assert.match(notice.text, /Unsupported Harness/, 'names the harness that dropped it (display name)');
  assert.match(notice.text, /harness-builtin/, 'states what the agent will actually launch with');
});

test('the notice is gone after an explicit re-pick, and after switching back', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = { harnesses, sandboxImpl, groups: [{ name: 'crew' }] };

  const cleared = model.selectSpawnHarness(
    { group: 'crew', harness: 'claude', sandboxImpl: 'tclaude-layer' }, 'unsupported', context,
  );
  assert.ok(model.sandboxImplClearedNoticeFor(cleared), 'precondition: the notice stands');

  // Speaking for the field again retires it — the state it describes no longer holds.
  const repicked = model.setSpawnSandboxImpl(cleared, 'harness-builtin');
  assert.equal(repicked.sandboxImpl, 'harness-builtin');
  assert.equal(model.sandboxImplClearedNoticeFor(repicked), null);

  // So does an explicit clear back to "inherit" — still the operator speaking.
  assert.equal(model.sandboxImplClearedNoticeFor(model.setSpawnSandboxImpl(cleared, '')), null);

  // And so does returning to a harness that can host the layer.
  const back = model.selectSpawnHarness(cleared, 'claude', context);
  assert.equal(model.sandboxImplClearedNoticeFor(back), null);
});

test('a harness switch with nothing selected raises no notice', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = { harnesses, sandboxImpl, groups: [{ name: 'crew' }] };

  // Nothing was lost, so there is nothing to disclose — the notice must not
  // become background noise on every harness change.
  const after = model.selectSpawnHarness(
    { group: 'crew', harness: 'claude', sandboxImpl: '' }, 'unsupported', context,
  );
  assert.equal(model.sandboxImplClearedNoticeFor(after), null);
});
