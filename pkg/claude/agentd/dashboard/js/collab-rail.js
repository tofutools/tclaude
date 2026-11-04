// collab-rail.js -- the Collab view's rail (tcl-lx7nb7): one list of
// everything, by group, then the agents' terminals. A search and the state
// chips (All, Open, Minimized, Background, Closed) narrow it. Clicking an
// item opens it or brings it forward; hovering one open here offers to
// minimize, pop out or close it. Closing a tab moves the item to Closed or
// Background, so nothing is lost. The rail folds to its icons. What it lists
// comes from collab-rail-model.js.

import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { signal } from '@preact/signals';
import { RAIL_FILTERS, RAIL_MARK, filterRail, railGlyph, railItems } from './collab-rail-model.js';
import { overlayResourceKey } from './resource-model.js';
import { resourcePlaces, revealResourceOverlay } from './resource-places.js';
import { closeOverlayTab, minimizeOverlayTab, openOverlayWindow, overlayTabs } from './overlay-instances.js';
import { tabMoves } from './overlay-tab-moves.js';
import { attachBackgroundTerminal, backgroundTerminals, rowView, visibleBackgroundTerminals } from './background-terminals.js';
import { openBoardPane } from './board-overlay-state.js';
import { expectPage, openPagePane } from './page-place.js';
import { openProcessTab } from './process-editor-place.js';
import { openGroupWebTermPane } from './terminals-tab.js';
import { pillLabel } from './resource-pill.js';

const html = htm.bind(h);

const COLLAPSED_PREF = 'tclaude.dash.collab-rail.collapsed';

function readCollapsed() {
  try { return globalThis.localStorage?.getItem(COLLAPSED_PREF) === '1'; } catch { return false; }
}

// railCollapsed is the rail folded to its icons, remembered in this browser.
export const railCollapsed = signal(readCollapsed());

export function setRailCollapsed(next) {
  railCollapsed.value = !!next;
  try { globalThis.localStorage?.setItem(COLLAPSED_PREF, next ? '1' : '0'); } catch { /* a convenience only */ }
}

const overlayOf = (item) => item.overlay || overlayTabs.peek().find((tab) => overlayResourceKey(tab) === item.key) || null;

// openRailItem opens an item, or brings it forward where it already is.
export async function openRailItem(item, { actions, snapshot, fetchImpl = (...args) => globalThis.fetch(...args), notify = () => {} }) {
  if (item.view === 'open-tab' && item.paneKey) return actions?.activatePane?.(item.paneKey);
  if (item.overlay) return openOverlayWindow(item.overlay);
  if (item.row) return attachBackgroundTerminal(item.row, snapshot?.value);
  const r = item.resource;
  if (!r || revealResourceOverlay(item.key)) return null;
  switch (r.kind) {
    case 'page':
      if (item.view !== 'closed') return openPagePane(r.id, item.title);
      try {
        const response = await fetchImpl(`/api/pages/${encodeURIComponent(r.id)}/reopen`, { method: 'POST', credentials: 'same-origin' });
        if (!response.ok) {
          const body = await response.json().catch(() => null);
          notify(`Could not reopen ${item.title}: ${body?.error || response.status}`, true);
          return null;
        }
        expectPage(r.id);
      } catch (cause) {
        notify(`Could not reopen ${item.title}: ${cause?.message || cause}`, true);
      }
      return null;
    case 'board': return openBoardPane(r.id, item.title);
    case 'process': return openProcessTab(r.id, item.title);
    case 'shell': return openGroupWebTermPane(r.id, `${r.id} shell`);
    default: return null;
  }
}

// The hover actions of an item open here: a strip tab pops out or closes; a
// floating one minimizes or closes.
function hoverActions(item, actions, floatsOf) {
  if (item.view === 'open-tab' && item.paneKey) {
    return [
      ['⧉', 'Pop out into a window of its own', () => actions?.popOutPane?.(item.paneKey, { floats: floatsOf(item.paneKey) })],
      ['×', 'Close the tab', () => actions?.closePane?.(item.paneKey)],
    ];
  }
  if (item.view === 'floating') {
    const tab = overlayOf(item);
    if (!tab) return [];
    return [
      ['–', 'Minimize to the dock', () => minimizeOverlayTab(tab.id)],
      // The kind's own close: a page ends and its agent is told, an editor
      // asks before dropping unsaved edits.
      ['×', 'Close the window', () => { const { close } = tabMoves(tab) || {}; return close ? close() : closeOverlayTab(tab.id); }],
    ];
  }
  return [];
}

