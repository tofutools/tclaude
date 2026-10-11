// Peer proposals (tcl-hj04jt): one operator offers a list of items to a peer
// node: share a group's name and description (optionally its member list) and
// directed cross-node link edges. Each side applies only its own items, and
// only after its operator accepts. A single cross-node link request is the
// one-item case and keeps its short card (GroupLinkReader).
//
// This module holds the pure shaping between the dashboard drafts and the
// /api/federation/proposals wire format, so it is testable without a DOM.

export const LINK_MODES = ['members->members', 'owners->members'];

export function modeText(mode) {
  return String(mode || 'members->members').replace('->', ' → ');
}

// composeItems turns the composer draft into API items. A pair links one of
// our groups with one of the peer's; "both" is two independent edges, each
// approved and revocable on its own.
export function composeItems({ shares = [], pairs = [] }) {
  const items = [];
  for (const share of shares) {
    items.push({ kind: 'share_visibility', host: 'us', group_id: share.group, include_members: !!share.includeMembers });
  }
  for (const pair of pairs) {
    if (!pair.ours || !pair.theirs) continue;
    const mode = pair.mode || 'members->members';
    if (pair.dir !== 'in') {
      items.push({ kind: 'group_link', host: 'them', group_id: pair.theirs, source: 'us', source_group_id: pair.ours, mode });
    }
    if (pair.dir !== 'out') {
      items.push({ kind: 'group_link', host: 'us', group_id: pair.ours, source: 'them', source_group_id: pair.theirs, mode });
    }
  }
  return items;
}

function names(list) {
  if (list.length <= 1) return list.join('');
  return `${list.slice(0, -1).join(', ')} and ${list[list.length - 1]}`;
}

// composerSummary is the plain-language preview of a draft, from our side.
export function composerSummary({ peer, shares = [], pairs = [], peerGroupName = (id) => id }) {
  const lines = [];
  if (shares.length) {
    const withMembers = shares.filter((share) => share.includeMembers).map((share) => share.group);
    lines.push(`${peer} sees the name and description of ${names(shares.map((share) => share.group))}`
      + (withMembers.length ? `, plus the member list of ${names(withMembers)}` : ''));
  }
  for (const pair of pairs) {
    if (!pair.ours || !pair.theirs) continue;
    const theirs = `${peer}:${peerGroupName(pair.theirs)}`;
    const arrow = pair.dir === 'out' ? '→' : pair.dir === 'in' ? '←' : '⇄';
    lines.push(`link ${pair.ours} ${arrow} ${theirs} (${modeText(pair.mode)}${pair.dir === 'both' || !pair.dir ? ', two separate edges' : ''})`);
  }
  return lines;
}

// itemText describes one proposal item from this node's side.
export function itemText(item, peer) {
  if (item.kind === 'share_visibility') {
    const count = `${item.member_count ?? 0} member${item.member_count === 1 ? '' : 's'}`;
    return item.host === 'us'
      ? `${peer} sees your ${item.group_name} (${count})`
      : `you see ${peer}:${item.group_name} (${count})`;
  }
  if (item.kind === 'group_link') {
    return item.host === 'us'
      ? `${peer}:${item.peer_group_name} → your ${item.group_name}`
      : `your ${item.peer_group_name} → ${peer}:${item.group_name}`;
  }
  return item.description || item.kind;
}

// proposalLines summarizes a proposal the way the card's "X wants to" box
// reads: shares, roster requests and link edges, from this node's side.
export function proposalLines(view) {
  const peer = view?.peer_label || view?.peer || 'peer';
  const items = view?.items || [];
  const lines = [];
  const theirShares = items.filter((item) => item.kind === 'share_visibility' && item.host === 'them');
  const ourShares = items.filter((item) => item.kind === 'share_visibility' && item.host === 'us');
  if (theirShares.length) lines.push(`show you ${names(theirShares.map((item) => item.group_name))} (name and description${theirShares.some((item) => item.include_members) ? '; member list where marked' : ''})`);
  if (ourShares.length) lines.push(`let ${peer} see ${names(ourShares.map((item) => `your ${item.group_name}`))}${ourShares.some((item) => item.include_members) ? ' (member list where marked)' : ''}`);
  for (const item of items.filter((candidate) => candidate.kind === 'group_link')) {
    lines.push(`link ${itemText(item, peer)} (${modeText(item.mode)})`);
  }
  return lines;
}

