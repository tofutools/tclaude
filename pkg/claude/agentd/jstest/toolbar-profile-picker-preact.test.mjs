import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function mountPicker(t, overrides = {}) {
  const harness = await createPreactHarness(t);
  const { createToolbarProfilePickerState } = await harness.importDashboardModule('js/toolbar-profile-picker-state.js');
  const { mountToolbarProfilePickerIsland } = await harness.importDashboardModule('js/toolbar-profile-picker-island.js');
  const profileHost = harness.document.body.appendChild(harness.document.createElement('span'));
  const sandboxHost = harness.document.body.appendChild(harness.document.createElement('span'));
  const state = createToolbarProfilePickerState();
  let cleanup;
  mountToolbarProfilePickerIsland({
    profileHost, sandboxHost,
    state,
    actions: {
      load: async () => ({ choices: [{ value: 'alpha', label: 'alpha' }] }),
      commit: async () => true,
      openNew() {},
      commitFromEditor: async () => true,
      ...overrides,
    },
    registerCleanup: (registered) => { cleanup = registered; },
  });
  return { harness, profileHost, sandboxHost, state, cleanup };
}

function popup(host) {
  return host.querySelector('.tc-select-popover');
}

function option(host, label) {
  return [...host.querySelectorAll('[role="option"]')]
    .find((item) => item.textContent.trim().endsWith(label));
}

test('toolbar profile controls open browser-contained listboxes', async (t) => {
  const picker = await mountPicker(t);
  await picker.harness.act(() => {
    picker.state.update('profile', 'alpha');
    picker.state.update('sandbox', 'safe');
  });
  const profileButton = picker.profileHost.querySelector('button');
  const sandboxButton = picker.sandboxHost.querySelector('button');
  assert.equal(profileButton.textContent, '🧠 alpha▾');
  assert.equal(profileButton.getAttribute('aria-haspopup'), 'listbox');
  assert.equal(profileButton.getAttribute('aria-expanded'), 'false');
  assert.equal(sandboxButton.textContent, '🛡 safe▾');

  await picker.harness.act(() => picker.harness.fireEvent(profileButton, 'click'));
  await picker.harness.act(async () => {});
  const listbox = popup(picker.profileHost);
  assert.equal(listbox.getAttribute('popover'), 'auto', 'the popup uses the browser top layer');
  assert.equal(profileButton.getAttribute('aria-expanded'), 'true');
  assert.equal(picker.harness.document.activeElement, listbox);
  assert.ok(picker.profileHost.querySelector('button'), 'the stable trigger remains mounted while open');
  assert.ok(picker.sandboxHost.querySelector('#dashboard-default-sandbox-profile'),
    'the other toolbar control remains interactive');
  assert.ok(option(picker.profileHost, '＋ new profile…'));

  await picker.harness.act(() => picker.harness.fireEvent(listbox, 'keydown', { key: 'Escape' }));
  assert.equal(picker.state.editor.value, null);
  assert.equal(picker.harness.document.activeElement.id, 'dashboard-default-profile',
    'Escape restores focus to the persistent trigger');

  picker.harness.document.body.classList.add('wizard');
  await picker.harness.act(() => picker.harness.fireEvent(profileButton, 'click'));
  await picker.harness.act(async () => {});
  assert.ok(option(picker.profileHost, '＋ new pattern…'),
    'opening after a live theme change uses wizard vocabulary');
  await picker.harness.act(() => picker.harness.fireEvent(popup(picker.profileHost), 'keydown', { key: 'Escape' }));
  await picker.harness.act(() => picker.harness.fireEvent(sandboxButton, 'click'));
  await picker.harness.act(async () => {});
  assert.ok(option(picker.sandboxHost, '＋ new ward…'),
    'the sandbox Select uses wizard ward vocabulary');
  picker.cleanup();
});

test('toolbar listbox stays focused while a delayed option load completes', async (t) => {
  const picker = await mountPicker(t, { load: () => new Promise((resolve) => {
    setTimeout(() => resolve({ choices: [{ value: 'alpha', label: 'alpha' }] }), 10);
  }) });
  await picker.harness.act(() => picker.state.open({ kind: 'profile', current: '' }));
  await picker.harness.act(async () => {});
  await new Promise((resolve) => setTimeout(resolve, 10));
  const listbox = popup(picker.profileHost);
  assert.equal(listbox.getAttribute('aria-busy'), null, 'the delayed load finished rendering');
  assert.equal(picker.harness.document.activeElement, listbox);
  assert.ok(option(picker.profileHost, 'alpha'));
  picker.cleanup();
});

