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
import { remoteShellPath, remoteTerminalPath } from './terminals-core.js';
import { shellToast } from './shell-state.js';

const html = htm.bind(h);
const SESSIONS_TTL_MS = 10000;

// openRemotePane opens a peer's terminal as a tab in the Terminals tab, like
// a local web terminal (pop-out and drag-out work the same way).
// options (reveal, activate) overrides the reveal flag when a caller passes it.
export function openRemotePane({ wsPath, label, remote, reveal = true }, options = { reveal }) {
  return openTerminalPane({ ws: wsPath, label, initialRetry: true, remote: Object.freeze({ peer: String(remote.peer), peerLabel: String(remote.peerLabel || ''), agent: String(remote.agent || '') }) }, options);
}
const SAFE_ID = /^(agt|inst)_[A-Za-z0-9]{4,64}$/;
const SESSIONS_WAIT_STEP_MS = 1500;

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
// it cannot. It resolves to the opened descriptor or null. wait (ms) keeps
// rereading the peer's sessions until the agent is listed, for one that was
// just spawned there and is not yet in its catalog.
export async function openRemoteTerminal({ instance, agent, peerLabel = '', label = '', fetchImpl, toast = shellToast, open = openRemotePane, background = false, wait = 0, sleep = (ms) => new Promise((r) => setTimeout(r, ms)), now = () => Date.now() } = {}) {
  if (!SAFE_ID.test(agent || '') || !SAFE_ID.test(instance || '')) {
    toast('This agent has no stable ID to open its terminal on the peer', true);
    return null;
  }
  const peer = peerLabel || instance.slice(0, 13);
  const deadline = now() + wait;
  let rows; let row;
  for (let fresh = false; ; fresh = true) {
    try {
      rows = await remoteSessions({ fetchImpl, fresh });
    } catch (e) {
      toast(`Could not read ${peer}'s sessions: ${e?.message || e}`, true);
      return null;
    }
    row = rows.find((r) => r?.instance === instance && r?.agent === agent);
    if (row || now() >= deadline) break;
    await sleep(SESSIONS_WAIT_STEP_MS);
  }
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
    label: name,
    ...(row.harness ? { harness: row.harness } : {}),
    remote: { peer: instance, peerLabel: peer, agent },
    ...(background ? { reveal: false } : {}),
  });
  if (!opened) toast('The terminal could not open here (the terminal runtime did not load)', true);
  return opened || null;
}

const SHELLS = new Set(['group', 'start', 'current', 'worktree']);
// A group name as a peer's snapshot carries it (no group@node suffix).
const SAFE_GROUP_NAME = /^[A-Za-z0-9._:-]{1,128}$/;

// openRemoteShell opens one of instance's tclaude shells in the Terminals tab:
// its group's shell in the group's default directory (shell 'group'), or a
// member agent's directory shell (shell 'start' | 'current' | 'worktree',
// with agent), started there if it is not running, as a click on that node
// does. The peer decides: its sessions.shell grant on the group, which
// unrestricted trust holds; a refusal comes back with its reason.
export async function openRemoteShell({ instance, group, shell, agent = '', peerLabel = '', label = '', toast = shellToast, open = openRemotePane, background = false } = {}) {
  const valid = SAFE_ID.test(instance || '') && SHELLS.has(shell) && SAFE_GROUP_NAME.test(group || '')
    && (shell === 'group' ? !agent : SAFE_ID.test(agent || ''));
  if (!valid) {
    toast('This shell cannot be opened on the peer', true);
    return null;
  }
  const peer = peerLabel || instance.slice(0, 13);
  const name = shell === 'group' ? `group shell · ${group}` : `shell · ${label || agent}${shell === 'current' ? '' : ` (${shell})`}`;
  const opened = await open({
    wsPath: remoteShellPath({ peer: instance, group, shell, agent }),
    label: name,
    remote: { peer: instance, peerLabel: peer, agent: '' },
    ...(background ? { reveal: false } : {}),
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
    open({ wsPath: remoteTerminalPath({ peer: node.id, agent: row.agent, mode }), label: row.name || row.agent, ...(row.harness ? { harness: row.harness } : {}), remote: { peer: node.id, peerLabel: node.name, agent: row.agent } });
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
    <div class="muted fa-cli-note">Opening never stops the agent. Viewers share the size: the smallest attached terminal sets it. With the take-over grant you can disconnect the others. ${node.name} shows REMOTE WATCH or REMOTE INPUT on its pane and can disconnect you.</div>
    <div class="modal-buttons"><span class="spacer"></span><button type="button" onClick=${onClose}>Close</button></div>
  </${Overlay}>`;
}