function RailItem({ item, collapsed, actions, snapshot, notify, floatsOf }) {
  const mark = item.bucket === 'open' ? RAIL_MARK.open : item.bucket === 'minimized' ? RAIL_MARK.minimized
    : item.bucket === 'background' ? (item.kind === 'page' ? pillLabel('background', 'page') : RAIL_MARK.background) : RAIL_MARK.closed;
  const tools = collapsed ? [] : hoverActions(item, actions, floatsOf);
  return html`<div class=${`cr-item cr-${item.bucket}`} data-rail-item=${item.key} data-rail-bucket=${item.bucket}>
    <button data-ui-exempt type="button" class="cr-name" title=${item.group ? `${item.title} · ${item.group}` : item.title}
      onClick=${() => void openRailItem(item, { actions, snapshot, notify })}>
      <span class="cr-glyph" aria-hidden="true">${railGlyph(item.kind)}</span>
      ${!collapsed && html`<span class="cr-title">${item.title}</span>`}
    </button>
    ${!collapsed && html`<span class="cr-tools">${tools.map(([glyph, title, run]) => html`<button data-ui-exempt type="button" key=${glyph}
      class="cr-tool" title=${title} aria-label=${`${title}: ${item.title}`} onClick=${(event) => { event.stopPropagation(); void run(); }}>${glyph}</button>`)}</span>
      <span class=${`cr-mark cr-mark-${item.bucket}`} aria-label=${item.bucket}>${mark}</span>`}
  </div>`;
}

// CollabRail lists every group resource and agent terminal. panes are the
// strip's (the terminal shell's state); snapshot the dashboard's.
export function CollabRail({ panes = [], actions, snapshot, notify = () => {}, floatsOf = () => false }) {
  const [query, setQuery] = useState('');
  const [bucket, setBucket] = useState('all');
  const collapsed = railCollapsed.value;
  const value = snapshot?.value;
  const background = visibleBackgroundTerminals(backgroundTerminals.value, panes);
  const all = railItems({
    resources: value?.group_resources || [], groups: value?.groups || [], places: resourcePlaces.value,
    panes, overlays: overlayTabs.value, background, titleOf: (row) => rowView(row, value).label,
  });
  const { sections, terminals, counts } = filterRail(all, collapsed ? {} : { bucket, query });
  const item = (it) => html`<${RailItem} key=${it.key} item=${it} collapsed=${collapsed} actions=${actions} snapshot=${snapshot} notify=${notify} floatsOf=${floatsOf} />`;
  return html`<aside class=${`collab-rail${collapsed ? ' collapsed' : ''}`} data-collab-rail="" aria-label="Resources and terminals">
    <div class="cr-head">
      ${!collapsed && html`<input class="cr-search" type="search" placeholder="Find a resource or terminal…" aria-label="Find a resource or terminal"
        value=${query} onInput=${(event) => setQuery(event.currentTarget.value)} />`}
      <button data-ui-exempt type="button" class="cr-fold" data-rail-fold="" title=${collapsed ? 'Show the rail' : 'Fold the rail to its icons'}
        aria-expanded=${collapsed ? 'false' : 'true'} onClick=${() => setRailCollapsed(!collapsed)}>${collapsed ? '»' : '«'}</button>
    </div>
    ${!collapsed && html`<div class="cr-chips" role="group" aria-label="Show">
      ${RAIL_FILTERS.map(([key, label]) => html`<button data-ui-exempt type="button" key=${key} class=${`ui-tag cr-chip${bucket === key ? ' ui-tag-accent' : ''}`}
        data-rail-filter=${key} aria-pressed=${bucket === key ? 'true' : 'false'} onClick=${() => setBucket(key)}>${label} ${counts[key]}</button>`)}
    </div>`}
    <div class="cr-list">
      ${sections.map((section) => html`<div key=${section.id} class="cr-section" data-rail-group=${section.name}>
        ${!collapsed && html`<div class="ui-eyebrow cr-section-name">${section.name}</div>`}
        ${section.items.map(item)}
      </div>`)}
      ${terminals.length > 0 && html`<div class="cr-section" data-rail-group="">
        ${!collapsed && html`<div class="ui-eyebrow cr-section-name">Agents' terminals</div>`}
        ${terminals.map(item)}
      </div>`}
      ${!collapsed && !sections.length && !terminals.length && html`<div class="cr-empty ui-empty">${query || bucket !== 'all' ? 'Nothing matches.' : 'No resources or terminals yet.'}</div>`}
    </div>
  </aside>`;
}
