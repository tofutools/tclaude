// Tests for a process editor in a browser window of its own (tcl-kjl2fr): the
// draft crosses as data, both ways, and the page that sent it keeps its editor
// until the window has taken the draft up.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const quietValidation = { delayMs: 60_000, fetchFn: async () => ({ ok: true, json: async () => ({ diagnostics: [] }) }) };

async function openEditors(t) {
  const harness = await createPreactHarness(t);
  const previous = { raf: globalThis.requestAnimationFrame, css: globalThis.CSS };
  globalThis.requestAnimationFrame = () => 0;
  globalThis.CSS = { escape: (value) => String(value) };
  t.after(() => {
    if (previous.raf === undefined) delete globalThis.requestAnimationFrame;
    else globalThis.requestAnimationFrame = previous.raf;
    if (previous.css === undefined) delete globalThis.CSS;
    else globalThis.CSS = previous.css;
  });
  const { openTemplateEditor } = await harness.importDashboardModule('js/process-editor.js');
  const open = async (options) => {
    const host = harness.document.body.appendChild(harness.document.createElement('div'));
    let editor;
    await harness.act(async () => { editor = await openTemplateEditor(host, { ...options, config: { validation: quietValidation } }); });
    editors.push(editor);
    return editor;
  };
  // Torn down while the page is still there.
  const editors = [];
  const close = () => harness.act(() => editors.forEach((editor) => editor.destroy()));
  return { harness, open, close };
}

test('a handed-off draft goes on in another editor with its history', async (t) => {
  const { harness, open, close } = await openEditors(t);
  const sent = await open({ id: 'flow', blank: true });
  await harness.act(() => {
    sent.model.addNode('task', { x: 10, y: 20, id: 'one' });
    sent.model.addNode('task', { x: 10, y: 120, id: 'two' });
    sent.applyHistory('undo');
  });
  assert.equal(sent.model.dirty, true);
  // Through JSON, as it crosses between windows.
  const handoff = JSON.parse(JSON.stringify(sent.handoff()));
  const taken = await open({ id: 'flow', handoff });
  assert.equal(taken.model.dirty, true, 'unsaved edits stay unsaved');
  assert.deepEqual(Object.keys(taken.model.template.nodes).sort(), Object.keys(sent.model.template.nodes).sort());
  assert.equal(taken.model.undoStack.length, sent.model.undoStack.length);
  assert.equal(taken.model.redoStack.length, 1, 'the undone edit can come back');
  assert.equal(taken.model.sourceHash, sent.model.sourceHash, 'a save checks against the version the draft began from');
  await harness.act(() => taken.applyHistory('redo'));
  assert.ok(taken.model.template.nodes.two, 'redo replays the edit made in the other window');
  await harness.act(() => { taken.applyHistory('undo'); taken.applyHistory('undo'); });
  assert.equal(taken.model.dirty, false, 'undoing everything is the saved version again');
  await close();
});

function fakeWindows(harness) {
  const timers = [];
  const popups = [];
  const win = {
    screen: {}, screenX: 0, screenY: 0, outerWidth: 1, innerWidth: 1, outerHeight: 1, innerHeight: 1,
    setInterval: (callback) => { timers.push(callback); return timers.length; },
    clearInterval: () => {},
    focus: () => { win.focused += 1; },
    focused: 0,
    open: (url, name) => {
      const popup = { url, name, closed: false, focus() {}, opener: win };
      popups.push(popup);
      return popup;
    },
  };
  const live = () => {
    const mount = harness.document.body.appendChild(harness.document.createElement('div'));
    return { mount, editor: { destroyed: 0, destroy() { this.destroyed += 1; }, handoff: () => ({ base: { template: { id: 'flow' } }, rev: 1 }) } };
  };
  return { win, timers, popups, live, tick: () => timers.forEach((callback) => callback()) };
}

