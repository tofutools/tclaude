import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function mountManagement(harness, state, actions) {
  const cleanups = [];
  const host = harness.document.createElement('div');
  harness.document.body.appendChild(host);
  return harness.importDashboardModule('js/management-island.js').then(({ mountManagementIsland }) => {
    mountManagementIsland({ host, state, actions, confirmDiscard: async () => true, openProfilePermissions() {}, registerCleanup(fn) { cleanups.push(fn); } });
    return { host, unmount: () => cleanups.reverse().forEach((fn) => fn()) };
  });
}

test('sandbox summary marks break-glass and the minimal read baseline', async (t) => {
  const harness = await createPreactHarness(t);
  const { sandboxProfileSummary } = await harness.importDashboardModule('js/sandbox-profiles-data.js');
  assert.equal(sandboxProfileSummary({ name: 'plain', filesystem: [{ path: '/x', access: 'read' }] }), '1 read');
  assert.equal(
    sandboxProfileSummary({
      name: 'debug',
      filesystem: [{ path: '/x', access: 'read' }],
      read_baseline: 'minimal',
      break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'read' }, { path: '/home/op/.codex', access: 'write' }],
    }),
    '⚠ 2 break-glass · 1 read · minimal reads',
  );
});

test('sandbox manager cards render break-glass as danger and minimal reads as a chip', async (t) => {
  const harness = await createPreactHarness(t);
  const { createManagementState } = await harness.importDashboardModule('js/management-state.js');
  const state = createManagementState();
  state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [{
    name: 'debug',
    read_baseline: 'minimal',
    break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'write' }],
    filesystem: [{ path: '/work', access: 'write' }],
  }]);
  state.openManager('sandbox');
  const { host, unmount } = await mountManagement(harness, state, { load() {}, openSandboxEditor() {}, removeSandbox() {} });
  await harness.act(() => Promise.resolve());
  const dangerTag = host.querySelector('.sbx-cap-bg');
  assert.ok(dangerTag, 'break-glass rules render with their own danger tag');
  assert.match(dangerTag.textContent, /break-glass write/);
  assert.match(dangerTag.getAttribute('title'), /credentials and session state/);
  assert.match(host.querySelector('.sbx-cap-baseline').textContent, /minimal reads/);
  const values = [...host.querySelectorAll('.sbx-cap-val')].map((el) => el.textContent);
  assert.ok(values.some((value) => value.includes('/home/op/.tclaude/data')));
  unmount();
});

test('editor renders the break-glass section and refuses to save without the acknowledgement', async (t) => {
  const harness = await createPreactHarness(t);
  const { createManagementState } = await harness.importDashboardModule('js/management-state.js');
  const state = createManagementState();
  state.openDialog({
    kind: 'sandbox-editor',
    seed: {
      name: 'debug',
      filesystem: [],
      environment: [],
      includes: [],
      agent_directories: [],
      read_baseline: 'minimal',
      break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'read' }],
    },
    options: {},
  });
  const saves = [];
  const { host, unmount } = await mountManagement(harness, state, {
    async inspectDirectories() { return { missing: [], creatable: [] }; },
    async createDirectories() {},
    saveSandbox(value) { saves.push(value); },
    configureSandboxWithAgent() {},
  });
  await harness.act(() => Promise.resolve());

  const baselineSelect = host.querySelector('#sandbox-profile-editor-read-baseline');
  assert.ok(baselineSelect.querySelector('option[value="minimal"]'), 'minimal is offered');
  assert.match(baselineSelect.querySelector('option[value=""]').textContent, /Default/, 'omission stays the default posture');
  const section = host.querySelector('.sbx-break-glass');
  assert.ok(section, 'break-glass has its own dangerous section, not a filesystem row');
  const warning = section.querySelector('.sbx-bg-warning');
  assert.ok(warning, 'a populated section shows the full warning');
  for (const consequence of ['credentials and session state', 'corrupt', 'authorization', 'host-control sockets', 'break the daemon or harness']) {
    assert.match(warning.textContent, new RegExp(consequence), `warning names the concrete consequence: ${consequence}`);
  }
  const accessOptions = [...section.querySelectorAll('.sbx-access option')].map((option) => option.value);
  assert.deepEqual(accessOptions, ['read', 'write'], 'break-glass rules are exactly read or write — never deny');

  const ack = host.querySelector('#sandbox-profile-editor-break-glass-ack');
  assert.ok(ack, 'the acknowledgement checkbox renders when rules exist');
  assert.notEqual(ack.checked, true, 'every editor session starts unacknowledged');
  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(saves, [], 'saving without the acknowledgement is refused');
  assert.match(host.querySelector('.cron-create-error').textContent, /acknowledgement/);

  ack.checked = true;
  ack.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.equal(saves.length, 1);
  assert.equal(saves[0].breakGlassAcknowledged, true);
  assert.deepEqual(saves[0].draft.break_glass_filesystem, [{ path: '/home/op/.tclaude/data', access: 'read' }]);
  assert.equal(saves[0].draft.read_baseline, 'minimal');
  unmount();
});

test('an editor without break-glass rules shows no acknowledgement and saves untouched profiles as before', async (t) => {
  const harness = await createPreactHarness(t);
  const { createManagementState } = await harness.importDashboardModule('js/management-state.js');
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-editor', seed: { name: 'plain', filesystem: [], environment: [], includes: [], agent_directories: [] }, options: {} });
  const saves = [];
  const { host, unmount } = await mountManagement(harness, state, {
    async inspectDirectories() { return { missing: [], creatable: [] }; },
    async createDirectories() {},
    saveSandbox(value) { saves.push(value); },
    configureSandboxWithAgent() {},
  });
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#sandbox-profile-editor-break-glass-ack'), null, 'no rules, no acknowledgement prompt');
  assert.ok(host.querySelector('.sbx-break-glass .sbx-bg-intro'), 'the empty section carries only the quiet caution');
  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.equal(saves.length, 1);
  assert.equal(saves[0].breakGlassAcknowledged, false);
  assert.deepEqual(saves[0].draft.break_glass_filesystem, []);
  assert.equal(saves[0].draft.read_baseline, '');
  unmount();
});

test('the normalized diff confirmation keeps break-glass and the strict baseline visible', async (t) => {
  const harness = await createPreactHarness(t);
  const { createManagementState } = await harness.importDashboardModule('js/management-state.js');
  const state = createManagementState();
  const { host, unmount } = await mountManagement(harness, state, {});
  await harness.act(() => Promise.resolve());
  const decision = state.confirmSandboxDiff(
    { name: 'debug', filesystem: [] },
    { name: 'debug', filesystem: [], read_baseline: 'minimal', break_glass_filesystem: [{ path: '/home/op/.codex', access: 'write' }] },
  );
  await harness.act(() => Promise.resolve());
  const banner = host.querySelector('#sandbox-profile-diff-break-glass');
  assert.ok(banner, 'the confirmation dialog carries the danger banner');
  assert.match(banner.textContent, /write \/home\/op\/\.codex/);
  assert.match(banner.textContent, /host-control sockets/);
  assert.match(host.querySelector('#sandbox-profile-diff-read-baseline').textContent, /minimal/);
  host.querySelector('#sandbox-profile-diff-cancel').click();
  await harness.act(() => Promise.resolve());
  assert.equal(await decision, false);

  const removal = state.confirmSandboxDiff(
    { name: 'debug', break_glass_filesystem: [{ path: '/home/op/.codex', access: 'write' }] },
    { name: 'debug' },
  );
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#sandbox-profile-diff-break-glass'), null);
  assert.match(host.querySelector('#sandbox-profile-diff-break-glass-removed').textContent, /removed/);
  host.querySelector('#sandbox-profile-diff-cancel').click();
  await harness.act(() => Promise.resolve());
  assert.equal(await removal, false);
  unmount();
});

