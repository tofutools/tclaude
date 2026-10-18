// palette-fused.js — the command palette's commands for the fused Groups
// view's other nodes: each group there (group@node) gets the spawn and
// message commands one node's own groups get, sent to that node. What the
// node does not let this operator do stays listed, disabled, saying why.
import { isPageNode, nodeSnapshot, selfNodeID } from './node-api.js';
import { featureNeeds, featureState } from './peer-view-limits.js';
import { openRemoteAgentSpawn } from './agent-spawn-controller.js';
import { openMessageCreateModal } from './message-access-dialog-controller.js';

const SAFE_AGENT = /^agt_[A-Za-z0-9]{4,64}$/;

// ownName is a merged group's name on its own node (group@node → group).
function ownName(group) {
  const at = `@${group.fleet_node?.name || ''}`;
  return group.name?.endsWith(at) ? group.name.slice(0, -at.length) : group.name;
}

// nodeAllows reports whether a node takes feature from this operator: this
// node always, a peer trusting it unrestricted, else the peer's grant.
function nodeAllows(node, feature, snapshot) {
  if (node === selfNodeID()) return '';
  const view = snapshot?.peer_view;
  if (view?.level === 'unrestricted' || featureState(feature, view) === 'shared') return '';
  return featureNeeds(feature, view) || 'unrestricted trust';
}

// buildFusedNodeCommands lists, for each group of the fused listing on
// another node than the page's, a spawn and a message command bound to that
// node, and an open-terminal command for each of a peer's live agents. Each names its node (node), so the page's own peer-view limits leave
// it alone: it carries that node's gate instead.
export function buildFusedNodeCommands(listing, {
  wiz = (regular) => regular, snapshotOf = nodeSnapshot, openSpawn = openRemoteAgentSpawn, openMessage = openMessageCreateModal,
  openTerminal = (opts) => import('./remote-terminal.js').then((m) => m.openRemoteTerminal(opts)),
} = {}) {
  const cmds = [];
  const seen = new Set();
  for (const g of listing?.groups || []) {
    const id = g.fleet_node?.id || '';
    if (!id || isPageNode(id)) continue;
    const nodeName = g.fleet_node.name || id;
    const snapshot = snapshotOf(id);
    const group = ownName(g);
    const gate = (feature, what) => {
      const needs = nodeAllows(id, feature, snapshot);
      return needs ? { enabled: false, disabledReason: `${nodeName} does not share ${what} with you (needs ${needs})` } : {};
    };
    cmds.push({
      node: id, icon: wiz('＋', '🔮'), label: wiz(`Spawn agent in ${g.name}…`, `Summon a familiar into ${g.name}…`),
      hint: wiz(`open the spawn dialog pinned to this group on ${nodeName}`, `open the summoning circle bound to this party on ${nodeName}`),
      keywords: `new agent create spawn launch start group ${g.name} ${nodeName} summon conjure invoke familiar party`,
      run: () => openSpawn({ nodeId: id, node: nodeName, groupName: group, snapshot }),
      ...gate('spawn.request', 'spawning'),
    });
    cmds.push({
      node: id, icon: wiz('✉', '✒'), label: wiz(`Message group: ${g.name}…`, `Send missive to party: ${g.name}…`),
      hint: wiz(`compose a one-shot message to this group on ${nodeName}`, `inscribe a missive for this party on ${nodeName}`),
      keywords: `message send compose notify group broadcast multicast all members agents ${g.name} ${nodeName} missive inscribe party familiars circle`,
      run: () => openMessage({ targetMode: 'group', groupName: group }, id, nodeName),
      ...gate('messaging', 'messaging'),
    });
    // A peer's live agents open here in the browser terminal, as their rows'
    // jump does; the peer authorises it. (This node's, from a peer's page,
    // open from its own dashboard.)
    if (g.fleet_node.local) continue;
    for (const m of g.members || []) {
      if (!m?.online || !SAFE_AGENT.test(m.agent_id || '') || seen.has(`${id}/${m.agent_id}`)) continue;
      seen.add(`${id}/${m.agent_id}`);
      const label = `${m.title || m.agent_id}@${nodeName}`;
      cmds.push({
        id: `agent-focus:${id}/${m.agent_id}`,
        node: id, icon: wiz('◎', '👁'), label: wiz(`Open terminal: ${label}`, `Reveal familiar: ${label}`),
        hint: wiz(`open this agent's terminal on ${nodeName} here`, `conjure this familiar's scrying portal from ${nodeName}`),
        keywords: `focus show jump open terminal window agent ${label} reveal behold conjure portal scrying familiar`,
        run: () => openTerminal({ instance: id, agent: m.agent_id, peerLabel: nodeName, label: m.title || '' }),
      });
    }
  }
  return cmds;
}
