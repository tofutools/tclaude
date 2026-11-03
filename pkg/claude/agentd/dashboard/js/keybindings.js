// keybindings.js -- the one registry of the dashboard's keyboard shortcuts.
//
// Every named shortcut lives here: its id, what it does, its chord, where it
// works, and whether it still fires while a web terminal has focus. Handlers
// match through matchesShortcut, and every tooltip, menu, palette row and the
// Config › Keyboard list prints through shortcutText / shortcutParts, so a
// chord is written down exactly once. Generic widget keys (Escape closes,
// Enter submits, arrows move in a list) are not shortcuts and stay out.
//
// A chord is modifiers then one physical key, joined by '+':
//   'Mod+KeyK', 'Ctrl+Shift+Period', 'Alt+Digit1-9'.
// The key is a KeyboardEvent.code, so a shortcut sits on the same key
// position on every layout (the label follows the layout, see
// loadKeyboardLayout). 'Mod' is the platform's primary modifier: ⌘ on macOS,
// Ctrl elsewhere. It is the only place Cmd vs Ctrl is decided. Plain 'Ctrl'
// stays Ctrl on macOS too, for chords whose ⌘ form the browser or OS keeps
// (⌘ , opens browser settings, ⌘ M minimizes the window).
//
// Display is "Ctrl ," style: key names separated by spaces, never '+', so a
// chord on the + or , key can't be misread.

export const SHORTCUT_SCOPES = Object.freeze([
  { id: 'global', title: 'Anywhere', summary: 'Work from any tab. Those marked yes also work while a web terminal has focus.' },
  { id: 'terminal', title: 'Web terminals', summary: 'While a web terminal or its tab strip has focus.' },
  { id: 'dialog', title: 'Dialogs', summary: 'Inside the dialog or editor that has focus.' },
  { id: 'process', title: 'Process editor', summary: 'While a process template is open in the editor. The review keys work while reviewing changes.' },
  { id: 'board', title: 'Boards', summary: 'While a board has focus.' },
  { id: 'viewer', title: 'Image viewer', summary: 'While an image or video is open in the viewer.' },
]);

// terminal: does the chord still fire while a web terminal has focus?
//   true, false, or 'setting' (only with an opt-in setting).
// fixed: listed but not rebindable — owned by the browser or a widget
//   convention, or (editor, board and viewer keys) matched inside their own
//   widget rather than through matchesShortcut.
// also: further chords that do the same thing.
const S = (id, scope, chord, label, extra = {}) => Object.freeze({ id, scope, chord, label, terminal: false, fixed: false, ...extra });

