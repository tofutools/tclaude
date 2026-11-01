// terminal-here.js -- the ⌨ in an agent terminal's floating tools (tcl-99t8tr):
// a shell of the operator's own where the agent is working. It is the directory
// shell a click on the agent's row opens, here or on a peer under
// sessions.shell, so it stays away where that is not allowed. The daemon picks
// the directory from its own tracking; the path here only says where it lands.

import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { isPageNode, nodeFetch, nodeSnapshot } from './node-api.js';
import { peerShellAllowed } from './peer-view-limits.js';
import { shellToast } from './shell-state.js';
import { openWebTermPane } from './terminals-tab.js';

const html = htm.bind(h);

// The worktree root, which the agent's row shows as where it is now.
const SHELL_WHICH = 'worktree';

const openPeerShell = (options) => import('./remote-terminal.js').then((m) => m.openRemoteShell(options));

// paneShellAgent is the agent a pane's ⌨ opens a shell for, or null. A
// directory shell has an agent too, but it is already the shell; a detached
// pop-out has no Terminals tab to open one in.
export function paneShellAgent(pane, solo = false) {
  if (solo) return null;
  const peer = pane?.seed?.remote;
  if (peer) return peer.agent ? { agent: peer.agent, node: peer.peer, peerLabel: peer.peerLabel || '' } : null;
  return pane?.seed?.agent && String(pane.key).startsWith('window:') ? { agent: pane.seed.agent } : null;
}

// shellTarget is where an agent's shell opens on the node snapshot describes,
// or null where there is none to open. Its group brings the group's environment,
// and on a peer it is the group whose sessions.shell grant lets this in.
export function shellTarget(snapshot, agent) {
  const matches = (row) => row?.agent_id === agent || row?.conv_id === agent;
  const groups = (snapshot?.groups || []).filter((group) => group.members?.some(matches));
  const row = groups[0]?.members.find(matches) || (snapshot?.agents || []).find(matches);
  const dir = row?.current_dir || row?.startup_dir || row?.state?.cwd || '';
  if (!dir || row.state?.harness === 'shell') return null;
  const peerView = snapshot.peer_view;
  const group = peerView ? groups.find((candidate) => peerShellAllowed(peerView, candidate.name)) : groups[0];
  if (peerView && !group) return null;
  return { agent: row.agent_id || agent, group: group?.name || '', dir };
}

// A peer's snapshot is at hand only while the fused Groups view polls it, so
// otherwise this asks once for the terminal on show.
function useNodeSnapshot(node, page) {
  const onPage = isPageNode(node);
  const polled = onPage ? null : nodeSnapshot(node);
  const [asked, setAsked] = useState(null);
  useEffect(() => {
    if (onPage || polled) return undefined;
    let live = true;
    nodeFetch(node)('/api/snapshot', { credentials: 'same-origin' })
      .then((response) => (response.ok ? response.json() : null))
      .then((snapshot) => { if (live) setAsked(snapshot); })
      .catch(() => {});
    return () => { live = false; };
  }, [node, onPage, !polled]);
  return onPage ? page : polled || asked;
}

export function ShellHere({
  agent, node = '', peerLabel = '', name = 'this agent', snapshot, notify = shellToast, openLocal = openWebTermPane, openPeer = openPeerShell,
}) {
  const target = shellTarget(useNodeSnapshot(node, snapshot), agent);
  if (!target) return null;
  const label = `shell · ${name}`;
  async function open() {
    if (node) {
      await openPeer({ instance: node, group: target.group, shell: SHELL_WHICH, agent: target.agent, peerLabel, label: name, toast: notify });
      return;
    }
    const opened = await openLocal(target.agent, label, SHELL_WHICH, { group: target.group });
    if (!opened) notify('The terminal could not open here (the terminal runtime did not load)', true);
  }
  const where = node ? ` on ${peerLabel || 'the peer'}` : '';
  return html`<button type="button" class="companion-tool companion-shell" aria-label=${`Open a shell where ${name} is working`}
    title=${`Open a shell${where} in ${target.dir}. It runs as you, outside ${name}'s sandbox, and ${name} can write to this directory.`}
    onClick=${open}>⌨</button>`;
}
