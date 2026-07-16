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
