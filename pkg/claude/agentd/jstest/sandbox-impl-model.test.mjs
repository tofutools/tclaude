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
    { value: 'harness-builtin', label: '{harness} built-in', descr: 'Current behavior: {harness} owns containment.' },
    { value: 'tclaude-layer', label: 'tclaude built-in OS sandbox (experimental)', experimental: true, descr: 'Linux only' },
    { value: 'stacked', label: 'Stacked: tclaude + {harness} (experimental)', experimental: true },
  ],
  default: 'harness-builtin',
  host_available: true,
  server_host_available: true,
  stacked: {
    claude: { available: true, executable_identity: '/bin/srt|1' },
    opencode: { available: false, unavailable_reason: 'no reviewed nested OS-sandbox contract' },
    unsupported: { available: false, unavailable_reason: 'no reviewed nested OS-sandbox contract' },
  },
};

const modeHelp = JSON.parse(readFileSync(new URL('./mode-help-fixture.json', import.meta.url), 'utf8'));

const harnesses = [
  {
    name: 'claude', display_name: 'Claude Code', models: ['sonnet'],
    can_tclaude_layer: true, can_stacked: true, can_builtin_os_sandbox: true,
  },
  {
    name: 'codex', display_name: 'Codex CLI', models: [],
    can_tclaude_layer: true, can_stacked: true, can_builtin_os_sandbox: true,
  },
  {
    name: 'opencode', display_name: 'OpenCode', models: [], can_tclaude_layer: true,
    can_builtin_os_sandbox: false, tclaude_layer_server_boundary: true,
  },
  { name: 'unsupported', display_name: 'Unsupported Harness', models: [], can_tclaude_layer: false },
];

test('sandbox mode help tells the truth for the selected implementation', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const off = modeHelp['claude/sandbox/off'];

  const inherited = model.sandboxModeHelpForImplementation(off, '', 'claude');
  assert.match(inherited, /comes from the resolved defaults at launch/);
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
      modeHelp['opencode/sandbox/tclaude-layer'], 'tclaude-layer', 'opencode',
    ),
    modeHelp['opencode/sandbox/tclaude-layer'],
    'OpenCode keeps its dedicated mode-help branch because its soft rules stay on',
  );
  assert.equal(
    model.sandboxModeHelpForImplementation(
      modeHelp['opencode/sandbox/tclaude-layer'], '', 'opencode',
    ),
    modeHelp['opencode/sandbox/tclaude-layer'],
    'an inherited OpenCode implementation never shadows its dedicated mode help',
  );
  assert.match(
    model.sandboxModeHelpForImplementation(off, 'stacked', 'claude'),
    /outer mounts and the harness's real nested OS sandbox both enforce/,
  );
});

test('sandbox-implementation view gates on the harness, discloses on the host', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');

  // The HARNESS half decides whether there is a choice to render at all.
  const claude = model.spawnCapabilityView({ harness: 'claude' }, { harnesses, sandboxImpl });
  assert.equal(claude.showSandboxImpl, true);
  assert.equal(claude.sandboxImplDefault, 'harness-builtin');
  assert.deepEqual(
    claude.sandboxImplOptions.map((o) => o.value),
    ['harness-builtin', 'tclaude-layer', 'stacked'],
  );

  // Read from the capability flag, not the harness NAME — so a later workstream
  // that teaches the layer a new topology needs no dashboard edit.
  const opencode = model.spawnCapabilityView({ harness: 'opencode' }, { harnesses, sandboxImpl });
  assert.equal(opencode.showSandboxImpl, true);
  assert.equal(opencode.sandboxImplHostAvailable, true);
  assert.equal(opencode.sandboxImplCanBuiltin, false);
  assert.deepEqual(
    opencode.sandboxImplOptions.map((o) => o.value),
    ['tclaude-layer', 'stacked'],
    'OpenCode must never offer a harness-builtin OS sandbox that does not exist',
  );
  // A blank row for OpenCode resolves to harness-builtin server-side, which is
  // exactly the option OpenCode does not offer. Naming it would advertise a
  // sandbox this harness cannot run, so the mapper declines to name anything and
  // the row falls back to an unnamed resolved default.
  assert.equal(
    model.sandboxImplResolvedLabel(opencode.sandboxImplOptions, 'harness-builtin'), '',
    'an answer with no matching option must not be named',
  );
});

