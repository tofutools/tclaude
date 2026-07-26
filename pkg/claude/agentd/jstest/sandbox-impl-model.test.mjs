import test from 'node:test';
import assert from 'node:assert/strict';
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
};

const harnesses = [
  { name: 'claude', display_name: 'Claude Code', models: ['sonnet'], can_tclaude_layer: true },
  { name: 'opencode', display_name: 'OpenCode', models: [], can_tclaude_layer: false },
];

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
  assert.equal(opencode.showSandboxImpl, false);
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

  // A harness that cannot host the layer has no row, so it has no hint either.
  const oc = model.spawnCapabilityView({ harness: 'opencode' }, context);
  assert.equal(model.sandboxImplHintFor({ sandboxImpl: 'tclaude-layer' }, oc), null);
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

  // A harness that cannot host the layer never sends the field, even if a stale
  // draft value survived — it would be a 400 the operator never typed.
  const oc = model.buildSpawnRequest(
    { ...base, harness: 'opencode', sandboxImpl: 'tclaude-layer' }, context, null,
  );
  assert.equal('sandbox_implementation' in oc.body, false);
});

test('switching to a harness without the layer clears the selection', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = { harnesses, sandboxImpl, groups: [{ name: 'crew' }] };

  const switched = model.selectSpawnHarness(
    { group: 'crew', harness: 'claude', sandboxImpl: 'tclaude-layer' }, 'opencode', context,
  );
  assert.equal(switched.sandboxImpl, '');
});