test('a window takes the draft up, and only then does the editor here go', async (t) => {
  const harness = await createPreactHarness(t);
  const place = await harness.importDashboardModule('js/process-editor-window.js');
  const { win, popups, live, tick } = fakeWindows(harness);
  const uninstall = place.installProcessWindows(win, { open: async () => true });
  t.after(uninstall);
  const sent = live();
  const restored = [];
  assert.equal(place.editorToWindow({ id: 'flow', label: 'Flow', live: sent, win, restore: (back) => restored.push(back) }), true);
  const [popup] = popups;
  assert.equal(popup.url, '/automations/processes/templates/flow');
  assert.ok(place.isProcessSolo(popup));
  assert.equal(place.heldForWindow(sent.editor), true, 'kept aside while the window opens');
  assert.equal(place.processEditorAway('flow'), popup, 'one place: the window has it');

  const solo = place.soloHandoff(popup);
  assert.deepEqual({ id: solo.id, label: solo.label, handoff: solo.handoff }, { id: 'flow', label: 'Flow', handoff: { base: { template: { id: 'flow' } }, rev: 1 } });
  assert.equal(sent.editor.destroyed, 0, 'claimed is not yet taken');
  solo.taken();
  assert.equal(sent.editor.destroyed, 1);
  assert.equal(place.heldForWindow(sent.editor), false);
  tick();
  assert.deepEqual(restored, []);
  assert.equal(place.soloHandoff(popup), null, 'a draft is taken up once');
});

test('a window closed before it takes the draft up gives the editor back', async (t) => {
  const harness = await createPreactHarness(t);
  const place = await harness.importDashboardModule('js/process-editor-window.js');
  const { win, popups, live, tick } = fakeWindows(harness);
  const sent = live();
  const restored = [];
  place.editorToWindow({ id: 'flow', label: 'Flow', live: sent, win, restore: (back) => restored.push(back) });
  tick();
  assert.deepEqual(restored, [], 'still opening');
  popups[0].closed = true;
  tick();
  assert.deepEqual(restored, [sent]);
  assert.equal(sent.editor.destroyed, 0);
  assert.equal(place.processEditorAway('flow'), null);
});

test('a blocked pop-up keeps the editor where it is', async (t) => {
  const harness = await createPreactHarness(t);
  const place = await harness.importDashboardModule('js/process-editor-window.js');
  const { win, live } = fakeWindows(harness);
  win.open = () => null;
  const said = [];
  assert.equal(place.editorToWindow({ id: 'flow', label: 'Flow', live: live(), win, notify: (text, error) => said.push([text, error]), restore: () => assert.fail('nothing to restore') }), false);
  assert.match(said[0][0], /allow pop-ups/);
  assert.equal(place.processEditorAway('flow'), null);
});

test('a window hands its draft back to the dashboard that opened it', async (t) => {
  const harness = await createPreactHarness(t);
  const place = await harness.importDashboardModule('js/process-editor-window.js');
  const { win } = fakeWindows(harness);
  const opened = [];
  let accept = true;
  let floats = false;
  t.after(place.installProcessWindows(win, { open: async (...args) => { opened.push(args); return accept; }, floats: () => floats }));
  const popup = { name: `${place.PROCESS_WINDOW_NAME}x`, opener: win };
  assert.equal(place.openerTakesBack(popup), true);
  assert.equal(place.openerFloats(popup), false, 'no overlay to go back to with Collab off');
  floats = true;
  assert.equal(place.openerFloats(popup), true);
  const editor = { handoff: () => ({ rev: 3 }) };
  assert.equal(await place.editorBack({ id: 'flow', label: 'Flow', editor, overlay: true, win: popup }), true);
  assert.deepEqual(opened, [['flow', 'Flow', { rev: 3 }, { overlay: true }]]);
  assert.equal(win.focused, 1, 'the dashboard comes forward');
  accept = false;
  assert.equal(await place.editorBack({ id: 'flow', label: 'Flow', editor, win: popup }), false, 'a refused draft stays in the window');
  win.closed = true;
  assert.equal(place.openerTakesBack(popup), false);
});
