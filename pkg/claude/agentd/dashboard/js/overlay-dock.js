// overlay-dock.js -- the dock of minimized overlay windows: its ☰ menu of
// actions on every window, and a pill for each window or tab sent to it.
//
// It rests in the footer (tcl-9qsgqs), right-aligned beside the Steward's chips:
// from the right, Open PRs, the chips, then ☰ and the pills, which grow to the
// left. It takes only the room the bar has left: it pushes and covers nothing
// there, and keeps clear the room of the chips (footerChipReserve). Pills that
// don't fit fold into a "+N" button at their left end, and when none fits, into
// ☰ itself. Either opens a list of them. On a page without the footer it floats
// over the room.
//
// Its menus render beside it, in the overlay host (OverlayDockPopup): inside the
// footer they would sit under the overlay windows, the footer's level being theirs.

import { h } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import { signal } from '@preact/signals';
import htm from 'htm';
import { companionAgents, companionStatus, decisionsFor, approvalsFor, stewardAgent } from './companion-model.js';
import { endCompanionPeek, peekInCompanion } from './companion-facade.js';
import { footerChipReserve } from './companion-chip-footer.js';
import { useDismissablePopover } from './hover-popover.js';
import { closeOverlayTab, focusOverlayWindow, minimizedOverlayTabs, openOverlayWindow, overlayHostUp, overlayWindows } from './overlay-instances.js';
import { CHAT_WINDOW_KIND, DECIDE_WINDOW_KIND, START_WINDOW_KIND } from './chat-window-model.js';
import { TERMINAL_WINDOW_KIND } from './terminal-window-model.js';
import { BOARD_WINDOW_KIND } from './board-overlay-state.js';
import { PROCESS_WINDOW_KIND } from './process-editor-place.js';
import { PAGE_WINDOW_KIND } from './page-pane-model.js';
import { dockEntriesOf } from './overlay-tab-model.js';
import { tabMoves } from './overlay-tab-moves.js';
import { overlayBulkActions } from './overlay-window-bulk.js';
import { withShortcut } from './keybindings.js';

const html = htm.bind(h);

export const WINDOW_GLYPHS = { [BOARD_WINDOW_KIND]: '✎', [TERMINAL_WINDOW_KIND]: '⌨', [PROCESS_WINDOW_KIND]: '⬡', [PAGE_WINDOW_KIND]: '◧' };

// What taking a pill off for good does, by kind. A Steward has none: only its ⚙
// shuts it down.
const REMOVE_TITLES = {
  [CHAT_WINDOW_KIND]: ['Unpin', 'Unpin (the agent keeps running)'],
  [TERMINAL_WINDOW_KIND]: ['Close', 'Close (the shell keeps running)'],
  [BOARD_WINDOW_KIND]: ['Close', 'Close this board’s window'],
  [PROCESS_WINDOW_KIND]: ['Close', 'Close this editor (asks first if it has unsaved edits)'],
  [PAGE_WINDOW_KIND]: ['Close', 'Close this page (the agent that showed it is told)'],
};

// The gap between the dock's buttons; dashboard.css has the same.
const DOCK_GAP = 6;

// The menu open from the dock: { kind: 'actions' | 'more', at: { left, bottom } }.
export const dockPopup = signal(null);
// How many pills the footer shows; the rest are in the "+N" list, or with none
// shown, in ☰'s menu.
const dockShown = signal(Infinity);

// fitDockPills is how many pills fit in budget beside ☰ (menu wide), each pill
// as wide as pills says. Leaving some out costs the "+N" button (more wide);
// when not even one fits beside it, they all fold into ☰ and the answer is 0.
export function fitDockPills({ budget, menu, more, pills, gap = DOCK_GAP }) {
  const row = (count, extra) => menu + pills.slice(0, count).reduce((sum, width) => sum + gap + width, 0) + (extra ? gap + extra : 0);
  if (row(pills.length, 0) <= budget) return pills.length;
  for (let count = pills.length - 1; count > 0; count--) if (row(count, more) <= budget) return count;
  return 0;
}

