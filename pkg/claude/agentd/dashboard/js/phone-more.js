// phone-more.js -- the phone shell's More sheet: the node on screen (and the
// node picker), every tab the bottom nav has no slot for, the power actions
// and the switch to the desktop site. Also the Power sheet behind the
// Agents bar's ⏻.

import { h } from 'preact';
import htm from 'htm';
import { footerMetaView } from './shell-model.js';
import { MORE_TABS } from './phone-shell-model.js';
import { NodeChip } from './phone-node-picker.js';

const html = htm.bind(h);

// MoreMenu is the More sheet: the node chip, every other tab, the power
// actions and the layout switch.
export function MoreMenu({ state, actions, close, fleet = null }) {
  const go = (entry) => {
    close();
    if (entry.byAnchor) actions.showTab(entry.tab);
    else actions.navigate(entry.path);
  };
  const run = (act) => { close(); act(); };
  const meta = footerMetaView(state.snapshot.value);
  return html`${fleet && html`<div class="phone-more-node"><${NodeChip} fleet=${fleet} /></div>`}
    <ul class="phone-menu">
      ${MORE_TABS.filter((entry) => entry.byAnchor || actions.tabAvailable(entry.tab)).map((entry) => html`<li key=${entry.tab}>
        <button data-ui-exempt type="button" class="phone-menu-item" onClick=${() => go(entry)}>
          <span class="phone-menu-icon" aria-hidden="true">${entry.icon}</span>${entry.label}
        </button>
      </li>`)}
    </ul>
    <h2 class="ui-eyebrow phone-menu-head">Actions</h2>
    <ul class="phone-menu">
      <li><button data-ui-exempt type="button" class="phone-menu-item" onClick=${() => run(actions.powerOnAll)}>
        <span class="phone-menu-icon" aria-hidden="true">🟢</span>Power on all</button></li>
      <li><button data-ui-exempt type="button" class="phone-menu-item phone-menu-danger" onClick=${() => run(actions.shutdownAll)}>
        <span class="phone-menu-icon" aria-hidden="true">🛑</span>Shut down all…</button></li>
      <li><button data-ui-exempt type="button" class="phone-menu-item" onClick=${() => run(actions.desktopSite)}>
        <span class="phone-menu-icon" aria-hidden="true">🖥</span>Desktop site</button></li>
    </ul>
    ${meta && html`<p class="phone-sheet-meta">tclaude ${meta.version}</p>`}`;
}

// PowerMenu is the Power sheet: power every agent on, or shut them all down
// (the desktop's own confirmation follows).
export function PowerMenu({ actions, close }) {
  const run = (act) => { close(); act(); };
  return html`<h2 class="ui-eyebrow phone-menu-head">Power</h2>
    <ul class="phone-menu">
      <li><button data-ui-exempt type="button" class="phone-menu-item" onClick=${() => run(actions.powerOnAll)}>
        <span class="phone-menu-icon" aria-hidden="true">🟢</span>Power on all</button></li>
      <li><button data-ui-exempt type="button" class="phone-menu-item phone-menu-danger" onClick=${() => run(actions.shutdownAll)}>
        <span class="phone-menu-icon" aria-hidden="true">🛑</span>Shut down all…</button></li>
    </ul>`;
}
