// agent-presence.js — where a federated agent is relative to its home
// (tcl-hocp2h). The snapshot's member.federation_presence is the daemon's
// agent_federation_presence row as seen by the node that rendered it:
//   state "away"  on the home node while the agent visits elsewhere; the row
//                 stays in its home groups as a placeholder.
//   state "here"  where it runs now. home_instance != current_instance means
//                 it is visiting; equal means it came back home.
// Pure, so the wording and the ten-minute "back home" window are unit-tested.
import { nodeColor } from './skynet-model.js';

// BACK_HOME_MS is how long the "back home" tag shows after the arrival.
export const BACK_HOME_MS = 10 * 60 * 1000;

function when(raw) {
  const t = Date.parse(raw || '');
  // Go renders an unset time as year 1; treat anything that old as absent.
  return Number.isFinite(t) && t > 0 ? t : null;
}

// presenceKind classifies one member's presence: 'away', 'visiting', 'back',
// or '' for an agent that never left (or whose return is old news).
export function presenceKind(presence, now = Date.now()) {
  const p = presence;
  if (!p || !p.home_instance) return '';
  if (p.state === 'away') return 'away';
  if (p.state !== 'here') return '';
  if (p.current_instance && p.home_instance !== p.current_instance) return 'visiting';
  const arrived = when(p.arrived_at);
  if ((p.hop_count || 0) > 0 && arrived !== null && now - arrived >= 0 && now - arrived < BACK_HOME_MS) return 'back';
  return '';
}

export function isAwayPlaceholder(member) {
  return member?.federation_presence?.state === 'away';
}

// nodeRef names an instance the way the chip row does: this node, a trusted
// peer's label, or a short instance ID for a node this one does not know.
export function nodeRef(id, fleet) {
  if (!id) return { id: '', name: 'another node', color: '' };
  if (fleet?.self?.id === id) return { id, name: fleet.self.name, color: fleet.self.color, local: true };
  const peer = fleet?.peers?.find((p) => p.id === id);
  if (peer) return { id, name: peer.name, color: peer.color, local: false };
  return { id, name: String(id).slice(0, 13), color: nodeColor(id), local: false };
}

const hops = (n) => `${n} hop${n === 1 ? '' : 's'}`;

// presenceView is what a row shows for its presence, or null.
export function presenceView(member, fleet, now = Date.now()) {
  const p = member?.federation_presence;
  const kind = presenceKind(p, now);
  if (!kind) return null;
  const home = nodeRef(p.home_instance, fleet);
  const hop = p.hop_count || 0;
  const name = member.title || member.agent_id || 'this agent';
  if (kind === 'away') {
    const at = nodeRef(p.current_instance || p.current_peer, fleet);
    return {
      kind, home, at, hop,
      label: `away → on ${at.name}${hop ? ` · hop ${hop}` : ''}`,
      title: `${name} is away: it runs on ${at.name} now${hop ? ` (hop ${hop})` : ''}. Its place in this group is kept, but it is not counted as a member here.`,
    };
  }
  if (kind === 'visiting') {
    const arrived = when(p.arrived_at);
    return {
      kind, home, hop,
      label: `away from ${home.name}${hop ? ` · hop ${hop}` : ''}`,
      title: `Home: ${home.name}${member.agent_id ? ` (${member.agent_id})` : ''}${hop ? ` · hop ${hop}` : ''}${arrived ? ` · arrived ${ago(now - arrived)}` : ''}. It is visiting this node; its home keeps its place.`,
    };
  }
  const from = p.predecessor_instance ? nodeRef(p.predecessor_instance, fleet) : null;
  return {
    kind, home, hop, from,
    label: `back home${from ? ` · from ${from.name}` : ''}${hop ? ` · ${hops(hop)}` : ''}`,
    title: `${name} came back home to ${home.name} as the same agent${member.agent_id ? ` (${member.agent_id})` : ''}, with its inbox, groups and permissions. This tag fades ten minutes after it arrived.`,
  };
}

function ago(ms) {
  const m = Math.max(0, Math.round(ms / 60000));
  if (m < 1) return 'just now';
  if (m < 60) return `${m} min ago`;
  const h = Math.round(m / 60);
  return `${h} h ago`;
}