export const SHORTCUTS = Object.freeze([
  S('palette', 'global', 'Mod+KeyK', 'Open the command palette', { terminal: 'setting' }),
  S('steward', 'global', 'Ctrl+Period', 'Open or minimize the Steward window', { terminal: true }),
  S('annotate', 'global', 'Ctrl+Shift+Period', 'Mark up the page for the agent in the window', { terminal: true }),
  S('expose', 'global', 'Ctrl+Comma', 'Show all windows side by side, to pick one', { terminal: true }),
  S('compose', 'global', 'Ctrl+KeyM', 'Message the agent in the current terminal or window', { terminal: true }),
  S('tab-next', 'global', 'BracketRight', 'Next dashboard tab'),
  S('tab-prev', 'global', 'BracketLeft', 'Previous dashboard tab'),
  S('node-switch', 'global', 'Alt+Digit1-9', 'Switch node: this node, then peers in order', { fixed: true }),
  S('wizard', 'global', 'Mod+Alt+Shift+KeyW', 'Toggle wizard mode'),
  S('slop', 'global', 'Mod+Alt+Shift+KeyS', 'Toggle slop mode'),

  S('terminal-copy', 'terminal', 'Mod+Shift+KeyC', 'Copy the selected terminal text', { terminal: true }),
  S('terminal-paste', 'terminal', 'Mod+KeyV', 'Paste text or files', { terminal: true, fixed: true }),
  S('terminal-newline', 'terminal', 'Shift+Enter', 'New line in the prompt', { terminal: true, fixed: true }),
  S('terminal-tab-left', 'terminal', 'Alt+Shift+ArrowLeft', 'Move the focused terminal tab or tab group left'),
  S('terminal-tab-right', 'terminal', 'Alt+Shift+ArrowRight', 'Move the focused terminal tab or tab group right'),

  S('dialog-submit', 'dialog', 'Mod+Enter', 'Submit the dialog or send the message', { fixed: true }),
  S('dialog-queue', 'dialog', 'Mod+Shift+Enter', 'Spawn dialog: add to the queue', { fixed: true }),

  S('process-save', 'process', 'Mod+KeyS', 'Save a new version', { fixed: true }),
  S('process-open', 'process', 'Mod+KeyO', 'Back to the template list', { fixed: true }),
  S('process-commands', 'process', 'Mod+KeyK', 'Process commands', { fixed: true }),
  S('process-undo', 'process', 'Mod+KeyZ', 'Undo', { fixed: true }),
  S('process-redo', 'process', 'Mod+Shift+KeyZ', 'Redo', { fixed: true, also: ['Mod+KeyY'] }),
  S('process-auto-layout', 'process', 'Mod+Shift+KeyL', 'Auto layout', { fixed: true }),
  S('process-leave', 'process', 'Alt+ArrowUp', 'Leave the subprocess', { fixed: true }),
  S('process-diff-accept', 'process', 'KeyA', 'Review changes: accept', { fixed: true }),
  S('process-diff-reject', 'process', 'KeyR', 'Review changes: reject', { fixed: true }),
  S('process-diff-next', 'process', 'KeyJ', 'Review changes: next', { fixed: true }),
  S('process-diff-prev', 'process', 'KeyK', 'Review changes: previous', { fixed: true }),

  S('board-undo', 'board', 'Mod+KeyZ', 'Undo', { fixed: true }),
  S('board-redo', 'board', 'Mod+Shift+KeyZ', 'Redo', { fixed: true, also: ['Mod+KeyY'] }),
  S('board-select-all', 'board', 'Mod+KeyA', 'Select everything', { fixed: true }),
  S('board-copy', 'board', 'Mod+KeyC', 'Copy the selection', { fixed: true }),
  S('board-cut', 'board', 'Mod+KeyX', 'Cut the selection', { fixed: true }),
  S('board-paste', 'board', 'Mod+KeyV', 'Paste', { fixed: true }),
  S('board-duplicate', 'board', 'Mod+KeyD', 'Duplicate the selection', { fixed: true }),
  S('board-zoom-in', 'board', 'Equal', 'Zoom in', { fixed: true }),
  S('board-zoom-out', 'board', 'Minus', 'Zoom out', { fixed: true }),
  S('board-zoom-reset', 'board', 'Shift+Digit0', 'Zoom to 100%', { fixed: true }),
  S('board-fit', 'board', 'Shift+Digit1', 'Fit the board in view', { fixed: true }),

  S('viewer-zoom-in', 'viewer', 'Equal', 'Zoom in', { fixed: true }),
  S('viewer-zoom-out', 'viewer', 'Minus', 'Zoom out', { fixed: true }),
  S('viewer-zoom-reset', 'viewer', 'Digit0', 'Reset zoom', { fixed: true }),
  S('viewer-fullscreen', 'viewer', 'KeyF', 'Full screen', { fixed: true }),
  S('viewer-maximize', 'viewer', 'KeyT', 'Maximize the viewer', { fixed: true }),

  S('mini-undo', 'dialog', 'Mod+KeyZ', 'Undo in the group and markup editors', { fixed: true }),
]);

const BY_ID = new Map(SHORTCUTS.map(entry => [entry.id, entry]));

// The operator's rebindings (dashboard.keybindings), applied from the
// snapshot. An override replaces the default chord and its alternates.
let overrides = new Map();
let effective = new Map();
const changeListeners = new Set();

// shortcutDefault is the registry entry as shipped, ignoring rebindings.
export function shortcutDefault(id) {
  const entry = BY_ID.get(id);
  if (!entry) throw new Error(`unknown shortcut ${id}`);
  return entry;
}

// shortcut is the entry as it works now: a rebound one carries its new chord.
export function shortcut(id) {
  return effective.get(id) || shortcutDefault(id);
}

export const isRebindable = (id) => !shortcutDefault(id).fixed;

