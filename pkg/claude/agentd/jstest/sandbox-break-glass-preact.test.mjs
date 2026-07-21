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