// useDockEntries is what the dock holds now. The chip stands for the seated
// Steward's window, and for the page summoning one, so those have no pill.
function useDockEntries(snapshot) {
  const agents = companionAgents(snapshot.value);
  const seated = stewardAgent(agents)?.agentId;
  const onChip = ({ kind, agent }) => kind === START_WINDOW_KIND || (kind === CHAT_WINDOW_KIND && agent === seated);
  const entries = dockEntriesOf(overlayWindows.value, minimizedOverlayTabs.value).filter(({ tabs }) => !(overlayHostUp.value && (tabs.every(onChip) || tabs.some(({ kind, agent }) => kind === CHAT_WINDOW_KIND && agent === seated))));
  // A chat that needs the operator says so here, as a chip does, so it never
  // looks silently busy.
  const needs = ({ kind, agent }) => {
    const row = kind === CHAT_WINDOW_KIND ? agents.get(agent) : null;
    return !!row && (companionStatus(row) === 'needs' || approvalsFor(snapshot.value, row).length + decisionsFor(snapshot.value, row).length > 0);
  };
  return { entries, actions: overlayBulkActions.value, needs, onChip };
}

const bringBack = (tab) => { endCompanionPeek(); openOverlayWindow(tab); focusOverlayWindow(tab.id); };

const peeksOf = (tab) => (tab.kind === CHAT_WINDOW_KIND ? {
  onMouseEnter: (event) => peekInCompanion(tab.agent, event.currentTarget),
  onMouseLeave: endCompanionPeek,
  onFocus: (event) => peekInCompanion(tab.agent, event.currentTarget),
  onBlur: endCompanionPeek,
} : null);

// DockPill is a minimized window: its active tab, and how many it holds. A tab
// comes back from here to the window it left.
function DockPill({ entry: { tab, tabs }, needs, onChip }) {
  const removes = tabs.length === 1 && !onChip(tab) && tab.kind !== DECIDE_WINDOW_KIND && REMOVE_TITLES[tab.kind];
  return html`<span class=${`overlay-dock-pill${tabs.some(needs) ? ' needs' : ''}`}>
    <button type="button" class="ui-btn ui-btn-ghost ui-sm overlay-dock-item" title=${`Bring back ${tab.label}`} ...${peeksOf(tab)}
      onClick=${() => bringBack(tab)}>${WINDOW_GLYPHS[tab.kind] || '▣'} ${tab.label}${tabs.length > 1 && html` <span class="overlay-dock-count ui-badge" aria-label=${`${tabs.length} tabs`}>${tabs.length}</span>`}</button>
    ${removes && html`<button type="button" class="ui-btn ui-btn-ghost ui-sm overlay-dock-remove" aria-label=${`${removes[0]} ${tab.label}`} title=${removes[1]} onClick=${() => (tabMoves(tab).close || (() => closeOverlayTab(tab.id)))()}>×</button>`}
  </span>`;
}

// toggle opens a menu upwards from its button, or closes it.
function toggle(kind, event) {
  if (dockPopup.peek()?.kind === kind) {
    dockPopup.value = null;
    return;
  }
  const rect = event.currentTarget.getBoundingClientRect();
  const height = event.currentTarget.ownerDocument.defaultView?.innerHeight || 0;
  dockPopup.value = { kind, at: { left: rect.left, bottom: height - rect.top + 6 } };
}

// DockMenuButton is the dock's first button: actions on every window at once, in
// the palette's words, and the pills folded into it, counted.
function DockMenuButton({ folded = [], needs }) {
  const open = dockPopup.value?.kind === 'actions';
  const label = folded.length ? `All windows, ${folded.length} minimized` : 'All windows';
  return html`<span class=${`overlay-dock-menu${folded.some(({ tabs }) => tabs.some(needs)) ? ' needs' : ''}`}>
    <button type="button" class="ui-btn ui-btn-ghost ui-sm overlay-dock-menu-button" aria-haspopup="menu" aria-expanded=${open ? 'true' : 'false'}
      title=${label} aria-label=${label} onClick=${(event) => toggle('actions', event)}>☰${folded.length > 0 && html` <span class="overlay-dock-count ui-badge">${folded.length}</span>`}</button>
  </span>`;
}

