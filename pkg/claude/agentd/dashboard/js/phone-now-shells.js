// phone-now-shells.js -- Now's Shells section on a phone: the shells running
// in the background, the ones the desktop's Collab panel lists under its
// Shells filter (a group's shell, an agent's shell). It sits at the bottom of
// Now, folded by default; the fold is remembered per viewer (this browser).
// It is left out while no shell runs.
//
// A row says whose shell it is, how long it has been quiet and how long it
// has run. A tap opens it in the phone terminal, as the panel does
// (openRailItem); a touch and hold (phone-hold.js, as on an agent's row), or
// its ⋯, opens its menu (ShellMenu): the panel row's entries (railRowActions:
// Attach, Kill session…, asking first), then its name, directory and tmux
// attach command to copy. The open-sessions list (phone-sessions.js) opens
// the same menu for its shells (openShellMenu). ＋ Shell
// in the header starts a group's shell, as the Groups tab's ＋ → Shell does;
// with more than one group it asks which.

import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { idleText } from './background-terminals.js';
import { collabItems, openRailItem } from './collab-rail-actions.js';
import { defaultRowContext, railRowActions } from './collab-rail-menu.js';
import { onPeerPage } from './node-api.js';
import { openPhoneSheet } from './phone-sheet.js';
import { useHold } from './phone-hold.js';
import { Avatar, Card, RowMore, Section, SheetTitle } from './phone-ui.js';
import { terminalShellState } from './terminal-shell-state.js';
import { openShellResource } from './terminals-tab.js';
import { writeClipboard } from './insecure-origin.js';

const html = htm.bind(h);

const SHELL_ROWS = new Set(['shell', 'group_shell']);

// isShellItem is whether a Collab panel item is a running shell: a group's
// or an agent's shell resource, or a shell session no group lists.
export function isShellItem(item) {
  if (!item || (item.bucket || '') === 'closed') return false;
  if (item.kind === 'shell') return true;
  return item.kind === 'terminal' && SHELL_ROWS.has(item.row?.kind);
}

// upText is how long a session has run: "up 3h", or '' when unknown.
export function upText(created, nowSeconds = Date.now() / 1000) {
  const at = Number(created || 0);
  if (!at) return '';
  const age = Math.max(0, nowSeconds - at);
  if (age < 3600) return `up ${Math.max(1, Math.floor(age / 60))}m`;
  if (age < 86400) return `up ${Math.floor(age / 3600)}h`;
  return `up ${Math.floor(age / 86400)}d`;
}

// nowShells is what the section lists, in the panel's order: [{ item, title,
// sub }], sub saying whose shell it is (and its group, for an agent's), how
// long it has been quiet, and how long it has run.
export function nowShells(sections, nowSeconds = Date.now() / 1000) {
  const out = [];
  for (const section of sections || []) {
    for (const item of section.items || []) {
      if (!isShellItem(item)) continue;
      const row = item.shellRow || item.row || null;
      const agent = item.kind === 'shell' ? !!item.resource?.agent : row?.kind === 'shell';
      const group = item.resource?.group_id || row?.group || '';
      const idle = idleText(row?.last_activity, nowSeconds).text;
      const sub = [
        agent ? 'agent shell' : 'group shell',
        agent && (section.name || group),
        row?.node?.name,
        idle === 'just now' ? 'active just now' : idle,
        upText(row?.created, nowSeconds),
      ].filter(Boolean).join(' · ');
      out.push({ item, title: item.title, sub });
    }
  }
  return out;
}

// attachCommand is how to reach a shell by hand, from a terminal on its
// host; '' when its session is unknown or on another node.
export function attachCommand(item) {
  const row = item.shellRow || item.row;
  if (!row?.tmux_session || row.node) return '';
  return `tmux -L tclaude attach -t '${row.tmux_session}'`;
}

// FOLD_KEY remembers, per viewer, whether the section is open.
const FOLD_KEY = 'tclaude.phone.now.shells.open';
export function storedShellsOpen(storage = globalThis.localStorage) {
  try { return storage?.getItem(FOLD_KEY) === '1'; } catch { return false; }
}
function storeShellsOpen(open, storage = globalThis.localStorage) {
  try { storage?.setItem(FOLD_KEY, open ? '1' : '0'); } catch { /* private mode: the fold lasts this visit */ }
}

// shellGroups are the groups a shell can be started in: every live group,
// by name.
export function shellGroups(snapshot) {
  return (snapshot?.groups || []).filter((g) => g?.name && !g.archived && !g.scribe).map((g) => g.name)
    .sort((a, b) => a.localeCompare(b, undefined, { sensitivity: 'base', numeric: true }));
}

const startShell = async (group) => (await import('./groups-resources.js')).addGroupShell({ name: group });

// shellDir is the directory a shell runs in, as its group resource says
// (a group's default directory, an agent's shell's), or ''.
export function shellDir(item) {
  return item?.kind === 'shell' ? item.resource?.detail || '' : '';
}

// shellCopies are the menu's copy entries: [{ id, label, text, done }].
export function shellCopies(item) {
  const dir = shellDir(item);
  const command = attachCommand(item);
  return [
    { id: 'copy-name', label: 'Copy name', text: item.title, done: 'Copied the name' },
    dir && { id: 'copy-dir', label: 'Copy directory', text: dir, done: 'Copied the directory' },
    command && { id: 'copy-attach', label: 'Copy tmux attach command', text: command, done: 'Copied the attach command' },
  ].filter(Boolean);
}