// validOverride reports whether a stored rebinding can be used: a known,
// rebindable shortcut and a chord that parses to modifiers and one key.
export function validOverride(id, chord) {
  if (!BY_ID.has(id) || shortcutDefault(id).fixed || typeof chord !== 'string') return false;
  try {
    // One real key: not a modifier, and not a range like Digit1-9.
    const { code } = parseChord(chord);
    return /^[A-Za-z0-9]+$/.test(code) && !MODIFIERS.includes(code);
  } catch { return false; }
}

// setShortcutOverrides applies a rebinding map ({ id: chord }). Entries for
// unknown or fixed shortcuts, or with a chord that does not parse, are
// ignored, so a stale config never breaks a handler. Returns whether anything
// changed.
export function setShortcutOverrides(map) {
  const next = new Map();
  for (const [id, chord] of Object.entries(map || {})) {
    if (!validOverride(id, chord)) continue;
    if (chord !== shortcutDefault(id).chord) next.set(id, chord);
  }
  const same = next.size === overrides.size && [...next].every(([id, chord]) => overrides.get(id) === chord);
  if (same) return false;
  overrides = next;
  effective = new Map([...next].map(([id, chord]) => [id, Object.freeze({ ...shortcutDefault(id), chord, also: undefined, rebound: true })]));
  for (const fn of changeListeners) fn();
  return true;
}

export function shortcutOverrides() { return Object.fromEntries(overrides); }

// onShortcutsChange calls fn after the rebindings change; returns unsubscribe.
export function onShortcutsChange(fn) {
  changeListeners.add(fn);
  return () => changeListeners.delete(fn);
}

// While Config records a new chord, no shortcut fires: the keys belong to
// the recorder.
let recording = false;
export function setShortcutRecording(on) { recording = !!on; }

// ---- platform -------------------------------------------------------------

let platformOverride = null;

// setShortcutPlatform pins the platform ('macos' | 'linux' | 'windows'), for
// tests and the Config list's per-platform columns; null restores detection.
export function setShortcutPlatform(os) { platformOverride = os || null; }

export function shortcutPlatform() {
  if (platformOverride) return platformOverride;
  // helpers.js's browserOS, inlined: this module stays a leaf so any island
  // (slop.js included, which helpers.js itself imports) can use it.
  const platform = globalThis.navigator?.platform || globalThis.navigator?.userAgent || '';
  if (/Mac|iPhone|iPad/.test(platform)) return 'macos';
  return /Win/.test(platform) ? 'windows' : 'linux';
}

const isMac = (os) => (os || shortcutPlatform()) === 'macos';

// ---- parsing --------------------------------------------------------------

const MODIFIERS = ['Mod', 'Ctrl', 'Meta', 'Alt', 'Shift'];

export function parseChord(chord) {
  const tokens = String(chord).split('+');
  const code = tokens.pop();
  const mods = new Set(tokens);
  for (const mod of mods) if (!MODIFIERS.includes(mod)) throw new Error(`bad modifier ${mod} in ${chord}`);
  return { mods, code };
}

// resolveMods turns Mod into the platform's Ctrl or Meta.
function resolveMods(mods, os) {
  const out = { ctrl: mods.has('Ctrl'), meta: mods.has('Meta'), alt: mods.has('Alt'), shift: mods.has('Shift') };
  if (mods.has('Mod')) { if (isMac(os)) out.meta = true; else out.ctrl = true; }
  return out;
}

// ---- matching -------------------------------------------------------------

// The character a code types on a US layout, for synthetic events that carry
// only event.key (and as the label before the real layout is known).
const CODE_CHARS = {
  Period: '.', Comma: ',', Slash: '/', Semicolon: ';', Quote: "'", Backquote: '`',
  BracketLeft: '[', BracketRight: ']', Backslash: '\\', Minus: '-', Equal: '=',
};
const SHIFTED_CHARS = {
  Period: '>', Comma: '<', Slash: '?', Semicolon: ':', Quote: '"', Backquote: '~',
  BracketLeft: '{', BracketRight: '}', Backslash: '|', Minus: '_', Equal: '+',
  Digit0: ')', Digit1: '!', Digit2: '@', Digit3: '#', Digit4: '$', Digit5: '%', Digit6: '^', Digit7: '&', Digit8: '*', Digit9: '(',
};

