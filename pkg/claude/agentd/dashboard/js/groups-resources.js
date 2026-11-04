// groups-resources.js -- a group's Resources section in Both (tcl-dwzsq0,
// option A): everything attached to the group, grouped by kind. Processes
// come first (groups-saved-processes.js, with the runs), then the boards
// linked to the group and the live pages shown into it (context
// groups[].boards / .pages, the operator's only). A kind with nothing in it
// is hidden. Every resource wears the operator's agent lock as a chip:
// '🔒 agents: read-only' or '✎ agents can edit'; clicking it toggles the lock
// (POST /api/resource-locks). Only the operator's dashboard can.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import { useDismissablePopover } from './hover-popover.js';
import htm from 'htm';
import { openBoardPane } from './board-overlay-state.js';
import { freeBoardId } from './board-manage.js';
import { expectPage, openPagePane } from './page-place.js';
import { defaultNotify } from './groups-quickstart.js';
import { Button, Menu, MenuItem } from './ui-components.js';
import { LockChip } from './resource-lock.js';
import { nodeFetch } from './node-api.js';
import { shellConfirm } from './shell-state.js';
import { effect } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';
import { revealResourceOverlay } from './resource-places.js';
import { ResourcePill, resourceView } from './resource-pill.js';
import { openGroupWebTermPane } from './terminals-tab.js';
import { killBackgroundTerminal } from './background-terminals.js';

export { LockChip };

const html = htm.bind(h);