test('the harness-owned option is named after the actual harness', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');

  // "Harness built-in" reads as if tclaude were the harness. The option must
  // name the one the operator picked.
  const claude = model.spawnCapabilityView({ harness: 'claude' }, { harnesses, sandboxImpl });
  const builtin = claude.sandboxImplOptions.find((o) => o.value === 'harness-builtin');
  assert.equal(builtin.label, 'Claude Code built-in');
  assert.equal(builtin.descr, 'Current behavior: Claude Code owns containment.');
  assert.equal(
    claude.sandboxImplOptions.find((o) => o.value === 'stacked').label,
    'Stacked: tclaude + Claude Code (experimental)',
  );
  // Options without the placeholder are passed through untouched.
  assert.equal(
    claude.sandboxImplOptions.find((o) => o.value === 'tclaude-layer').label,
    'tclaude built-in OS sandbox (experimental)',
  );

  const oc = model.spawnCapabilityView({ harness: 'opencode' }, { harnesses, sandboxImpl });
  assert.equal(oc.sandboxImplOptions.find((o) => o.value === 'harness-builtin'), undefined);

  // Defensive only: the rows are gated on a selected harness, but a missing
  // name must still produce a readable, capitalized label.
  const orphan = model.sandboxImplOptionsFor(sandboxImpl.options, '');
  assert.equal(orphan[0].label, 'The harness built-in');
  assert.equal(orphan[0].descr, 'Current behavior: the harness owns containment.');

  const codex = model.spawnCapabilityView({ harness: 'codex' }, { harnesses, sandboxImpl });
  const codexBuiltin = codex.sandboxImplOptions.find((o) => o.value === 'harness-builtin');
  // A selector option names the implementation and nothing else. The
  // filtered-network caveat is real, but it belongs on the description and the
  // hint below the row, where it can be stated in full — not squeezed into a
  // parenthetical that a closed <select> truncates.
  assert.equal(codexBuiltin.label, 'Codex CLI built-in');
  assert.doesNotMatch(codexBuiltin.label, /filtered network/,
    'option labels carry no metadata beyond which implementation is being chosen');
  assert.match(codexBuiltin.descr, /no filtered network sandbox yet/,
    'the caveat survives the label change, on the description');
  assert.match(codexBuiltin.descr, /upstream proxy is experimental and off by default/);
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

  const codexView = model.spawnCapabilityView(
    { harness: 'codex' }, { harnesses, sandboxImpl },
  );
  // The resolved default is named with the SAME label its concrete option
  // carries, so the operator can see it is one of the choices below and not a
  // fourth thing.
  assert.equal(
    model.sandboxImplResolvedLabel(codexView.sandboxImplOptions, 'harness-builtin'),
    'Codex CLI built-in',
  );
  assert.equal(model.sandboxImplResolvedLabel(codexView.sandboxImplOptions, ''), '',
    'an unresolved answer names nothing rather than guessing');
  assert.equal(model.sandboxImplHintFor({ sandboxImpl: '' }, codexView), null,
    'inherit leaves the profile chain in control and must not claim the builtin target won');
  const codexHint = model.sandboxImplHintFor(
    { sandboxImpl: 'harness-builtin' }, codexView,
  );
  assert.equal(codexHint.warn, true);
  assert.match(codexHint.text, /built-in filesystem sandbox remains available/);
  assert.match(codexHint.text, /no filtered network sandbox yet/);
  assert.match(codexHint.text, /upstream proxy is experimental and off by default/);
  assert.match(codexHint.text, /tclaude-layer filtering on Linux/);
  assert.match(codexHint.text, /network open \(Allow all\)/);

  const openCodeView = model.spawnCapabilityView(
    { harness: 'opencode' }, { harnesses, sandboxImpl },
  );
  const inheritedOpenCode = model.sandboxImplHintFor({ sandboxImpl: '' }, openCodeView);
  assert.equal(inheritedOpenCode.warn, true);
  assert.equal(
    inheritedOpenCode.text,
    'No built-in OS sandbox; access-control is a command filter, not confinement.',
  );
  const pinnedOpenCode = model.sandboxImplHintFor(
    { sandboxImpl: 'harness-builtin' }, openCodeView,
  );
  assert.match(pinnedOpenCode.text, /harness-builtin is invalid for OpenCode/);
  assert.match(pinnedOpenCode.text, /use tclaude's built-in OS sandbox or spawn with the sandbox off/);

  // Selecting the layer on a capable host explains what it DOES — never what it
  // guarantees (epic requirement 12).
  const ok = model.sandboxImplHintFor({ sandboxImpl: 'tclaude-layer' }, view);
  assert.equal(ok.warn, false);
  assert.match(ok.text, /Experimental/);

  // OpenCode has a row backed by the relay-free server capability.
  const oc = model.spawnCapabilityView({ harness: 'opencode' }, context);
  assert.equal(model.sandboxImplHintFor({ sandboxImpl: 'tclaude-layer' }, oc).warn, false);

  const stacked = model.sandboxImplHintFor({ sandboxImpl: 'stacked' }, view);
  assert.equal(stacked.warn, false);
  assert.match(stacked.text, /fresh model-free allowed\/denied round-trip/);

  const refused = model.sandboxImplHintFor({ sandboxImpl: 'stacked' }, oc);
  assert.equal(refused.warn, true);
  assert.match(refused.text, /Apply or launch will refuse/);
});

