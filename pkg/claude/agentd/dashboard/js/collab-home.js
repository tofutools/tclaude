// collab-home.js -- the Collab view's home (tcl-u58qst, tcl-8nfzbb): with no
// tab open on the strip, what is alive, by group as the side panel lists it
// (the agents first, each with its light, then the group's running shells and
// other live items), then a light list of the most recently used closed items
// and a line to show the rest. Clicking a tile opens it as the panel does;
// dragging one onto the strip or the home opens it as a tab, and dropping it
// anywhere else floats it (collab-rail-actions.js). With nothing to list it
// shows fallback, the empty strip's own hint.

import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { homeView, isAgentItem, itemState, needsYou, railGlyph } from './collab-rail-model.js';
import { collabItems, openRailItem, pressable, startRailDrag } from './collab-rail-actions.js';

const html = htm.bind(h);

function Tile({ item, ctx }) {
  const agent = isAgentItem(item);
  const state = itemState(item);
  const glyph = agent ? item.status.symbol : railGlyph(item.kind);
  const cls = `ui-card ui-inset ch-tile ch-is-${item.bucket}${agent ? ` ch-agent ch-status-${item.status.className || 'unknown'}` : ''}`
    + (needsYou(item) ? ' ch-needs-you' : '');
  return html`<div class=${cls} data-home-tile=${item.key} draggable="true"
    onDragStart=${(event) => startRailDrag(event, item, ctx)}>
    <span role="button" tabIndex="0" class="ch-open" title=${[item.title, item.group, state || item.status?.label].filter(Boolean).join(' · ')}
      ...${pressable(() => void openRailItem(item, ctx))}>
      <span class="ch-glyph" aria-hidden="true">${glyph}</span>
      <span class="ch-title">${item.title}</span>
      ${state && html`<span class="ch-state">${state}</span>`}
    </span>
  </div>`;
}

// RecentItem is a closed item in the light Recent list: its name, and its
// group in muted text.
function RecentItem({ item, ctx }) {
  return html`<li class="ch-recent-item" data-home-tile=${item.key} draggable="true"
    onDragStart=${(event) => startRailDrag(event, item, ctx)}>
    <span role="button" tabIndex="0" class="ch-recent-open" title=${`Reopen ${item.title}`}
      ...${pressable(() => void openRailItem(item, ctx))}>
      <span class="ch-glyph" aria-hidden="true">${railGlyph(item.kind)}</span>${item.title}${item.group && html`<span class="ch-recent-group">${item.group}</span>`}
    </span>
  </li>`;
}

// CollabHome shows the tiles, or fallback when there is nothing to list. ctx
// is what openRailItem takes, with floatsOf(paneKey).
export function CollabHome({ panes = [], actions, snapshot, notify = () => {}, floatsOf = () => false, fallback = null }) {
  const [showAll, setShowAll] = useState(false);
  const { groups, recent, more } = homeView(collabItems(panes, snapshot), { showAll });
  if (!groups.length && !recent.length) return fallback;
  const ctx = { actions, snapshot, notify, floatsOf };
  return html`<div class="collab-home" data-collab-home="">
    ${!groups.length && html`<p class="ui-empty ch-quiet">Nothing running.</p>`}
    ${groups.map((g) => html`<section key=${g.id} class="ch-section" data-home-group=${g.name}>
      <h3 class="ui-eyebrow ch-section-name">${g.name}</h3>
      <div class="ch-grid">${g.items.map((item) => html`<${Tile} key=${item.key} item=${item} ctx=${ctx} />`)}</div>
    </section>`)}
    ${recent.length > 0 && html`<section class="ch-section ch-recent" data-home-section="recent">
      <h3 class="ui-eyebrow ch-section-name">Recent</h3>
      <ul class="ch-recent-list">${recent.map((item) => html`<${RecentItem} key=${item.key} item=${item} ctx=${ctx} />`)}</ul>
      ${(more > 0 || showAll) && html`<button data-ui-exempt type="button" class="ch-more" data-home-more=""
        onClick=${() => setShowAll(!showAll)}>${showAll ? 'Show fewer' : `${more} more closed · show all`}</button>`}
    </section>`}
  </div>`;
}
