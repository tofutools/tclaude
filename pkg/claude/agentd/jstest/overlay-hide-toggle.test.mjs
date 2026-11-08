// Tests for the hide toggle (tcl-ht7r39): Ctrl ; minimizes every overlay
// window, and pressing it again brings back just the ones it minimized.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function setup(t) {
  const harness = await createPreactHarness(t);
  const [instances, bulk, kb] = await Promise.all([
    harness.importDashboardModule('js/overlay-instances.js'),
    harness.importDashboardModule('js/overlay-window-bulk.js'),
    harness.importDashboardModule('js/keybindings.js'),
  ]);
  instances.overlayHostUp.value = true;
  bulk.resetOverlayWindowsHiddenForTest();
  const stopKey = bulk.installOverlayHideKey(harness.window);
  t.after(() => { stopKey(); kb.setShortcutOverrides({}); bulk.resetOverlayWindowsHiddenForTest(); });
  const doc = harness.window.document;
  const key = (init, target = doc.body) => harness.act(() => harness.fireEvent(target, 'keydown', init));
  const press = (target) => key({ key: ';', code: 'Semicolon', ctrlKey: true }, target);
  const open = (id) => harness.act(() => instances.openOverlayWindow({ id, kind: 'chat', agent: id, label: id }));
  const away = () => instances.overlayWindows.peek().filter((window) => window.minimized).map((window) => window.active).sort();
  const shown = () => instances.overlayWindows.peek().filter((window) => !window.minimized).map((window) => window.active).sort();
  return { harness, instances, bulk, kb, doc, key, press, open, away, shown };
}

test('Ctrl+; minimizes every window, and again brings back the same ones', async (t) => {
  const s = await setup(t);
  await s.open('a');
  await s.open('b');
  await s.open('c');
  await s.harness.act(() => s.instances.minimizeOverlayWindow('c'));
  await s.press();
  assert.deepEqual(s.away(), ['a', 'b', 'c'], 'every window is on the dock');
  await s.press();
  assert.deepEqual(s.shown(), ['a', 'b'], 'the two it hid come back');
  assert.deepEqual(s.away(), ['c'], 'the one minimized before stays on the dock');
  await s.press();
  assert.deepEqual(s.away(), ['a', 'b', 'c'], 'and the next press hides again');
});

test('a window opened or minimized in between is left alone', async (t) => {
  const s = await setup(t);
  await s.open('a');
  await s.open('b');
  await s.press();
  await s.open('new');
  assert.deepEqual(s.shown(), ['new']);
  await s.press();
  assert.deepEqual(s.shown(), ['a', 'b', 'new'], 'the new window stays open, the hidden ones come back');
  await s.press();
  assert.deepEqual(s.away(), ['a', 'b', 'new']);
  await s.harness.act(() => s.instances.minimizeOverlayWindow('b', false));
  await s.harness.act(() => s.instances.minimizeOverlayWindow('b'));
  await s.harness.act(() => s.instances.minimizeOverlayWindow('a', false));
  await s.press();
  assert.deepEqual(s.shown(), ['a', 'new'], 'b, brought back and minimized again by hand, stays on the dock');
  assert.deepEqual(s.away(), ['b']);
});

test('a held key toggles once', async (t) => {
  const s = await setup(t);
  await s.open('a');
  await s.press();
  const held = s.harness.fireEvent(s.doc.body, 'keydown', { key: ';', code: 'Semicolon', ctrlKey: true, repeat: true });
  await s.harness.act(() => {});
  assert.deepEqual(s.away(), ['a'], 'the repeat does not bring it back');
  assert.equal(held.defaultPrevented, true, 'but is still taken from the terminal');
});

test('with none of its windows left on the dock, the press hides again', async (t) => {
  const s = await setup(t);
  await s.open('a');
  await s.press();
  await s.harness.act(() => s.bulk.restoreAllOverlayWindows());
  await s.press();
  assert.deepEqual(s.away(), ['a'], 'restored by hand: the press minimizes, not a no-op');
});

test('it fires from a focused terminal, and not over a dialog', async (t) => {
  const s = await setup(t);
  await s.open('a');
  const terminal = s.doc.body.appendChild(s.doc.createElement('textarea'));
  t.after(() => terminal.remove());
  terminal.focus();
  const event = s.harness.fireEvent(terminal, 'keydown', { key: ';', code: 'Semicolon', ctrlKey: true });
  await s.harness.act(() => {});
  assert.deepEqual(s.away(), ['a']);
  assert.equal(event.defaultPrevented, true, 'the key is taken, so xterm does not type it');
  await s.press(terminal);
  assert.deepEqual(s.shown(), ['a']);

  const dialog = s.doc.body.appendChild(s.doc.createElement('div'));
  dialog.className = 'modal-overlay show';
  t.after(() => dialog.remove());
  await s.press();
  assert.deepEqual(s.shown(), ['a'], 'nothing happens over a dialog');
});

test('a rebound chord takes over from Ctrl+;', async (t) => {
  const s = await setup(t);
  await s.open('a');
  s.kb.setShortcutOverrides({ 'windows-hide-toggle': 'Ctrl+Alt+KeyH' });
  await s.press();
  assert.deepEqual(s.shown(), ['a'], 'the old chord does nothing');
  await s.key({ key: 'h', code: 'KeyH', ctrlKey: true, altKey: true });
  assert.deepEqual(s.away(), ['a']);
});

test('the dock menu and the palette show the shortcut on minimize and restore', async (t) => {
  const s = await setup(t);
  await s.open('a');
  await s.open('b');
  await s.harness.act(() => s.instances.minimizeOverlayWindow('b'));
  const commands = s.bulk.buildOverlayBulkCommands();
  for (const id of ['overlay.minimize-all', 'overlay.restore-all']) {
    assert.equal(commands.find((command) => command.id === id)?.shortcut, 'windows-hide-toggle', id);
  }
  assert.equal(s.kb.shortcutText('windows-hide-toggle', 'linux'), 'Ctrl ;');
  assert.deepEqual(s.kb.rebindProblems('windows-hide-toggle', 'Ctrl+Semicolon', {}, 'linux'), [], 'Ctrl+; collides with nothing');
});