// ShellMenu is a shell's sheet (a hold on its row, or its ⋯): the panel
// row's entries, then what to copy, with the destructive ones last.
export function ShellMenu({ item, ctx, notify = () => {}, copy = writeClipboard, close }) {
  const { open, end } = railRowActions(item);
  const dir = shellDir(item);
  const entry = (action) => html`<li key=${action.id}><button data-ui-exempt type="button" data-shell-action=${action.id}
    class=${`phone-menu-item${action.danger ? ' phone-menu-danger' : ''}`} disabled=${action.disabled} title=${action.title || undefined}
    onClick=${() => { close(); void action.run(ctx); }}>${action.label}</button></li>`;
  const copyEntry = (c) => html`<li key=${c.id}><button data-ui-exempt type="button" data-shell-action=${c.id} class="phone-menu-item"
    onClick=${async () => { close(); notify(await copy(c.text) ? c.done : c.text); }}>${c.label}</button></li>`;
  return html`<div class="phone-shell-menu">
    <${SheetTitle}>${item.title}<//>
    ${dir && html`<p class="phone-sheet-meta phone-shell-dir">${dir}</p>`}
    <ul class="phone-menu">${open.map(entry)}${shellCopies(item).map(copyEntry)}${end.map(entry)}</ul>
  </div>`;
}

// openShellMenu opens a shell's menu; open(item) is how the list opens it.
export function openShellMenu(item, { open, notify = () => {} }) {
  const ctx = { ...defaultRowContext(), open, notify };
  openPhoneSheet({ label: item.title, Body: ShellMenu, props: { item, ctx, notify } });
}

// GroupPick is ＋ Shell's sheet when there is more than one group.
function GroupPick({ groups, start, close }) {
  return html`<div class="phone-shell-menu">
    <${SheetTitle}>Start a shell in<//>
    <ul class="phone-menu">${groups.map((group) => html`<li key=${group}><button data-ui-exempt type="button" class="phone-menu-item"
      data-shell-group=${group} onClick=${() => { close(); void start(group); }}>${group}</button></li>`)}</ul>
  </div>`;
}

// ShellRow is one shell: a tap opens it; a hold, or its ⋯, its menu.
function ShellRow({ shell, onOpen, onMenu }) {
  const { holding, handlers } = useHold(() => onMenu(shell.item));
  return html`<div class=${`phone-now-agent phone-now-shell${holding ? ' phone-holding' : ''}`} data-shell-row=${shell.item.key}>
    <button data-ui-exempt type="button" class="phone-row phone-holdable" ...${handlers} onClick=${() => onOpen(shell.item)}
      aria-label=${`${shell.title}, ${shell.sub}: open it`}>
      <${Avatar} name=${shell.title} glyph="›_" bot />
      <span class="phone-row-text"><span class="phone-who-name">${shell.title}</span>
        <span class="phone-now-when">${shell.sub}</span></span>
    </button>
    <${RowMore} label=${`${shell.title}: more`} class="phone-now-more" onClick=${() => onMenu(shell.item)} />
  </div>`;
}

// NowShells is the section. listed, open and start are seams for tests.
export function NowShells({ snapshot, notify = () => {}, panes = terminalShellState.panes.value,
  listed = (p, s) => collabItems(p, s).sections, open = openRailItem, start = startShell, storage = globalThis.localStorage }) {
  const [unfolded, setUnfolded] = useState(() => storedShellsOpen(storage));
  const shells = nowShells(listed(panes, snapshot));
  if (!shells.length) return null;
  const toggle = () => { storeShellsOpen(!unfolded, storage); setUnfolded(!unfolded); };
  // A shell already open as a pane is brought forward by opening it again
  // (openShellResource finds its pane): Now has no strip to activate.
  const attach = (item) => open(item, { snapshot, notify,
    actions: { activatePane: () => (item.resource ? openShellResource(item.resource, item.title) : null) } });
  const onMenu = (item) => openShellMenu(item, { open: attach, notify });
  // A session the dashboard cannot attach (superseded, locked: it has no
  // key) opens its menu instead, where it can be killed, as on the desktop.
  const onOpen = (item) => (item.row && !item.row.key ? onMenu(item) : attach(item));
  const groups = onPeerPage() ? [] : shellGroups(snapshot?.value);
  const add = groups.length === 0 ? null : () => (groups.length === 1
    ? start(groups[0])
    : openPhoneSheet({ label: 'Start a shell', Body: GroupPick, props: { groups, start } }));
  return html`<${Section} title="Shells" count=${shells.length} class="phone-now-shells"
    fold=${{ open: unfolded, onToggle: toggle }}
    more=${add && html`<button data-ui-exempt type="button" class="phone-link" data-shell-add onClick=${add}>＋ Shell</button>`}>
    ${unfolded && html`<${Card} class="phone-rows-card">
      <ul class="phone-rows">${shells.map((shell) => html`<li key=${shell.item.key}>
        <${ShellRow} shell=${shell} onOpen=${onOpen} onMenu=${onMenu} />
      </li>`)}</ul>
    <//>`}
  <//>`;
}
