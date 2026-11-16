// phone-open-shell.js -- a phone opens a shell of the operator's own where an
// agent is working (its sheet's "Shell here"), or in a group's default
// directory (the group page's Shell), in the phone terminal with the shell
// key strip. The desktop paths underneath: an agent's directory shell
// (terminals-tab.js openWebTermPane, terminal-here.js shellTarget) and the
// group's shell (groups-resources.js addGroupShell); on a peer's page, the
// peer's shell over the federation terminal stream (remote-terminal.js
// openRemoteShell), only in groups whose sessions.shell grant lets this in.
// A shell already running opens again on the same key, so it is brought
// forward, not started twice.

import { shellTarget } from './terminal-here.js';
import { peerShellAllowed } from './peer-view-limits.js';
import { onPeerPage } from './node-api.js';
import { shellToast } from './shell-state.js';

// agentShellEntries are the shell entries of an agent's sheet, as plain
// data: Shell here (the worktree it works in, where its row says it is)
// and, when it was started somewhere else, Shell in its launch directory.
// Empty where no shell opens: a shell's own row, no known directory, or a
// peer that does not grant sessions.shell on its group. groupName is the
// group the sheet was opened in: the shell runs there (its environment, and
// the pane key a shell listed for that group has), as a desktop row's does,
// where it may; else in the agent's first group that may.
export function agentShellEntries(snapshot, row, groupName = '') {
  const id = row?.member?.agent_id || row?.agent || row?.conv || '';
  const picked = id ? shellTarget(snapshot, id) : null;
  if (!picked) return [];
  const target = { ...picked, group: inGroup(snapshot, id, groupName) ? groupName : picked.group };
  const member = row.member || {};
  const here = member.current_dir || target.dir;
  const start = member.startup_dir || member.state?.cwd || '';
  const entries = [{ key: 'shell', which: 'worktree', label: 'Shell here', sub: here || `Where ${row.name} is working`, target }];
  if (start && here && start !== here) entries.push({ key: 'shell-start', which: 'start', label: 'Shell in its launch directory', sub: start, target });
  return entries;
}

// inGroup reports whether the agent id is a member of group name in which
// a shell may open: any on this node, one granting sessions.shell on a peer.
function inGroup(snapshot, id, name) {
  if (!name) return false;
  const group = (snapshot?.groups || []).find((g) => g?.name === name);
  if (!group?.members?.some((m) => m?.agent_id === id || m?.conv_id === id)) return false;
  return !snapshot.peer_view || peerShellAllowed(snapshot.peer_view, name);
}

// groupShellOffer is what a group page offers for a shell: { dir } to open
// one in its default directory, { why } when it cannot, or null where the
// page shows no shell at all (archived, or a peer that does not grant
// sessions.shell on it, or one that does not show its default directory:
// the desktop offers no shell there either, and "none set" would be wrong).
export function groupShellOffer(snapshot, group) {
  if (!group?.name || group.archived) return null;
  const peerView = snapshot?.peer_view;
  if (peerView && !peerShellAllowed(peerView, group.name)) return null;
  const dir = (group.default_cwd || '').trim();
  if (dir) return { dir };
  return peerView ? null : { why: 'This group has no default directory. Set one on the desktop to open a shell in it.' };
}

const remote = () => globalThis.__tclaudeRemoteNode || null;
// peerName is how a peer's page names the peer it shows (as palette.js).
const peerName = (node) => globalThis.document?.documentElement?.dataset?.remoteNodeName || String(node.id).slice(0, 13);
const peerShell = (options) => import('./remote-terminal.js').then((m) => m.openRemoteShell(options));
const localAgentShell = (agent, label, which, group) => import('./terminals-tab.js').then((m) => m.openWebTermPane(agent, label, which, { group }));
const localGroupShell = (name) => import('./groups-resources.js').then((m) => m.addGroupShell({ name }));

// openAgentShell opens entry's shell (an agentShellEntries entry) for the
// agent named name. The seams are for tests.
export async function openAgentShell(entry, name, {
  notify = shellToast, peer = peerShell, local = localAgentShell, node = remote(),
} = {}) {
  const { target, which } = entry;
  if (onPeerPage(node)) {
    return peer({ instance: node.id, peerLabel: peerName(node), group: target.group, shell: which, agent: target.agent, label: name, toast: notify });
  }
  const opened = await local(target.agent, `shell · ${name}`, which, target.group);
  if (!opened) notify('The terminal could not open here (the terminal runtime did not load)', true);
  return opened;
}

// openGroupShell opens the group's shell in its default directory.
export function openGroupShell(groupName, { notify = shellToast, peer = peerShell, local = localGroupShell, node = remote() } = {}) {
  if (onPeerPage(node)) return peer({ instance: node.id, peerLabel: peerName(node), group: groupName, shell: 'group', toast: notify });
  return local(groupName);
}
