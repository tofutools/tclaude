// Tests for the keyboard shortcut registry (tcl-12p3ex): matching by key
// position, the one Cmd vs Ctrl decision, the "Ctrl ," display, and the
// read-only Config › Keyboard list.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createPreactHarness } from './preact-harness.mjs';

const kb = await import('../dashboard/js/keybindings.js');

const key = (init) => ({ type: 'keydown', ctrlKey: false, metaKey: false, altKey: false, shiftKey: false, ...init });

test('every shortcut has a known scope, a parseable chord and a unique id', () => {
  const scopes = new Set(kb.SHORTCUT_SCOPES.map((scope) => scope.id));
  const ids = new Set();
  for (const entry of kb.SHORTCUTS) {
    assert.ok(scopes.has(entry.scope), `${entry.id} scope`);
    assert.ok(!ids.has(entry.id), `${entry.id} is unique`);
    ids.add(entry.id);
    for (const chord of kb.shortcutChords(entry.id)) kb.parseChord(chord);
    assert.ok(entry.label, `${entry.id} label`);
  }
});

test('display spells keys with spaces, never "+", and Mod follows the platform', () => {
  assert.equal(kb.shortcutText('expose', 'linux'), 'Ctrl ,');
  assert.equal(kb.shortcutText('expose', 'macos'), 'Ctrl ,', 'Ctrl , stays Ctrl on macOS: ⌘ , is browser settings');
  assert.equal(kb.shortcutText('palette', 'linux'), 'Ctrl K');
  assert.equal(kb.shortcutText('palette', 'macos'), '⌘ K');
  assert.equal(kb.shortcutText('annotate', 'linux'), 'Ctrl Shift .');
  assert.equal(kb.shortcutText('wizard', 'macos'), 'Option Shift ⌘ W');
  assert.equal(kb.shortcutText('terminal-tab-left', 'windows'), 'Alt Shift ←');
  assert.equal(kb.shortcutText('node-switch', 'linux'), 'Alt 1…9');
  assert.equal(kb.shortcutText('board-zoom-in', 'linux'), '=');
  assert.equal(kb.withShortcut('Undo', 'board-undo', 'macos'), 'Undo (⌘ Z)');
  assert.equal(kb.modClickText('linux'), 'Ctrl-click');
  assert.equal(kb.modClickText('macos'), '⌘-click');
  for (const entry of kb.SHORTCUTS) {
    for (const os of ['linux', 'macos']) {
      assert.doesNotMatch(kb.shortcutText(entry.id, os), /\w\+/, `${entry.id} on ${os}`);
    }
  }
});

test('compose is Ctrl M on macOS too, because ⌘ M minimizes the window', () => {
  assert.equal(kb.shortcutText('compose', 'macos'), 'Ctrl M');
  assert.ok(kb.matchesShortcut(key({ code: 'KeyM', key: 'm', ctrlKey: true }), 'compose', 'macos'));
  assert.ok(!kb.matchesShortcut(key({ code: 'KeyM', key: 'm', metaKey: true }), 'compose', 'macos'));
});

test('chords match by key position, so a non-QWERTY layout uses the same key', () => {
  // Dvorak: the key in QWERTY's K position types "t".
  assert.ok(kb.matchesShortcut(key({ code: 'KeyK', key: 't', ctrlKey: true }), 'palette'));
  assert.ok(!kb.matchesShortcut(key({ code: 'KeyT', key: 'k', ctrlKey: true }), 'palette'));
  // A scripted event without a code falls back to the US character.
  assert.ok(kb.matchesShortcut(key({ key: 'K', ctrlKey: true }), 'palette'));
  assert.ok(kb.matchesShortcut(key({ key: '>', ctrlKey: true, shiftKey: true }), 'annotate'));
});