test('toolbar listbox remains cancellable while its profile load stalls', async (t) => {
  const picker = await mountPicker(t, { load: () => new Promise(() => {}) });
  await picker.harness.act(() => picker.state.open({ kind: 'sandbox', current: '' }));
  await new Promise((resolve) => setTimeout(resolve, 0));
  const listbox = popup(picker.sandboxHost);
  assert.equal(listbox.getAttribute('aria-busy'), 'true');
  assert.equal(picker.harness.document.activeElement, listbox);
  await picker.harness.act(() => picker.harness.fireEvent(listbox, 'keydown', { key: 'Escape' }));
  assert.equal(picker.state.editor.value, null);
  picker.cleanup();
});

test('toolbar listbox renders accessible load and save failures', async (t) => {
  const loadPicker = await mountPicker(t, { load: async () => { throw new Error('registry unavailable'); } });
  await loadPicker.harness.act(() => loadPicker.state.open({ kind: 'sandbox', current: '' }));
  await loadPicker.harness.act(async () => {});
  const loadError = loadPicker.sandboxHost.querySelector('[role="alert"]');
  assert.match(loadError.textContent, /registry unavailable/);
  assert.equal(loadError.closest('[role="listbox"]'), popup(loadPicker.sandboxHost));
  loadPicker.cleanup();

  const savePicker = await mountPicker(t, { commit: async () => { throw new Error('save rejected'); } });
  await savePicker.harness.act(() => savePicker.state.open({ kind: 'profile', current: '' }));
  await savePicker.harness.act(async () => {});
  await savePicker.harness.act(() => savePicker.harness.fireEvent(option(savePicker.profileHost, 'alpha'), 'click'));
  await new Promise((resolve) => setTimeout(resolve, 0));
  await savePicker.harness.act(async () => {});
  const saveError = savePicker.profileHost.querySelector('[role="alert"]');
  assert.match(saveError.textContent, /save rejected/);
  assert.equal(savePicker.profileHost.querySelector('button').getAttribute('aria-describedby'), saveError.id);
  assert.equal(savePicker.profileHost.querySelector('button').disabled, false,
    'a failed save is retryable');
  assert.equal(savePicker.state.editor.value.kind, 'profile', 'a failed save keeps the listbox open');
  savePicker.cleanup();
});

test('a successful listbox save releases its lock before the control is reopened', async (t) => {
  const picker = await mountPicker(t);
  await picker.harness.act(() => picker.state.open({ kind: 'profile', current: '' }));
  await picker.harness.act(async () => {});
  picker.harness.fireEvent(option(picker.profileHost, 'alpha'), 'click');
  await new Promise((resolve) => setTimeout(resolve, 0));
  await picker.harness.act(async () => {});
  assert.equal(picker.state.editor.value, null, 'success closes the listbox');
  assert.equal(picker.harness.document.activeElement, picker.profileHost.querySelector('button'),
    'successful selection restores focus to its trigger');
  await picker.harness.act(() => picker.state.open({ kind: 'profile', current: '' }));
  assert.equal(picker.profileHost.querySelector('button').disabled, false);
  picker.cleanup();
});

test('a stale listbox save completion cannot close a replacement control', async (t) => {
  let resolveCommit;
  let commitStarted = false;
  const commitGate = new Promise((resolve) => { resolveCommit = resolve; });
  const picker = await mountPicker(t, {
    commit: () => { commitStarted = true; return commitGate; },
  });
  await picker.harness.act(() => picker.state.open({ kind: 'profile', current: '' }));
  await picker.harness.act(async () => {});
  await new Promise((resolve) => setTimeout(resolve, 10));
  picker.harness.fireEvent(option(picker.profileHost, 'alpha'), 'click');
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(commitStarted, true);

  const replacement = picker.state.open({ kind: 'sandbox', current: 'beta' });
  await new Promise((resolve) => setTimeout(resolve, 0));
  resolveCommit(true);
  await Promise.resolve();
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(picker.state.editor.value === replacement, true,
    'the old generation cannot clear the replacement descriptor');
  assert.equal(popup(picker.sandboxHost).hidden, false);
  await picker.harness.act(() => picker.state.close(replacement));
  await picker.harness.act(() => picker.state.open({ kind: 'profile', current: '' }));
  assert.equal(picker.profileHost.querySelector('button').disabled, false,
    'the stale successful save releases its local lock before this control is reopened');
  picker.cleanup();
});

