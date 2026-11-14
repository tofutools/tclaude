// phone-desktop-only.js -- what a phone says about a view it does not offer.
//
// Some views need a wide screen to be worth anything (the route map, Fleet
// admin, Vegas) or are authoring tools a phone should not pretend to be (process
// templates, plugin and drawing board editors). A phone shows these instead:
// DesktopOnlyNote, a line inside a phone view naming what is left out, and
// DesktopOnlyPage, which stands in for a whole view. Both link to the desktop
// layout of the same page (setLayoutOverride keeps the URL).

import { h } from 'preact';
import htm from 'htm';
import { setLayoutOverride } from './layout.js';
import { Button } from './ui-components.js';

const html = htm.bind(h);

export const desktopSite = () => setLayoutOverride('desktop');

// DESKTOP_ONLY_TABS are the dashboard tabs a phone does not show at all, by
// the name the phone page gives them.
export const DESKTOP_ONLY_TABS = Object.freeze({
  'fleet-admin': 'Fleet admin',
  vegas: 'Vegas',
});

// desktopOnlyView names the view a phone should not show for the active tab,
// or '' when the tab has a phone view. groupsSubview is the Groups island's
// subview: its route map is desktop only, the agent list is not.
export function desktopOnlyView(activeTab, groupsSubview = '') {
  if (activeTab === 'groups' && groupsSubview === 'routes') return 'Route map';
  return DESKTOP_ONLY_TABS[activeTab] || '';
}

// DesktopOnlyNote is one line naming what a phone view leaves out ("Editing
// overrides is", "Catalog and editor are": `what` carries its own verb).
export function DesktopOnlyNote({ what, open = desktopSite }) {
  return html`<p class="phone-desktop-only">${what} <b>desktop only</b>.${' '}<button data-ui-exempt type="button" class="phone-link" onClick=${() => open()}>Desktop site ›</button></p>`;
}

// DesktopOnlyPage stands in for a whole view. `other` is a way on within the
// phone layout ({label, onClick}), when the view sits on top of one.
export function DesktopOnlyPage({ what, open = desktopSite, other = null }) {
  return html`<div class="phone-desktop-page">
    <div class="phone-desktop-page-icon" aria-hidden="true">🖥</div>
    <h2 class="phone-desktop-page-title">${what} is desktop only</h2>
    <p class="phone-desktop-page-body">It needs a wide screen to be useful. Switch this page to the desktop layout, or open it on a computer.</p>
    <${Button} variant="primary" class="phone-desktop-page-go" onClick=${() => open()}>Desktop site<//>
    ${other && html`<${Button} class="phone-desktop-page-go" onClick=${other.onClick}>${other.label}<//>`}
  </div>`;
}
