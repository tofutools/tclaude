// peer-drawing-boards-ui.js -- the pieces of a peer's drawing boards on its page
// (tcl-ug8bb8, peer-drawing-boards.js has the data): the edit / read-only badge,
// the ＋ that makes a drawing board in a group this node writes there, and a group's
// Drawing boards in the peer's Groups tab (placement B). The Collab panel lists the
// same drawing boards under their groups (placement A); both open the one editor.

import { h } from 'preact';
import htm from 'htm';
import { openDrawingBoardPane } from './drawing-board-overlay-state.js';
import { Button } from './ui-components.js';
import { loadPeerDrawingBoards, peerDrawingBoardAccess, peerDrawingBoards, peerDrawingBoardsShared, peerGroupDrawingBoards, peerName } from './peer-drawing-boards.js';
import { shellToast } from './shell-state.js';
import { relTime } from './helpers.js';

const html = htm.bind(h);

// PeerDrawingBoardBadge says what this node may do with a peer's drawing board.
export function PeerDrawingBoardBadge({ access }) {
  if (!access) return null;
  const write = access === 'write';
  return html`<span class=${`ui-tag peer-drawing-board-badge ${write ? 'write' : 'read'}`} data-peer-drawing-board-access=${access}
    title=${write ? `You can edit this drawing board on ${peerName()}` : `${peerName()} shares this drawing board read-only`}>${write ? 'edit' : 'read-only'}</span>`;
}

// freePeerTitle is "<group> drawing board", or the first "<group> drawing board N" no drawing board
// of the peer's list is called (a peer's new drawing board takes its id from it).
export function freePeerTitle(group, drawingBoards = peerDrawingBoards.value) {
  const taken = new Set((drawingBoards || []).flatMap((b) => [b.title, b.id]).filter(Boolean).map((t) => String(t).toLowerCase()));
  const base = `${group} drawing board`;
  if (!taken.has(base.toLowerCase()) && !taken.has(base.toLowerCase().replace(/\s+/g, '-'))) return base;
  for (let n = 2; ; n++) {
    const title = `${base} ${n}`;
    if (!taken.has(title.toLowerCase()) && !taken.has(title.toLowerCase().replace(/\s+/g, '-'))) return title;
  }
}

// createPeerDrawingBoard makes a drawing board in group on the peer (POST /api/drawing-boards with
// its group: a peer's drawing board always has one) and opens it.
export async function createPeerDrawingBoard(group, { fetchImpl = (...args) => globalThis.fetch(...args), notify = shellToast, open = openDrawingBoardPane } = {}) {
  const title = freePeerTitle(group);
  try {
    const response = await fetchImpl('/api/drawing-boards', {
      method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ title, group }),
    });
    const body = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(body.message || body.error || `${response.status}`);
    notify(`Created ${body.title || title} in ${group} on ${peerName()}`);
    void loadPeerDrawingBoards(fetchImpl);
    open(body.id, body.title || title);
    return body;
  } catch (cause) {
    notify(`Could not create a drawing board on ${peerName()}: ${cause.message}`, true);
    return null;
  }
}

// PeerNewDrawingBoard is a group section's ＋ in the Collab panel, for a group this
// node writes on the peer.
export function PeerNewDrawingBoard({ group, create = createPeerDrawingBoard }) {
  if (peerDrawingBoardAccess(group) !== 'write') return null;
  const title = `New drawing board in ${group} on ${peerName()}`;
  return html`<button data-ui-exempt type="button" class="cr-tool cr-section-add" data-peer-drawing-board-new=${group}
    title=${title} aria-label=${title} onClick=${() => void create(group)}>＋</button>`;
}

// PeerGroupDrawingBoards is a group's drawing boards on the peer's Groups tab: each drawing board
// with its badge, opened as a tab here; a group this node writes adds a
// drawing board. Nothing for a group it cannot read, or off a peer's page.
export function PeerGroupDrawingBoards({ group, open = openDrawingBoardPane, create = createPeerDrawingBoard }) {
  if (!peerDrawingBoardsShared()) return null;
  const access = peerDrawingBoardAccess(group.name);
  if (!access) return null;
  const drawingBoards = peerGroupDrawingBoards(group.name);
  return html`<div class="gr-resources peer-group-drawing-boards" data-peer-group-drawing-boards=${group.name}>
    <div class="gr-kind" data-resource-kind="drawing-boards">
      <span class="ui-eyebrow gr-kind-label">Drawing boards</span>
      ${drawingBoards.map((b) => html`<div key=${b.id} class="gr-row" data-peer-drawing-board=${b.id}>
        <${Button} size="sm" variant="link" class="gr-name" title=${`Open ${b.title || b.id}`} onClick=${() => open(b.id, b.title || b.id)}>
          <span class="gr-glyph" aria-hidden="true">▦</span>${b.title || b.id}<//>
        <span class="gq-dim gr-meta">drawing board${Number.isFinite(b.element_count) ? ` · ${b.element_count} element${b.element_count === 1 ? '' : 's'}` : ''}${b.updated_at ? ` · updated ${relTime(b.updated_at)}` : ''}</span>
        <span class="gr-spacer"></span>
        <${PeerDrawingBoardBadge} access=${access} />
        <${Button} size="sm" class="gr-primary" data-peer-drawing-board-open=${b.id} onClick=${() => open(b.id, b.title || b.id)}>Open<//>
      </div>`)}
      ${!drawingBoards.length && html`<div class="gp-empty ui-empty">No drawing boards in ${group.name} yet.</div>`}
      ${access === 'write' && html`<div><${Button} size="sm" class="peer-drawing-board-add" data-peer-drawing-board-new=${group.name}
        onClick=${() => void create(group.name)}>＋ Drawing board<//></div>`}
    </div>
  </div>`;
}
