// phone-now-shells.js -- Now's Shells section on a phone: the shells running
// in the background, the ones the desktop's Collab panel lists under its
// Shells filter (a group's shell, an agent's shell). It sits at the bottom of
// Now, folded by default; the fold is remembered per viewer (this browser).
// It is left out while no shell runs.
//
// A row says whose shell it is, how long it has been quiet and how long it
// has run. A tap opens it in the phone terminal, as the panel does
// (openRailItem); its ⋯ is the panel row's menu (railRowActions: Attach,
// Kill session…, asking first) plus the tmux attach command to copy. ＋ Shell
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
import { Avatar, Card, IconButton, Section } from './phone-ui.js';
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

// ShellMenu is a row's ⋯ sheet: the panel row's entries, then the attach
// command to copy, with the destructive ones last.
export function ShellMenu({ item, ctx, notify = () => {}, copy = writeClipboard, close }) {
  const { open, end } = railRowActions(item);
  const command = attachCommand(item);
  const entry = (action) => html`<li key=${action.id}><button data-ui-exempt type="button" data-shell-action=${action.id}
    class=${`phone-menu-item${action.danger ? ' phone-menu-danger' : ''}`} disabled=${action.disabled} title=${action.title || undefined}
    onClick=${() => { close(); void action.run(ctx); }}>${action.label}</button></li>`;
  const copyEntry = command && html`<li><button data-ui-exempt type="button" data-shell-action="copy-attach" class="phone-menu-item"
    onClick=${async () => { close(); notify(await copy(command) ? 'Copied the attach command' : command); }}>Copy tmux attach command</button></li>`;
  return html`<div class="phone-shell-menu">
    <h2 class="phone-sheet-title">${item.title}</h2>
    <ul class="phone-menu">${open.map(entry)}${copyEntry}${end.map(entry)}</ul>
  </div>`;
}

// GroupPick is ＋ Shell's sheet when there is more than one group.
function GroupPick({ groups, start, close }) {
  return html`<div class="phone-shell-menu">
    <h2 class="phone-sheet-title">Start a shell in</h2>
    <ul class="phone-menu">${groups.map((group) => html`<li key=${group}><button data-ui-exempt type="button" class="phone-menu-item"
      data-shell-group=${group} onClick=${() => { close(); void start(group); }}>${group}</button></li>`)}</ul>
  </div>`;
}

// ShellRow is one shell: a tap opens it, ⋯ its menu.
function ShellRow({ shell, onOpen, onMenu }) {
  return html`<div class="phone-now-agent phone-now-shell" data-shell-row=${shell.item.key}>
    <button data-ui-exempt type="button" class="phone-row" onClick=${() => onOpen(shell.item)}
      aria-label=${`${shell.title}, ${shell.sub}: open it`}>
      <${Avatar} name=${shell.title} glyph="›_" bot />
      <span class="phone-row-text"><span class="phone-who-name">${shell.title}</span>
        <span class="phone-now-when">${shell.sub}</span></span>
    </button>
    <${IconButton} icon="⋯" label=${`${shell.title}: more`} aria-haspopup="dialog" class="phone-now-more" onClick=${() => onMenu(shell.item)} />
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
  const ctx = { ...defaultRowContext(), open: attach, notify };
  const onMenu = (item) => openPhoneSheet({ label: item.title, Body: ShellMenu, props: { item, ctx, notify } });
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