function usChar(code) {
  if (/^Key[A-Z]$/.test(code)) return code.slice(3).toLowerCase();
  if (/^Digit\d$/.test(code)) return code.slice(5);
  return CODE_CHARS[code] || null;
}

// codeMatches compares the physical key, falling back to event.key when an
// event carries no code (scripted input, some IMEs).
function codeMatches(event, code) {
  const range = /^Digit(\d)-(\d)$/.exec(code);
  if (range) {
    const digit = /^Digit(\d)$/.exec(event.code || '')?.[1] ?? (/^\d$/.test(event.key || '') ? event.key : null);
    return digit !== null && digit >= range[1] && digit <= range[2];
  }
  if (event.code) return event.code === code;
  const key = event.key || '';
  if (key === code) return true; // Enter, ArrowLeft, Escape…
  const char = usChar(code);
  return char !== null && (key.toLowerCase() === char || key === SHIFTED_CHARS[code]);
}

// chordMatches is exact on modifiers: Ctrl+K does not fire for Ctrl+Shift+K.
// 'Mod' accepts either Ctrl or ⌘ (never both) on every platform, as the
// dashboard always has; it only *displays* as the platform's own key.
export function chordMatches(event, chord, os) {
  if (!event) return false;
  const { mods, code } = parseChord(chord);
  if (!codeMatches(event, code)) return false;
  const want = resolveMods(mods, os);
  if (!!event.altKey !== want.alt || !!event.shiftKey !== want.shift) return false;
  if (mods.has('Mod')) {
    return (!!event.ctrlKey !== !!event.metaKey) && !mods.has('Ctrl') && !mods.has('Meta');
  }
  return !!event.ctrlKey === want.ctrl && !!event.metaKey === want.meta;
}

export function shortcutChords(id, { defaults = false } = {}) {
  const entry = defaults ? shortcutDefault(id) : shortcut(id);
  return [entry.chord, ...(entry.also || [])];
}

export function matchesShortcut(event, id, os) {
  if (recording || !event || event.isComposing || event.keyCode === 229) return false;
  return shortcutChords(id).some(chord => chordMatches(event, chord, os));
}

// shortcutDigit returns the digit of a matched range chord (Alt+Digit1-9).
export function shortcutDigit(event) {
  const code = /^Digit(\d)$/.exec(event?.code || '')?.[1];
  if (code) return Number(code);
  return /^\d$/.test(event?.key || '') ? Number(event.key) : null;
}

// ---- display --------------------------------------------------------------

let layoutMap = null;

// loadKeyboardLayout asks the browser what each key position types on the
// operator's layout, so a chord matched by position is labelled with the
// character printed on that key. Optional: without it labels are US.
export async function loadKeyboardLayout(nav = globalThis.navigator) {
  try {
    const map = await nav?.keyboard?.getLayoutMap?.();
    if (map && typeof map.get === 'function') layoutMap = map;
  } catch { /* not permitted or not supported: keep US labels */ }
  return layoutMap;
}

export function setKeyboardLayoutForTest(map) { layoutMap = map; }

const NAMED_KEYS = {
  Enter: 'Enter', Escape: 'Esc', Space: 'Space', Tab: 'Tab', Backspace: 'Backspace', Delete: 'Delete',
  ArrowLeft: '←', ArrowRight: '→', ArrowUp: '↑', ArrowDown: '↓', PageUp: 'Page Up', PageDown: 'Page Down',
};

export function keyLabel(code) {
  const range = /^Digit(\d)-(\d)$/.exec(code);
  if (range) return `${keyLabel(`Digit${range[1]}`)}…${keyLabel(`Digit${range[2]}`)}`;
  if (NAMED_KEYS[code]) return NAMED_KEYS[code];
  const mapped = layoutMap?.get?.(code);
  if (mapped && mapped.trim()) return mapped.toUpperCase();
  const char = usChar(code);
  if (char) return char.toUpperCase();
  return code;
}

function modLabels(mods, os) {
  const r = resolveMods(mods, os);
  const mac = isMac(os);
  const out = [];
  if (r.ctrl) out.push('Ctrl');
  if (r.alt) out.push(mac ? 'Option' : 'Alt');
  if (r.shift) out.push('Shift');
  if (r.meta) out.push(mac ? '⌘' : 'Meta');
  return out;
}