test('a likely AppArmor nested-bwrap block warns and links the guide', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = {
    harnesses,
    sandboxImpl: { ...sandboxImpl, stacked_apparmor_nested_bwrap_likely: true },
  };
  const view = model.spawnCapabilityView({ harness: 'claude' }, context);

  // The engine resolved, so per-harness availability still says AVAILABLE on
  // exactly this host. Without this branch the operator would meet the block
  // as a failed launch after a green hint.
  assert.equal(view.sandboxImplStackedAvailability.available, true);
  const hint = model.sandboxImplHintFor({ sandboxImpl: 'stacked' }, view);
  assert.equal(hint.warn, true);
  assert.match(hint.text, /likely blocked on this host/);
  assert.match(hint.text, /bwrap-userns-restrict/);
  assert.match(hint.text, /probably refuse/, 'says likely, never asserts the deny');
  assert.equal(hint.doc.href, model.SANDBOX_APPARMOR_DOC.href);
  assert.match(hint.doc.href, /#stacked-refuses-on-apparmor-restricted-hosts$/);

  // A link to a heading that no longer exists is worse than no link, so the
  // anchor is pinned to the guide rather than trusted to survive an edit.
  const guide = readFileSync(new URL('../../../../docs/sandboxing.md', import.meta.url), 'utf8');
  assert.ok(
    guide.includes('### Stacked refuses on AppArmor-restricted hosts'),
    'docs/sandboxing.md must still carry the heading the hint links to',
  );

  // The hint is scoped to stacked: the single-layer wall works on such a host,
  // so claiming otherwise under tclaude-layer would be a fresh overclaim.
  assert.equal(model.sandboxImplHintFor({ sandboxImpl: 'tclaude-layer' }, view).warn, false);
  assert.equal(model.sandboxImplHintFor({ sandboxImpl: 'tclaude-layer' }, view).doc, undefined);

  // A host without the policy keeps the plain experimental copy and no link.
  const clean = model.spawnCapabilityView({ harness: 'claude' }, { harnesses, sandboxImpl });
  const plain = model.sandboxImplHintFor({ sandboxImpl: 'stacked' }, clean);
  assert.equal(plain.warn, false);
  assert.equal(plain.doc, undefined);
});

// The hint is only useful if the link actually reaches the DOM. Both the spawn
// dialog and the profile editor render this component, so one test covers the
// surfacing for both — and would catch a call site that went back to
// interpolating plain text and silently dropped the link.
test('the rendered hint carries its documentation link', async (t) => {
  const harness = await createPreactHarness(t);
  const { SandboxImplHint } = await harness.importDashboardModule('js/sandbox-impl-hint.js');
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const view = model.spawnCapabilityView({ harness: 'claude' }, {
    harnesses,
    sandboxImpl: { ...sandboxImpl, stacked_apparmor_nested_bwrap_likely: true },
  });
  const hint = model.sandboxImplHintFor({ sandboxImpl: 'stacked' }, view);

  const rendered = await harness.mount(
    harness.html`<${SandboxImplHint} hint=${hint} id="hint" />`,
  );
  const node = rendered.container.querySelector('#hint');
  assert.ok(node.classList.contains('warn'), 'a warning hint keeps its warn styling');
  assert.match(node.textContent, /likely blocked on this host/);
  const link = node.querySelector('a');
  assert.equal(link.getAttribute('href'), model.SANDBOX_APPARMOR_DOC.href);
  assert.equal(link.getAttribute('rel'), 'noopener');
  assert.equal(link.getAttribute('target'), '_blank');
  assert.equal(link.textContent, model.SANDBOX_APPARMOR_DOC.label);

  // A hint without a doc renders no anchor at all, and no hint renders nothing.
  const plain = await harness.mount(harness.html`<${SandboxImplHint}
    hint=${{ warn: false, text: 'Experimental.' }} id="plain" />`);
  assert.equal(plain.container.querySelector('#plain a'), null);
  const absent = await harness.mount(harness.html`<${SandboxImplHint} hint=${null} />`);
  assert.equal(absent.container.textContent, '');
});

