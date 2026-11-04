// collab-home.js -- the Collab view's home (tcl-u58qst, tcl-8nfzbb): with no
// tab open on the strip, what is alive, densely: the agents first (each with
// its group and what it is doing), then the other live resources, then the
// few most recently used closed ones and a line to show the rest. Clicking a
// tile opens it as the panel does; dragging one onto the strip or the home
// opens it as a tab, and dropping it anywhere else floats it
// (collab-rail-actions.js). With nothing to list it shows fallback, the empty
// strip's own hint.

import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { homeView, isAgentItem, itemState, railGlyph } from './collab-rail-model.js';
import { collabItems, openRailItem, pressable, startRailDrag } from './collab-rail-actions.js';

const html = htm.bind(h);

function Tile({ item, ctx }) {
  const agent = isAgentItem(item);
  const state = itemState(item);
  const glyph = agent ? item.status.symbol : railGlyph(item.kind);
  const cls = `ui-card ui-inset ch-tile ch-is-${item.bucket}${agent ? ` ch-agent ch-status-${item.status.className || 'unknown'}` : ''}`;
  return html`<div class=${cls} data-home-tile=${item.key} draggable="true"
    onDragStart=${(event) => startRailDrag(event, item, ctx)}>
    <span role="button" tabIndex="0" class="ch-open" title=${[item.title, item.group, state].filter(Boolean).join(' · ')}
      ...${pressable(() => void openRailItem(item, ctx))}>
      <span class="ch-glyph" aria-hidden="true">${glyph}</span>
      <span class="ch-title">${item.title}</span>
      ${state && html`<span class="ch-state">${state}</span>`}
      ${item.group && html`<span class="ui-tag ch-group">${item.group}</span>`}
    </span>
  </div>`;
}

// CollabHome shows the tiles, or fallback when there is nothing to list. ctx
// is what openRailItem takes, with floatsOf(paneKey).
export function CollabHome({ panes = [], actions, snapshot, notify = () => {}, floatsOf = () => false, fallback = null }) {
  const [showAll, setShowAll] = useState(false);
  const { agents, live, recent, more } = homeView(collabItems(panes, snapshot), { showAll });
  if (!agents.length && !live.length && !recent.length) return fallback;
  const ctx = { actions, snapshot, notify, floatsOf };
  const section = (key, name, items) => items.length > 0 && html`<section key=${key} class="ch-section" data-home-section=${key}>
    <h3 class="ui-eyebrow ch-section-name">${name}</h3>
    <div class="ch-grid">${items.map((item) => html`<${Tile} key=${item.key} item=${item} ctx=${ctx} />`)}</div>
  </section>`;
  const quiet = !agents.length && !live.length;
  return html`<div class="collab-home" data-collab-home="">
    ${quiet && html`<p class="ui-empty ch-quiet">Nothing running.</p>`}
    ${section('agents', 'Agents', agents)}
    ${section('live', 'Running', live)}
    ${section('recent', 'Recent', recent)}
    ${(more > 0 || showAll) && html`<button data-ui-exempt type="button" class="ch-more" data-home-more=""
      onClick=${() => setShowAll(!showAll)}>${showAll ? 'Show fewer' : `${more} more closed · show all`}</button>`}
  </div>`;
}
