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
// collab: the feature behind it exists only with Collab on (features.collab),
//   so the list leaves it out while Collab is off.
const S = (id, scope, chord, label, extra = {}) => Object.freeze({ id, scope, chord, label, terminal: false, fixed: false, ...extra });

export const SHORTCUTS = Object.freeze([
  S('palette', 'global', 'Mod+KeyK', 'Open the command palette', { terminal: 'setting' }),
  S('steward', 'global', 'Ctrl+Period', 'Open or minimize the Steward window', { collab: true, terminal: true }),
  S('annotate', 'global', 'Ctrl+Shift+Period', 'Mark up the page for the agent in the window', { collab: true, terminal: true }),
  S('expose', 'global', 'Ctrl+Comma', 'Show all windows side by side, to pick one', { collab: true, terminal: true }),
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

  S('process-save', 'process', 'Mod+KeyS', 'Save a new version', { collab: true, fixed: true }),
  S('process-open', 'process', 'Mod+KeyO', 'Back to the template list', { collab: true, fixed: true }),
  S('process-commands', 'process', 'Mod+KeyK', 'Process commands', { collab: true, fixed: true }),
  S('process-undo', 'process', 'Mod+KeyZ', 'Undo', { collab: true, fixed: true }),
  S('process-redo', 'process', 'Mod+Shift+KeyZ', 'Redo', { collab: true, fixed: true, also: ['Mod+KeyY'] }),
  S('process-auto-layout', 'process', 'Mod+Shift+KeyL', 'Auto layout', { collab: true, fixed: true }),
  S('process-leave', 'process', 'Alt+ArrowUp', 'Leave the subprocess', { collab: true, fixed: true }),
  S('process-diff-accept', 'process', 'KeyA', 'Review changes: accept', { collab: true, fixed: true }),
  S('process-diff-reject', 'process', 'KeyR', 'Review changes: reject', { collab: true, fixed: true }),
  S('process-diff-next', 'process', 'KeyJ', 'Review changes: next', { collab: true, fixed: true }),
  S('process-diff-prev', 'process', 'KeyK', 'Review changes: previous', { collab: true, fixed: true }),

  S('board-undo', 'board', 'Mod+KeyZ', 'Undo', { collab: true, fixed: true }),
  S('board-redo', 'board', 'Mod+Shift+KeyZ', 'Redo', { collab: true, fixed: true, also: ['Mod+KeyY'] }),
  S('board-select-all', 'board', 'Mod+KeyA', 'Select everything', { collab: true, fixed: true }),
  S('board-copy', 'board', 'Mod+KeyC', 'Copy the selection', { collab: true, fixed: true }),
  S('board-cut', 'board', 'Mod+KeyX', 'Cut the selection', { collab: true, fixed: true }),
  S('board-paste', 'board', 'Mod+KeyV', 'Paste', { collab: true, fixed: true }),
  S('board-duplicate', 'board', 'Mod+KeyD', 'Duplicate the selection', { collab: true, fixed: true }),
  S('board-zoom-in', 'board', 'Equal', 'Zoom in', { collab: true, fixed: true }),
  S('board-zoom-out', 'board', 'Minus', 'Zoom out', { collab: true, fixed: true }),
  S('board-zoom-reset', 'board', 'Shift+Digit0', 'Zoom to 100%', { collab: true, fixed: true }),
  S('board-fit', 'board', 'Shift+Digit1', 'Fit the board in view', { collab: true, fixed: true }),

  S('viewer-zoom-in', 'viewer', 'Equal', 'Zoom in', { fixed: true }),
  S('viewer-zoom-out', 'viewer', 'Minus', 'Zoom out', { fixed: true }),
  S('viewer-zoom-reset', 'viewer', 'Digit0', 'Reset zoom', { fixed: true }),
  S('viewer-fullscreen', 'viewer', 'KeyF', 'Full screen', { fixed: true }),
  S('viewer-maximize', 'viewer', 'KeyT', 'Maximize the viewer', { fixed: true }),

  S('mini-undo', 'dialog', 'Mod+KeyZ', 'Undo in the group and markup editors', { fixed: true }),
]);

const BY_ID = new Map(SHORTCUTS.map(entry => [entry.id, entry]));

export function shortcut(id) {
  const entry = BY_ID.get(id);
  if (!entry) throw new Error(`unknown shortcut ${id}`);
  return entry;
}

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

export function shortcutChords(id) {
  const entry = shortcut(id);
  return [entry.chord, ...(entry.also || [])];
}

export function matchesShortcut(event, id, os) {
  if (!event || event.isComposing || event.keyCode === 229) return false;
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
  const keys = (id, os) => shortcutChords(id).map(chord => kbd(chord, os)).join(' or ');
  const where = { true: 'yes', false: 'no', setting: 'with **Command palette in terminals**' };
  const lines = ['| Action | Windows & Linux | macOS | In a terminal |', '|---|---|---|---|'];
  for (const scope of SHORTCUT_SCOPES.filter(item => scopes.includes(item.id))) {
    lines.push(`| **${scope.title}** | | | |`);
    for (const entry of SHORTCUTS.filter(item => item.scope === scope.id)) {
      lines.push(`| ${entry.label}${entry.collab ? ' (Collab)' : ''} | ${keys(entry.id, 'linux')} | ${keys(entry.id, 'macos')} | ${where[String(entry.terminal)]} |`);
    }
  }
  return `${lines.join('\n')}\n`;
}
