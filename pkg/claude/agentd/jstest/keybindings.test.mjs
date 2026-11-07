//ci:reads docs/dashboard.md
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
  assert.equal(kb.shortcutText('drawing-board-zoom-in', 'linux'), '=');
  assert.equal(kb.withShortcut('Undo', 'drawing-board-undo', 'macos'), 'Undo (⌘ Z)');
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
  assert.ok(kb.matchesShortcut(key({ code: 'KeyY', key: 'y', ctrlKey: true }), 'drawing-board-redo'));
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
  const mounted = await harness.mount(harness.html`<${KeyboardShortcutList} Setting=${Setting} />`);
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

test('a rebinding replaces the chord everywhere, and a bad or fixed one is ignored', (t) => {
  t.after(() => kb.setShortcutOverrides({}));
  assert.equal(kb.setShortcutOverrides({ expose: 'Ctrl+Alt+KeyE', 'terminal-paste': 'Ctrl+KeyQ', nope: 'Ctrl+KeyQ', steward: 'Ctrl+Bogus+KeyQ' }), true);
  assert.deepEqual(kb.shortcutOverrides(), { expose: 'Ctrl+Alt+KeyE' }, 'fixed, unknown and unparsable entries are dropped');
  assert.equal(kb.shortcutText('expose', 'linux'), 'Ctrl Alt E');
  assert.equal(kb.shortcutText('expose', 'macos'), 'Ctrl Option E');
  assert.ok(kb.matchesShortcut(key({ code: 'KeyE', key: 'e', ctrlKey: true, altKey: true }), 'expose'));
  assert.ok(!kb.matchesShortcut(key({ code: 'Comma', key: ',', ctrlKey: true }), 'expose'), 'the old chord stops working');
  assert.equal(kb.shortcutDefault('expose').chord, 'Ctrl+Comma');
  assert.equal(kb.setShortcutOverrides({ expose: 'Ctrl+Alt+KeyE' }), false, 'the same map is no change');
  let calls = 0;
  const off = kb.onShortcutsChange(() => calls++);
  kb.setShortcutOverrides({ redo: 'x', expose: 'Ctrl+Comma' });
  off();
  assert.equal(calls, 1);
  assert.deepEqual(kb.shortcutOverrides(), {}, 'a rebinding back to the default is no rebinding');
});

test('while Config records keys, no shortcut fires', (t) => {
  t.after(() => kb.setShortcutRecording(false));
  kb.setShortcutRecording(true);
  assert.ok(!kb.matchesShortcut(key({ code: 'KeyK', key: 'k', ctrlKey: true }), 'palette'));
  kb.setShortcutRecording(false);
  assert.ok(kb.matchesShortcut(key({ code: 'KeyK', key: 'k', ctrlKey: true }), 'palette'));
});

test('a recorded chord is what was pressed, and modifiers alone are not a chord', () => {
  assert.equal(kb.chordFromEvent(key({ code: 'KeyE', key: 'e', ctrlKey: true, altKey: true })), 'Ctrl+Alt+KeyE');
  assert.equal(kb.chordFromEvent(key({ code: 'KeyK', key: 'k', metaKey: true, shiftKey: true })), 'Meta+Shift+KeyK');
  assert.equal(kb.chordFromEvent(key({ code: 'ControlLeft', key: 'Control', ctrlKey: true })), null);
  assert.equal(kb.chordFromEvent(key({ key: 'k' })), null, 'no key position, no chord');
});

test('rebinding checks other shortcuts, browser and OS chords, and terminal keys', () => {
  const kinds = (problems) => problems.map((problem) => `${problem.kind}:${problem.blocking}`);
  assert.deepEqual(kb.rebindProblems('expose', 'Ctrl+Alt+KeyE', {}, 'linux'), [], 'a free chord');
  assert.deepEqual(kinds(kb.rebindProblems('expose', 'Ctrl+KeyT', {}, 'linux')), ['browser:true'], 'the browser keeps Ctrl T');
  assert.deepEqual(kb.rebindProblems('expose', 'Ctrl+KeyT', {}, 'macos').filter((p) => p.blocking), [], 'Ctrl T is free on a Mac');
  assert.deepEqual(kinds(kb.rebindProblems('expose', 'Meta+KeyM', {}, 'macos')), ['browser:true']);
  assert.ok(kb.rebindProblems('expose', 'Ctrl+KeyK', {}, 'linux').some((p) => p.kind === 'shortcut' && p.id === 'palette' && p.blocking),
    'Mod K is the palette, which Ctrl K also fires on every platform');
  assert.ok(kb.rebindProblems('expose', 'Ctrl+KeyK', {}, 'linux').some((p) => p.kind === 'terminal' && !p.blocking),
    'it works in terminals, where Ctrl K clears the line');
  assert.deepEqual(kinds(kb.rebindProblems('expose', 'Ctrl+Alt+KeyP', { palette: 'Ctrl+Alt+KeyP' }, 'linux')), ['shortcut:true'],
    'the draft rebindings count, not just the saved ones');
  assert.deepEqual(kb.rebindProblems('expose', 'Ctrl+KeyK', { palette: 'Ctrl+Alt+KeyP', 'process-commands': 'x' }, 'linux').filter((p) => p.id === 'palette'), [],
    'a shortcut moved away no longer collides');
  assert.ok(kb.rebindProblems('steward', 'Period', {}, 'linux').some((p) => p.kind === 'invalid' && p.blocking),
    'a shortcut that works in terminals needs a real modifier');
  assert.deepEqual(kb.rebindProblems('tab-next', 'KeyN', {}, 'linux'), [], 'tab cycling can stay a bare key');
  assert.ok(kb.rebindProblems('tab-next', 'Escape', {}, 'linux').some((p) => p.blocking));
  for (const [id, chord] of [['slop', 'KeyS'], ['wizard', 'Shift+KeyW'], ['tab-next', 'Shift+Tab'], ['tab-next', 'Enter'], ['tab-prev', 'Space']]) {
    assert.ok(kb.rebindProblems(id, chord, {}, 'linux').some((p) => p.kind === 'invalid' && p.blocking), `${id} can't be ${chord}`);
  }
  assert.deepEqual(kb.rebindProblems('slop', 'F8', {}, 'linux').filter((p) => p.blocking), [], 'an F-key needs no modifier');
  assert.ok(kb.validOverride('expose', 'Ctrl+Alt+KeyE'));
  assert.ok(!kb.validOverride('expose', 'Ctrl+Ctrl'), 'a chord that does not parse');
  assert.ok(!kb.validOverride('no-such-shortcut', 'Ctrl+KeyE'));
  assert.deepEqual(kinds(kb.rebindProblems('drawing-board-duplicate', 'Mod+KeyD', {}, 'linux').filter((p) => p.kind === 'shortcut')), [],
    'scopes that are never live together do not collide');
});

