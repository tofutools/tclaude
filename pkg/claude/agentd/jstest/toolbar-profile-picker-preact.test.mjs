import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('toolbar profile picker keeps its draft mounted and does not steal focus from a newer overlay', async (t) => {
  const harness = await createPreactHarness(t);
  const { createToolbarProfilePickerState } = await harness.importDashboardModule('js/toolbar-profile-picker-state.js');
  const { mountToolbarProfilePickerIsland } = await harness.importDashboardModule('js/toolbar-profile-picker-island.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const producer = harness.document.body.appendChild(harness.document.createElement('button'));
  producer.id = 'dashboard-default-profile';
  let focused = 0;
  producer.addEventListener('focus', () => { focused++; });
  const state = createToolbarProfilePickerState();
  const cleanups = [];
  mountToolbarProfilePickerIsland({
    host,
    state,
    actions: {
      load: async () => ({ choices: [{ value: 'alpha', label: 'alpha' }] }),
      commit: async () => true,
      openNew() {},
    },
    registerCleanup: (cleanup) => cleanups.push(cleanup),
  });

  await harness.act(() => state.open({
    kind: 'profile', current: '', producerId: producer.id,
  }));
  const dialog = host.querySelector('#toolbar-profile-picker-modal');
  assert.ok(dialog);
  await harness.act(async () => {});
  await new Promise((resolve) => setTimeout(resolve, 10));
  assert.ok(dialog.querySelector('.group-default-profile-select'),
    'the picker draft remains mounted outside snapshot reconciliation');

  await harness.act(() => harness.fireEvent(dialog.querySelector('.modal-buttons button'), 'click'));
  const newer = harness.document.body.appendChild(harness.document.createElement('div'));
  newer.className = 'modal-overlay show';
  await new Promise((resolve) => setTimeout(resolve, 10));
  assert.equal(focused, 0, 'deferred restore yields to the newer dialog owner');
  newer.remove();

  await harness.act(() => state.open({
    kind: 'profile', current: '', producerId: producer.id,
  }));
  await harness.act(async () => {});
  await new Promise((resolve) => setTimeout(resolve, 10));
  await harness.act(() => harness.fireEvent(host.querySelector('.modal-buttons button'), 'click'));
  await new Promise((resolve) => setTimeout(resolve, 10));
  assert.equal(focused, 1, 'an uncontested close restores the stable producer focus');
  for (const cleanup of cleanups.reverse()) cleanup();
});

async function mountPicker(t, load) {
  const harness = await createPreactHarness(t);
  const { createToolbarProfilePickerState } = await harness.importDashboardModule('js/toolbar-profile-picker-state.js');
  const { mountToolbarProfilePickerIsland } = await harness.importDashboardModule('js/toolbar-profile-picker-island.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const state = createToolbarProfilePickerState();
  let cleanup;
  mountToolbarProfilePickerIsland({
    host,
    state,
    actions: { load, commit: async () => true, openNew() {} },
    registerCleanup: (registered) => { cleanup = registered; },
  });
  return { harness, host, state, cleanup };
}

test('toolbar profile picker remains focused and cancellable while loading stalls', async (t) => {
  const picker = await mountPicker(t, () => new Promise(() => {}));
  picker.state.open({ kind: 'profile', current: '' });
  await new Promise((resolve) => setTimeout(resolve, 10));
  const dialog = picker.host.querySelector('[role="dialog"]');
  const cancel = [...dialog.querySelectorAll('button')].find((button) => button.textContent === 'Cancel');
  assert.equal(picker.harness.document.activeElement === cancel, true,
    'the enabled Cancel button keeps focus inside the loading dialog');
  picker.harness.fireEvent(cancel, 'click');
  assert.equal(picker.state.dialog.value, null, 'Cancel closes before the load settles');
  picker.cleanup();
});

test('toolbar profile picker focuses the select after a delayed load', async (t) => {
  const picker = await mountPicker(t, () => new Promise((resolve) => {
    setTimeout(() => resolve({ choices: [{ value: 'alpha', label: 'alpha' }] }), 10);
  }));
  await picker.harness.act(() => picker.state.open({ kind: 'profile', current: '' }));
  await picker.harness.act(async () => {});
  await new Promise((resolve) => setTimeout(resolve, 10));
  const dialog = picker.host.querySelector('[role="dialog"]');
  const active = picker.harness.document.activeElement;
  const select = dialog.querySelector('.group-default-profile-select');
  assert.equal(select.disabled, false, 'the delayed load finished rendering');
  assert.equal(active === select, true,
    'the enabled select explicitly receives focus when loading completes');
  picker.harness.fireEvent(picker.host.querySelector('.modal-overlay'), 'mousedown');
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(picker.state.dialog.value, null, 'backdrop cancellation remains available after loading');
  picker.cleanup();
});

test('toolbar profile picker delayed load does not steal focus from a newer overlay', async (t) => {
  let focusNewerOverlay;
  const picker = await mountPicker(t, () => new Promise((resolve) => {
    setTimeout(() => {
      focusNewerOverlay();
      resolve({ choices: [] });
    }, 10);
  }));
  const newerOverlay = picker.harness.document.body.appendChild(picker.harness.document.createElement('div'));
  newerOverlay.className = 'modal-overlay show';
  const newerButton = newerOverlay.appendChild(picker.harness.document.createElement('button'));
  focusNewerOverlay = () => newerButton.focus();
  await picker.harness.act(() => picker.state.open({ kind: 'profile', current: '' }));
  await picker.harness.act(async () => {});
  await new Promise((resolve) => setTimeout(resolve, 10));
  assert.equal(picker.harness.document.activeElement === newerButton, true);
  newerOverlay.remove();
  picker.cleanup();
});

test('a stale save completion cannot close a replacement picker draft', async (t) => {
  const harness = await createPreactHarness(t);
  const { createToolbarProfilePickerState } = await harness.importDashboardModule('js/toolbar-profile-picker-state.js');
  const { mountToolbarProfilePickerIsland } = await harness.importDashboardModule('js/toolbar-profile-picker-island.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const state = createToolbarProfilePickerState();
  let resolveCommit;
  let commitStarted = false;
  const commitGate = new Promise((resolve) => { resolveCommit = resolve; });
  const cleanups = [];
  mountToolbarProfilePickerIsland({
    host,
    state,
    actions: {
      load: async () => ({ choices: [{ value: 'alpha', label: 'alpha' }] }),
      commit: () => { commitStarted = true; return commitGate; },
      openNew() {},
    },
    registerCleanup: (cleanup) => cleanups.push(cleanup),
  });

  await harness.act(() => state.open({ kind: 'profile', current: '' }));
  await harness.act(async () => {});
  await new Promise((resolve) => setTimeout(resolve, 10));
  const select = host.querySelector('.group-default-profile-select');
  Object.defineProperty(select, 'value', { configurable: true, writable: true, value: 'alpha' });
  harness.fireEvent(select, 'change');
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(commitStarted, true);

  const replacement = state.open({ kind: 'sandbox', current: 'beta' });
  await new Promise((resolve) => setTimeout(resolve, 0));
  resolveCommit(true);
  await Promise.resolve();
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(state.dialog.value === replacement, true,
    'the old generation cannot clear the replacement descriptor');
  assert.match(host.querySelector('#toolbar-profile-picker-title').textContent, /global sandbox profile/);
  for (const cleanup of cleanups.reverse()) cleanup();
});

test('partial picker initialization releases its controller and permits a clean remount', async (t) => {
  const harness = await createPreactHarness(t);
  const [island, stateModule, controller] = await Promise.all([
    harness.importDashboardModule('js/toolbar-profile-picker-island.js'),
    harness.importDashboardModule('js/toolbar-profile-picker-state.js'),
    harness.importDashboardModule('js/toolbar-profile-picker.js'),
  ]);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const failedCleanups = [];
  let disposed = 0;
  const brokenState = {
    dialog: { get value() { throw new Error('picker render failed'); } },
    open() {},
    dispose() { disposed += 1; },
  };
  assert.throws(() => island.mountToolbarProfilePickerIsland({
    host,
    state: brokenState,
    actions: {},
    registerCleanup: (cleanup) => failedCleanups.push(cleanup),
  }), /picker render failed/);
  assert.equal(failedCleanups.length, 0, 'failed initialization never publishes a page cleanup');
  assert.equal(disposed, 1);
  assert.equal(host.childElementCount, 0);
  assert.throws(() => controller.openToolbarProfilePicker(), /not ready/,
    'failed initialization releases the global launcher seam');

  const cleanups = [];
  const state = stateModule.createToolbarProfilePickerState();
  island.mountToolbarProfilePickerIsland({
    host,
    state,
    actions: { load: async () => ({ choices: [] }), commit: async () => true, openNew() {} },
    registerCleanup: (cleanup) => cleanups.push(cleanup),
  });
  await harness.act(() => controller.openToolbarProfilePicker({ kind: 'profile', current: '' }));
  assert.ok(host.querySelector('#toolbar-profile-picker-modal'));
  for (const cleanup of cleanups.reverse()) cleanup();
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
  assert.deepEqual(notices, [['set dashboard default profile failed: save rejected', true]],
    'a non-awaited editor callback is converted into visible feedback');
  delete globalThis.__toolbarProfileTestEvents;
});
