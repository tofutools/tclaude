// groups-resources.js -- a group's Resources section in Both (tcl-dwzsq0,
// option A): everything attached to the group, grouped by kind. Processes
// come first (groups-saved-processes.js, with the runs), then the boards
// linked to the group and the live pages shown into it (context
// groups[].boards / .pages, the operator's only). A kind with nothing in it
// is hidden. Every resource wears the operator's agent lock as a chip:
// '🔒 agents: read-only' or '✎ agents can edit'; clicking it toggles the lock
// (POST /api/resource-locks). Only the operator's dashboard can.

import { h } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import { useDismissablePopover } from './hover-popover.js';
import htm from 'htm';
import { openBoardPane } from './board-overlay-state.js';
import { freeBoardId } from './board-manage.js';
import { expectPage, openPagePane } from './page-place.js';
import { defaultNotify } from './groups-quickstart.js';
import { Button, Menu, MenuItem, MenuSep, SortTh } from './ui-components.js';
import { applySortState, tableSortState } from './sort.js';
import { LockChip } from './resource-lock.js';
import { moveTargets } from './resource-moves.js';
import { nodeFetch } from './node-api.js';
import { shellConfirm } from './shell-state.js';
import { effect } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';
import { revealResourceOverlay } from './resource-places.js';
import { ResourcePill, resourceView } from './resource-pill.js';
import { openGroupWebTermPane, openShellResource } from './terminals-tab.js';
import { killShell } from './background-terminals.js';
import { EyeIcon, TrashIcon } from './row-icons.js';
import { registerResourceRemover } from './resource-dnd.js';

export { LockChip };

const html = htm.bind(h);

// pageDetail is a page's Detail cell: the Kind and State columns already
// say page, and live or closed.
function pageDetail(page, nameOf) {
  if (page.state === 'closed') return page.closedReason || '';
  if (page.groupOwned) return 'blank · no member has taken it over';
  return `${page.kind === 'static' ? 'static' : 'live'} · ${nameOf(page.owner) || page.owner}`;
}

// KIND_WORD is a resource's Kind cell. A board is a drawing board: the group
// task board is another thing.
export const KIND_WORD = Object.freeze({ board: 'drawing board', page: 'page', shell: 'shell', process: 'process' });

// RESOURCE_COLS are ResourceTable's columns, in the agents table's order
// where they share one: the controls, Name, State, then what only a
// resource has. A col is sortable (sort table "resources", sort.js), as an
// agent column is.
export const RESOURCE_COLS = Object.freeze([
  { key: 'ctl', label: '' },
  { key: 'name', label: 'Name', col: 'name' },
  { key: 'state', label: 'State', col: 'state' },
  { key: 'kind', label: 'Kind', col: 'kind' },
  { key: 'lock', label: 'Lock', col: 'lock' },
  { key: 'detail', label: 'Detail', col: 'detail' },
]);

// RESOURCE_SORT is the sort table the Resources headers share: the processes'
// table and the other resources' table sort alike.
export const RESOURCE_SORT = 'resources';

const RESOURCE_ACCESSORS = Object.freeze({
  name: (r) => r.name, state: (r) => r.state, kind: (r) => r.kind, lock: (r) => (r.lock == null ? null : r.lock ? 'locked' : 'open'), detail: (r) => r.detail,
});

// sortResourceRows orders rows ({ name, state, kind, lock, detail, … }) by
// the Resources headers' sort, or keeps them as they are with none.
export function sortResourceRows(rows) {
  return applySortState(rows, RESOURCE_ACCESSORS, tableSortState(RESOURCE_SORT, RESOURCE_COLS));
}

// agentColumns finds, for a Resources table, where the agents table above it
// in the same group starts its Name and State columns, as widths for this
// table's controls and Name columns; null when there is no agents table to
// line up with (folded, or none).
export function agentColumns(table) {
  const section = table?.closest?.('[data-fused-section]');
  const agents = section?.parentElement?.querySelector?.(':scope > [data-fused-section="agents"] table');
  const head = agents?.querySelector('thead');
  if (!head) return null;
  const name = head.querySelector('th[data-sort-col="title"]');
  const state = head.querySelector('th[data-sort-col="state"]');
  const left = table.getBoundingClientRect().left;
  const nameLeft = name?.getBoundingClientRect().left;
  if (!name || !(nameLeft > left)) return null;
  const ctl = Math.round(nameLeft - left);
  const nameWidth = state ? Math.round(state.getBoundingClientRect().left - nameLeft) : 0;
  return { ctl, name: nameWidth > 60 ? nameWidth : 0 };
}