function DockMoreButton({ hidden, needs }) {
  const open = dockPopup.value?.kind === 'more';
  const label = `${hidden.length} more minimized`;
  return html`<span class=${`overlay-dock-more${hidden.some(({ tabs }) => tabs.some(needs)) ? ' needs' : ''}`}>
    <button type="button" class="ui-btn ui-btn-ghost ui-sm overlay-dock-more-button" aria-haspopup="menu" aria-expanded=${open ? 'true' : 'false'}
      title=${label} aria-label=${label} onClick=${(event) => toggle('more', event)}>+${hidden.length}</button>
  </span>`;
}

// OverlayDock floats over the room, for a page without the footer.
export function OverlayDock({ snapshot }) {
  const { entries, actions, needs, onChip } = useDockEntries(snapshot);
  useEffect(() => { dockShown.value = Infinity; }, []);
  if (!entries.length && !actions.length) return null;
  return html`<div class="overlay-dock floating" role="group" aria-label="Minimized windows">
    ${actions.length > 0 && html`<${DockMenuButton} needs=${needs} />`}
    ${entries.map((entry) => html`<${DockPill} key=${entry.key} entry=${entry} needs=${needs} onChip=${onChip} />`)}
  </div>`;
}

// FooterDock is the dock in the footer. It renders every pill once more, off
// screen, to know how wide each is before it picks the ones that fit, so the
// row the bar shows never overflows it, not even for a frame.
export function FooterDock({ snapshot, win }) {
  const { entries, actions, needs, onChip } = useDockEntries(snapshot);
  const root = useRef(null);
  const measure = useRef(null);
  const [shown, setShown] = useState(Infinity);
  // Whether the chips' room is kept: not when ☰ itself would not fit beside it,
  // as ☰ never leaves the bar and the chips float then anyway.
  const [reserving, setReserving] = useState(true);
  const [, setWidth] = useState(0);
  // Chips resting at footer-right sit between the dock and Open PRs, so the dock
  // keeps their room at its right end; elsewhere it only leaves them the width.
  const { width: reserve, spot: chipSpot } = footerChipReserve.value;
  useLayoutEffect(() => {
    const box = root.current?.getBoundingClientRect?.();
    const widths = [...(measure.current?.children || [])].map((node) => node.getBoundingClientRect().width);
    // Unlaid out, as under test: everything shows.
    const keep = !box?.width || !widths.length || box.width - reserve >= widths[0];
    if (keep !== reserving) setReserving(keep);
    const next = !box?.width || !widths.length ? Infinity : fitDockPills({ budget: box.width - (keep ? reserve : 0), menu: widths[0], more: widths[1], pills: widths.slice(2) });
    const fitted = next >= entries.length ? Infinity : next;
    if (fitted !== shown) setShown(fitted);
    dockShown.value = fitted;
  });
  useEffect(() => {
    const node = root.current;
    if (!node || typeof win?.ResizeObserver !== 'function') return undefined;
    const observer = new win.ResizeObserver(([entry]) => setWidth(Math.round(entry.contentRect.width)));
    observer.observe(node);
    return () => observer.disconnect();
  }, [win]);
  useEffect(() => () => { dockShown.value = Infinity; dockPopup.value = null; }, []);
  const visible = entries.slice(0, shown);
  const hidden = entries.slice(visible.length);
  const folded = visible.length ? [] : hidden;
  return html`<div ref=${root} class="overlay-dock in-footer" role="group" aria-label="Minimized windows"
    style=${chipSpot === 'footer-right' && reserve && reserving ? `padding-right:${reserve}px` : null}>
    ${(entries.length > 0 || actions.length > 0) && html`<${DockMenuButton} folded=${folded} needs=${needs} />`}
    ${visible.map((entry) => html`<${DockPill} key=${entry.key} entry=${entry} needs=${needs} onChip=${onChip} />`)}
    ${visible.length > 0 && hidden.length > 0 && html`<${DockMoreButton} hidden=${hidden} needs=${needs} />`}
    <div ref=${measure} class="overlay-dock overlay-dock-measure" aria-hidden="true" inert>
      <${DockMenuButton} needs=${needs} />
      <${DockMoreButton} hidden=${entries} needs=${needs} />
      ${entries.map((entry) => html`<${DockPill} key=${entry.key} entry=${entry} needs=${needs} onChip=${onChip} />`)}
    </div>
  </div>`;
}

