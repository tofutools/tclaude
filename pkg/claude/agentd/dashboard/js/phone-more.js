// phone-more.js -- the phone shell's More sheet: the node on screen (and the
// node picker), every tab the bottom nav has no slot for with its scope
// badge, the power actions, whether notifications are on and the switch to
// the desktop site, and Update tclaude (phone-update.js). Also the Power
// sheet behind the Agents bar's ⏻.

import { h } from 'preact';
import htm from 'htm';
import { footerMetaView } from './shell-model.js';
import { MORE_TABS } from './phone-shell-model.js';
import { NodeChip } from './phone-node-picker.js';
import { ScopeBadge } from './phone-ui.js';
import { featureState } from './feature-state-registry.js';
import { openSettingsSection } from './phone-config-island.js';
import { openPhoneSheet } from './phone-sheet.js';
import { UpdateSheet, canUpdateHere } from './phone-update.js';

const html = htm.bind(h);

// DiagnosticsCounts is the Diagnostics entry's badges: the Doctor's decisions
// waiting for the operator, and its blocking problems, as the desktop's
// Diagnostics tab shows them (doctor-island.js DoctorBadge).
export function DiagnosticsCounts({ doctor = featureState('doctor') }) {
  const { decision = 0, blocking = 0 } = doctor?.counts?.value || {};
  return html`${decision > 0 && html`<span class="ui-badge ui-badge-accent phone-more-count" title="Decisions waiting for you">${decision}</span>`}${
    blocking > 0 && html`<span class="ui-badge ui-badge-danger phone-more-count" title="Blocking problems">${blocking}</span>`}`;
}

// openNotifications opens Settings › Notifications. Already on its
// location (the Settings list names the section it last showed), the
// navigation records no entry, so ‹ shows the list in place.
const NOTIFICATIONS_PATH = '/config/notifications';
function openNotifications(go, location = globalThis.location) {
  const pushed = location?.pathname !== NOTIFICATIONS_PATH;
  go({ path: NOTIFICATIONS_PATH });
  openSettingsSection('notifications', pushed);
}

function Entry({ icon, label, after = null, end = null, danger = false, disabled = false, title, onClick }) {
  return html`<li><button data-ui-exempt type="button" class=${`phone-menu-item${danger ? ' phone-menu-danger' : ''}`} disabled=${disabled} title=${title} onClick=${onClick}>
    <span class="phone-menu-icon" aria-hidden="true">${icon}</span><span class="phone-menu-label">${label}</span>${after}${end}
  </button></li>`;
}

// MoreMenu is the More sheet: the node chip, every other tab with what a
// phone can do there, the power actions, notifications and the layout switch.
export function MoreMenu({ state, actions, close, fleet = null, doctor = featureState('doctor') }) {
  const go = (entry) => {
    close();
    if (entry.byAnchor) actions.showTab(entry.tab);
    else actions.navigate(entry.path);
  };
  const run = (act) => { close(); act(); };
  const snap = state.snapshot.value;
  const meta = footerMetaView(snap);
  const notifying = !!snap?.notifications_enabled;
  const updatable = canUpdateHere(snap);
  return html`${fleet && html`<div class="phone-more-node"><${NodeChip} fleet=${fleet} /></div>`}
    <ul class="phone-menu">
      ${MORE_TABS.filter((entry) => entry.byAnchor || actions.tabAvailable(entry.tab)).map((entry) => html`<${Entry} key=${entry.tab}
        icon=${entry.icon} label=${entry.label} onClick=${() => go(entry)}
        after=${entry.tab === 'diagnostics' ? html`<${DiagnosticsCounts} doctor=${doctor} />` : null}
        end=${entry.scope && html`<span class="phone-menu-end"><${ScopeBadge} kind=${entry.scope.kind}>${entry.scope.text}<//></span>`} />`)}
    </ul>
    <h2 class="ui-eyebrow phone-menu-head">Actions</h2>
    <ul class="phone-menu">
      <${Entry} icon="🟢" label="Power on all" onClick=${() => run(actions.powerOnAll)} />
      <${Entry} icon="🛑" label="Shut down all…" danger onClick=${() => run(actions.shutdownAll)} />
      <${Entry} icon="🔔" label="Notifications" onClick=${() => openNotifications(go)}
        end=${snap && html`<span class="phone-menu-end phone-menu-note">${notifying ? 'on' : 'off'}</span>`} />
      <${Entry} icon="⬆" label="Update tclaude" disabled=${!updatable} title=${updatable ? undefined : 'This node does not let you update it (needs node.update)'}
        onClick=${() => openPhoneSheet({ label: 'Update tclaude', Body: UpdateSheet, props: { navigate: actions.navigate } })}
        end=${!updatable && html`<span class="phone-menu-end phone-menu-note">needs node.update</span>`} />
      <${Entry} icon="🖥" label="Desktop site" onClick=${() => run(actions.desktopSite)}
        end=${html`<span class="phone-menu-end phone-menu-note">this device</span>`} />
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
