// group-links-model.js -- a group's links as both its Links sections list
// them: the desktop's (groups-links.js) and the phone's (phone-group-links.js).
// Pure: rows, a cross-node link's state, and what deleteLink takes.

export const peerName = (link) => link.peer_label || String(link.peer || '').slice(0, 13);

// groupLinks is the group's links as its page lists them: the ones leaving
// it, then the ones coming in. Each row:
//   link       the snapshot's link
//   out        whether it leaves this group (this group is FROM)
//   other      the group at the other end ("peer:group" across nodes)
//   remote     whether the other end is on a peer node
//   state      a cross-node link's state as the desktop's Links table words
//              it ('' for a local one), and tone its pill's
// A cross-node link names this group on its own side only: FROM going out,
// TO coming in.
export function groupLinks(snapshot, name) {
  const rows = [];
  for (const link of snapshot?.links || []) {
    const remote = !!link.peer;
    const incoming = remote && link.direction === 'in';
    const out = remote ? !incoming && link.from === name : link.from === name;
    const into = remote ? incoming && link.to === name : link.to === name;
    if (!out && !into) continue;
    const far = out ? link.to : link.from;
    rows.push({
      link, out, remote,
      other: remote ? `${peerName(link)}:${far || link.remote_group || '?'}` : far || '(deleted)',
      ...linkState(link),
    });
  }
  return [...rows.filter((row) => row.out), ...rows.filter((row) => !row.out)];
}

// linkState is a cross-node link's state in a word or two: this node's half
// if it is not active, else what the peer last said of its own.
export function linkState(link) {
  if (!link.peer) return { state: '', tone: '' };
  const local = link.local_state || 'active';
  if (local !== 'active') return { state: local, tone: local === 'revoked' || local === 'declined' ? 'err' : 'ask' };
  const remote = link.remote_state || 'unknown';
  if (link.remote_stale || remote === 'unknown') return { state: 'peer offline', tone: 'off' };
  if (remote === 'active') return { state: 'active', tone: 'work' };
  if (remote === 'pending' || remote === 'connecting') return { state: remote, tone: 'ask' };
  return { state: remote, tone: remote === 'declined' || remote === 'revoked' || remote === 'expired' ? 'err' : 'ask' };
}

// deleteFields is what the desktop's deleteLink takes for a link: the group
// whose route removes it (a cross-node one comes in on TO), and for a
// cross-node one the peer and whether it is still a request.
export function deleteFields(link) {
  if (!link.peer) return { ...link, scope: link.from || link.to };
  const incoming = link.direction === 'in';
  return {
    ...link,
    scope: incoming ? link.to : link.from || link.to,
    peerLabel: peerName(link),
    pending: !incoming && link.remote_state === 'pending',
  };
}

