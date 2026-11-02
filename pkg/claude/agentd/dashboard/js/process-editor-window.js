// process-editor-window.js -- a process template's editor in a browser window
// of its own (tcl-kjl2fr), the third place it can be beside the Processes tab
// and an overlay window. A live editor cannot cross into another page, so its
// draft does: the version it was loaded at, the edits since and their history
// (ProcessTemplateEditor.handoff), as JSON through window.opener, both ways.
//
// The window is this dashboard opened at the template, named for the handoff,
// which shows the editor alone. The page that sends a draft keeps its editor
// aside until the window has taken the draft up, and takes it back if the window
// closes first, so nothing is lost on the way. While the window has it, opening
// the template here brings the window forward: one place at a time.

import { POPUP_BLOCKED } from './overlay-tab-moves.js';
import { detachWindowFeatures, elementScreenPoint } from './terminal-drag-out.js';

export const PROCESS_WINDOW_NAME = 'tclaude-process:';
export const TO_DASHBOARD_LABEL = 'Open in the Processes tab';
const API = 'tclaudeProcessWindows';
const WATCH_MS = 1000;
const CLAIM_MS = 60_000;

// Drafts on their way out, by token: { id, label, draft, live, popup, restore }.
const leaving = new Map();
// Templates whose editor is in a window of their own, by id: the window.
const away = new Map();

const token = () => `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 10)}`;

export function processEditorAway(id) {
  const popup = away.get(id);
  if (popup && !popup.closed) return popup;
  away.delete(id);
  return null;
}

// heldForWindow is whether a live editor waits for a window to take its draft,
// so whatever showed it must not tear it down.
export const heldForWindow = (editor) => [...leaving.values()].some((entry) => entry.live.editor === editor);

// editorToWindow sends a live editor's draft to a new browser window, opened as
// large as rect where it was. restore(live) takes the editor back if the window
// never takes the draft up. It answers whether the window opened.
export function editorToWindow({ id, label, live, rect = null, win = globalThis.window, notify = null, restore }) {
  // A save on its way would land after the draft left, against its old base.
  if (live.editor?.savePending) {
    notify?.('Saving: move the editor once the save is done.', true);
    return false;
  }
  const key = token();
  const features = rect ? detachWindowFeatures({ size: rect, at: elementScreenPoint(win, rect), screen: win.screen }) : '';
  let popup = null;
  try {
    popup = win.open(`/automations/processes/templates/${encodeURIComponent(id)}`, `${PROCESS_WINDOW_NAME}${key}`, features || undefined);
  } catch { popup = null; }
  if (!popup) {
    notify?.(POPUP_BLOCKED, true);
    return false;
  }
  const entry = { id, label, draft: JSON.stringify(live.editor.handoff()), live, popup, restore, claimed: false };
  leaving.set(key, entry);
  away.set(id, popup);
  const started = Date.now();
  const watch = win.setInterval(() => {
    if (!leaving.has(key)) { win.clearInterval(watch); return; }
    if (popup.closed || (!entry.claimed && Date.now() - started > CLAIM_MS)) {
      win.clearInterval(watch);
      leaving.delete(key);
      away.delete(id);
      restore(live);
    }
  }, WATCH_MS);
  return true;
}

// installProcessWindows lets this page's windows hand drafts back and forth.
// open(id, label, handoff, { overlay }) is how this page takes one in, and
// floats() whether it has overlay windows to take one into.
export function installProcessWindows(win, { open, floats = () => false }) {
  const api = {
    floats: () => !!floats(),
    claim(key) {
      const entry = leaving.get(key);
      if (!entry) return null;
      entry.claimed = true;
      return { id: entry.id, label: entry.label, draft: entry.draft };
    },
    // The window has the draft open: the editor kept aside here goes.
    taken(key) {
      const entry = leaving.get(key);
      if (!entry) return;
      leaving.delete(key);
      entry.live.mount.remove();
      entry.live.editor?.destroy?.();
    },
    // A window holding a template says so, so a reload here still knows.
    here(id, popup) {
      if (typeof id === 'string' && popup) away.set(id, popup);
    },
    async back(id, label, draft, overlay = false) {
      win.focus?.();
      if (!(await open(id, label, JSON.parse(draft), { overlay }))) return false;
      away.delete(id);
      return true;
    },
  };
  win[API] = api;
  return () => {
    if (win[API] === api) delete win[API];
  };
}

// soloHandoff is, in a window opened by editorToWindow, the draft it came for:
// { id, label, handoff, taken }, or null on any other page. It asks once.
export function soloHandoff(win = globalThis.window) {
  const name = String(win?.name || '');
  if (!name.startsWith(PROCESS_WINDOW_NAME)) return null;
  const key = name.slice(PROCESS_WINDOW_NAME.length);
  let api = null;
  try { api = win.opener?.[API] || null; } catch { api = null; }
  const claimed = api?.claim(key);
  if (!claimed) return null;
  return {
    id: claimed.id, label: claimed.label, handoff: JSON.parse(claimed.draft),
    taken: () => { try { api.taken(key); } catch { /* the dashboard closed meanwhile */ } },
  };
}

// announceSolo keeps telling the dashboard that opened this window that it holds
// template id, through that dashboard's reloads. It returns the way to stop.
export function announceSolo(id, win = globalThis.window, everyMs = 2000) {
  const tell = () => { try { win.opener?.[API]?.here(id, win); } catch { /* not there now */ } };
  tell();
  const timer = win.setInterval(tell, everyMs);
  return () => win.clearInterval(timer);
}

// isProcessSolo is whether this page is such a window.
export const isProcessSolo = (win = globalThis.window) => String(win?.name || '').startsWith(PROCESS_WINDOW_NAME);

// openerTakesBack is whether the dashboard that opened this window is still
// there to take its editor back.
export function openerTakesBack(win = globalThis.window) {
  try { return !!win.opener && !win.opener.closed && !!win.opener[API]; } catch { return false; }
}

// openerFloats is whether that dashboard can take the editor into an overlay window.
export function openerFloats(win = globalThis.window) {
  try { return openerTakesBack(win) && !!win.opener[API].floats(); } catch { return false; }
}

// editorBack hands this window's draft back to the dashboard that opened it, to
// the Processes tab or an overlay window, and answers whether it was taken.
export async function editorBack({ id, label, editor, overlay = false, win = globalThis.window }) {
  if (!openerTakesBack(win)) return false;
  try {
    return !!(await win.opener[API].back(id, label, JSON.stringify(editor.handoff()), overlay));
  } catch {
    return false;
  }
}
