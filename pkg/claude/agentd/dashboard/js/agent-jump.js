// agent-jump.js — raise an agent's terminal on this node, as its row's jump
// (eye) does: an open web pane in the Terminals tab first; else, with web
// terminals as the default (config dashboard.default_terminal = "web"), its
// live session as a browser pane; else the native OS window.
import { focusTerminalForConv, openWebWindowPane } from './terminals-tab.js';

export async function jumpAgent(conv, label, preferWebTerminal, harness = '', toast = () => {}) {
  if (focusTerminalForConv([conv])) { toast(`focused: ${label}`); return; }
  if (preferWebTerminal) { openWebWindowPane(conv, label, { harness }); toast(`focused: ${label}`); return; }
  try {
    const r = await fetch(`/api/jump/${encodeURIComponent(conv)}`, {
      method: 'POST', credentials: 'same-origin',
    });
    if (!r.ok) { toast(`focus ${label}: ${await r.text()}`, true); return; }
    toast(`focused: ${label}`);
  } catch (e) {
    toast(`focus ${label}: ${(e && e.message) || e}`, true);
  }
}