// useAgentAlignment keeps table's controls and Name columns as wide as the
// agents table's above it, so Name and State line up across the two: on
// every render and whenever the agents table changes size.
function useAgentAlignment(ref) {
  const apply = () => {
    const table = ref.current;
    if (!table) return;
    const cols = agentColumns(table);
    // Not narrower than the row's own controls (light, 👁 🗑 ⚙).
    table.style.setProperty('--gr-ctl-w', cols?.ctl >= 112 ? `${cols.ctl}px` : '');
    table.style.setProperty('--gr-name-w', cols?.name ? `${cols.name}px` : '');
  };
  useLayoutEffect(apply);
  useEffect(() => {
    const table = ref.current;
    const agents = table?.closest?.('[data-fused-section]')?.parentElement?.querySelector?.(':scope > [data-fused-section="agents"]');
    if (!agents || typeof ResizeObserver !== 'function') return undefined;
    const observer = new ResizeObserver(() => apply());
    observer.observe(agents);
    return () => observer.disconnect();
  }, []);
}

// ResourceTable is a group's resources as a table like the agents table:
// RESOURCE_COLS, with fixed widths lined up with the agents table above
// (useAgentAlignment), so the processes' table above the runs and the other
// resources' table below them line up with it and with each other. head
// shows the header row (only on the first of the two); its headers sort.
export function ResourceTable({ head = true, attrs = {}, children }) {
  const ref = useRef(null);
  useAgentAlignment(ref);
  const active = tableSortState(RESOURCE_SORT, RESOURCE_COLS);
  return html`<table class="gr-rtable" ref=${ref} ...${attrs}>
    <colgroup>${RESOURCE_COLS.map((c) => html`<col key=${c.key} class=${`gr-col-${c.key}`} />`)}</colgroup>
    ${head && html`<thead><tr>${RESOURCE_COLS.map((c) => (c.col
    ? html`<${SortTh} key=${c.key} active=${active?.col === c.col} dir=${active?.dir} data-sort-table=${RESOURCE_SORT} data-sort-col=${c.col} sortLabel=${c.label}>${c.label}<//>`
    : html`<th key=${c.key}></th>`))}</tr></thead>`}
    <tbody>${children}</tbody>
  </table>`;
}

// --- one state per resource (tcl-k6vici) ---

// The primary action follows the state: what is open here comes forward
// (Focus, or Restore from the dock), what runs out of sight opens (a shell
// attaches), and what is closed opens (a page reopens, a shell starts).
const PRIMARY = Object.freeze({
  'open-tab': 'Focus', floating: 'Focus', minimized: 'Restore', running: 'Open', closed: 'Open',
});

function primaryLabel(kind, view) {
  if (view === 'background') return kind === 'shell' ? 'Attach' : 'Open';
  if (view === 'closed' && kind === 'page') return 'Reopen';
  return PRIMARY[view] || 'Open';
}

