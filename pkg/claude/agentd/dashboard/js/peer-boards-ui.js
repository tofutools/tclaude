// peer-boards-ui.js -- the pieces of a peer's boards on its page
// (tcl-ug8bb8, peer-boards.js has the data): the edit / read-only badge,
// the ＋ that makes a board in a group this node writes there, and a group's
// Boards in the peer's Groups tab (placement B). The Collab panel lists the
// same boards under their groups (placement A); both open the one editor.

import { h } from 'preact';
import htm from 'htm';
import { openBoardPane } from './board-overlay-state.js';
import { Button } from './ui-components.js';
import { loadPeerBoards, peerBoardAccess, peerBoards, peerBoardsShared, peerGroupBoards, peerName } from './peer-boards.js';
import { shellToast } from './shell-state.js';
import { relTime } from './helpers.js';

const html = htm.bind(h);

// PeerBoardBadge says what this node may do with a peer's board.
export function PeerBoardBadge({ access }) {
  if (!access) return null;
  const write = access === 'write';
  return html`<span class=${`ui-tag peer-board-badge ${write ? 'write' : 'read'}`} data-peer-board-access=${access}
    title=${write ? `You can edit this board on ${peerName()}` : `${peerName()} shares this board read-only`}>${write ? 'edit' : 'read-only'}</span>`;
}

// freePeerTitle is "<group> board", or the first "<group> board N" no board
// of the peer's list is called (a peer's new board takes its id from it).
export function freePeerTitle(group, boards = peerBoards.value) {
  const taken = new Set((boards || []).flatMap((b) => [b.title, b.id]).filter(Boolean).map((t) => String(t).toLowerCase()));
  const base = `${group} board`;
  if (!taken.has(base.toLowerCase()) && !taken.has(base.toLowerCase().replace(/\s+/g, '-'))) return base;
  for (let n = 2; ; n++) {
    const title = `${base} ${n}`;
    if (!taken.has(title.toLowerCase()) && !taken.has(title.toLowerCase().replace(/\s+/g, '-'))) return title;
  }
}

// createPeerBoard makes a board in group on the peer (POST /api/boards with
// its group: a peer's board always has one) and opens it.
export async function createPeerBoard(group, { fetchImpl = (...args) => globalThis.fetch(...args), notify = shellToast, open = openBoardPane } = {}) {
  const title = freePeerTitle(group);
  try {
    const response = await fetchImpl('/api/boards', {
      method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ title, group }),
    });
    const body = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(body.message || body.error || `${response.status}`);
    notify(`Created ${body.title || title} in ${group} on ${peerName()}`);
    void loadPeerBoards(fetchImpl);
    open(body.id, body.title || title);
    return body;
  } catch (cause) {
    notify(`Could not create a board on ${peerName()}: ${cause.message}`, true);
    return null;
  }
}

// PeerNewBoard is a group section's ＋ in the Collab panel, for a group this
// node writes on the peer.
export function PeerNewBoard({ group, create = createPeerBoard }) {
  if (peerBoardAccess(group) !== 'write') return null;
  const title = `New board in ${group} on ${peerName()}`;
  return html`<button data-ui-exempt type="button" class="cr-tool cr-section-add" data-peer-board-new=${group}
    title=${title} aria-label=${title} onClick=${() => void create(group)}>＋</button>`;
}

// PeerGroupBoards is a group's Boards on the peer's Groups tab: each board
// with its badge, opened as a tab here; a group this node writes adds a
// board. Nothing for a group it cannot read, or off a peer's page.
export function PeerGroupBoards({ group, open = openBoardPane, create = createPeerBoard }) {
  if (!peerBoardsShared()) return null;
  const access = peerBoardAccess(group.name);
  if (!access) return null;
  const boards = peerGroupBoards(group.name);
  return html`<div class="gr-resources peer-group-boards" data-peer-group-boards=${group.name}>
    <div class="gr-kind" data-resource-kind="boards">
      <span class="ui-eyebrow gr-kind-label">Boards</span>
      ${boards.map((b) => html`<div key=${b.id} class="gr-row" data-peer-board=${b.id}>
        <${Button} size="sm" variant="link" class="gr-name" title=${`Open ${b.title || b.id}`} onClick=${() => open(b.id, b.title || b.id)}>
          <span class="gr-glyph" aria-hidden="true">▦</span>${b.title || b.id}<//>
        <span class="gq-dim gr-meta">board${Number.isFinite(b.element_count) ? ` · ${b.element_count} element${b.element_count === 1 ? '' : 's'}` : ''}${b.updated_at ? ` · updated ${relTime(b.updated_at)}` : ''}</span>
        <span class="gr-spacer"></span>
        <${PeerBoardBadge} access=${access} />
        <${Button} size="sm" class="gr-primary" data-peer-board-open=${b.id} onClick=${() => open(b.id, b.title || b.id)}>Open<//>
      </div>`)}
      ${!boards.length && html`<div class="gp-empty ui-empty">No boards in ${group.name} yet.</div>`}
      ${access === 'write' && html`<div><${Button} size="sm" class="peer-board-add" data-peer-board-new=${group.name}
        onClick=${() => void create(group.name)}>＋ Board<//></div>`}
    </div>
  </div>`;
}