test('import accepts v3 envelopes and gates break-glass bundles behind a fresh acknowledgement', async (t) => {
  const harness = await createPreactHarness(t);
  const { createManagementState } = await harness.importDashboardModule('js/management-state.js');
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-import' });
  const imports = [];
  const { host, unmount } = await mountManagement(harness, state, {
    async inspectSandboxBundle(value) { return { profiles: value.profiles, warnings: [] }; },
    async importSandboxBundle(...args) { imports.push(args); },
  });
  await harness.act(() => Promise.resolve());
  const envelope = {
    format: 'tclaude-sandbox-profiles',
    format_version: 3,
    profiles: [
      // The break-glass carrier hides behind an include inside the bundle, so
      // detection must flatten, not just read each profile's own field.
      { name: 'wrapper', includes: ['debug-base'] },
      { name: 'debug-base', break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'read' }] },
    ],
  };
  const raw = host.querySelector('#sandbox-profile-import-modal textarea');
  raw.value = JSON.stringify(envelope);
  raw.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  [...host.querySelectorAll('#sandbox-profile-import-modal button')].find((button) => button.textContent === 'Preview').click();
  await harness.act(() => Promise.resolve());

  const warning = host.querySelector('#sandbox-profile-import-modal .sbx-bg-warning');
  assert.ok(warning, 'break-glass bundles show the danger warning before import');
  assert.match(warning.textContent, /wrapper/, 'the include-carrying profile is named, not hidden');
  assert.match(warning.textContent, /debug-base/);
  assert.match(warning.textContent, /fresh acknowledgement/);
  const importButton = [...host.querySelectorAll('#sandbox-profile-import-modal .modal-buttons button')].find((button) => button.textContent === 'Import');
  assert.equal(importButton.disabled, true, 'import stays disabled until acknowledged');
  const ack = host.querySelector('#sandbox-profile-import-break-glass-ack');
  ack.checked = true;
  ack.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  assert.equal(importButton.disabled, false);
  importButton.click();
  await harness.act(() => Promise.resolve());
  assert.equal(imports.length, 1);
  assert.equal(imports[0][2], true, 'the acknowledgement rides the import call');
  unmount();
});

test('import of a break-glass-free bundle needs no acknowledgement', async (t) => {
  const harness = await createPreactHarness(t);
  const { createManagementState } = await harness.importDashboardModule('js/management-state.js');
  const state = createManagementState();
  state.openDialog({ kind: 'sandbox-import' });
  const imports = [];
  const { host, unmount } = await mountManagement(harness, state, {
    async inspectSandboxBundle(value) { return { profiles: value.profiles, warnings: [] }; },
    async importSandboxBundle(...args) { imports.push(args); },
  });
  await harness.act(() => Promise.resolve());
  const raw = host.querySelector('#sandbox-profile-import-modal textarea');
  raw.value = JSON.stringify({ format: 'tclaude-sandbox-profiles', format_version: 2, profiles: [{ name: 'offline', network_access: 'none' }] });
  raw.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  [...host.querySelectorAll('#sandbox-profile-import-modal button')].find((button) => button.textContent === 'Preview').click();
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#sandbox-profile-import-break-glass-ack'), null);
  const importButton = [...host.querySelectorAll('#sandbox-profile-import-modal .modal-buttons button')].find((button) => button.textContent === 'Import');
  assert.equal(importButton.disabled, false);
  importButton.click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(imports, [[{ format: 'tclaude-sandbox-profiles', format_version: 2, profiles: [{ name: 'offline', network_access: 'none' }] }, 'skip', false]]);
  unmount();
});