// chordParts lists one chord's keys for display, e.g. ['Ctrl', ','].
export function chordParts(chord, os) {
  const { mods, code } = parseChord(chord);
  return [...modLabels(mods, os), keyLabel(code)];
}

export function chordText(chord, os) { return chordParts(chord, os).join(' '); }

// modifierText names one modifier for prose: modifierText('Mod') is
// "Ctrl" or "⌘" by platform.
export function modifierText(mod, os) { return modLabels(new Set([mod]), os)[0]; }

// modClickText is the "Ctrl-click" / "⌘-click" gesture that opens things in
// the background.
export function modClickText(os) { return `${modifierText('Mod', os)}-click`; }

// shortcutParts is the primary chord of a shortcut, split into keys.
export function shortcutParts(id, os) { return chordParts(shortcut(id).chord, os); }

// shortcutText prints a shortcut's primary chord, "Ctrl ," style, for
// titles, aria-labels and placeholders.
export function shortcutText(id, os) { return chordText(shortcut(id).chord, os); }

// withShortcut appends a shortcut to a label: "Undo (Ctrl Z)".
export function withShortcut(label, id, os) { return `${label} (${shortcutText(id, os)})`; }

// ariaKeyShortcuts renders the aria-keyshortcuts value for a shortcut.
export function ariaKeyShortcuts(id, os) {
  return shortcutChords(id).map(chord => {
    const { mods, code } = parseChord(chord);
    const r = resolveMods(mods, os);
    const names = [r.ctrl && 'Control', r.alt && 'Alt', r.shift && 'Shift', r.meta && 'Meta'].filter(Boolean);
    const char = usChar(code);
    return [...names, char ? char.toUpperCase() : code].join('+');
  }).join(' ');
}

// shortcutDocsTable is the shortcut table in docs/dashboard.md, between its
// shortcuts:begin/end markers; a jstest keeps the two equal. Regenerate:
//   node -e "import('./pkg/claude/agentd/dashboard/js/keybindings.js').then(m => process.stdout.write(m.shortcutDocsTable()))"
export function shortcutDocsTable(scopes = ['global', 'terminal', 'dialog']) {
  const kbd = (chord, os) => chordParts(chord, os).map(part => `<kbd>${part === '|' ? '&#124;' : part}</kbd>`).join(' ');
  const keys = (id, os) => shortcutChords(id, { defaults: true }).map(chord => kbd(chord, os)).join(' or ');
  const where = { true: 'yes', false: 'no', setting: 'with **Command palette in terminals**' };
  const lines = ['| Action | Windows & Linux | macOS | In a terminal |', '|---|---|---|---|'];
  for (const scope of SHORTCUT_SCOPES.filter(item => scopes.includes(item.id))) {
    lines.push(`| **${scope.title}** | | | |`);
    for (const entry of SHORTCUTS.filter(item => item.scope === scope.id)) {
      lines.push(`| ${entry.label} | ${keys(entry.id, 'linux')} | ${keys(entry.id, 'macos')} | ${where[String(entry.terminal)]} |`);
    }
  }
  return `${lines.join('\n')}\n`;
}

// ---- rebinding ------------------------------------------------------------

// KEYBINDINGS_LIST is the Config list id holding the draft rebindings as
// [{ id, chord }]; the config adapter loads and saves it as
// dashboard.keybindings.
export const KEYBINDINGS_LIST = 'cfg-keybindings';

const MODIFIER_KEYS = new Set(['Control', 'Shift', 'Alt', 'Meta', 'AltGraph', 'CapsLock', 'Fn', 'OS']);

// chordFromEvent records a keydown as a chord, exactly as pressed: Ctrl stays
// Ctrl and ⌘ is Meta, so what was recorded is what fires. Null while only
// modifiers are down, or for a key with no position (event.code).
export function chordFromEvent(event) {
  if (!event || MODIFIER_KEYS.has(event.key) || !event.code) return null;
  const mods = [event.ctrlKey && 'Ctrl', event.metaKey && 'Meta', event.altKey && 'Alt', event.shiftKey && 'Shift'].filter(Boolean);
  return [...mods, event.code].join('+');
}