test('modifiers match exactly; Mod takes Ctrl or ⌘ but not both', () => {
  const k = (mods) => key({ code: 'KeyK', key: 'k', ...mods });
  assert.ok(kb.matchesShortcut(k({ ctrlKey: true }), 'palette', 'linux'));
  assert.ok(kb.matchesShortcut(k({ metaKey: true }), 'palette', 'macos'));
  assert.ok(kb.matchesShortcut(k({ metaKey: true }), 'palette', 'linux'));
  assert.ok(!kb.matchesShortcut(k({ ctrlKey: true, metaKey: true }), 'palette'));
  assert.ok(!kb.matchesShortcut(k({ ctrlKey: true, shiftKey: true }), 'palette'));
  assert.ok(!kb.matchesShortcut(k({}), 'palette'));
  // Ctrl , is Ctrl only.
  assert.ok(kb.matchesShortcut(key({ code: 'Comma', key: ',', ctrlKey: true }), 'expose', 'macos'));
  assert.ok(!kb.matchesShortcut(key({ code: 'Comma', key: ',', metaKey: true }), 'expose', 'macos'));
  // IME composition never fires a shortcut.
  assert.ok(!kb.matchesShortcut(key({ code: 'KeyK', key: 'k', ctrlKey: true, isComposing: true }), 'palette'));
  // Alternates: redo is also Mod Y.
  assert.ok(kb.matchesShortcut(key({ code: 'KeyY', key: 'y', ctrlKey: true }), 'board-redo'));
});

test('a digit range matches each digit in it', () => {
  assert.ok(kb.matchesShortcut(key({ code: 'Digit3', key: '3', altKey: true }), 'node-switch'));
  assert.ok(!kb.matchesShortcut(key({ code: 'Digit0', key: '0', altKey: true }), 'node-switch'));
  assert.equal(kb.shortcutDigit(key({ code: 'Digit7' })), 7);
});

test('labels follow the keyboard layout once it is known', async (t) => {
  t.after(() => kb.setKeyboardLayoutForTest(null));
  await kb.loadKeyboardLayout({ keyboard: { getLayoutMap: async () => new Map([['BracketRight', '+'], ['KeyK', 't']]) } });
  assert.equal(kb.shortcutText('tab-next', 'linux'), '+');
  assert.equal(kb.shortcutText('palette', 'linux'), 'Ctrl T');
});

test('Config › Keyboard lists every shortcut with both platforms', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ KeyboardShortcutList }] = await Promise.all([harness.importDashboardModule('js/shortcut-keys.js')]);
  const Setting = ({ children }) => children;
  const mounted = await harness.mount(harness.html`<${KeyboardShortcutList} Setting=${Setting} collab=${true} />`);
  const rows = [...mounted.container.querySelectorAll('tr[data-shortcut]')];
  assert.equal(rows.length, kb.SHORTCUTS.length);
  const expose = mounted.container.querySelector('tr[data-shortcut="expose"]');
  const cells = [...expose.querySelectorAll('td')].map((td) => td.textContent.replace(/\s+/g, ' ').trim());
  assert.deepEqual(cells.slice(1, 3), ['Ctrl,', 'Ctrl,']);
  assert.deepEqual([...expose.querySelectorAll('td')[1].querySelectorAll('kbd')].map((k) => k.textContent), ['Ctrl', ',']);
  assert.equal(cells[3], 'yes');
  const palette = mounted.container.querySelector('tr[data-shortcut="palette"] td:nth-child(3)');
  assert.deepEqual([...palette.querySelectorAll('kbd')].map((k) => k.textContent), ['⌘', 'K']);
});

test('with Collab off, the list leaves out the shortcuts of Collab-only features', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ KeyboardShortcutList }] = await Promise.all([harness.importDashboardModule('js/shortcut-keys.js')]);
  const Setting = ({ children }) => children;
  const mounted = await harness.mount(harness.html`<${KeyboardShortcutList} Setting=${Setting} collab=${false} />`);
  const ids = [...mounted.container.querySelectorAll('tr[data-shortcut]')].map((row) => row.dataset.shortcut);
  assert.ok(ids.includes('palette') && ids.includes('compose'));
  assert.ok(!ids.includes('steward') && !ids.includes('expose') && !ids.includes('board-undo'));
  assert.equal(mounted.container.querySelector('[data-shortcut-scope="board"]'), null, 'an emptied scope has no section');
});

test('the docs shortcut table is the registry', () => {
  const doc = readFileSync(new URL('../../../../docs/dashboard.md', import.meta.url), 'utf8');
  const table = /<!-- shortcuts:begin -->\n([\s\S]*?)<!-- shortcuts:end -->/.exec(doc)?.[1];
  assert.ok(table, 'docs/dashboard.md carries the generated shortcut table');
  assert.equal(table, kb.shortcutDocsTable(), 'regenerate it as the comment on shortcutDocsTable says');
});