test('global assignment demands confirmation for break-glass profiles and sends the acknowledgement', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/profiles.js', `
    export async function loadProfiles() { return []; }
    export function profileChoices() { return []; }
    export async function setDashDefaultProfile() { return ''; }
  `);
  await harness.replaceDashboardModule('js/sandbox-profiles.js', `
    export async function loadSandboxProfiles() { return []; }
    export function openSandboxProfileEditor() {}
  `);
  await harness.replaceDashboardModule('js/modal-profiles.js', 'export function openProfileEditor() {}');
  await harness.replaceDashboardModule('js/toolbar-profile-renderers.js', `
    export function renderDashDefaultProfile() {}
    export function renderDashSandboxProfile() {}
  `);
  await harness.replaceDashboardModule('js/agent-spawn-controller.js', 'export function refreshAgentSpawnSandboxPolicy() {}');
  const { createToolbarProfilePickerActions } = await harness.importDashboardModule('js/toolbar-profile-picker-actions.js');
  const profiles = [
    { name: 'wrapper', includes: ['debug-base'] },
    { name: 'debug-base', break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'write' }] },
    { name: 'plain', filesystem: [{ path: '/work', access: 'write' }] },
  ];
  const fetches = [];
  const prompts = [];
  let allow = false;
  const actions = createToolbarProfilePickerActions({
    fetchImpl: async (url, options) => { fetches.push([url, options]); return { ok: true, text: async () => '' }; },
    notify() {},
    refresh: async () => {},
    confirmDanger: async (prompt) => { prompts.push(prompt); return allow; },
    loadSandboxProfilesImpl: async () => profiles,
  });

  assert.equal(await actions.commit('sandbox', 'wrapper'), false, 'declining the warning aborts the assignment');
  assert.equal(fetches.length, 0, 'nothing is sent when the operator declines');
  assert.match(prompts[0].title, /break-glass/i);
  assert.match(prompts[0].body, /GLOBAL default sandbox profile/);
  assert.match(prompts[0].body, /write \/home\/op\/\.tclaude\/data \(global:debug-base\)/, 'the include origin is spelled out');
  assert.match(prompts[0].body, /host-control sockets/);

  allow = true;
  assert.equal(await actions.commit('sandbox', 'wrapper'), true);
  assert.equal(fetches.length, 1);
  assert.deepEqual(JSON.parse(fetches[0][1].body), { name: 'wrapper', break_glass_acknowledged: true });

  assert.equal(await actions.commit('sandbox', 'plain'), true, 'break-glass-free profiles assign without ceremony');
  assert.equal(prompts.length, 2, 'no extra confirmation for ordinary profiles');
  assert.deepEqual(JSON.parse(fetches[1][1].body), { name: 'plain' });
});

test('group assignment demands the same confirmation and acknowledgement', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/sandbox-profiles.js', `
    export async function loadSandboxProfiles() {
      return [{ name: 'debug', break_glass_filesystem: [{ path: '/home/op/.codex', access: 'read' }] }];
    }
    export function openSandboxProfileEditor() {}
  `);
  await harness.replaceDashboardModule('js/agent-spawn-controller.js', 'export function refreshAgentSpawnSandboxPolicy() {}');
  const { createGroupsActions } = await harness.importDashboardModule('js/groups-actions.js');
  const fetches = [];
  const prompts = [];
  let allow = false;
  const previousFetch = globalThis.fetch;
  globalThis.fetch = async (url, options) => { fetches.push([url, options]); return { ok: true, text: async () => '' }; };
  t.after(() => { globalThis.fetch = previousFetch; });
  const actions = createGroupsActions({
    state: {}, refresh: () => {}, notify() {},
    confirmDanger: async (prompt) => { prompts.push(prompt); return allow; },
  });

  assert.equal(await actions.setGroupProfile({ name: 'ops' }, 'sandbox', 'debug'), false);
  assert.equal(fetches.length, 0);
  assert.match(prompts[0].body, /group "ops"/);
  assert.match(prompts[0].body, /read \/home\/op\/\.codex \(group:ops:debug\)/);

  allow = true;
  assert.equal(await actions.setGroupProfile({ name: 'ops' }, 'sandbox', 'debug'), true);
  assert.deepEqual(JSON.parse(fetches[0][1].body), { name: 'debug', break_glass_acknowledged: true });
});

test('the resolved spawn policy exposes break-glass and the spawn confirmation names the risk', async (t) => {
  const harness = await createPreactHarness(t);
  const { createAgentSpawnActions } = await harness.importDashboardModule('js/agent-spawn-actions.js');
  const profiles = [
    { name: 'group-debug', break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'read' }] },
    { name: 'plain', filesystem: [{ path: '/work', access: 'write' }] },
  ];
  const prompts = [];
  const actions = createAgentSpawnActions({
    fetchImpl: async (url) => ({
      ok: true,
      json: async () => (url.includes('/api/groups/') ? { name: 'group-debug' } : { name: '' }),
      text: async () => '',
    }),
    loadSandboxProfiles: async () => profiles,
    confirm: async (prompt) => { prompts.push(prompt); return true; },
  });
  const policy = await actions.loadSandboxPolicy('ops', 'plain');
  assert.match(policy.preview, /⚠ BREAK-GLASS protected access: read \/home\/op\/\.tclaude\/data \(group:group-debug\)/,
    'the effective preview keeps break-glass visible even when only the group layer carries it');
  assert.deepEqual(policy.breakGlass, [{ path: '/home/op/.tclaude/data', access: 'read', origins: ['group:group-debug'] }]);

  await actions.confirmBreakGlassSpawn(policy.breakGlass);
  assert.match(prompts[0].title, /break-glass/i);
  assert.match(prompts[0].body, /read \/home\/op\/\.tclaude\/data \(group:group-debug\)/);
  assert.match(prompts[0].body, /never a routine or recommended posture/);
});

test('editor detects break-glass carried only by includes and gates save on acknowledgement', async (t) => {
  const harness = await createPreactHarness(t);
  const { createManagementState } = await harness.importDashboardModule('js/management-state.js');
  const state = createManagementState();
  state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [
    { name: 'debug-base', break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'read' }] },
    { name: 'wrapper', includes: ['debug-base'] },
  ]);
  state.openDialog({ kind: 'sandbox-editor', seed: { name: 'wrapper', filesystem: [], environment: [], includes: ['debug-base'], agent_directories: [] }, options: {} });
  const saves = [];
  const { host, unmount } = await mountManagement(harness, state, {
    async inspectDirectories() { return { missing: [], creatable: [] }; },
    async createDirectories() {},
    saveSandbox(value) { saves.push(value); },
    configureSandboxWithAgent() {},
  });
  await harness.act(() => Promise.resolve());

  const warning = host.querySelector('.sbx-break-glass .sbx-bg-warning');
  assert.ok(warning, 'a wrapper with zero own rules still warns when an include carries break-glass');
  assert.match(warning.textContent, /read \/home\/op\/\.tclaude\/data \(profile:debug-base\)/, 'the include origin is spelled out');
  const ack = host.querySelector('#sandbox-profile-editor-break-glass-ack');
  assert.ok(ack, 'include-carried break-glass demands the acknowledgement');

  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(saves, [], 'saving without the acknowledgement is refused');
  assert.match(host.querySelector('.cron-create-error').textContent, /includes/);

  ack.checked = true;
  ack.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.equal(saves.length, 1);
  assert.equal(saves[0].breakGlassAcknowledged, true);
  assert.deepEqual(saves[0].draft.break_glass_filesystem, [], 'no own rules are invented; the danger lives in the include');
  unmount();
});

test('manager cards surface include-carried break-glass with its origin', async (t) => {
  const harness = await createPreactHarness(t);
  const { createManagementState } = await harness.importDashboardModule('js/management-state.js');
  const state = createManagementState();
  state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [
    { name: 'debug-base', break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'write' }] },
    { name: 'wrapper', includes: ['debug-base'] },
  ]);
  state.openManager('sandbox');
  const { host, unmount } = await mountManagement(harness, state, { load() {}, openSandboxEditor() {}, removeSandbox() {} });
  await harness.act(() => Promise.resolve());
  const wrapperCard = [...host.querySelectorAll('.template-card')].find((card) => card.dataset.key === 'wrapper');
  const dangerValue = wrapperCard.querySelector('.sbx-cap-bg')?.parentElement?.querySelector('.sbx-cap-val');
  assert.ok(wrapperCard.querySelector('.sbx-cap-bg'), 'the include-only wrapper card still shows the danger tag');
  assert.match(dangerValue.textContent, /via debug-base/, 'the card names the include that introduced the rule');
  unmount();
});

test('the diff confirmation resolves includes so break-glass cannot hide behind them', async (t) => {
  const harness = await createPreactHarness(t);
  const { createManagementState } = await harness.importDashboardModule('js/management-state.js');
  const state = createManagementState();
  state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [
    { name: 'debug-base', break_glass_filesystem: [{ path: '/home/op/.codex', access: 'read' }] },
  ]);
  const { host, unmount } = await mountManagement(harness, state, {});
  await harness.act(() => Promise.resolve());
  const decision = state.confirmSandboxDiff(null, { name: 'wrapper', includes: ['debug-base'] });
  await harness.act(() => Promise.resolve());
  const banner = host.querySelector('#sandbox-profile-diff-break-glass');
  assert.ok(banner, 'an include-only profile still shows the danger banner in the confirmation');
  assert.match(banner.textContent, /read \/home\/op\/\.codex \(profile:debug-base\)/);
  host.querySelector('#sandbox-profile-diff-cancel').click();
  await harness.act(() => Promise.resolve());
  assert.equal(await decision, false);
  unmount();
});

async function importScenario(harness, state, envelope, overrides = {}) {
  state.openDialog({ kind: 'sandbox-import' });
  const imports = [];
  const inspects = [];
  const loads = [];
  const mountedResult = await mountManagement(harness, state, {
    // Mirrors the real /api/sandbox-profiles/import/inspect response shape:
    // normalized profiles, path warnings, and per-conflict-policy include
    // errors (plus break_glass_profiles/break_glass_risk when carriers exist).
    async inspectSandboxBundle(value) {
      inspects.push(value);
      if (overrides.inspectImpl) return overrides.inspectImpl(value, inspects.length);
      return { profiles: value.profiles, warnings: [], include_errors: {}, ...(overrides.inspect ? overrides.inspect(value) : {}) };
    },
    async importSandboxBundle(...args) {
      imports.push(args);
      if (overrides.importImpl) return overrides.importImpl(...args);
      return undefined;
    },
    async load(kind) {
      loads.push(kind);
      if (overrides.loadImpl) return overrides.loadImpl(kind);
      return true;
    },
  });
  await harness.act(() => Promise.resolve());
  const { host } = mountedResult;
  const raw = host.querySelector('#sandbox-profile-import-modal textarea');
  raw.value = JSON.stringify(envelope);
  raw.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  [...host.querySelectorAll('#sandbox-profile-import-modal button')].find((button) => button.textContent === 'Preview').click();
  await harness.act(() => Promise.resolve());
  const conflictSelect = () => host.querySelector('#sandbox-profile-import-conflict');
  const setConflict = async (value) => {
    const select = conflictSelect();
    for (const option of select.options) option.selected = option.value === value;
    Object.defineProperty(select, 'value', { configurable: true, writable: true, value });
    select.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
    await harness.act(() => Promise.resolve());
  };
  return { ...mountedResult, imports, inspects, loads, setConflict };
}

test('import under skip warns for retained local break-glass reached through a new wrapper', async (t) => {
  const harness = await createPreactHarness(t);
  const { createManagementState } = await harness.importDashboardModule('js/management-state.js');
  const state = createManagementState();
  // Local "lib" carries break-glass; the bundle ships a clean "lib" and a new
  // wrapper including it.
  state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [
    { name: 'lib', break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'read' }] },
  ]);
  const { host, imports, setConflict, unmount } = await importScenario(harness, state, {
    format: 'tclaude-sandbox-profiles', format_version: 3,
    profiles: [{ name: 'lib' }, { name: 'wrapper', includes: ['lib'] }],
  });

  // skip (the default): the incoming clean "lib" is discarded, so the new
  // wrapper resolves against the RETAINED local carrier and must warn.
  const warning = host.querySelector('#sandbox-profile-import-modal .sbx-bg-warning');
  assert.ok(warning, 'retained local break-glass reached via the imported wrapper is warned about');
  assert.match(warning.textContent, /wrapper/);
  assert.match(warning.textContent, /import:lib/);
  const importButton = [...host.querySelectorAll('#sandbox-profile-import-modal .modal-buttons button')].find((button) => button.textContent === 'Import');
  assert.equal(importButton.disabled, true);

  // overwrite: the clean incoming "lib" replaces the local carrier, so no
  // break-glass survives anywhere and no acknowledgement is demanded.
  await setConflict('overwrite');
  assert.equal(host.querySelector('#sandbox-profile-import-modal .sbx-bg-warning'), null, 'overwrite replaces the carrier, so nothing needs acknowledging');
  assert.equal(host.querySelector('#sandbox-profile-import-break-glass-ack'), null);
  assert.equal(importButton.disabled, false);
  importButton.click();
  await harness.act(() => Promise.resolve());
  assert.equal(imports.length, 1);
  assert.equal(imports[0][1], 'overwrite');
  assert.equal(imports[0][2], false, 'no acknowledgement rides an import with no effective break-glass');
  unmount();
});

test('import under skip never demands acknowledgement for discarded incoming break-glass', async (t) => {
  const harness = await createPreactHarness(t);
  const { createManagementState } = await harness.importDashboardModule('js/management-state.js');
  const state = createManagementState();
  // Local "lib" is clean; the bundle ships a break-glass-carrying "lib" and a
  // new wrapper including it.
  state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [
    { name: 'lib', filesystem: [{ path: '/work', access: 'write' }] },
  ]);
  const { host, imports, setConflict, unmount } = await importScenario(harness, state, {
    format: 'tclaude-sandbox-profiles', format_version: 3,
    profiles: [
      { name: 'lib', break_glass_filesystem: [{ path: '/home/op/.codex', access: 'write' }] },
      { name: 'wrapper', includes: ['lib'] },
    ],
  });

  // skip: the dangerous incoming "lib" is discarded and the wrapper resolves
  // against the clean local one — demanding acknowledgement here would train
  // operators to acknowledge rules that are never imported.
  assert.equal(host.querySelector('#sandbox-profile-import-modal .sbx-bg-warning'), null, 'discarded incoming rules must not demand acknowledgement');
  const importButton = [...host.querySelectorAll('#sandbox-profile-import-modal .modal-buttons button')].find((button) => button.textContent === 'Import');
  assert.equal(importButton.disabled, false);

  // overwrite: the dangerous incoming "lib" now lands, so both it and the
  // wrapper that includes it require the acknowledgement.
  await setConflict('overwrite');
  const warning = host.querySelector('#sandbox-profile-import-modal .sbx-bg-warning');
  assert.ok(warning);
  assert.match(warning.textContent, /lib/);
  assert.match(warning.textContent, /wrapper/);
  assert.equal(importButton.disabled, true);
  const ack = host.querySelector('#sandbox-profile-import-break-glass-ack');
  ack.checked = true;
  ack.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  importButton.click();
  await harness.act(() => Promise.resolve());
  assert.equal(imports.length, 1);
  assert.deepEqual([imports[0][1], imports[0][2]], ['overwrite', true]);
  unmount();
});

test('resolvedBreakGlass survives a rename whose includes still reference the prior self-name', async (t) => {
  const harness = await createPreactHarness(t);
  const { resolvedBreakGlass } = await harness.importDashboardModule('js/sandbox-break-glass.js');
  const registry = [{ name: 'old', break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'read' }] }];
  // Renaming "old" → "new" while the draft still includes "old": the prior
  // name aliases to the draft itself, which must terminate as a cycle rather
  // than recurse until RangeError.
  const entries = resolvedBreakGlass({ name: 'new', includes: ['old'], break_glass_filesystem: [] }, registry, 'old');
  assert.deepEqual(entries, [], 'the self-referential include contributes nothing rather than crashing');
  // The prior-name alias still resolves OTHER profiles' references to the
  // draft: a wrapper including "old" sees the draft's rules, not the stale
  // stored version's.
  const viaOther = resolvedBreakGlass(
    { name: 'new', includes: ['wrapper'], break_glass_filesystem: [] },
    [...registry, { name: 'wrapper', includes: ['old'] }],
    'old',
  );
  assert.deepEqual(viaOther, [], 'the stored pre-rename rules are not resurrected through an indirect include');
  const mutual = resolvedBreakGlass(
    { name: 'new', includes: ['loop'], break_glass_filesystem: [{ path: '/home/op/.codex', access: 'read' }] },
    [{ name: 'loop', includes: ['old'] }],
    'old',
  );
  assert.equal(mutual.length, 1, 'a mutual cycle through the prior name terminates and keeps own rules');
});

test('the editor survives an advanced rename whose raw includes reference the prior self-name', async (t) => {
  const harness = await createPreactHarness(t);
  const { createManagementState } = await harness.importDashboardModule('js/management-state.js');
  const state = createManagementState();
  state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [
    { name: 'old', break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'read' }] },
  ]);
  state.openDialog({ kind: 'sandbox-editor', seed: { name: 'old', filesystem: [], environment: [], includes: [], agent_directories: [] }, options: {} });
  const { host, unmount } = await mountManagement(harness, state, {
    async inspectDirectories() { return { missing: [], creatable: [] }; },
    async createDirectories() {},
    saveSandbox() {},
    configureSandboxWithAgent() {},
  });
  await harness.act(() => Promise.resolve());

  host.querySelector('.sbx-advanced-toggle').click();
  await harness.act(() => Promise.resolve());
  const nameInput = host.querySelector('#sandbox-profile-editor-modal .cron-create-row input');
  nameInput.value = 'new';
  nameInput.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  const includes = host.querySelector('#sandbox-profile-editor-includes');
  includes.value = '["old"]';
  includes.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  await harness.act(() => Promise.resolve());

  assert.ok(host.querySelector('#sandbox-profile-editor-modal'), 'the editor renders instead of crashing on the self-referential rename');
  assert.ok(host.querySelector('#sandbox-profile-editor-submit'), 'the editor stays interactive so normal validation can run on save');
  unmount();
});

// The retained-local-A vs incoming A<->B cycle from the backend regression:
// under "skip" the clashing incoming A is discarded (local A has no
// includes), so the graph is valid; under "overwrite" (and "error", which
// shares the all-incoming graph) the A<->B cycle lands and must block import
// with the server-normalized error.
test('import preview gates on per-conflict-policy include errors', async (t) => {
  const harness = await createPreactHarness(t);
  const { createManagementState } = await harness.importDashboardModule('js/management-state.js');
  const state = createManagementState();
  state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [{ name: 'A' }]);
  const cycleError = 'sandbox profile include cycle: A → B → A';
  const { host, imports, setConflict, unmount } = await importScenario(harness, state, {
    format: 'tclaude-sandbox-profiles', format_version: 3,
    profiles: [{ name: 'A', includes: ['B'] }, { name: 'B', includes: ['A'] }],
  }, { inspect: () => ({ include_errors: { overwrite: cycleError } }) });

  const importButton = [...host.querySelectorAll('#sandbox-profile-import-modal .modal-buttons button')].find((button) => button.textContent === 'Import');
  assert.equal(host.querySelector('#sandbox-profile-import-include-error'), null, 'skip has a valid graph — no error shown');
  assert.equal(importButton.disabled, false, 'skip stays importable');

  await setConflict('overwrite');
  const shown = host.querySelector('#sandbox-profile-import-include-error');
  assert.ok(shown, 'overwrite renders the server-normalized include error');
  assert.match(shown.textContent, /cycle: A → B → A/);
  assert.equal(importButton.disabled, true, 'the invalid policy blocks import');

  await setConflict('error');
  assert.ok(host.querySelector('#sandbox-profile-import-include-error'), '"error" shares the all-incoming graph and stays blocked');
  assert.equal(importButton.disabled, true);

  await setConflict('skip');
  assert.equal(host.querySelector('#sandbox-profile-import-include-error'), null);
  importButton.click();
  await harness.act(() => Promise.resolve());
  assert.equal(imports.length, 1, 'the valid policy imports');
  assert.equal(imports[0][1], 'skip');
  unmount();
});

test('the data layer preserves the daemon status and typed error code', async (t) => {
  const harness = await createPreactHarness(t);
  const data = await harness.importDashboardModule('js/sandbox-profiles-data.js');
  const previousFetch = globalThis.fetch;
  globalThis.fetch = async () => ({
    ok: false, status: 422,
    text: async () => JSON.stringify({ error: 'sandbox profile "debug" grants break-glass access', code: 'break_glass_acknowledgement_required' }),
  });
  t.after(() => { globalThis.fetch = previousFetch; });
  await assert.rejects(
    data.saveSandboxProfile('debug', { name: 'debug' }, 'r1'),
    (error) => {
      assert.equal(error.status, 422);
      assert.equal(error.code, 'break_glass_acknowledgement_required');
      assert.match(error.message, /grants break-glass access/);
      return true;
    },
  );
});

test('save recovery on the typed 422 reloads the registry, blocks resend, and demands a fresh acknowledgement', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { createManagementActions }, { BREAK_GLASS_ACK_CODE }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/management-actions.js'),
    harness.importDashboardModule('js/sandbox-break-glass.js'),
  ]);
  const state = createManagementState();
  let loads = 0;
  let saves = 0;
  const sandboxAPI = {
    loadSandboxProfiles: async () => { loads += 1; return [{ name: 'lib', break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'read' }] }]; },
    previewSandboxProfile: async (name, body) => ({ before: null, after: body, revision: 'r1' }),
    saveSandboxProfile: async () => {
      saves += 1;
      throw Object.assign(new Error('sandbox profile "wrapper" now carries break-glass access: read /home/op/.tclaude/data'), { status: 422, code: BREAK_GLASS_ACK_CODE });
    },
  };
  const actions = createManagementActions({ state, confirm: async () => true, notify() {}, refreshSandboxSpawn: async () => {}, sandboxAPI });
  const draft = { name: 'wrapper', filesystem: [], environment: [], includes: ['lib'], agent_directories: [], network_access: '' };
  const outcome = actions.saveSandbox({ draft, original: null, breakGlassAcknowledged: false });
  await Promise.resolve();
  state.cancelSandboxDiff(true);
  assert.deepEqual(await outcome, { breakGlassAckRequired: true, recovered: true }, 'the typed refusal is reported distinctly with the recovery result');
  assert.equal(saves, 1, 'the unacknowledged request is never retried automatically');
  assert.equal(loads, 1, 'the registry is reloaded so the editor re-resolves includes');
  assert.match(state.error.value, /re-acknowledge/);
});

test('save recovery reports a failed registry reload so the editor stays blocked', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { createManagementActions }, { BREAK_GLASS_ACK_CODE }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/management-actions.js'),
    harness.importDashboardModule('js/sandbox-break-glass.js'),
  ]);
  const state = createManagementState();
  let saves = 0;
  const sandboxAPI = {
    loadSandboxProfiles: async () => { throw new Error('daemon unreachable'); },
    previewSandboxProfile: async (name, body) => ({ before: null, after: body, revision: 'r1' }),
    saveSandboxProfile: async () => {
      saves += 1;
      throw Object.assign(new Error('save requires break-glass acknowledgement'), { status: 422, code: BREAK_GLASS_ACK_CODE });
    },
  };
  const actions = createManagementActions({ state, confirm: async () => true, notify() {}, refreshSandboxSpawn: async () => {}, sandboxAPI });
  const draft = { name: 'wrapper', filesystem: [], environment: [], includes: ['lib'], agent_directories: [], network_access: '' };
  const outcome = actions.saveSandbox({ draft, original: null, breakGlassAcknowledged: false });
  await Promise.resolve();
  state.cancelSandboxDiff(true);
  assert.deepEqual(await outcome, { breakGlassAckRequired: true, recovered: false });
  assert.equal(saves, 1, 'no automatic retry');
  assert.match(state.error.value, /stays blocked/);
});

test('the editor invalidates its acknowledgement when the daemon demands a fresh one', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { BREAK_GLASS_ACK_CODE }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/sandbox-break-glass.js'),
  ]);
  const state = createManagementState();
  state.openDialog({
    kind: 'sandbox-editor',
    seed: { name: 'debug', filesystem: [], environment: [], includes: [], agent_directories: [], break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'read' }] },
    options: {},
  });
  const saves = [];
  const { host, unmount } = await mountManagement(harness, state, {
    async inspectDirectories() { return { missing: [], creatable: [] }; },
    async createDirectories() {},
    saveSandbox(value) { saves.push(value); return { breakGlassAckRequired: true, recovered: true }; },
    configureSandboxWithAgent() {},
  });
  await harness.act(() => Promise.resolve());
  const ack = host.querySelector('#sandbox-profile-editor-break-glass-ack');
  ack.checked = true;
  ack.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.equal(saves.length, 1);
  assert.equal(saves[0].breakGlassAcknowledged, true);
  assert.notEqual(host.querySelector('#sandbox-profile-editor-break-glass-ack').checked, true,
    'the stale acknowledgement does not carry over to the refreshed rules');
  assert.equal(host.querySelector('#sandbox-profile-editor-submit').disabled, false,
    'a SUCCESSFUL registry reload leaves saving available for a fresh acknowledged attempt');
  assert.equal(host.querySelector('#sandbox-profile-editor-recovery'), null, 'no recovery banner when the reload succeeded');
  unmount();
});

test('a failed registry reload after the typed 422 blocks saving until an authoritative reload succeeds', async (t) => {
  const harness = await createPreactHarness(t);
  const { createManagementState } = await harness.importDashboardModule('js/management-state.js');
  const state = createManagementState();
  // The stale cached registry shows a CLEAN "lib": the editor cannot see the
  // break-glass the daemon just refused, so nothing here may be acknowledged.
  state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [{ name: 'lib' }]);
  state.openDialog({ kind: 'sandbox-editor', seed: { name: 'wrapper', filesystem: [], environment: [], includes: ['lib'], agent_directories: [] }, options: {} });
  const saves = [];
  let loadOk = false;
  const { host, unmount } = await mountManagement(harness, state, {
    async inspectDirectories() { return { missing: [], creatable: [] }; },
    async createDirectories() {},
    saveSandbox(value) { saves.push(value); return { breakGlassAckRequired: true, recovered: false }; },
    configureSandboxWithAgent() {},
    async load() {
      if (!loadOk) return false;
      state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [
        { name: 'lib', break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'write' }] },
      ]);
      return true;
    },
  });
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#sandbox-profile-editor-break-glass-ack'), null, 'the stale clean registry offers nothing to acknowledge');
  host.querySelector('#sandbox-profile-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.equal(saves.length, 1);

  const banner = host.querySelector('#sandbox-profile-editor-recovery');
  assert.ok(banner, 'a failed reload surfaces the recovery-blocked state');
  assert.match(banner.textContent, /registry reload failed/);
  assert.equal(host.querySelector('#sandbox-profile-editor-submit').disabled, true, 'saving is blocked from stale state');
  assert.equal(host.querySelector('#sandbox-profile-editor-break-glass-ack'), null, 'no acknowledgement control for rules the editor cannot see');

  host.querySelector('#sandbox-profile-editor-recovery-retry').click();
  await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#sandbox-profile-editor-recovery'), 'a second failed reload stays blocked');
  assert.equal(host.querySelector('#sandbox-profile-editor-submit').disabled, true);

  loadOk = true;
  host.querySelector('#sandbox-profile-editor-recovery-retry').click();
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#sandbox-profile-editor-recovery'), null, 'a successful reload lifts the block');
  assert.equal(host.querySelector('#sandbox-profile-editor-submit').disabled, false);
  const warning = host.querySelector('.sbx-break-glass .sbx-bg-warning');
  assert.ok(warning, 'the refreshed registry reveals the real rules');
  assert.match(warning.textContent, /write \/home\/op\/\.tclaude\/data \(profile:lib\)/, 'the exact new rule and origin are shown');
  const ack = host.querySelector('#sandbox-profile-editor-break-glass-ack');
  assert.ok(ack, 'a fresh acknowledgement is required for the revealed rules');
  assert.notEqual(ack.checked, true);
  unmount();
});

test('import recovery on the typed 422 refreshes the authoritative preview and demands a fresh acknowledgement', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { BREAK_GLASS_ACK_CODE }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/sandbox-break-glass.js'),
  ]);
  const state = createManagementState();
  state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), []);
  let failNext = true;
  const { host, imports, inspects, unmount } = await importScenario(harness, state, {
    format: 'tclaude-sandbox-profiles', format_version: 3,
    profiles: [{ name: 'debug', break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'write' }] }],
  }, {
    importImpl: () => {
      if (failNext) {
        failNext = false;
        throw Object.assign(new Error('import requires break-glass acknowledgement'), { status: 422, code: BREAK_GLASS_ACK_CODE });
      }
      return undefined;
    },
  });
  const importButton = [...host.querySelectorAll('#sandbox-profile-import-modal .modal-buttons button')].find((button) => button.textContent === 'Import');
  const ack = host.querySelector('#sandbox-profile-import-break-glass-ack');
  ack.checked = true;
  ack.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  importButton.click();
  await harness.act(() => Promise.resolve());
  await harness.act(() => Promise.resolve());
  assert.equal(imports.length, 1, 'the refused import is never retried automatically');
  assert.equal(inspects.length, 2, 'the authoritative preview is re-run after the refusal');
  assert.notEqual(host.querySelector('#sandbox-profile-import-break-glass-ack').checked, true, 'the stale acknowledgement is invalidated');
  assert.equal(importButton.disabled, true, 'import stays blocked until a fresh acknowledgement');
  assert.match([...host.querySelectorAll('#sandbox-profile-import-modal .cron-create-error')].map((el) => el.textContent).join(' '), /re-acknowledge/);

  const freshAck = host.querySelector('#sandbox-profile-import-break-glass-ack');
  freshAck.checked = true;
  freshAck.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  importButton.click();
  await harness.act(() => Promise.resolve());
  assert.equal(imports.length, 2, 'a fresh explicit acknowledgement allows a fresh attempt');
  assert.equal(imports[1][2], true);
  unmount();
});

test('assignment surfaces surface the typed 422 as a fresh-acknowledgement retry without resending', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/profiles.js', `
    export async function loadProfiles() { return []; }
    export function profileChoices() { return []; }
    export async function setDashDefaultProfile() { return ''; }
  `);
  await harness.replaceDashboardModule('js/sandbox-profiles.js', `
    export async function loadSandboxProfiles() { return []; }
    export function openSandboxProfileEditor() {}
  `);
  await harness.replaceDashboardModule('js/modal-profiles.js', 'export function openProfileEditor() {}');
  await harness.replaceDashboardModule('js/toolbar-profile-renderers.js', `
    export function renderDashDefaultProfile() {}
    export function renderDashSandboxProfile() {}
  `);
  await harness.replaceDashboardModule('js/agent-spawn-controller.js', 'export function refreshAgentSpawnSandboxPolicy() {}');
  const { createToolbarProfilePickerActions } = await harness.importDashboardModule('js/toolbar-profile-picker-actions.js');
  const body422 = JSON.stringify({ error: 'assignment requires break-glass acknowledgement', code: 'break_glass_acknowledgement_required' });
  let fetches = 0;
  const actions = createToolbarProfilePickerActions({
    fetchImpl: async () => { fetches += 1; return { ok: false, status: 422, text: async () => body422 }; },
    notify() {},
    refresh: async () => {},
    confirmDanger: async () => true,
    loadSandboxProfilesImpl: async () => [
      { name: 'debug', break_glass_filesystem: [{ path: '/home/op/.codex', access: 'read' }] },
    ],
  });
  await assert.rejects(actions.commit('sandbox', 'debug'), (error) => {
    assert.equal(error.code, 'break_glass_acknowledgement_required');
    assert.match(error.message, /re-acknowledge/);
    return true;
  });
  assert.equal(fetches, 1, 'the stale acknowledgement is never re-sent automatically');

  const { createGroupsActions } = await harness.importDashboardModule('js/groups-actions.js');
  const previousFetch = globalThis.fetch;
  let groupFetches = 0;
  globalThis.fetch = async () => { groupFetches += 1; return { ok: false, status: 422, text: async () => body422 }; };
  t.after(() => { globalThis.fetch = previousFetch; });
  await harness.replaceDashboardModule('js/sandbox-profiles.js', `
    export async function loadSandboxProfiles() {
      return [{ name: 'debug', break_glass_filesystem: [{ path: '/home/op/.codex', access: 'read' }] }];
    }
    export function openSandboxProfileEditor() {}
  `);
  const groupActions = createGroupsActions({ state: {}, refresh: () => {}, notify() {}, confirmDanger: async () => true });
  await assert.rejects(groupActions.setGroupProfile({ name: 'ops' }, 'sandbox', 'debug'), (error) => {
    assert.equal(error.code, 'break_glass_acknowledgement_required');
    assert.match(error.message, /re-acknowledge/);
    return true;
  });
  assert.equal(groupFetches, 1);
});

// The reviewer's concrete stale-registry case: cached local A is clean, the
// bundle ships a conflicting clean A plus wrapper→A under skip, and the
// RETAINED local A gains break-glass server-side after the preview. The
// typed 422 recovery must refresh BOTH the local registry and the bundle
// inspection — re-inspect alone would leave the cached clean A composing no
// carriers, no checkbox, and endless 422s.
test('import recovery refreshes the local registry so retained-local break-glass becomes acknowledgeable', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { BREAK_GLASS_ACK_CODE }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/sandbox-break-glass.js'),
  ]);
  const state = createManagementState();
  state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [{ name: 'A' }]);
  let refused = false;
  const { host, imports, inspects, loads, unmount } = await importScenario(harness, state, {
    format: 'tclaude-sandbox-profiles', format_version: 3,
    profiles: [{ name: 'A' }, { name: 'wrapper', includes: ['A'] }],
  }, {
    importImpl: () => {
      if (!refused) {
        refused = true;
        throw Object.assign(new Error('retained profile "A" now carries break-glass access: read /home/op/.aws-adjacent/.tclaude/data'), { status: 422, code: BREAK_GLASS_ACK_CODE });
      }
      return undefined;
    },
    loadImpl: () => {
      // The registry reload reveals the server-side mutation.
      state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [
        { name: 'A', break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'read' }] },
      ]);
      return true;
    },
  });
  const importButton = [...host.querySelectorAll('#sandbox-profile-import-modal .modal-buttons button')].find((button) => button.textContent === 'Import');
  assert.equal(host.querySelector('#sandbox-profile-import-break-glass-ack'), null, 'the stale clean registry composes no carriers under skip');
  importButton.click();
  await harness.act(() => Promise.resolve());
  await harness.act(() => Promise.resolve());

  assert.equal(imports.length, 1, 'exactly one refused request; nothing is resent automatically');
  assert.equal(loads.length, 1, 'the local registry is reloaded');
  assert.equal(inspects.length, 2, 'the authoritative inspection is re-run');
  const warning = host.querySelector('#sandbox-profile-import-modal .sbx-bg-warning');
  assert.ok(warning, 'the refreshed registry reveals the retained-local carrier');
  assert.match(warning.textContent, /wrapper/);
  assert.match(warning.textContent, /import:A/);
  const ack = host.querySelector('#sandbox-profile-import-break-glass-ack');
  assert.ok(ack, 'the checkbox appears only after recovery reveals the current rules');
  assert.notEqual(ack.checked, true);
  assert.equal(importButton.disabled, true, 'import stays blocked until the fresh acknowledgement');

  ack.checked = true;
  ack.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  importButton.click();
  await harness.act(() => Promise.resolve());
  assert.equal(imports.length, 2, 'a fresh explicit user attempt follows recovery');
  assert.equal(imports[1][2], true, 'the fresh attempt carries the fresh acknowledgement');
  unmount();
});

test('import recovery failures — registry reload or re-inspect — keep import blocked', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { BREAK_GLASS_ACK_CODE }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/sandbox-break-glass.js'),
  ]);
  const typedRefusal = () => Object.assign(new Error('import requires break-glass acknowledgement'), { status: 422, code: BREAK_GLASS_ACK_CODE });
  const envelope = {
    format: 'tclaude-sandbox-profiles', format_version: 3,
    profiles: [{ name: 'debug', break_glass_filesystem: [{ path: '/home/op/.codex', access: 'read' }] }],
  };
  for (const [label, overrides] of [
    ['failed registry reload', { importImpl: () => { throw typedRefusal(); }, loadImpl: () => false }],
    ['failed re-inspect', { importImpl: () => { throw typedRefusal(); }, inspectImpl: (value, call) => { if (call > 1) throw new Error('daemon unreachable'); return { profiles: value.profiles, warnings: [], include_errors: {} }; } }],
  ]) {
    const state = createManagementState();
    state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), []);
    const { host, imports, unmount } = await importScenario(harness, state, envelope, overrides);
    const importButton = [...host.querySelectorAll('#sandbox-profile-import-modal .modal-buttons button')].find((button) => button.textContent === 'Import');
    const ack = host.querySelector('#sandbox-profile-import-break-glass-ack');
    ack.checked = true;
    ack.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
    await harness.act(() => Promise.resolve());
    importButton.click();
    await harness.act(() => Promise.resolve());
    await harness.act(() => Promise.resolve());
    assert.equal(imports.length, 1, `${label}: no automatic resend`);
    assert.equal(importButton.disabled, true, `${label}: import stays blocked`);
    assert.match([...host.querySelectorAll('#sandbox-profile-import-modal .cron-create-error')].map((el) => el.textContent).join(' '), /stays blocked/, `${label}: the block is stated`);
    unmount();
  }
});

// After a typed refusal with NO automatic resend, the authoritative rules
// change; an explicit user retry must load fresh state and confirm the fresh
// rules before exactly one acknowledged request.
test('assignment retry after a typed refusal reloads and confirms the fresh rules', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/profiles.js', `
    export async function loadProfiles() { return []; }
    export function profileChoices() { return []; }
    export async function setDashDefaultProfile() { return ''; }
  `);
  await harness.replaceDashboardModule('js/sandbox-profiles.js', `
    export async function loadSandboxProfiles() { return []; }
    export function openSandboxProfileEditor() {}
  `);
  await harness.replaceDashboardModule('js/modal-profiles.js', 'export function openProfileEditor() {}');
  await harness.replaceDashboardModule('js/toolbar-profile-renderers.js', `
    export function renderDashDefaultProfile() {}
    export function renderDashSandboxProfile() {}
  `);
  await harness.replaceDashboardModule('js/agent-spawn-controller.js', 'export function refreshAgentSpawnSandboxPolicy() {}');
  const { createToolbarProfilePickerActions } = await harness.importDashboardModule('js/toolbar-profile-picker-actions.js');
  const rulesV1 = [{ path: '/home/op/.tclaude/data', access: 'read' }];
  const rulesV2 = [{ path: '/home/op/.codex', access: 'write' }];
  let authoritative = rulesV1;
  const fetches = [];
  const prompts = [];
  const body422 = JSON.stringify({ error: 'assignment requires break-glass acknowledgement', code: 'break_glass_acknowledgement_required' });
  const actions = createToolbarProfilePickerActions({
    fetchImpl: async (url, options) => {
      fetches.push(options);
      if (fetches.length === 1) return { ok: false, status: 422, text: async () => body422 };
      return { ok: true, text: async () => '' };
    },
    notify() {},
    refresh: async () => {},
    confirmDanger: async (prompt) => { prompts.push(prompt); return true; },
    loadSandboxProfilesImpl: async () => [{ name: 'debug', break_glass_filesystem: authoritative }],
  });

  await assert.rejects(actions.commit('sandbox', 'debug'), /re-acknowledge/);
  assert.equal(fetches.length, 1, 'the refusal produces no automatic resend');
  assert.match(prompts[0].body, /read \/home\/op\/\.tclaude\/data/);

  // The daemon's rules change before the user retries.
  authoritative = rulesV2;
  assert.equal(await actions.commit('sandbox', 'debug'), true);
  assert.equal(prompts.length, 2, 'the explicit retry re-confirms');
  assert.match(prompts[1].body, /write \/home\/op\/\.codex/, 'the fresh confirmation shows the CURRENT rules');
  assert.ok(!prompts[1].body.includes('read /home/op/.tclaude/data'), 'the fresh confirmation no longer describes the stale rule');
  assert.equal(fetches.length, 2, 'exactly one acknowledged request follows the fresh confirmation');
  assert.deepEqual(JSON.parse(fetches[1].body), { name: 'debug', break_glass_acknowledged: true });
});

// The final merge-gate probe: after a typed 422 whose registry reload FAILED,
// the ordinary Preview button must not bypass recovery — while the registry
// is unloadable, a succeeding inspect alone would recompose against the
// stale cached clean local A, show no acknowledgement control, and re-enable
// an unacknowledged Import.
test('the Preview button honors registry-recovery-required and cannot bypass a failed registry reload', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { BREAK_GLASS_ACK_CODE }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/sandbox-break-glass.js'),
  ]);
  const state = createManagementState();
  state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [{ name: 'A' }]);
  let registryReachable = false;
  let refused = false;
  const { host, imports, inspects, loads, unmount } = await importScenario(harness, state, {
    format: 'tclaude-sandbox-profiles', format_version: 3,
    profiles: [{ name: 'A' }, { name: 'wrapper', includes: ['A'] }],
  }, {
    importImpl: () => {
      if (!refused) {
        refused = true;
        throw Object.assign(new Error('retained profile "A" now carries break-glass access'), { status: 422, code: BREAK_GLASS_ACK_CODE });
      }
      return undefined;
    },
    loadImpl: () => {
      if (!registryReachable) return false;
      state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [
        { name: 'A', break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'read' }] },
      ]);
      return true;
    },
  });
  const importButton = [...host.querySelectorAll('#sandbox-profile-import-modal .modal-buttons button')].find((button) => button.textContent === 'Import');
  const previewButton = [...host.querySelectorAll('#sandbox-profile-import-modal button')].find((button) => button.textContent === 'Preview');

  importButton.click();
  await harness.act(() => Promise.resolve());
  await harness.act(() => Promise.resolve());
  assert.equal(imports.length, 1, 'the typed refusal is not resent');
  assert.equal(loads.length, 1, 'recovery attempted the registry reload');
  assert.equal(importButton.disabled, true, 'immediate Import is disabled after the failed recovery');

  // The ordinary Preview button: inspect would succeed, but the registry is
  // still unreachable — recovery must be honored, not bypassed.
  const inspectsBefore = inspects.length;
  previewButton.click();
  await harness.act(() => Promise.resolve());
  await harness.act(() => Promise.resolve());
  assert.equal(loads.length, 2, 'Preview re-attempts the registry reload');
  assert.equal(inspects.length, inspectsBefore, 'the bundle is not re-inspected while the registry is unloadable');
  assert.equal(host.querySelector('#sandbox-profile-import-break-glass-ack'), null, 'no acknowledgement control while the current rules are unavailable');
  assert.equal(importButton.disabled, true, 'Import stays blocked');
  assert.match([...host.querySelectorAll('#sandbox-profile-import-modal .cron-create-error')].map((el) => el.textContent).join(' '), /stay blocked|stays blocked/);
  importButton.click();
  await harness.act(() => Promise.resolve());
  assert.equal(imports.length, 1, 'no second import request can be sent from stale state');

  // Registry and inspect both succeed: the current carrier appears with its
  // origin, a fresh acknowledgement is required, and only an explicit
  // acknowledged retry sends.
  registryReachable = true;
  previewButton.click();
  await harness.act(() => Promise.resolve());
  await harness.act(() => Promise.resolve());
  assert.equal(loads.length, 3);
  assert.equal(inspects.length, inspectsBefore + 1, 'a successful reload allows the re-inspect');
  const warning = host.querySelector('#sandbox-profile-import-modal .sbx-bg-warning');
  assert.ok(warning, 'the refreshed registry reveals the retained-local carrier');
  assert.match(warning.textContent, /wrapper/);
  assert.match(warning.textContent, /import:A/);
  const ack = host.querySelector('#sandbox-profile-import-break-glass-ack');
  assert.ok(ack, 'a fresh acknowledgement is required');
  assert.notEqual(ack.checked, true);
  assert.equal(importButton.disabled, true, 'import stays blocked until the fresh acknowledgement');

  ack.checked = true;
  ack.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  importButton.click();
  await harness.act(() => Promise.resolve());
  assert.equal(imports.length, 2, 'only the explicit acknowledged retry sends');
  assert.equal(imports[1][2], true);
  unmount();
});

// The final lifecycle probe: closing and reopening the import dialog must
// not discard the failed-registry recovery state — the stale cached registry
// it describes survives the dialog, so the marker must too.
test('close/reopen of the import dialog cannot bypass registry-recovery-required', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { BREAK_GLASS_ACK_CODE }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/sandbox-break-glass.js'),
  ]);
  const state = createManagementState();
  state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [{ name: 'A' }]);
  let registryReachable = false;
  let refused = false;
  const envelope = {
    format: 'tclaude-sandbox-profiles', format_version: 3,
    profiles: [{ name: 'A' }, { name: 'wrapper', includes: ['A'] }],
  };
  const { host, imports, inspects, loads, unmount } = await importScenario(harness, state, envelope, {
    importImpl: () => {
      if (!refused) {
        refused = true;
        throw Object.assign(new Error('retained profile "A" now carries break-glass access'), { status: 422, code: BREAK_GLASS_ACK_CODE });
      }
      return undefined;
    },
    loadImpl: () => {
      if (!registryReachable) return false;
      state.sandboxRequest.commitRequest(state.sandboxRequest.beginRequest(), [
        { name: 'A', break_glass_filesystem: [{ path: '/home/op/.tclaude/data', access: 'read' }] },
      ]);
      return true;
    },
  });
  const importButton = () => [...host.querySelectorAll('#sandbox-profile-import-modal .modal-buttons button')].find((button) => button.textContent === 'Import');
  const previewButton = () => [...host.querySelectorAll('#sandbox-profile-import-modal button')].find((button) => button.textContent === 'Preview');
  const pasteBundle = async () => {
    const rawInput = host.querySelector('#sandbox-profile-import-modal textarea');
    rawInput.value = JSON.stringify(envelope);
    rawInput.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
    await harness.act(() => Promise.resolve());
  };

  importButton().click();
  await harness.act(() => Promise.resolve());
  await harness.act(() => Promise.resolve());
  assert.equal(imports.length, 1, 'the typed refusal is not resent');
  assert.equal(loads.length, 1, 'recovery attempted the registry reload');

  // Close and reopen the dialog: the recovery marker must survive.
  state.closeDialog();
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#sandbox-profile-import-modal'), null);
  state.openDialog({ kind: 'sandbox-import' });
  await harness.act(() => Promise.resolve());
  await pasteBundle();

  const inspectsBefore = inspects.length;
  previewButton().click();
  await harness.act(() => Promise.resolve());
  await harness.act(() => Promise.resolve());
  assert.equal(loads.length, 2, 'the reopened Preview still re-attempts the registry reload');
  assert.equal(inspects.length, inspectsBefore, 'no inspect against the stale cached registry');
  assert.equal(host.querySelector('#sandbox-profile-import-break-glass-ack'), null, 'no acknowledgement control while current rules are unavailable');
  assert.equal(importButton().disabled, true, 'Import stays blocked after reopen');
  importButton().click();
  await harness.act(() => Promise.resolve());
  assert.equal(imports.length, 1, 'no import request can be sent from the reopened stale state');

  // Registry and inspect both succeed after reopen: current carrier appears,
  // fresh acknowledgement required, only the explicit retry sends.
  registryReachable = true;
  previewButton().click();
  await harness.act(() => Promise.resolve());
  await harness.act(() => Promise.resolve());
  assert.equal(inspects.length, inspectsBefore + 1);
  const warning = host.querySelector('#sandbox-profile-import-modal .sbx-bg-warning');
  assert.ok(warning, 'the refreshed registry reveals the retained-local carrier');
  assert.match(warning.textContent, /wrapper/);
  assert.match(warning.textContent, /import:A/);
  const ack = host.querySelector('#sandbox-profile-import-break-glass-ack');
  assert.ok(ack);
  assert.notEqual(ack.checked, true);
  ack.checked = true;
  ack.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  importButton().click();
  await harness.act(() => Promise.resolve());
  assert.equal(imports.length, 2, 'only the explicit acknowledged retry sends');
  assert.equal(imports[1][2], true);
  unmount();
});
