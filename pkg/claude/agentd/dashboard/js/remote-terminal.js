// remote-terminal.js — open a peer's agent terminal in the browser.
//
// The browser talks only to this node's daemon: GET /api/federation/sessions
// (the cached catalog of peers' sessions, with what each lets this node do)
// decides watch or interactive up front, and the terminal itself is the
// federation terminal stream bridged by /api/federation/terminal. The peer
// still authorises every open, so a stale catalog only costs a refused open
// with its reason. The CLI attach command stays as a secondary "native
// terminal" option.

import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { openTerminalPane } from './terminals-tab.js';
import { remoteTerminalPath } from './terminals-core.js';
import { shellToast } from './shell-state.js';

const html = htm.bind(h);
const SESSIONS_TTL_MS = 10000;

// openRemotePane opens a peer's terminal as a tab in the Terminals tab, like
// a local web terminal (pop-out and drag-out work the same way).
export function openRemotePane({ wsPath, label, remote }) {
  return openTerminalPane({ ws: wsPath, label, initialRetry: true, remote: Object.freeze({ peer: String(remote.peer), peerLabel: String(remote.peerLabel || ''), agent: String(remote.agent || '') }) });
}
const SAFE_ID = /^(agt|inst)_[A-Za-z0-9]{4,64}$/;

let cache = null;

// remoteSessions reads the catalog, briefly cached so a row of clicks does
// not refetch it each time.
export async function remoteSessions({ fetchImpl = (...a) => globalThis.fetch(...a), now = Date.now(), fresh = false } = {}) {
  if (!fresh && cache && now - cache.at < SESSIONS_TTL_MS) return cache.rows;
  const res = await fetchImpl('/api/federation/sessions', { credentials: 'same-origin', cache: 'no-store' });
  if (!res.ok) throw new Error(`sessions: HTTP ${res.status}`);
  const rows = await res.json();
  cache = { at: now, rows: Array.isArray(rows) ? rows : [] };
  return cache.rows;
}

export function resetRemoteSessionsCache() { cache = null; }

// remoteTerminalMode picks how a session opens: interactive when the peer
// lets this node type into it, watch when it only lets it look, else ''.
export function remoteTerminalMode(row) {
  if (row?.attach === true) return 'interactive';
  if (row?.watch === true) return 'watch';
  return '';
}

export function nativeAttachCommand(agent, instance) {
  return SAFE_ID.test(agent) && SAFE_ID.test(instance) ? `tclaude federation attach ${agent}@${instance}` : '';
}

// openRemoteTerminal opens instance's agent in the Terminals tab, or says why
// it cannot. It resolves to the opened descriptor or null.
export async function openRemoteTerminal({ instance, agent, peerLabel = '', label = '', fetchImpl, toast = shellToast, open = openRemotePane } = {}) {
  if (!SAFE_ID.test(agent || '') || !SAFE_ID.test(instance || '')) {
    toast('This agent has no stable ID to open its terminal on the peer', true);
    return null;
  }
  const peer = peerLabel || instance.slice(0, 13);
  let rows;
  try {
    rows = await remoteSessions({ fetchImpl });
  } catch (e) {
    toast(`Could not read ${peer}'s sessions: ${e?.message || e}`, true);
    return null;
  }
  const row = rows.find((r) => r?.instance === instance && r?.agent === agent);
  const mode = remoteTerminalMode(row);
  if (!mode) {
    const cmd = nativeAttachCommand(agent, instance);
    toast(row
      ? `${peer} does not share this agent's terminal with you (it needs sessions.watch to look, sessions.attach to type)`
      : `${peer} has not listed this agent's session; ${cmd ? `try the CLI: ${cmd}` : 'it may not be shared with you'}`, true);
    return null;
  }
  const name = label || row.name || agent;
  const opened = await open({
    wsPath: remoteTerminalPath({ peer: instance, agent, mode }),
    label: `${name} @ ${peer}`,
    remote: { peer: instance, peerLabel: peer, agent },
  });
  if (!opened) toast('The terminal could not open here (the terminal runtime did not load)', true);
  return opened || null;
}

// RemoteSessionsDialog lists a peer's sessions (the map's "Terminals…") with
// how each opens here; the CLI attach command is the native-terminal option.
export function RemoteSessionsDialog({ node, onClose, fetchImpl, toast = shellToast, open = openRemotePane, copy = (t) => globalThis.navigator.clipboard.writeText(t) }) {
  const [rows, setRows] = useState(null);
  useEffect(() => {
    let off = false;
    remoteSessions({ fetchImpl, fresh: true })
      .then((all) => { if (!off) setRows(all.filter((r) => r?.instance === node.id)); })
      .catch((e) => { if (!off) setRows({ error: e?.message || String(e) }); });
    return () => { off = true; };
  }, [node.id]);
  const go = (row) => {
    const mode = remoteTerminalMode(row);
    if (!mode || !SAFE_ID.test(row.agent || '')) return;
    onClose();
    open({ wsPath: remoteTerminalPath({ peer: node.id, agent: row.agent, mode }), label: `${row.name || row.agent} @ ${node.name}`, remote: { peer: node.id, peerLabel: node.name, agent: row.agent } });
  };
  const copyCmd = (cmd) => Promise.resolve().then(() => copy(cmd)).then(() => toast('Command copied', false)).catch(() => toast(`Run in a terminal: ${cmd}`, false));
  return html`<${Overlay} id="remote-sessions-modal" labelledby="remote-sessions-title" onClose=${onClose}>
    <h3 id="remote-sessions-title">Terminals on ${node.name}</h3>
    ${rows?.error ? html`<div class="fa-danger" role="alert">${rows.error}</div>`
      : !rows ? html`<div class="muted">Loading…</div>`
      : rows.length === 0 ? html`<div class="muted">${node.name} shares no sessions with you (it needs sessions.watch to look, sessions.attach to type).</div>`
      : html`<table class="fa-table" id="remote-sessions"><tbody>${rows.map((r) => {
        const mode = remoteTerminalMode(r);
        const cmd = nativeAttachCommand(r.agent, node.id);
        return html`<tr key=${r.agent} data-agent=${r.agent}>
          <td>${r.name || r.agent}${r.stale ? html` <span class="muted" title="The catalog entry is out of date; the peer decides when you open it">· stale</span>` : ''}</td>
          <td class="muted">${(r.groups || []).join(', ')}</td>
          <td>${mode ? html`<button type="button" data-open=${mode} onClick=${() => go(r)}>${mode === 'interactive' ? '⌨ Open (interactive)' : '👁 Watch'}</button>` : html`<span class="muted">not shared</span>`}</td>
          <td class="fa-acts">${cmd && html`<button type="button" class="fa-link" data-copy-cli title=${`Native terminal: ${cmd}`} onClick=${() => copyCmd(cmd)}>CLI command</button>`}</td>
        </tr>`;
      })}</tbody></table>`}
    <div class="muted fa-cli-note">Opening never stops or resizes the agent; ${node.name} shows REMOTE WATCH or REMOTE INPUT on its pane and can disconnect you.</div>
    <div class="modal-buttons"><span class="spacer"></span><button type="button" onClick=${onClose}>Close</button></div>
  </${Overlay}>`;
}
