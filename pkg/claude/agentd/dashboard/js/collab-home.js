// collab-home.js -- the Collab view's home (tcl-u58qst): with no tab open on
// the strip, everything the rail lists shows as tiles, by group, then the
// agents' terminals, each with its state pill. Clicking a tile opens it as the
// rail does; dragging one onto the strip or the home opens it as a tab, and
// dropping it anywhere else floats it (collab-rail-actions.js). With nothing to
// list it shows fallback, the empty strip's own hint.

import { h } from 'preact';
import htm from 'htm';
import { railGlyph } from './collab-rail-model.js';
import { collabItems, openRailItem, pressable, startRailDrag } from './collab-rail-actions.js';
import { ResourcePill } from './resource-pill.js';

const html = htm.bind(h);

const KIND_LABEL = Object.freeze({ page: 'Live page', board: 'Board', process: 'Process', shell: 'Group shell', terminal: 'Terminal' });

function Tile({ item, ctx }) {
  return html`<div class=${`ui-card ui-inset ch-tile ch-is-${item.bucket}`} data-home-tile=${item.key} draggable="true"
    onDragStart=${(event) => startRailDrag(event, item, ctx)}>
    <span role="button" tabIndex="0" class="ch-open" title=${`Open ${item.title}, or drag it onto the strip or out to float it`}
      ...${pressable(() => void openRailItem(item, ctx))}>
      <span class="ch-glyph" aria-hidden="true">${railGlyph(item.kind)}</span>
      <span class="ch-text">
        <span class="ch-title">${item.title}</span>
        <span class="ch-kind">${KIND_LABEL[item.kind] || item.kind}</span>
      </span>
    </span>
    <${ResourcePill} view=${item.view} kind=${item.kind} />
  </div>`;
}

// CollabHome shows the tiles, or fallback when there are none. ctx is what
// openRailItem takes, with floatsOf(paneKey).
export function CollabHome({ panes = [], actions, snapshot, notify = () => {}, floatsOf = () => false, fallback = null }) {
  const { sections, terminals } = collabItems(panes, snapshot);
  if (!sections.length && !terminals.length) return fallback;
  const ctx = { actions, snapshot, notify, floatsOf };
  const section = (key, name, items) => html`<section key=${key} class="ch-section" data-home-group=${name}>
    <div class="ui-eyebrow ch-section-name">${name}</div>
    <div class="ch-grid">${items.map((item) => html`<${Tile} key=${item.key} item=${item} ctx=${ctx} />`)}</div>
  </section>`;
  return html`<div class="collab-home" data-collab-home="">
    <div class="ch-head">
      <div class="mux-empty-title">Nothing open on the strip</div>
      <div class="ch-hint">Click a tile to open it, drag it onto the strip above to open it as a tab, or drop it anywhere else to float it.</div>
    </div>
    ${sections.map((s) => section(s.id, s.name, s.items))}
    ${terminals.length > 0 && section('terminals', "Agents' terminals", terminals)}
  </div>`;
}