test('controller replays values painted before mount and is released on cleanup', async (t) => {
  const harness = await createPreactHarness(t);
  const [island, stateModule, controller] = await Promise.all([
    harness.importDashboardModule('js/toolbar-profile-picker-island.js'),
    harness.importDashboardModule('js/toolbar-profile-picker-state.js'),
    harness.importDashboardModule('js/toolbar-profile-picker.js'),
  ]);
  controller.updateToolbarProfileValue('profile', 'cached');
  const profileHost = harness.document.body.appendChild(harness.document.createElement('span'));
  const sandboxHost = harness.document.body.appendChild(harness.document.createElement('span'));
  const state = stateModule.createToolbarProfilePickerState();
  let cleanup;
  island.mountToolbarProfilePickerIsland({
    profileHost, sandboxHost, state,
    actions: { load: async () => ({ choices: [] }), commit: async () => true, openNew() {} },
    registerCleanup: (registered) => { cleanup = registered; },
  });
  assert.equal(profileHost.querySelector('button').textContent, '🧠 cached▾');
  cleanup();
  assert.throws(() => controller.openToolbarProfilePicker(), /not ready/);
});

test('profile mutation starts and awaits a newer poll generation before repaint', async (t) => {
  const harness = await createPreactHarness(t);
  const events = [];
  globalThis.__toolbarProfileTestEvents = events;
  await harness.replaceDashboardModule('js/profiles.js', `
    export async function loadProfiles() { return []; }
    export function profileChoices() { return []; }
    export async function setDashDefaultProfile(name) {
      globalThis.__toolbarProfileTestEvents.push('mutate:' + name);
      if (name === 'broken') throw new Error('save rejected');
      return name;
    }
  `);
  await harness.replaceDashboardModule('js/sandbox-profiles.js', `
    export async function loadSandboxProfiles() { return []; }
    export function openSandboxProfileEditor() {}
  `);
  await harness.replaceDashboardModule('js/modal-profiles.js', 'export function openProfileEditor() {}');
  await harness.replaceDashboardModule('js/toolbar-profile-renderers.js', `
    export function renderDashDefaultProfile() { globalThis.__toolbarProfileTestEvents.push('repaint'); }
    export function renderDashSandboxProfile() {}
  `);
  await harness.replaceDashboardModule('js/agent-spawn-controller.js', 'export function refreshAgentSpawnSandboxPolicy() {}');
  const { createToolbarProfilePickerActions } = await harness.importDashboardModule('js/toolbar-profile-picker-actions.js');
  let releaseRefresh;
  const refreshGate = new Promise((resolve) => { releaseRefresh = resolve; });
  const actions = createToolbarProfilePickerActions({
    notify() {},
    refresh: async () => {
      events.push('refresh:start');
      await refreshGate;
      events.push('refresh:accepted');
    },
  });
  const committing = actions.commit('profile', 'alpha');
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.deepEqual(events, ['mutate:alpha', 'refresh:start'],
    'an older in-flight publish is invalidated before the chip can repaint');
  releaseRefresh();
  await committing;
  assert.deepEqual(events, ['mutate:alpha', 'refresh:start', 'refresh:accepted', 'repaint']);

  const notices = [];
  const editorActions = createToolbarProfilePickerActions({
    notify: (...args) => notices.push(args),
    refresh: async () => {},
  });
  assert.equal(await editorActions.commitFromEditor('profile', 'broken'), false);
  assert.deepEqual(notices, [['set global default spawn profile failed: save rejected', true]],
    'a non-awaited editor callback is converted into visible feedback');
  delete globalThis.__toolbarProfileTestEvents;
});
