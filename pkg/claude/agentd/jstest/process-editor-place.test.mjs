// Tests for where a process template's editor is (tcl-kjl2fr): docked in the
// Processes tab or in an overlay tab, one place at a time, with the live editor
// moving between them so unsaved edits go with it.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function setup(t) {
  const harness = await createPreactHarness(t);
  const [place, instances] = await Promise.all([
    harness.importDashboardModule('js/process-editor-place.js'),
    harness.importDashboardModule('js/overlay-instances.js'),
  ]);
  const doc = harness.window.document;
  const editor = (dirty = false) => {
    const mount = place.processEditorMount(doc);
    doc.body.appendChild(mount);
    return { mount, model: { dirty, template: { id: 'release-train' } }, destroyed: 0, destroy() { this.destroyed += 1; } };
  };
  const tabs = () => instances.overlayTabs.peek().filter((tab) => tab.kind === place.PROCESS_WINDOW_KIND);
  t.after(() => instances.overlayWindows.value = []);
  return { harness, place, instances, editor, tabs };
}

test('floating an editor moves it, as it is, to an overlay tab of its own', async (t) => {
  const { harness, place, editor, tabs } = await setup(t);
  const live = editor(true);
  await harness.act(() => place.floatProcessEditor({ id: 'release-train', label: 'Release train', editor: live }));
  assert.deepEqual(tabs().map(({ id, template, label }) => ({ id, template, label })), [{ id: 'process:release-train', template: 'release-train', label: 'Release train' }]);
  assert.equal(live.mount.isConnected, false, 'it leaves the Processes tab');
  assert.equal(place.floatingProcessEditor('release-train').editor, live);
  assert.equal(live.destroyed, 0, 'nothing is torn down on the way');

  const taken = place.takeFloatingEditor('release-train');
  assert.equal(taken.editor, live, 'docking hands over the same editor');
  assert.deepEqual(tabs(), [], 'and its overlay tab goes');
  assert.equal(live.destroyed, 0, 'without tearing it down');
  assert.equal(place.floatingProcessEditor('release-train'), null);
});

test('an overlay tab closed for good takes its editor with it', async (t) => {
  const { harness, place, instances, editor, tabs } = await setup(t);
  const live = editor();
  await harness.act(() => place.floatProcessEditor({ id: 'release-train', label: 'Release train', editor: live }));
  await harness.act(() => instances.minimizeOverlayTab('process:release-train'));
  assert.equal(live.destroyed, 0, 'a tab on the dock keeps its editor');
  await harness.act(() => instances.closeOverlayTab('process:release-train'));
  assert.equal(live.destroyed, 1);
  assert.deepEqual(tabs(), []);
});

test('closing a floating editor with unsaved edits asks first', async (t) => {
  const { harness, place, editor, tabs } = await setup(t);
  let answer = false;
  let asked = 0;
  place.registerProcessEditorHost({ confirmDiscard: async () => { asked += 1; return answer; } });
  const live = editor(true);
  await harness.act(() => place.floatProcessEditor({ id: 'release-train', label: 'Release train', editor: live }));
  assert.equal(await place.closeFloatingEditor('release-train'), false);
  assert.equal(asked, 1);
  assert.equal(tabs().length, 1, 'kept edits keep the tab');
  assert.equal(live.destroyed, 0);
  answer = true;
  assert.equal(await place.closeFloatingEditor('release-train'), true);
  assert.equal(live.destroyed, 1, 'discarded edits go with the editor');
  assert.deepEqual(tabs(), []);
});

test('docking waits for the Processes tab to let its own editor go', async (t) => {
  const { harness, place, editor, tabs } = await setup(t);
  let leave = false;
  const docked = [];
  place.registerProcessEditorHost({ canLeave: async () => leave, dock: (id, live, label) => docked.push({ id, live, label }) });
  const live = editor(true);
  await harness.act(() => place.floatProcessEditor({ id: 'release-train', label: 'Release train', editor: live }));
  assert.equal(await place.dockProcessEditor('release-train'), false);
  assert.equal(tabs().length, 1, 'refused, it stays floating');
  leave = true;
  assert.equal(await place.dockProcessEditor('release-train'), true);
  assert.deepEqual(docked.map(({ id, label }) => ({ id, label })), [{ id: 'release-train', label: 'Release train' }]);
  assert.equal(docked[0].live.editor, live);
  assert.equal(live.destroyed, 0);
  assert.deepEqual(tabs(), []);
});

test('the docked editor hands its live mount over, and takes one back', async (t) => {
  const { harness, place, editor } = await setup(t);
  const [{ createProcessesState }, { ProcessEditorBoundary }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes') });
  let opened = 0;
  const openEditor = async (mount) => {
    opened += 1;
    const live = editor(true);
    live.mount.remove();
    return { ...live, mount };
  };
  const spec = { id: 'release-train', name: 'Release train', blank: false, key: 'a:1' };
  const mounted = await harness.mount(harness.html`<${ProcessEditorBoundary} spec=${spec} state=${state} openEditor=${openEditor} />`);
  await harness.act(async () => {});
  const shown = state.currentEditor();
  assert.equal(opened, 1);
  assert.equal(mounted.container.querySelector('#process-editor-canvas'), shown.mount);
  // Float it, as the action does, and the Processes tab lets the canvas go.
  await harness.act(() => place.floatProcessEditor({ id: 'release-train', label: 'Release train', editor: shown }));
  await mounted.rerender(harness.html`<div />`);
  assert.equal(shown.destroyed, 0, 'an editor that floated lives on');
  assert.equal(state.currentEditor(), null);
  // Docked again, it comes back as it was, without opening another.
  const live = place.takeFloatingEditor('release-train');
  await mounted.rerender(harness.html`<${ProcessEditorBoundary} spec=${{ ...spec, key: 'a:2', live }} state=${state} openEditor=${openEditor} />`);
  assert.equal(opened, 1);
  assert.equal(state.currentEditor(), shown);
  assert.equal(mounted.container.querySelector('#process-editor-canvas'), shown.mount);
  await mounted.unmount();
  assert.equal(shown.destroyed, 1, 'leaving the tab for good still tears it down');
});

test('a floating editor takes Ctrl/Cmd+S only while its window has the keyboard', async (t) => {
  const { harness, place } = await setup(t);
  const doc = harness.window.document;
  const window = doc.body.appendChild(doc.createElement('section'));
  window.className = 'process-overlay';
  const mount = window.appendChild(place.processEditorMount(doc));
  const field = mount.appendChild(doc.createElement('input'));
  const docked = () => true;
  assert.equal(place.processShortcutLive(mount, docked), false);
  field.focus();
  assert.equal(place.processShortcutLive(mount, docked), true);
  const dockedMount = doc.body.appendChild(place.processEditorMount(doc));
  assert.equal(place.processShortcutLive(dockedMount, () => false), false, 'docked, it follows the Processes tab');
  assert.equal(place.processShortcutLive(dockedMount, docked), true);
  dockedMount.remove();
  assert.equal(place.processShortcutLive(dockedMount, docked), false, 'a mount out of the page takes nothing');
});