function pageMeta(page, nameOf) {
  if (page.state === 'closed') return page.closedReason ? `page · closed · ${page.closedReason}` : 'page · closed';
  if (page.groupOwned) return 'page · blank · no member has taken it over';
  return `page · ${page.kind === 'static' ? 'static' : 'live'} · ${nameOf(page.owner) || page.owner}`;
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
function RowMenu({ label, items }) {
  const [open, setOpen] = useState(false);
  const [sub, setSub] = useState(null);
  const ref = useRef(null);
  const close = () => { setOpen(false); setSub(null); };
  useDismissablePopover(ref, open, (why) => { if (why !== 'away') close(); });
  if (!items.length) return null;
  const shown = sub ? [{ label: '‹ Back', attrs: { 'data-menu-back': '' }, back: true }, ...sub.children] : items;
  const pick = (item) => {
    if (item.back) return setSub(null);
    if (item.children) return setSub(item);
    close();
    void item.run();
  };
  return html`<span class="gr-add" ref=${ref}>
    <${Button} size="sm" variant="link" class="gq-link gr-more" aria-haspopup="menu" aria-expanded=${open ? 'true' : 'false'}
      title=${`More for ${label}`} onClick=${() => (open ? close() : setOpen(true))}>⋯<//>
    ${open && html`<${Menu} size="sm" class="gr-add-menu">
      ${shown.map((item) => html`<${MenuItem} key=${item.label} danger=${item.danger} ...${item.attrs || {}}
        onClick=${() => pick(item)}>${item.children ? `${item.label} ›` : item.label}<//>`)}
    <//>`}
  </span>`;
}

// RenameField edits a row's name in place: Enter saves, Escape or leaving
// it keeps the name.
function RenameField({ value, label, onSave, onDone }) {
  const ref = useRef(null);
  useEffect(() => { ref.current?.focus(); ref.current?.select(); }, []);
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

// moveTargets are the groups a resource can move to: the others, and for an
// agent's page only those its agent is a member of.
function moveTargets(snapshotValue, groupId, agentId = '') {
  return (snapshotValue?.groups || [])
    .filter((g) => g?.id > 0 && g.id !== groupId && g.name)
    .filter((g) => !agentId || (g.members || []).some((m) => m.agent_id === agentId))
    .map((g) => g.name);
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

// ResourceRow is one resource: glyph and name (the primary action too), what
// it is, its state pill, the primary action, the kind's own action, ⋯, and
// the agent lock at the row's end where the kind has one.
function ResourceRow({ attrs, kind, closed, glyph, label, meta, view, primary, action, menu, lock, renaming = null }) {
  return html`<div class=${closed ? 'gr-row gr-closed' : 'gr-row'} ...${attrs}>
    ${renaming ? html`<span class="gr-glyph" aria-hidden="true">${glyph}</span><${RenameField} ...${renaming} label=${label} />`
      : html`<${Button} size="sm" variant="link" class="gr-name" title=${`${primary.label} ${label}`}
      onClick=${primary.run}><span class="gr-glyph" aria-hidden="true">${glyph}</span>${label}<//>`}
    <span class="gq-dim gr-meta">${meta}</span>
    <span class="gr-spacer"></span>
    <${ResourcePill} view=${view} kind=${kind} />
    <${Button} size="sm" class="gr-primary" data-resource-primary=${primary.label} onClick=${primary.run}>${primary.label}<//>
    ${action && html`<${Button} size="sm" class="gr-action" title=${action.title} aria-label=${action.title} ...${action.attrs || {}}
      onClick=${action.run}>${action.glyph}<//>`}
    <${RowMenu} label=${label} items=${menu} />
    ${lock}
  </div>`;
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

function PageRow({ page, api, source, notify, confirm, nameOf, snapshotValue, groupId }) {
  const label = page.title || page.name;
  const [renaming, setRenaming] = useState(false);
  const view = resourceView(snapshotValue, groupId, 'page', page.id, page.state === 'closed' ? 'closed' : 'running');
  const run = view === 'closed' ? () => reopenPage(page, api, source, notify) : focusOr(`page:${page.id}`, () => openPagePane(page.id, label));
  const remove = async () => {
    const ok = await confirm({
      title: `Delete page ${label}?`,
      body: view === 'closed'
        ? 'The page is removed from the group and cannot be reopened.'
        : 'The page closes for everyone viewing it and is removed from the group; it cannot be reopened.',
      okLabel: 'Delete',
    });
    if (!ok) return;
    try {
      await api.deleteGroupPage(page.id);
      notify(`Deleted ${label}`);
      void source?.refresh?.();
    } catch (cause) {
      notify(`Could not delete ${label}: ${cause.message}`, true);
    }
  };
  const reload = async () => {
    try {
      await api.reloadGroupPage(page.id);
    } catch (cause) {
      notify(`Could not reload ${label}: ${cause.message}`, true);
    }
  };
  const act = rowAction(label, source, notify);
  const rename = act('rename', (title) => api.renameGroupPage(page.id, title), (title) => `Renamed ${label} to ${title}`);
  const move = act('move', (name) => api.moveGroupPage(page.id, name), (name) => `Moved ${label} to ${name}`);
  // An agent's page moves to a group its agent is in; a group's own blank
  // page only while closed.
  const groups = page.groupOwned ? (view === 'closed' ? moveTargets(snapshotValue, groupId) : []) : moveTargets(snapshotValue, groupId, page.owner);
  return html`<${ResourceRow} kind="page" attrs=${{ 'data-resource-page': page.id, 'data-page-state': view === 'closed' ? 'closed' : 'open' }}
    closed=${view === 'closed'} glyph="◧" label=${label} meta=${pageMeta(page, nameOf)} view=${view}
    primary=${{ label: primaryLabel('page', view), run }}
    action=${view !== 'closed' && { glyph: '↻', title: `Reload ${label} in every view`, attrs: { 'data-page-reload': page.id }, run: reload }}
    renaming=${renaming && { value: label, onSave: rename, onDone: () => setRenaming(false) }}
    menu=${[...renameAndMove({ startRename: () => setRenaming(true), groups, move, attrs: page.id }),
      { label: 'Delete…', danger: true, attrs: { 'data-page-delete': page.id }, run: remove }]}
    lock=${html`<${LockChip} kind="page" id=${page.id} name=${label} locked=${!!page.agentsLocked} api=${api} source=${source} notify=${notify} />`} />`;
}

function BoardRow({ board, api, source, notify, confirm, snapshotValue, groupId }) {
  const label = board.title || board.id;
  const [renaming, setRenaming] = useState(false);
  const act = rowAction(label, source, notify);
  const rename = act('rename', (title) => api.renameBoard(board.id, title), (title) => `Renamed ${label} to ${title}`);
  const move = act('move', (name) => api.moveBoard(board.id, name), (name) => `Moved ${label} to ${name}`);
  const view = resourceView(snapshotValue, groupId, 'board', board.id);
  const remove = async () => {
    const ok = await confirm({ title: `Delete board ${label}?`, body: 'The board and everything drawn on it are deleted for everyone.', okLabel: 'Delete' });
    if (!ok) return;
    try {
      await api.deleteBoard(board.id);
      notify(`Deleted ${label}`);
      void source?.refresh?.();
    } catch (cause) {
      notify(`Could not delete ${label}: ${cause.message}`, true);
    }
  };
  return html`<${ResourceRow} attrs=${{ 'data-resource-board': board.id }} glyph="▦" label=${label}
    meta=${board.editing > 0 ? `board · ${board.editing} editing` : 'board'} view=${view}
    primary=${{ label: primaryLabel('board', view), run: focusOr(`board:${board.id}`, () => openBoardPane(board.id, label)) }}
    renaming=${renaming && { value: label, onSave: rename, onDone: () => setRenaming(false) }}
    menu=${[...renameAndMove({ startRename: () => setRenaming(true), groups: moveTargets(snapshotValue, groupId), move, attrs: board.id }),
      { label: 'Delete…', danger: true, attrs: { 'data-board-delete': board.id }, run: remove }]}
    lock=${html`<${LockChip} kind="board" id=${board.id} name=${label} locked=${!!board.agentsLocked} api=${api} source=${source} notify=${notify} />`} />`;
}

function ShellRow({ group, shell, notify, confirm, snapshotValue }) {
  const view = resourceView(snapshotValue, group.id, 'shell', shell.id, shell.state);
  const label = `${group.name} shell`;
  const open = focusOr(`shell:${group.name}`, () => openGroupWebTermPane(group.name, label));
  const kill = async () => {
    const ok = await confirm({ title: `Kill ${label}?`, body: 'Whatever runs in the shell stops. Opening it again starts a fresh one.', okLabel: 'Kill' });
    if (!ok) return;
    const error = await killBackgroundTerminal({ kind: 'group_shell', tmux_session: shell.session });
    notify(error ? `Could not kill ${label}: ${error}` : `Killed ${label}`, !!error);
  };
  return html`<${ResourceRow} attrs=${{ 'data-resource-shell': group.name }} closed=${view === 'closed'} glyph="▭" label="Group shell"
    meta=${shell.detail ? `terminal · ${shell.detail}` : 'terminal'} view=${view}
    primary=${{ label: primaryLabel('shell', view), run: open }}
    action=${view === 'background' && shell.session && { glyph: '×', title: `Kill ${label}`, attrs: { 'data-shell-kill': group.name }, run: kill }}
    menu=${[]} lock=${null} />`;
}

// groupShellOf is the group's shell in the snapshot's group_resources.
export function groupShellOf(snapshotValue, group) {
  return (snapshotValue?.group_resources || []).find((r) => r.kind === 'shell' && r.group_id === group.id) || null;
}

// GroupResources lists a group's boards, pages and shell, each kind under its
// own heading and only when it has any.
export function GroupResources({ group, boards = [], pages = [], api, source, notify = defaultNotify, nameOf = () => '', confirm = shellConfirm,
  snapshot = dashboardState.snapshot }) {
  const snapshotValue = snapshot?.value;
  const shell = groupShellOf(snapshotValue, group);
  if (!boards.length && !pages.length && !shell) return null;
  const props = { api, source, notify, confirm, snapshotValue, groupId: group.id };
  return html`<div class="gr-resources" data-group-resources=${group.name}>
    ${boards.length > 0 && html`<div class="gr-kind" data-resource-kind="boards">
      <span class="ui-eyebrow gr-kind-label">Boards</span>
      ${boards.map((board) => html`<${BoardRow} key=${board.id} board=${board} ...${props} />`)}
    </div>`}
    ${pages.length > 0 && html`<div class="gr-kind" data-resource-kind="pages">
      <span class="ui-eyebrow gr-kind-label">Pages</span>
      ${pages.map((page) => html`<${PageRow} key=${page.id} page=${page} nameOf=${nameOf} ...${props} />`)}
    </div>`}
    ${shell && html`<div class="gr-kind" data-resource-kind="shells">
      <span class="ui-eyebrow gr-kind-label">Shell</span>
      <${ShellRow} group=${group} shell=${shell} ...${props} />
    </div>`}
  </div>`;
}

// freePageName is the first of sketch, sketch-2, … the group has no page by.
export function freePageName(pages = []) {
  const taken = new Set(pages.map((page) => page.name));
  if (!taken.has('sketch')) return 'sketch';
  for (let n = 2; ; n++) if (!taken.has(`sketch-${n}`)) return `sketch-${n}`;
}

// AddResource is the section's one ＋ New: a process (the Get started
// panel), a board linked to the group, a blank live page, or the group's
// shell, each with what it is in one line.
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
  const cwd = (group.default_cwd || '').trim();
  const addShell = () => {
    setOpen(false);
    const key = `shell:${group.name}`;
    if (!revealResourceOverlay(key)) void openGroupWebTermPane(group.name, `${group.name} shell`);
  };
  const item = (attr, onClick, name, line, disabled = false) => html`<${MenuItem} ...${{ [attr]: true }} class="gr-new-item" onClick=${onClick}
    disabled=${disabled}><span class="gr-new-name">${name}</span><span class="gr-new-line">${line}</span><//>`;
  return html`<span class="gr-add" ref=${ref}>
    <${Button} size="sm" variant="link" class="gq-link" data-resource-add=${group.name} aria-haspopup="menu" aria-expanded=${open ? 'true' : 'false'}
      title=${`Add a resource to ${group.name}`} onClick=${() => setOpen(!open)}>＋ New ▾<//>
    ${open && html`<${Menu} size="sm" class="gr-add-menu gr-new-menu">
      ${item('data-resource-add-process', () => { setOpen(false); onAddProcess(); }, wizard ? 'Rite' : 'Process', 'From a template, or start blank in the editor')}
      ${item('data-resource-add-board', addBoard, 'Board', "A drawing board shared with the group's agents")}
      ${item('data-resource-add-page', addPage, 'Live page', 'A blank page you draw on; a member takes it over with its own source')}
      ${item('data-resource-add-shell', addShell, 'Shell', cwd ? `A terminal in ${cwd}` : 'Set the group\'s directory first', !cwd)}
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

function peerRows(resources) {
  return [
    ['processes', 'Processes', '⚙', 'process', (resources.processes || []).map((p) => ({ id: p.templateId, name: p.name || p.templateId, locked: !!p.agentsLocked }))],
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
  return html`<div class="gr-resources" data-group-resources=${group.name} data-peer-resources=${node?.id || ''}>
    ${kinds.map(([key, label, glyph, kind, rows]) => html`<div key=${key} class="gr-kind" data-resource-kind=${key}>
      <span class="ui-eyebrow gr-kind-label">${label}</span>
      ${rows.map((row) => html`<div key=${row.id} class="gr-row" data-peer-resource=${`${kind}:${row.id}`}>
        <span class="gr-name-text" title=${row.name}><span class="gr-glyph" aria-hidden="true">${glyph}</span>${row.name}</span>
        <span class="gr-spacer"></span>
        <${LockChip} kind=${kind} id=${row.id} name=${row.name} locked=${row.locked} peer=${peer} notify=${notify}
          source=${{ refresh }} />
      </div>`)}
    </div>`)}
  </div>`;
}