// concreteCombos expands a chord into the exact key combinations that fire
// it: Mod is either Ctrl or ⌘, and a digit range is each digit.
function concreteCombos(chord) {
  const { mods, code } = parseChord(chord);
  const range = /^Digit(\d)-(\d)$/.exec(code);
  const codes = range ? Array.from({ length: Number(range[2]) - Number(range[1]) + 1 }, (_, i) => `Digit${Number(range[1]) + i}`) : [code];
  const base = { alt: mods.has('Alt'), shift: mods.has('Shift') };
  const variants = mods.has('Mod')
    ? [{ ctrl: true, meta: false }, { ctrl: false, meta: true }]
    : [{ ctrl: mods.has('Ctrl'), meta: mods.has('Meta') }];
  return codes.flatMap(c => variants.map(v => `${+v.ctrl}${+v.meta}${+base.alt}${+base.shift}:${c}`));
}

const overlaps = (a, b) => { const set = new Set(concreteCombos(a)); return concreteCombos(b).some(k => set.has(k)); };

// Scopes that can be live at once: a global shortcut collides with every
// scope, the rest only with their own.
const scopesMeet = (a, b) => a === b || a === 'global' || b === 'global';

// Known browser and OS chords. reserved: the browser or OS takes the keys
// and the page never sees them. Otherwise the dashboard takes them over from
// the browser. os limits an entry to one platform.
const BROWSER_CHORDS = [
  ['Ctrl+KeyT', 'opens a new browser tab', true, 'pc'], ['Ctrl+KeyW', 'closes the browser tab', true, 'pc'],
  ['Ctrl+KeyN', 'opens a new browser window', true, 'pc'], ['Ctrl+Shift+KeyN', 'opens an incognito window', true, 'pc'],
  ['Ctrl+Shift+KeyT', 'reopens a closed tab', true, 'pc'], ['Ctrl+Shift+KeyW', 'closes the browser window', true, 'pc'],
  ['Ctrl+Tab', 'switches browser tabs', true], ['Ctrl+Shift+Tab', 'switches browser tabs', true],
  ['Ctrl+PageUp', 'switches browser tabs', true, 'pc'], ['Ctrl+PageDown', 'switches browser tabs', true, 'pc'],
  ['Meta+KeyT', 'opens a new browser tab', true, 'macos'], ['Meta+KeyW', 'closes the browser tab', true, 'macos'],
  ['Meta+KeyN', 'opens a new browser window', true, 'macos'], ['Meta+KeyQ', 'quits the browser', true, 'macos'],
  ['Meta+KeyM', 'minimizes the window', true, 'macos'], ['Meta+KeyH', 'hides the browser', true, 'macos'],
  ['Meta+Tab', 'switches apps', true, 'macos'], ['Meta+Space', 'opens Spotlight', true, 'macos'],
  ['Meta+Comma', 'opens browser settings', true, 'macos'], ['Meta+Shift+KeyT', 'reopens a closed tab', true, 'macos'],
  ['Alt+F4', 'closes the window', true, 'pc'],
  ['Mod+KeyL', 'focuses the address bar'], ['Mod+KeyR', 'reloads the page'], ['Mod+KeyF', 'finds in the page'],
  ['Mod+KeyP', 'prints'], ['Mod+KeyS', 'saves the page'], ['Mod+KeyD', 'bookmarks the page'], ['Mod+KeyO', 'opens a file'],
  ['Mod+KeyH', 'opens history', false, 'pc'], ['Mod+KeyJ', 'opens downloads'], ['Mod+KeyU', 'shows the page source', false, 'pc'],
  ['Mod+Equal', 'zooms the page'], ['Mod+Minus', 'zooms the page'], ['Mod+Digit0', 'resets the page zoom'],
  ['Mod+Digit1-9', 'switches browser tabs'], ['Alt+Digit1-9', 'switches browser tabs', false, 'pc'],
  ['Mod+KeyC', 'copies'], ['Mod+KeyV', 'pastes'], ['Mod+KeyX', 'cuts'], ['Mod+KeyA', 'selects all'], ['Mod+KeyZ', 'undoes'],
  ['F5', 'reloads the page'], ['F11', 'toggles full screen'], ['F12', 'opens developer tools'],
];

