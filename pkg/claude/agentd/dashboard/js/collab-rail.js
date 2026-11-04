// collab-rail.js -- the Collab view's side panel (tcl-lx7nb7, tcl-8nfzbb):
// the live agents and resources, by group, in the dashboard's shared right
// side panel (side-panel.js), folded until the operator opens it. Active
// shows by default; Closed and a search find the rest. A group's header folds
// it. Clicking an item opens it or brings it forward. Dragging it onto the
// strip opens it as a tab there, and dropping it anywhere else on the page
// floats it (collab-rail-actions.js). Hovering one open here offers to
// minimize, pop out or close it. What it lists comes from collab-rail-model.js.

import { h, render } from 'preact';
import { useEffect, useLayoutEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { signal } from '@preact/signals';
import { DEFAULT_RAIL_FILTER, RAIL_FILTERS, filterRail, isAgentItem, itemState, needsYou, railFilterOK, railGlyph } from './collab-rail-model.js';
import { closeOverlayTab, minimizeOverlayTab } from './overlay-instances.js';
import { tabMoves } from './overlay-tab-moves.js';
import { dashPrefs } from './prefs.js';
import { offerSidePanel, registerSidePanel } from './side-panel.js';
import { collabItems, openRailItem, overlayOf, pressable, startRailDrag } from './collab-rail-actions.js';

export { openRailItem };

const html = htm.bind(h);

const OPEN_PREF = 'tclaude.dash.collab.panel.open';
const FILTER_PREF = 'tclaude.dash.collab.filter';
const FOLDED_PREF = 'tclaude.dash.collab.folded-groups';

// The Collab view's panel in the shared right side panel: folded by default,
// and offered only while the view is mounted (CollabRail below).
registerSidePanel('collab', {
  pane: 'tab-terminals', pref: OPEN_PREF, open: false, body: '#dock-collab-body', needsOffer: true,
  titles: [['Hide the Collab panel', 'Hide the Collab panel'], ['Show the Collab panel', 'Show the Collab panel']],
});

function readFilter() {
  const stored = dashPrefs.getItem(FILTER_PREF);
  return railFilterOK(stored) ? stored : DEFAULT_RAIL_FILTER;
}

function readFolded() {
  try {
    const raw = JSON.parse(dashPrefs.getItem(FOLDED_PREF) || '[]');
    return new Set(Array.isArray(raw) ? raw.filter((name) => typeof name === 'string') : []);
  } catch {
    return new Set();
  }
}

// railFolded is the group headers folded in the panel, by group name.
const railFolded = signal(null);

function toggleFolded(name) {
  const next = new Set(railFolded.peek() || readFolded());
  if (next.has(name)) next.delete(name); else next.add(name);
  railFolded.value = next;
  dashPrefs.setItem(FOLDED_PREF, JSON.stringify([...next]));
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

const statusClass = (item) => (isAgentItem(item) ? `cr-agent cr-status-${item.status.className || 'unknown'}${needsYou(item) ? ' cr-needs-you' : ''}` : '');

function RailItem({ item, actions, snapshot, notify, floatsOf }) {
  const tools = hoverActions(item, actions, floatsOf);
  const state = itemState(item);
  const glyph = isAgentItem(item) ? item.status.symbol : railGlyph(item.kind);
  return html`<div class=${`cr-item cr-${item.bucket} ${statusClass(item)}`} data-rail-item=${item.key} data-rail-bucket=${item.bucket}
    draggable="true" onDragStart=${(event) => startRailDrag(event, item, { actions, snapshot, notify, floatsOf })}>
    <span role="button" tabIndex="0" class="cr-name" title=${[item.title, item.group, state || item.status?.label].filter(Boolean).join(' · ')}
      ...${pressable(() => void openRailItem(item, { actions, snapshot, notify }))}>
      <span class="cr-glyph" aria-hidden="true">${glyph}</span>
      <span class="cr-title">${item.title}</span>
    </span>
    <span class="cr-tools">${tools.map(([g, title, run]) => html`<button data-ui-exempt type="button" key=${g}
      class="cr-tool" title=${title} aria-label=${`${title}: ${item.title}`} onClick=${(event) => { event.stopPropagation(); void run(); }}>${g}</button>`)}</span>
    ${state && html`<span class="cr-state">${state}</span>`}
  </div>`;
}

function RailList({ panes, actions, snapshot, notify, floatsOf }) {
  const [query, setQuery] = useState('');
  const [bucket, setBucketState] = useState(readFilter);
  const setBucket = (next) => { setBucketState(next); dashPrefs.setItem(FILTER_PREF, next); };
  const folded = railFolded.value || readFolded();
  const searching = !!query.trim();
  const { sections, counts } = filterRail(collabItems(panes, snapshot), { bucket, query });
  const item = (it) => html`<${RailItem} key=${it.key} item=${it} actions=${actions} snapshot=${snapshot} notify=${notify} floatsOf=${floatsOf} />`;
  const empty = searching ? 'Nothing matches.' : bucket === 'closed' ? 'Nothing closed.' : 'Nothing running.';
  return html`<div class="collab-rail" data-collab-rail="" aria-label="Agents and resources">
    <div class="cr-head">
      <input class="cr-search" type="search" placeholder="Find…" aria-label="Find an agent or resource"
        value=${query} onInput=${(event) => setQuery(event.currentTarget.value)} />
      <div class="cr-chips" role="group" aria-label="Show">
        ${RAIL_FILTERS.map(([key, label]) => html`<button data-ui-exempt type="button" key=${key}
          class=${`cr-chip${!searching && bucket === key ? ' is-on' : ''}`} data-rail-filter=${key}
          aria-pressed=${!searching && bucket === key ? 'true' : 'false'} onClick=${() => setBucket(key)}>${label} <span class="cr-count">${counts[key]}</span></button>`)}
      </div>
    </div>
    <div class="cr-list">
      ${sections.map((section) => {
        const shut = !searching && folded.has(section.name);
        return html`<div key=${section.id} class=${`cr-section${shut ? ' is-folded' : ''}`} data-rail-group=${section.name}>
          <button data-ui-exempt type="button" class="cr-section-head" aria-expanded=${shut ? 'false' : 'true'}
            onClick=${() => toggleFolded(section.name)}>
            <span class="cr-chevron" aria-hidden="true">${shut ? '▸' : '▾'}</span>
            <span class="ui-eyebrow cr-section-name">${section.name}</span>
            <span class="cr-count">${section.items.length}</span>
          </button>
          ${!shut && section.items.map(item)}
        </div>`;
      })}
      ${!sections.length && html`<p class="ui-empty cr-quiet">${empty}</p>`}
    </div>
  </div>`;
}

// CollabRail renders the panel body into the shared side panel's Collab slot
// (#dock-collab-body) and offers the panel while it is mounted. Without the
// shell (a page or a test with no dashboard chrome) it renders in place.
// panes are the strip's (the terminal shell's state); snapshot the dashboard's.
export function CollabRail(props) {
  const doc = globalThis.document;
  const host = doc?.getElementById?.('dock-collab-body') || null;
  useEffect(() => {
    if (!host) return undefined;
    offerSidePanel('collab', true);
    return () => {
      offerSidePanel('collab', false);
      render(null, host);
    };
  }, [host]);
  useLayoutEffect(() => {
    if (host) render(html`<${RailList} ...${props} />`, host);
  });
  return host ? null : html`<aside class="collab-rail-inline">${html`<${RailList} ...${props} />`}</aside>`;
}