test('the AppArmor link rides along with an already-unavailable stacked reason', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = {
    harnesses,
    sandboxImpl: {
      ...sandboxImpl,
      stacked_apparmor_nested_bwrap_likely: true,
      stacked: { ...sandboxImpl.stacked, claude: { available: false, unavailable_reason: 'claude is not on PATH' } },
    },
  };
  const view = model.spawnCapabilityView({ harness: 'claude' }, context);

  const hint = model.sandboxImplHintFor({ sandboxImpl: 'stacked' }, view);
  assert.equal(hint.warn, true);
  // The concrete refusal keeps the lead. The policy is named as a SECOND wall
  // to clear rather than as an explanation of this refusal — it is not one, and
  // claiming otherwise would send the operator to change host security policy
  // over a missing binary.
  assert.match(hint.text, /claude is not on PATH/);
  assert.match(hint.text, /will likely block the nested sandbox too/);
  assert.doesNotMatch(hint.text, /likely cause/);
  assert.equal(hint.doc.href, model.SANDBOX_APPARMOR_DOC.href);
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
  assert.equal(view.sandboxImplOptions.length, 3);

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
  const stacked = model.buildSpawnRequest({ ...base, sandboxImpl: 'stacked' }, context, null);
  assert.equal(stacked.body.sandbox_implementation, 'stacked');

  // OpenCode supports the layer through its managed server topology.
  const oc = model.buildSpawnRequest(
    { ...base, harness: 'opencode', sandboxImpl: 'tclaude-layer' }, context, null,
  );
  assert.equal(oc.body.sandbox_implementation, 'tclaude-layer');

  // The browser never erases an incapable selection: the server is the refusal
  // authority and the inline warning tells the operator what apply will do.
  const unsupported = model.buildSpawnRequest(
    { ...base, harness: 'unsupported', sandboxImpl: 'stacked' }, context, null,
  );
  assert.equal(unsupported.body.sandbox_implementation, 'stacked');
});

test('the unenforced-network override is wire-sparse and fresh-dialog only', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = { harnesses, sandboxImpl, groups: [{ name: 'crew' }] };
  const draft = model.createSpawnDraft({
    groups: context.groups,
    harnesses,
    groupName: 'crew',
  });
  const base = { ...draft, name: 'w', cwd: '/repo', wtRepo: '/repo' };

  assert.equal(draft.allowUnenforcedSandbox, false);
  const plain = model.buildSpawnRequest(base, context, null);
  assert.equal('allow_unenforced_sandbox' in plain.body, false,
    'an untouched dialog sends no override');

  const allowed = model.buildSpawnRequest(
    { ...base, allowUnenforcedSandbox: true }, context, null,
  );
  assert.equal(allowed.body.allow_unenforced_sandbox, true);

  const seed = model.spawnProfileSeed(
    { ...base, allowUnenforcedSandbox: true }, context,
  );
  assert.equal('allow_unenforced_sandbox' in seed, false,
    'the one-launch authorization is never persisted into a profile');

  const switched = model.selectSpawnHarness(
    { ...base, allowUnenforcedSandbox: true }, 'codex', context,
  );
  assert.equal(switched.allowUnenforcedSandbox, false,
    'a harness switch requires a fresh explicit authorization');
});

test('switching to an incapable harness preserves stacked and keeps its warning visible', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = { harnesses, sandboxImpl, groups: [{ name: 'crew' }] };

  const switched = model.selectSpawnHarness(
    { group: 'crew', harness: 'claude', sandboxImpl: 'stacked' }, 'unsupported', context,
  );
  assert.equal(switched.sandboxImpl, 'stacked');
  const view = model.spawnCapabilityView(switched, context);
  assert.equal(view.showSandboxImpl, true);
  assert.equal(model.sandboxImplHintFor(switched, view).warn, true);
  assert.equal(model.sandboxImplClearedNoticeFor(switched), null);
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
