// terminal-window-model.js -- a plain terminal's overlay window (tcl-0qd62y): a
// directory shell or a group shell, as a board or a chat floats. An agent's own
// terminal floats as its chat window instead, and a peer's terminal stays a tab.
// The window keeps the pane's key, so a terminal is in one place at a time and
// goes back to the same tab.

import { companionAgentOf } from './companion-model.js';

export const TERMINAL_WINDOW_KIND = 'terminal';
export const terminalWindowId = (paneKey) => `terminal:${paneKey}`;

// One path segment, which a URL parser cannot turn into another route.
const SHELL_ROUTE = /^\/api\/(?:term-ws|group-term-ws)\/(?!\.|%2e)(?![^?#]*%(?:2f|5c))[^/?#\\]+(?:\?[^#]*)?$/i;

// overlayablePane reports whether a Terminals pane can float.
export function overlayablePane(pane) {
  return !pane?.seed?.remote && !pane?.seed?.board && SHELL_ROUTE.test(pane?.seed?.ws || '');
}

// An agent's live session, as a tab opens it. A shell of its stays a shell.
const AGENT_ROUTE = /^\/api\/(?:open-window-ws|spawn-focus-ws)\//;

// chatAgentOfPane is the local agent running in a Terminals pane, null when the
// pane is anything else or the agent is not running: its chat would have nothing
// to attach to.
export function chatAgentOfPane(pane, snapshot) {
  if (pane?.seed?.remote || !pane?.seed?.agent || !AGENT_ROUTE.test(pane.seed.ws || '')) return null;
  const agent = companionAgentOf(snapshot, pane.seed.agent);
  return agent?.online ? agent : null;
}

export function terminalWindowOf(pane) {
  return { id: terminalWindowId(pane.key), kind: TERMINAL_WINDOW_KIND, paneKey: pane.key, ws: pane.seed.ws, agent: pane.seed.agent || '', label: pane.label };
}

// A saved window names a route it will connect to, so it must be one of these.
export function validTerminalWindow(window) {
  return window.id === terminalWindowId(window.paneKey) && SHELL_ROUTE.test(window.ws || '');
}

// The pane a window goes back to.
export function paneSeedOf(window) {
  return { ws: window.ws, key: window.paneKey, label: window.label, ...(window.agent && { agent: window.agent }) };
}
