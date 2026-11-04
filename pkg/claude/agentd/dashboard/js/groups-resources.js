// groups-resources.js -- a group's Resources section in Both (tcl-dwzsq0,
// option A): everything attached to the group, grouped by kind. Processes
// come first (groups-saved-processes.js, with the runs), then the boards
// linked to the group and the live pages shown into it (context
// groups[].boards / .pages, the operator's only). A kind with nothing in it
// is hidden. Every resource wears the operator's agent lock as a chip:
// '🔒 agents: read-only' or '✎ agents can edit'; clicking it toggles the lock
// (POST /api/resource-locks). Only the operator's dashboard can.

import { h } from 'preact';
import { useRef, useState } from 'preact/hooks';
import { useDismissablePopover } from './hover-popover.js';
import htm from 'htm';
import { openBoardPane } from './board-overlay-state.js';
import { freeBoardId } from './board-manage.js';
import { expectPage, openPagePane } from './page-place.js';
import { defaultNotify } from './groups-quickstart.js';
import { Button, Menu, MenuItem } from './ui-components.js';
import { LockChip } from './resource-lock.js';

export { LockChip };

const html = htm.bind(h);

function pageMeta(page, nameOf) {
  if (page.groupOwned) return 'blank · no member has taken it over';
  return `${nameOf(page.owner) || page.owner} · ${page.kind === 'static' ? 'static' : 'live'}`;
}

// GroupResources lists a group's boards and pages, each kind under its own
// heading and only when it has any.
export function GroupResources({ group, boards = [], pages = [], api, source, notify = defaultNotify, nameOf = () => '' }) {
  if (!boards.length && !pages.length) return null;
  return html`<div class="gr-resources" data-group-resources=${group.name}>
    ${boards.length > 0 && html`<div class="gr-kind" data-resource-kind="boards">
      <span class="ui-eyebrow gr-kind-label">Boards</span>
      ${boards.map((board) => html`<div key=${board.id} class="gr-row" data-resource-board=${board.id}>
        <${Button} size="sm" variant="link" class="gr-name" title=${`Open ${board.title || board.id}`}
          onClick=${() => openBoardPane(board.id, board.title || board.id)}>▦ ${board.title || board.id}<//>
        <span class="gr-spacer"></span>
        ${board.editing > 0 && html`<span class="gq-dim">${board.editing} editing</span>`}
        <${LockChip} kind="board" id=${board.id} name=${board.title || board.id} locked=${!!board.agentsLocked} api=${api} source=${source} notify=${notify} />
      </div>`)}
    </div>`}
    ${pages.length > 0 && html`<div class="gr-kind" data-resource-kind="pages">
      <span class="ui-eyebrow gr-kind-label">Pages</span>
      ${pages.map((page) => html`<div key=${page.id} class="gr-row" data-resource-page=${page.id}>
        <${Button} size="sm" variant="link" class="gr-name" title=${`Open ${page.title || page.name}`}
          onClick=${() => openPagePane(page.id, page.title || page.name)}>◧ ${page.title || page.name}<//>
        <span class="gr-spacer"></span>
        <span class="gq-dim">${pageMeta(page, nameOf)}</span>
        <${LockChip} kind="page" id=${page.id} name=${page.title || page.name} locked=${!!page.agentsLocked} api=${api} source=${source} notify=${notify} />
      </div>`)}
    </div>`}
  </div>`;
}

// AddResource is the section's one ＋▾: a process (the Get started panel) or
// a board linked to the group.
// freePageName is the first of sketch, sketch-2, … the group has no page by.
export function freePageName(pages = []) {
  const taken = new Set(pages.map((page) => page.name));
  if (!taken.has('sketch')) return 'sketch';
  for (let n = 2; ; n++) if (!taken.has(`sketch-${n}`)) return `sketch-${n}`;
}

export function AddResource({ group, pages = [], api, source, wizard = false, notify = defaultNotify, onAddProcess }) {
  const [open, setOpen] = useState(false);
  const ref = useRef(null);
  // The shared popover rule: a press outside, or Escape, closes the menu (a
  // click menu, so moving the pointer away does not).
  useDismissablePopover(ref, open, (why) => { if (why !== 'away') setOpen(false); });
  const addBoard = async () => {
    setOpen(false);
    const title = `${group.name} board`;
    try {
      // Board ids are node-wide, so the free one is chosen among them all.
      const taken = await api.boardIds();
      const board = await api.createGroupBoard(group.name, title, freeBoardId(title, taken));
      notify(`Created ${board.title || title} in ${group.name}`);
      void source?.refresh?.();
      openBoardPane(board.id, board.title || title);
    } catch (cause) {
      notify(`Could not create the board: ${cause.message}`, true);
    }
  };
  const addPage = async () => {
    setOpen(false);
    try {
      const page = await api.createGroupPage(group.name, freePageName(pages));
      notify(`Created a blank page in ${group.name}: draw on it, then a member takes it over`);
      void source?.refresh?.();
      // It opens, focused, when the snapshot lists it (page-place.js).
      expectPage(page.id);
    } catch (cause) {
      notify(`Could not create the page: ${cause.message}`, true);
    }
  };
  return html`<span class="gr-add" ref=${ref}>
    <${Button} size="sm" variant="link" class="gq-link" data-resource-add=${group.name} aria-haspopup="menu" aria-expanded=${open ? 'true' : 'false'}
      title=${`Add a resource to ${group.name}`} onClick=${() => setOpen(!open)}>＋▾<//>
    ${open && html`<${Menu} size="sm" class="gr-add-menu">
      <${MenuItem} data-resource-add-process onClick=${() => { setOpen(false); onAddProcess(); }}>${wizard ? 'Rite' : 'Process'}<//>
      <${MenuItem} data-resource-add-board onClick=${addBoard}>Board<//>
      <${MenuItem} data-resource-add-page onClick=${addPage} title="A blank page you draw on; a member takes it over with its own source">Page<//>
    <//>`}
  </span>`;
}