test('Config › Keyboard records new keys, refuses a conflict, and resets', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ KeyboardShortcutList }] = await Promise.all([harness.importDashboardModule('js/shortcut-keys.js')]);
  const Setting = ({ children }) => children;
  let drafts = [];
  const changes = [];
  const render = () => harness.html`<${KeyboardShortcutList} Setting=${Setting} collab=${true} drafts=${drafts}
    onChange=${(next) => { drafts = next; changes.push(next); }} />`;
  const mounted = await harness.mount(render());
  const rerender = () => mounted.rerender(render());
  const row = (id) => mounted.container.querySelector(`tr[data-shortcut="${id}"]`);
  assert.ok(!row('terminal-paste').querySelector('[data-shortcut-change]'), 'fixed shortcuts have no Change button');

  await harness.act(() => harness.fireEvent(row('expose').querySelector('[data-shortcut-change]'), 'click'));
  const recorder = () => mounted.container.querySelector('[data-shortcut-recorder="expose"] .shortcut-recorder-keys');
  assert.ok(recorder(), 'Change opens the recorder');
  assert.equal(harness.document.activeElement, recorder(), 'the recorder has focus');

  await harness.act(() => harness.fireEvent(recorder(), 'keydown', { key: 'k', code: 'KeyK', ctrlKey: true }));
  const use = () => [...mounted.container.querySelectorAll('[data-shortcut-recorder] button')].find((b) => b.textContent.startsWith('Use'));
  assert.equal(use().disabled, true, 'a chord another shortcut has is refused');
  assert.match(mounted.container.querySelector('.shortcut-problems').textContent, /Open the command palette/);

  await harness.act(() => harness.fireEvent(recorder(), 'keydown', { key: 'e', code: 'KeyE', ctrlKey: true, altKey: true }));
  assert.equal(use().disabled, false);
  assert.equal(use().textContent.trim(), 'Use Ctrl Alt E');
  await harness.act(() => harness.fireEvent(use(), 'click'));
  assert.deepEqual(changes.at(-1), [{ id: 'expose', chord: 'Ctrl+Alt+KeyE' }]);
  assert.equal(mounted.container.querySelector('[data-shortcut-recorder]'), null, 'the recorder closes');

  await rerender();
  assert.match(row('expose').textContent, /changed/);
  assert.deepEqual([...row('expose').querySelectorAll('td')[1].querySelectorAll('kbd')].map((k) => k.textContent), ['Ctrl', 'Alt', 'E']);
  await harness.act(() => harness.fireEvent(row('expose').querySelector('[data-shortcut-reset]'), 'click'));
  assert.deepEqual(changes.at(-1), [], 'Reset drops the rebinding');

  drafts = [{ id: 'expose', chord: 'Ctrl+Alt+KeyE' }, { id: 'steward', chord: 'Ctrl+Alt+KeyS' }];
  await rerender();
  const resetAll = [...mounted.container.querySelectorAll('.shortcut-reset-all button')][0];
  assert.match(mounted.container.querySelector('.shortcut-reset-all').textContent, /2 shortcuts changed/);
  await harness.act(() => harness.fireEvent(resetAll, 'click'));
  assert.deepEqual(changes.at(-1), []);

  await harness.act(() => harness.fireEvent(row('steward').querySelector('[data-shortcut-change]'), 'click'));
  await harness.act(() => harness.fireEvent(recorder2(), 'keydown', { key: 'Escape', code: 'Escape' }));
  assert.equal(mounted.container.querySelector('[data-shortcut-recorder]'), null, 'Escape cancels');
  function recorder2() { return mounted.container.querySelector('[data-shortcut-recorder="steward"] .shortcut-recorder-keys'); }
});

test('the docs shortcut table is the registry', () => {
  const doc = readFileSync(new URL('../../../../docs/dashboard.md', import.meta.url), 'utf8');
  const table = /<!-- shortcuts:begin -->\n([\s\S]*?)<!-- shortcuts:end -->/.exec(doc)?.[1];
  assert.ok(table, 'docs/dashboard.md carries the generated shortcut table');
  assert.equal(table, kb.shortcutDocsTable(), 'regenerate it as the comment on shortcutDocsTable says');
});
