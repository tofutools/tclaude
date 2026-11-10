// collab-home.js -- the Collab view's home (tcl-u58qst, tcl-bcki2c): with no
// tab open on the strip, a plain empty state. It says nothing is open, offers
// to open the side panel (which lists everything), and names the agents that
// need the operator, each one click from focus. With nothing at all to list
// it shows fallback, the empty strip's own hint.

import { h } from 'preact';
import htm from 'htm';
import { needsYou } from './collab-rail-model.js';
import { collabItems, openRailItem } from './collab-rail-actions.js';
import { setSidePanelOpen, sidePanelShown } from './side-panel.js';
import { Button } from './ui-components.js';

const html = htm.bind(h);

// CollabHome is the empty strip's body. ctx is what openRailItem takes.
export function CollabHome({ panes = [], actions, snapshot, notify = () => {}, fallback = null }) {
  const { sections } = collabItems(panes, snapshot);
  if (!sections.length) return fallback;
  const waiting = sections.flatMap((s) => s.items).filter(needsYou);
  const ctx = { actions, snapshot, notify };
  const shown = sidePanelShown.value;
  const canOpen = shown.name === 'collab' && !shown.open;
  return html`<div class="collab-home" data-collab-home="">
    <p class="ch-title">Nothing open</p>
    ${canOpen && html`<${Button} size="sm" class="ch-open-panel" data-home-open-panel=""
      onClick=${() => setSidePanelOpen(true, 'collab')}>Open side panel<//>`}
    ${waiting.length ? html`<ul class="ch-waiting" aria-label="Agents that need you">
      ${waiting.map((item) => html`<li key=${item.key}>
        <${Button} size="sm" class="ch-agent" data-home-agent=${item.key}
          title=${[item.group, item.status?.label].filter(Boolean).join(' · ')}
          onClick=${() => void openRailItem(item, ctx)}>
          <span class="ch-glyph" aria-hidden="true">${item.status.symbol}</span>${item.title}<span class="ch-state">needs you</span>
        <//>
      </li>`)}
    </ul>` : html`<p class="ui-empty ch-quiet">No agents need you.</p>`}
  </div>`;
}