// Keys a terminal program reads, for shortcuts that fire inside a terminal.
const TERMINAL_CHORDS = [
  ['Ctrl+KeyA', 'moves to the start of the line'], ['Ctrl+KeyB', 'is the tmux prefix'], ['Ctrl+KeyC', 'interrupts the program'],
  ['Ctrl+KeyD', 'ends input'], ['Ctrl+KeyE', 'moves to the end of the line'], ['Ctrl+KeyK', 'clears to the end of the line'],
  ['Ctrl+KeyL', 'clears the screen'], ['Ctrl+KeyR', 'searches history'], ['Ctrl+KeyU', 'clears the line'],
  ['Ctrl+KeyW', 'deletes a word'], ['Ctrl+KeyZ', 'suspends the program'], ['Ctrl+KeyJ', 'inserts a newline'],
];

const browserChordApplies = (os, platform) => !platform || (platform === 'macos' ? os === 'macos' : os !== 'macos');

// Shortcuts whose handler ignores keys typed into fields, so they may be a
// single printable key.
const BARE_OK = new Set(['tab-next', 'tab-prev']);
const PRINTABLE_CODE = /^(Key[A-Z]|Digit\d|Minus|Equal|BracketLeft|BracketRight|Backslash|Semicolon|Quote|Backquote|Comma|Period|Slash)$/;

// rebindProblems checks a proposed chord for shortcut id against the other
// shortcuts (with the draft rebindings in drafts), known browser and OS
// chords, and terminal keys. Each problem: { kind: 'invalid' | 'shortcut' |
// 'browser' | 'terminal', text, blocking }. A blocking problem means the
// chord can't work; the others are warnings.
export function rebindProblems(id, chord, drafts = shortcutOverrides(), os = shortcutPlatform()) {
  const entry = shortcutDefault(id);
  const problems = [];
  let parsed;
  try { parsed = parseChord(chord); } catch { return [{ kind: 'invalid', text: 'Not a key combination.', blocking: true }]; }
  const { mods, code } = parsed;
  const strong = mods.has('Ctrl') || mods.has('Meta') || mods.has('Mod') || mods.has('Alt');
  const fKey = /^F\d+$/.test(code);
  if (code === 'Escape' || (code === 'Tab' && !strong)) {
    problems.push({ kind: 'invalid', text: `${keyLabel(code)} closes and moves focus everywhere, so it needs Ctrl, Alt or ⌘.`, blocking: true });
  } else if (!strong && !fKey) {
    // Only tab cycling skips text fields; every other shortcut would take
    // the key wherever it is typed. Tab cycling still leaves Enter, Space
    // and the arrows to buttons, lists and scrolling.
    if (!BARE_OK.has(id)) {
      problems.push({ kind: 'invalid', text: entry.terminal
        ? 'This one also works inside terminals, so it needs Ctrl, Alt or ⌘; otherwise it would take keys you type.'
        : 'This one works while you type, so it needs Ctrl, Alt or ⌘; otherwise it would take keys you type.', blocking: true });
    } else if (!PRINTABLE_CODE.test(code)) {
      problems.push({ kind: 'invalid', text: `${keyLabel(code)} on its own works buttons, lists and scrolling, so it needs Ctrl, Alt or ⌘ here.`, blocking: true });
    }
  }
  for (const other of SHORTCUTS) {
    if (other.id === id || !scopesMeet(entry.scope, other.scope)) continue;
    const chords = drafts[other.id] ? [drafts[other.id]] : shortcutChords(other.id, { defaults: true });
    if (chords.some(c => overlaps(c, chord))) {
      problems.push({ kind: 'shortcut', id: other.id, text: `Taken by “${other.label}” (${SHORTCUT_SCOPES.find(s => s.id === other.scope)?.title}).`, blocking: true });
    }
  }
  for (const [browserChord, what, reserved, platform] of BROWSER_CHORDS) {
    if (!browserChordApplies(os, platform) || !overlaps(browserChord, chord)) continue;
    problems.push(reserved
      ? { kind: 'browser', text: `The browser or OS ${what} with these keys; the page never sees them.`, blocking: true }
      : { kind: 'browser', text: `Normally ${what}; the dashboard would take that over.`, blocking: false });
  }
  if (entry.terminal) {
    for (const [termChord, what] of TERMINAL_CHORDS) {
      if (overlaps(termChord, chord)) problems.push({ kind: 'terminal', text: `In a terminal this ${what}; the shortcut would take it.`, blocking: false });
    }
  }
  return problems;
}