// OverlayDockPopup is the menu open from the dock: ☰'s actions, followed by the
// pills folded into it, or the pills behind "+N". It opens upwards from its
// button and stays on screen.
export function OverlayDockPopup({ snapshot, win }) {
  const { entries, actions, needs } = useDockEntries(snapshot);
  const popup = dockPopup.value;
  const node = useRef(null);
  const [left, setLeft] = useState(null);
  const hidden = entries.slice(Math.min(dockShown.value, entries.length));
  const listed = popup?.kind === 'more' || (popup?.kind === 'actions' && dockShown.value === 0) ? hidden : [];
  const shownActions = popup?.kind === 'actions' ? actions : [];
  // "+N" gone from the bar takes its list with it; ☰ lists them now.
  const empty = (!shownActions.length && !listed.length) || (popup?.kind === 'more' && dockShown.value === 0);
  useEffect(() => {
    if (popup && empty) dockPopup.value = null;
  }, [popup, empty]);
  useLayoutEffect(() => {
    const box = node.current?.getBoundingClientRect?.();
    const room = win?.innerWidth || 0;
    setLeft(box?.width && room && popup ? Math.max(8, Math.min(popup.at.left, room - box.width - 8)) : null);
  }, [popup, listed.length, shownActions.length]);
  // A menu open when the last window goes, or Collab with it, is not open when one comes back.
  useEffect(() => () => { dockPopup.value = null; }, []);
  // A press outside the menu and its two buttons, or Escape, closes it.
  useDismissablePopover(
    () => [node.current, ...(win?.document?.querySelectorAll?.('.overlay-dock-menu, .overlay-dock-more') || [])],
    popup,
    () => { dockPopup.value = null; },
    { hold: () => true, doc: win?.document },
  );
  if (!popup || empty) return null;
  const close = () => { dockPopup.value = null; };
  return html`<div ref=${node} class="ui-menu overlay-dock-actions" role="menu" style=${`left:${left ?? popup.at.left}px;bottom:${popup.at.bottom}px`}>
    ${shownActions.map(({ key, icon, label, hint, shortcut, run }) => html`<button key=${key} type="button" role="menuitem" class=${`ui-menu-item overlay-dock-action overlay-dock-action-${key}`}
      title=${shortcut ? withShortcut(hint, shortcut) : hint} onClick=${() => { close(); run(); }}>${icon} ${label}</button>`)}
    ${shownActions.length > 0 && listed.length > 0 && html`<hr class="ui-menu-sep" />`}
    ${listed.map(({ key, tab, tabs }) => html`<button key=${key} type="button" role="menuitem" class=${`ui-menu-item overlay-dock-action overlay-dock-entry${tabs.some(needs) ? ' needs' : ''}`}
      title=${`Bring back ${tab.label}`} ...${peeksOf(tab)} onClick=${() => { close(); bringBack(tab); }}>
      ${WINDOW_GLYPHS[tab.kind] || '▣'} ${tab.label}${tabs.length > 1 && html` <span class="overlay-dock-entry-note">· ${tabs.length} tabs</span>`}${tabs.some(needs) && html` <span class="overlay-dock-entry-needs">needs you</span>`}
    </button>`)}
  </div>`;
}