// initialDraft ticks every item as proposed. Accepted items stay ticked and
// locked: they are already applied and a counter must keep them.
export function initialDraft(view) {
  const draft = { ticked: {}, members: {}, modes: {}, shareBack: [] };
  for (const item of view?.items || []) {
    draft.ticked[item.id] = item.status !== 'declined';
    if (item.kind === 'share_visibility') draft.members[item.id] = !!item.include_members;
    if (item.kind === 'group_link') draft.modes[item.id] = item.mode || 'members->members';
  }
  return draft;
}

// changed reports whether the draft differs in a way plain accept can't
// express (a new mode or a share back), so it must go back as a counter.
export function draftChanged(view, draft) {
  if (draft.shareBack.length) return true;
  return (view?.items || []).some((item) => item.kind === 'group_link' && draft.ticked[item.id]
    && draft.modes[item.id] !== (item.mode || 'members->members'));
}

// wireItem maps a proposal view item back to the API's item shape.
function wireItem(item, draft) {
  const out = { id: item.id, kind: item.kind, host: item.host, group_id: item.group_id };
  if (item.kind === 'share_visibility') out.include_members = !!draft.members[item.id];
  if (item.kind === 'group_link') {
    out.source = item.host === 'us' ? 'them' : 'us';
    out.source_group_id = item.peer_group_id;
    out.mode = draft.modes[item.id] || item.mode;
  }
  return out;
}

// decisionFor picks what the receiver's primary button sends: an accept of
// the ticked items (narrowing member lists), or a counter with the full
// replacement item list when the draft changed something.
export function decisionFor(view, draft) {
  const items = view?.items || [];
  const pending = items.filter((item) => item.status === 'pending');
  const ticked = pending.filter((item) => draft.ticked[item.id]);
  if (draftChanged(view, draft)) {
    const kept = items.filter((item) => item.status === 'accepted' || draft.ticked[item.id]);
    return {
      label: `Send counter-proposal (${kept.length + draft.shareBack.length} items)`,
      body: {
        revision: view.revision,
        decision: 'counter',
        items: [
          ...kept.map((item) => wireItem(item, draft)),
          ...draft.shareBack.map((group) => ({ kind: 'share_visibility', host: 'us', group_id: group, include_members: false })),
        ],
      },
    };
  }
  if (!ticked.length) return null;
  const without = ticked.filter((item) => item.kind === 'share_visibility' && item.include_members && !draft.members[item.id]).map((item) => item.id);
  const all = ticked.length === pending.length && !without.length;
  return {
    label: all ? 'Accept all' : `Accept selected (${ticked.length} of ${pending.length})`,
    body: { revision: view.revision, decision: 'accept', item_ids: ticked.map((item) => item.id), ...(without.length ? { without_members: without } : {}) },
  };
}

// proposalTurn says whose move it is. Older views lack `awaiting`; then the
// originator is assumed to be waiting while the proposal is open.
export function proposalTurn(view) {
  if (!view) return '';
  if (typeof view.awaiting === 'string') return view.awaiting;
  if (view.state !== 'sent' && view.state !== 'countered') return '';
  return view.ours ? 'them' : 'us';
}

// withdrawLabel names the originator's withdraw action. An already accepted
// edge is not undone by withdrawing; it has to be revoked on its own.
export function withdrawLabel(view) {
  return (view?.items || []).some((item) => item.status === 'accepted') ? 'Withdraw pending items' : 'Withdraw';
}

const STATE_TEXT = {
  sent: 'sent', countered: 'countered', applying: 'applying', accepted: 'accepted',
  partially_accepted: 'partially accepted', declined: 'declined', expired: 'expired', withdrawn: 'withdrawn',
};

export function proposalStateText(view) {
  return STATE_TEXT[view?.state] || view?.state || '';
}