// RowMenu is a row's ⋯: what is rarely wanted, and what cannot be undone
// behind a confirm. An item with children opens them in its place (Move to
// group lists the groups), with a way back.
export function RowMenu({ label, items, glyph = '⋯', title = '', buttonClass = 'gr-more', keepSlot = false }) {
  const [open, setOpen] = useState(false);
  const [sub, setSub] = useState(null);
  const ref = useRef(null);
  const close = () => { setOpen(false); setSub(null); };
  useDismissablePopover(ref, open, (why) => { if (why !== 'away') close(); });
  if (!items.length) {
    return keepSlot ? html`<${Button} size="sm" icon=${true} class=${buttonClass} disabled aria-label=${`Nothing more for ${label}`}>${glyph}<//>` : null;
  }
  const shown = sub ? [{ label: '‹ Back', attrs: { 'data-menu-back': '' }, back: true }, ...sub.children] : items;
  const pick = (item) => {
    if (item.back) return setSub(null);
    if (item.children) return setSub(item);
    close();
    void item.run();
  };
  return html`<span class="gr-add" ref=${ref}>
    <${Button} size="sm" variant=${keepSlot ? undefined : 'link'} icon=${keepSlot} class=${keepSlot ? buttonClass : `gq-link ${buttonClass}`} aria-haspopup="menu" aria-expanded=${open ? 'true' : 'false'}
      title=${title || `More for ${label}`} aria-label=${title || `More for ${label}`} onClick=${() => (open ? close() : setOpen(true))}>${glyph}<//>
    ${open && html`<${Menu} size="sm" class="gr-add-menu">
      ${shown.map((item) => html`<${MenuItem} key=${item.label} danger=${item.danger} ...${item.attrs || {}}
        onClick=${() => pick(item)}>${item.children ? `${item.label} ›` : item.label}<//>`)}
    <//>`}
  </span>`;
}

// RenameField edits a row's name in place: Enter saves, Escape or leaving
// it keeps the name.
export function RenameField({ value, label, onSave, onDone }) {
  const ref = useRef(null);
  useEffect(() => { ref.current?.focus(); ref.current?.select?.(); }, []);
  const key = (event) => {
    if (event.key === 'Escape') { event.preventDefault(); onDone(); }
    if (event.key !== 'Enter') return;
    event.preventDefault();
    const next = event.currentTarget.value.trim();
    onDone();
    if (next && next !== value) void onSave(next);
  };
  return html`<input ref=${ref} class="gr-rename" data-resource-rename-input="" aria-label=${`New name for ${label}`}
    value=${value} onKeyDown=${key} onBlur=${onDone} />`;
}

// rowAction runs a row's change and says how it went; what names it for the
// failure, done(arg) words the success.
const rowAction = (label, source, notify) => (what, call, done) => async (arg) => {
  try {
    await call(arg);
    notify(done(arg));
    void source?.refresh?.();
  } catch (cause) {
    notify(`Could not ${what} ${label}: ${cause.message}`, true);
  }
};

// renameAndMove are a row's Rename… and Move to group items.
function renameAndMove({ startRename, groups, move, attrs }) {
  const items = [{ label: 'Rename…', attrs: { 'data-resource-rename': attrs }, run: startRename }];
  if (groups.length) {
    items.push({
      label: 'Move to group', attrs: { 'data-resource-move': attrs },
      children: groups.map((name) => ({ label: name, attrs: { 'data-move-to': name }, run: () => move(name) })),
    });
  }
  return items;
}

// resourceRemover deletes a board or a page behind a confirm: its row's 🗑,
// a drop on the trash bin, and the menus' Delete….
export function resourceRemover(resource, { api, source, notify, confirm }) {
  const { kind, id, label } = resource;
  const page = kind === 'page';
  return async () => {
    const ok = await confirm(page
      ? {
        title: `Delete page ${label}?`,
        body: resource.closed
          ? 'The page is removed from the group and cannot be reopened.'
          : 'The page closes for everyone viewing it and is removed from the group; it cannot be reopened.',
        okLabel: 'Delete',
      }
      : { title: `Delete board ${label}?`, body: 'The board and everything drawn on it are deleted for everyone.', okLabel: 'Delete' });
    if (!ok) return;
    try {
      await (page ? api.deleteGroupPage(id) : api.deleteBoard(id));
      notify(`Deleted ${label}`);
      void source?.refresh?.();
    } catch (cause) {
      notify(`Could not delete ${label}: ${cause.message}`, true);
    }
  };
}

// resourceMenu is a board's or page's edit items, the ⋯ of its Groups row
// and the ⚙ of its Collab panel row (tcl-usrm7h): Rename…, Move to group ›
// (resource-moves.js' targets) and Delete…. resource is { kind, id, label,
// closed, owner } with owner the page's agent ('' for a group's own page);
// groupId is the group it is listed in (0 for none); startRename shows the
// row's name field.
export function resourceMenu(resource, { groupId = 0, snapshotValue, api, source, notify, confirm, startRename }) {
  const { kind, id, label } = resource;
  const act = rowAction(label, source, notify);
  const page = kind === 'page';
  const move = act('move', (name) => (page ? api.moveGroupPage(id, name) : api.moveBoard(id, name)), (name) => `Moved ${label} to ${name}`);
  // An agent's page moves to a group its agent is in; a group's own blank
  // page only while closed.
  const groups = !page ? moveTargets(snapshotValue, groupId)
    : !resource.owner ? (resource.closed ? moveTargets(snapshotValue, groupId) : [])
      : moveTargets(snapshotValue, groupId, resource.owner);
  const remove = resourceRemover(resource, { api, source, notify, confirm });
  return [...renameAndMove({ startRename, groups, move, attrs: id }),
    { label: 'Delete…', danger: true, attrs: { [page ? 'data-page-delete' : 'data-board-delete']: id }, run: remove }];
}

// renameResource sets a board's or page's title, as its Rename… field saves it.
export function renameResource(resource, { api, source, notify }) {
  const call = resource.kind === 'page' ? (title) => api.renameGroupPage(resource.id, title) : (title) => api.renameBoard(resource.id, title);
  return rowAction(resource.label, source, notify)('rename', call, (title) => `Renamed ${resource.label} to ${title}`);
}

// ResourceRow is one resource, a row of ResourceTable: a light for its
// state, then 👁 (open, focus or attach it), 🗑 (delete it, or kill a shell)
// and ⚙ (its other actions), the same buttons in the same places on every
// row, then its name, kind, state pill, agent lock and detail. remove is
// { title, attrs, run } or null (the 🗑 stays, disabled, so the buttons line
// up); a row that has one is also deleted by a drop on the trash bin
// (resource-dnd.js).
function ResourceRow({ attrs, kind, closed, glyph, label, meta, view, primary, remove = null, menu, lock, renaming = null, drag = null }) {
  const dragAttrs = drag && !renaming ? { draggable: 'true', 'data-dnd-resource': JSON.stringify({ kind, label, closed: !!closed, ...drag }) } : {};
  useEffect(() => (remove && drag ? registerResourceRemover(`${kind}:${drag.id}`, remove.run) : undefined), [kind, drag?.id, remove?.run]);
  return html`<tr class=${closed ? 'gr-trow gr-closed' : 'gr-trow'} data-resource-kind=${`${kind}s`} ...${attrs} ...${dragAttrs}>
    <td class="gr-ctl"><span class="agent-ctl gr-acts">
      <span class=${`gr-light gr-light-${view}`} data-resource-light=${view} title=${view} aria-hidden="true">●</span>
      <span class="row-actions">
        <${Button} size="sm" icon=${true} class="icon-btn gr-primary" data-resource-primary=${primary.label}
          title=${`${primary.label} ${label}`} aria-label=${`${primary.label} ${label}`} onClick=${primary.run}><${EyeIcon} /><//>
        <${Button} size="sm" icon=${true} class="icon-btn warn gr-remove" disabled=${!remove} title=${remove?.title || `${label} cannot be deleted here`}
          aria-label=${remove?.title || `Nothing to delete for ${label}`} ...${remove?.attrs || {}} onClick=${() => remove?.run()}><${TrashIcon} /><//>
        <${RowMenu} label=${label} items=${menu} glyph=${html`<span class="cog-glyph">⚙︎</span>`} buttonClass="cog-btn gr-cog" keepSlot=${true}
          title=${`Options for ${label}`} />
      </span>
    </span></td>
    <td class="name-cell gr-namecell">${renaming ? html`<span class="gr-glyph" aria-hidden="true">${glyph}</span><${RenameField} ...${renaming} label=${label} />`
      : html`<${Button} size="sm" variant="link" class="gr-name" title=${`${primary.label} ${label}`}
      onClick=${primary.run}><span class="gr-glyph" aria-hidden="true">${glyph}</span>${label}<//>`}</td>
    <td><${ResourcePill} view=${view} kind=${kind} /></td>
    <td class="gq-dim gr-kindcell">${KIND_WORD[kind] || kind}</td>
    <td class="gr-lockcell">${lock || html`<span class="gq-dim">—</span>`}</td>
    <td class="gq-dim gr-meta" title=${meta}>${meta}</td>
  </tr>`;
}

// reopenPage reopens a closed page saved with the group (POST
// /api/pages/{id}/reopen) and opens it once the snapshot lists it.
async function reopenPage(page, api, source, notify) {
  try {
    await api.reopenGroupPage(page.id);
    notify(`Reopened ${page.title || page.name}`);
    void source?.refresh?.();
    expectPage(page.id);
  } catch (cause) {
    notify(`Could not reopen ${page.title || page.name}: ${cause.message}`, true);
  }
}

// focusOr brings a resource's overlay forward, or runs open when it has none.
const focusOr = (key, open) => () => { if (!revealResourceOverlay(key)) void open(); };

function PageRow({ page, api, source, notify, confirm, nameOf, snapshotValue, groupId, groupName, draggable = true }) {
  const label = page.title || page.name;
  const [renaming, setRenaming] = useState(false);
  const view = resourceView(snapshotValue, groupId, 'page', page.id, page.state === 'closed' ? 'closed' : 'running');
  const run = view === 'closed' ? () => reopenPage(page, api, source, notify) : focusOr(`page:${page.id}`, () => openPagePane(page.id, label));
  const reload = async () => {
    try {
      await api.reloadGroupPage(page.id);
    } catch (cause) {
      notify(`Could not reload ${label}: ${cause.message}`, true);
    }
  };
  const resource = { kind: 'page', id: page.id, label, closed: view === 'closed', owner: page.groupOwned ? '' : page.owner || '' };
  const ctx = { groupId, snapshotValue, api, source, notify, confirm, startRename: () => setRenaming(true) };
  return html`<${ResourceRow} kind="page" attrs=${{ 'data-resource-page': page.id, 'data-page-state': view === 'closed' ? 'closed' : 'open' }}
    drag=${draggable && { id: page.id, group: groupName, owner: page.groupOwned ? '' : page.owner || '' }}
    closed=${view === 'closed'} glyph="◧" label=${label} meta=${pageDetail(page, nameOf)} view=${view}
    primary=${{ label: primaryLabel('page', view), run }}
    remove=${{ title: `Delete page ${label}`, attrs: { 'data-page-trash': page.id }, run: resourceRemover(resource, ctx) }}
    renaming=${renaming && { value: label, onSave: renameResource(resource, ctx), onDone: () => setRenaming(false) }}
    menu=${[...(view !== 'closed' ? [{ label: 'Reload in every view', attrs: { 'data-page-reload': page.id }, run: reload }] : []), ...resourceMenu(resource, ctx)]}
    lock=${html`<${LockChip} kind="page" id=${page.id} name=${label} locked=${!!page.agentsLocked} api=${api} source=${source} notify=${notify} />`} />`;
}

function BoardRow({ board, api, source, notify, confirm, snapshotValue, groupId, groupName, draggable = true }) {
  const label = board.title || board.id;
  const [renaming, setRenaming] = useState(false);
  const view = resourceView(snapshotValue, groupId, 'board', board.id);
  const resource = { kind: 'board', id: board.id, label };
  const ctx = { groupId, snapshotValue, api, source, notify, confirm, startRename: () => setRenaming(true) };
  return html`<${ResourceRow} kind="board" attrs=${{ 'data-resource-board': board.id }} glyph="▦" label=${label}
    drag=${draggable && { id: board.id, group: groupName }}
    meta=${board.editing > 0 ? `${board.editing} editing` : ''} view=${view}
    primary=${{ label: primaryLabel('board', view), run: focusOr(`board:${board.id}`, () => openBoardPane(board.id, label)) }}
    remove=${{ title: `Delete board ${label}`, attrs: { 'data-board-trash': board.id }, run: resourceRemover(resource, ctx) }}
    renaming=${renaming && { value: label, onSave: renameResource(resource, ctx), onDone: () => setRenaming(false) }}
    menu=${resourceMenu(resource, ctx)}
    lock=${html`<${LockChip} kind="board" id=${board.id} name=${label} locked=${!!board.agentsLocked} api=${api} source=${source} notify=${notify} />`} />`;
}

function ShellRow({ group, shell, notify, confirm, snapshotValue, draggable = true }) {
  const view = resourceView(snapshotValue, group.id, 'shell', shell.id, shell.state);
  // A group's own shell is named for its group; an agent's for its agent.
  const agentShell = !!shell.agent;
  const label = agentShell ? shell.title : `${group.name} shell`;
  const open = focusOr(`shell:${shell.id}`, () => openShellResource(shell, label));
  const kill = async () => {
    const ok = await confirm({ title: `Kill ${label}?`, body: 'Whatever runs in the shell stops. Opening it again starts a fresh one.', okLabel: 'Kill' });
    if (!ok) return;
    const error = await killShell(shell.session);
    notify(error ? `Could not kill ${label}: ${error}` : `Killed ${label}`, !!error);
  };
  const killable = view !== 'closed' && !!shell.session;
  return html`<${ResourceRow} kind="shell" attrs=${{ 'data-resource-shell': agentShell ? shell.id : group.name }} closed=${view === 'closed'} glyph="▭"
    label=${agentShell ? shell.title : 'Group shell'} drag=${draggable && killable && { id: shell.id, group: group.name }}
    meta=${shell.detail || ''} view=${view}
    primary=${{ label: primaryLabel('shell', view), run: open }}
    remove=${killable && { title: `Kill ${label}`, attrs: { 'data-shell-kill': agentShell ? shell.id : group.name }, run: kill }}
    menu=${[{ label: 'Kill…', danger: true, attrs: { 'data-shell-kill-menu': shell.id }, run: kill }].filter(() => killable)} lock=${null} />`;
}

// groupShellsOf is the group's shells in the snapshot's group_resources: its
// own, then its agents' (the snapshot's order).
export function groupShellsOf(snapshotValue, group) {
  const shells = (snapshotValue?.group_resources || []).filter((r) => r.kind === 'shell' && r.group_id === group.id);
  return [...shells.filter((r) => !r.agent), ...shells.filter((r) => r.agent)];
}

// GroupResources lists a group's drawing boards, pages and shells in one
// table (ResourceTable). head is false when the group's saved processes'
// table above already shows the header row.
export function GroupResources({ group, boards = [], pages = [], api, source, notify = defaultNotify, nameOf = () => '', confirm = shellConfirm,
  snapshot = dashboardState.snapshot, head = true }) {
  const snapshotValue = snapshot?.value;
  const shells = groupShellsOf(snapshotValue, group);
  if (!boards.length && !pages.length && !shells.length) return null;
  // A row drags onto this node's group boxes (resource-dnd.js), which the
  // fused fleet view does not have.
  const props = { api, source, notify, confirm, snapshotValue, groupId: group.id, groupName: group.id > 0 ? group.name : '', draggable: !group.fleet_node };
  // One list across kinds, so a header sorts the whole table; unsorted it
  // keeps drawing boards, then pages, then shells.
  const rows = sortResourceRows([
    ...boards.map((board) => ({ key: `board:${board.id}`, name: board.title || board.id, state: resourceView(snapshotValue, group.id, 'board', board.id), kind: KIND_WORD.board,
      lock: !!board.agentsLocked, detail: board.editing > 0 ? `${board.editing} editing` : '',
      render: () => html`<${BoardRow} key=${`board:${board.id}`} board=${board} ...${props} />` })),
    ...pages.map((page) => ({ key: `page:${page.id}`, name: page.title || page.name, state: resourceView(snapshotValue, group.id, 'page', page.id, page.state === 'closed' ? 'closed' : 'running'),
      kind: KIND_WORD.page, lock: !!page.agentsLocked, detail: pageDetail(page, nameOf),
      render: () => html`<${PageRow} key=${`page:${page.id}`} page=${page} nameOf=${nameOf} ...${props} />` })),
    ...shells.map((shell) => ({ key: `shell:${shell.id}`, name: shell.agent ? shell.title : 'Group shell', state: resourceView(snapshotValue, group.id, 'shell', shell.id, shell.state),
      kind: KIND_WORD.shell, lock: null, detail: shell.detail || '',
      render: () => html`<${ShellRow} key=${`shell:${shell.id}`} group=${group} shell=${shell} ...${props} />` })),
  ]);
  return html`<div class="gr-resources" data-group-resources=${group.name}>
    <${ResourceTable} head=${head}>${rows.map((row) => row.render())}<//>
  </div>`;
}

// freePageName is the first of sketch, sketch-2, … the group has no page by.
export function freePageName(pages = []) {
  const taken = new Set(pages.map((page) => page.name));
  if (!taken.has('sketch')) return 'sketch';
  for (let n = 2; ; n++) if (!taken.has(`sketch-${n}`)) return `sketch-${n}`;
}

// addGroupBoard creates a board linked to group, named for it, and opens it
// as a tab. The Groups tab's ＋ and the Collab panel's ＋ New both make one.
export async function addGroupBoard(group, { api, notify = defaultNotify, source } = {}) {
  const title = `${group.name} board`;
  try {
    // Board ids are node-wide, so the free one is chosen among them all.
    const taken = await api.boardIds();
    const board = await api.createGroupBoard(group.name, title, freeBoardId(title, taken));
    notify(`Created ${board.title || title} in ${group.name}`);
    void source?.refresh?.();
    openBoardPane(board.id, board.title || title);
    return board;
  } catch (cause) {
    notify(`Could not create the board: ${cause.message}`, true);
    return null;
  }
}

// addGroupPage creates a blank page group owns; it opens, focused, when the
// snapshot lists it (page-place.js).
export async function addGroupPage(group, pages = [], { api, notify = defaultNotify, source } = {}) {
  try {
    const page = await api.createGroupPage(group.name, freePageName(pages));
    notify(`Created a blank page in ${group.name}: draw on it, then a member takes it over`);
    void source?.refresh?.();
    expectPage(page.id);
    return page;
  } catch (cause) {
    notify(`Could not create the page: ${cause.message}`, true);
    return null;
  }
}

// addGroupShell opens group's shell, or brings forward the overlay holding it.
export function addGroupShell(group) {
  if (!revealResourceOverlay(`shell:${group.name}`)) void openGroupWebTermPane(group.name, `${group.name} shell`);
}

// AddResource is the section's one ＋ button (icon-only): a process for the whole
// group or for one agent (the start form, which also builds a new one), a
// board linked to the group, a blank live page, or the group's shell, each
// with what it is in one line. `processesOnly` keeps just the two process
// scopes, for the Processes perspective, which lists no boards, pages or
// shells. `open`/`onOpen` let the caller hold the menu's state, so it stays
// open when a click on a folded section line moves the button to the open
// section's title row (a new instance).
export function AddResource({ group, pages = [], api, source, wizard = false, notify = defaultNotify, onAddProcess, processesOnly = false, open: heldOpen, onOpen }) {
  const [ownOpen, setOwnOpen] = useState(false);
  const held = typeof onOpen === 'function';
  const open = held ? !!heldOpen : ownOpen;
  const setOpen = held ? onOpen : setOwnOpen;
  const ref = useRef(null);
  const trigger = useRef(null);
  // Mounted already open is the move from the folded line: the pressed
  // button is gone, so focus follows to this one (keyboard users keep their
  // place), unless the operator has focused something else meanwhile.
  useEffect(() => {
    const active = typeof document !== 'undefined' ? document.activeElement : null;
    if (open && (!active || active === document.body)) trigger.current?.focus();
  }, []);
  // The shared popover rule: a press outside, or Escape, closes the menu (a
  // click menu, so moving the pointer away does not).
  useDismissablePopover(ref, open, (why) => { if (why !== 'away') setOpen(false); });
  const addBoard = () => { setOpen(false); void addGroupBoard(group, { api, notify, source }); };
  const addPage = () => { setOpen(false); void addGroupPage(group, pages, { api, notify, source }); };
  const cwd = (group.default_cwd || '').trim();
  const addShell = () => { setOpen(false); addGroupShell(group); };
  const item = (attr, onClick, name, line, disabled = false) => html`<${MenuItem} ...${{ [attr]: true }} class="gr-new-item" onClick=${onClick}
    disabled=${disabled}><span class="gr-new-name">${name}</span><span class="gr-new-line">${line}</span><//>`;
  const process = (scope) => () => { setOpen(false); onAddProcess(scope); };
  // Icon-only, like the Agents section's spawn buttons beside it, so the
  // name lives in the title and aria-label.
  const addTitle = processesOnly
    ? `Start a process in ${group.name}: for the group or for one agent`
    : `Add a resource to ${group.name}: a process, board, live page or shell`;
  return html`<span class="gr-add" ref=${ref}>
    <${Button} size="sm" variant="primary" class="spawn-btn gr-add-btn" domRef=${trigger} data-resource-add=${group.name} aria-haspopup="menu" aria-expanded=${open ? 'true' : 'false'}
      title=${addTitle} aria-label=${addTitle} onClick=${() => setOpen(!open)}><span class="gr-add-plus" aria-hidden="true">＋</span><span class="gr-add-caret" aria-hidden="true">▾</span><//>
    ${open && html`<${Menu} size="sm" class="gr-add-menu gr-new-menu">
      ${item('data-resource-add-process', process('group'), wizard ? 'Rite for the party' : 'Process for the group', wizard ? 'Begin a written rite, or shape a new one' : 'Run a saved process, or build a new one')}
      ${item('data-resource-add-process-agent', process('agent'), wizard ? 'Rite for one hero' : 'Process for one agent', wizard ? 'Pick a hero; its steps are that hero\'s labour' : 'Pick a member; its steps run as that agent')}
      ${!processesOnly && html`<${MenuSep} />
      ${item('data-resource-add-board', addBoard, 'Board', "A drawing board shared with the group's agents")}
      ${item('data-resource-add-page', addPage, 'Live page', 'A blank page you draw on; a member takes it over with its own source')}
      ${item('data-resource-add-shell', addShell, 'Shell', cwd ? `A terminal in ${cwd}` : 'Set the group\'s directory first', !cwd)}`}
    <//>`}
  </span>`;
}

// --- a peer's group (tcl-i9elme) ---

// peerGroupName is a fused-view group's name on its own node (name@node
// there is name).
export function peerGroupName(group) {
  const at = `@${group.fleet_node?.name || ''}`;
  return group.fleet_node && group.name.endsWith(at) ? group.name.slice(0, -at.length) : group.name;
}

// loadPeerResources reads a peer's group resources through the peer view
// (GET /api/groups/{name}/resources): its processes, boards and pages with
// their locks, and whether that node lets this one change them (canLock).
export async function loadPeerResources(name, fetchImpl) {
  const response = await fetchImpl(`/api/groups/${encodeURIComponent(name)}/resources`, { credentials: 'same-origin' });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) {
    const error = new Error(body.message || body.error || `${response.status}`);
    error.status = response.status;
    throw error;
  }
  return body;
}

// A template's lock reaches every group it is pinned to, so each says for
// itself whether the node lets this one change it; a node from before that
// answers for the group only.
export function peerRows(resources) {
  const groupAllows = !!resources.canLock;
  return [
    ['processes', 'Processes', '⚙', 'process', (resources.processes || []).map((p) => ({ id: p.templateId, name: p.name || p.templateId, locked: !!p.agentsLocked,
      allowed: typeof p.canLock === 'boolean' ? p.canLock : groupAllows }))],
    ['boards', 'Boards', '▦', 'board', (resources.boards || []).map((b) => ({ id: b.id, name: b.title || b.id, locked: !!b.agentsLocked }))],
    ['pages', 'Pages', '◧', 'page', (resources.pages || []).map((p) => ({ id: p.id, name: p.title || p.name, locked: !!p.agentsLocked }))],
  ].filter(([, , , , rows]) => rows.length > 0);
}

// peerResourcesPollMs is how often, at most, a peer's group card reads its
// resources again, so a lock changed on that node (or a new board) shows
// here too. It rides the dashboard's one snapshot poll (snapshot-poll.js):
// each accepted snapshot (dashboardState.lastRefreshAt) is a tick, and a
// card reads again on the first tick after it is due.
export const peerResourcesPollMs = 20000;

// A node's group cards read it one at a time: the peer view holds a few
// requests per peer in flight (federation_peer_view.go), and a node showing
// many groups must not crowd out the operator's own clicks there.
const peerReadQueues = new Map();

export function queuePeerRead(node, read) {
  const key = node || '';
  const run = (peerReadQueues.get(key) || Promise.resolve()).then(read, read);
  const tail = run.catch(() => {});
  peerReadQueues.set(key, tail);
  void tail.then(() => { if (peerReadQueues.get(key) === tail) peerReadQueues.delete(key); });
  return run;
}

const tabHidden = () => typeof document !== 'undefined' && document.visibilityState === 'hidden';

// PeerGroupResources is a peer's group's Resources, read from that node. Its
// boards and pages are that node's own, so they are named rather than opened;
// each wears its lock chip, which toggles only where the node grants this one
// groups.resources.lock, and otherwise says so. A refusal is a toast. A node
// that does not grant this one groups.resources.read (403), or has no such
// read (404), shows no section; any other failure says so.
export function PeerGroupResources({ group, notify = defaultNotify, fetchImpl = null, pollMs = peerResourcesPollMs, tick = dashboardState.lastRefreshAt }) {
  const node = group.fleet_node;
  const name = peerGroupName(group);
  const [state, setState] = useState({ resources: null, error: null });
  const reach = fetchImpl || nodeFetch(node?.id);
  const current = useRef(0);
  const refresh = async () => {
    const ticket = ++current.current;
    try {
      const resources = await queuePeerRead(node?.id, () => loadPeerResources(name, reach));
      if (ticket === current.current) setState({ resources, error: null });
    } catch (error) {
      if (ticket === current.current) setState({ resources: null, error });
    }
  };
  useEffect(() => {
    void refresh();
    // Cards fall due at staggered times, and a hidden tab skips the tick.
    let due = Date.now() + pollMs + Math.floor(Math.random() * pollMs);
    const stop = pollMs > 0 && tick ? effect(() => {
      const at = tick.value;
      if (at == null || at < due || tabHidden()) return;
      due = at + pollMs;
      void refresh();
    }) : null;
    // A late answer for a previous node or group is dropped.
    return () => { current.current++; stop?.(); };
  }, [node?.id, name, pollMs, tick]);
  const { resources, error } = state;
  if (!resources) {
    if (!error || error.status === 403 || error.status === 404) return null;
    return html`<div class="gp-empty ui-empty" data-peer-resources-error=${node?.id || ''}>Could not read the resources of ${name} on ${node?.name || 'its node'}: ${error.message}</div>`;
  }
  const kinds = peerRows(resources);
  if (!kinds.length) return null;
  const peer = { fetchImpl: reach, allowed: !!resources.canLock, node: node?.name };
  const peerFor = (row) => (row.allowed === undefined || row.allowed === peer.allowed ? peer : { ...peer, allowed: row.allowed });
  return html`<div class="gr-resources" data-group-resources=${group.name} data-peer-resources=${node?.id || ''}>
    ${kinds.map(([key, label, glyph, kind, rows]) => html`<div key=${key} class="gr-kind" data-resource-kind=${key}>
      <span class="ui-eyebrow gr-kind-label">${label}</span>
      ${rows.map((row) => html`<div key=${row.id} class="gr-row" data-peer-resource=${`${kind}:${row.id}`}>
        <span class="gr-name-text" title=${row.name}><span class="gr-glyph" aria-hidden="true">${glyph}</span>${row.name}</span>
        <span class="gr-spacer"></span>
        <${LockChip} kind=${kind} id=${row.id} name=${row.name} locked=${row.locked} peer=${peerFor(row)} notify=${notify}
          source=${{ refresh }} />
      </div>`)}
    </div>`)}
  </div>`;
}
