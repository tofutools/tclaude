// phone-more.js -- the phone shell's More sheet: the node on screen (and the
// node picker), every tab the bottom nav has no slot for with its scope
// badge, the power actions, whether notifications are on and the switch to
// the desktop site, and Update tclaude (phone-update.js).

import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { footerMetaView } from './shell-model.js';
import { MORE_TABS } from './phone-shell-model.js';
import { NodeChip } from './phone-node-picker.js';
import { ScopeBadge, SheetHead } from './phone-ui.js';
import { featureState } from './feature-state-registry.js';
import { openSettingsSection } from './phone-config-island.js';
import { openPhoneSheet } from './phone-sheet.js';
import { UpdateSheet, canUpdateHere } from './phone-update.js';
import { accessAlertsChange, accessAlertsOn, accessAlertsToast } from './phone-access-alerts.js';
import { remoteNodeID } from './fleet-model.js';
import { requestBrowserNotifyPermission } from './browser-notify.js';
import { shellToast } from './shell-state.js';

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

// AccessAlertsEntry is "Alert me on access requests": one tap sets the
// three notification settings an access-request banner needs on a phone
// (phone-access-alerts.js), and asks this browser for permission from that
// tap. It reads the settings when More opens, and shows nothing until then.
export function AccessAlertsEntry({ notify = () => {}, fetchImpl = globalThis.fetch, win = globalThis, remote = !!remoteNodeID() }) {
  const [settings, setSettings] = useState(null);
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    let live = true;
    if (remote || typeof fetchImpl !== 'function') return undefined;
    Promise.resolve().then(() => fetchImpl('/api/notifications', { credentials: 'same-origin' }))
      .then((response) => (response.ok ? response.json() : null))
      .then((body) => { if (live && body) setSettings(body); })
      .catch(() => {});
    return () => { live = false; };
  }, []);
  // On a peer's page the settings are that node's, and its banners never
  // reach this phone: the row belongs to this node only.
  if (remote || !settings) return null;
  const on = accessAlertsOn(settings);
  const flip = async () => {
    setBusy(true);
    // The browser's permission is asked first, straight from the tap: iOS
    // Safari and Firefox drop a prompt that comes after network round trips.
    const permission = on ? null : requestBrowserNotifyPermission(win);
    try {
      const response = await fetchImpl('/api/notifications', {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(accessAlertsChange(settings, !on)),
      });
      if (!response.ok) throw new Error((await response.text()) || `HTTP ${response.status}`);
      setSettings(await response.json());
      if (!on) {
        const granted = await permission;
        notify(accessAlertsToast(granted, win), granted !== 'granted');
      } else {
        notify('Access-request alerts off.');
      }
    } catch (error) {
      notify(`Could not change access-request alerts: ${error?.message || error}`, true);
    } finally {
      setBusy(false);
    }
  };
  return html`<${Entry} icon="🔑" label="Alert me on access requests" disabled=${busy} onClick=${flip}
    title="Raise a notification on this phone when an agent asks for access (while the app is open)"
    end=${html`<span class="phone-menu-end phone-menu-note">${busy ? '…' : on ? 'on' : 'off'}</span>`} />`;
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
    <${SheetHead}>Actions<//>
    <ul class="phone-menu">
      <${Entry} icon="🟢" label="Power on all" onClick=${() => run(actions.powerOnAll)} />
      <${Entry} icon="🛑" label="Shut down all…" danger onClick=${() => run(actions.shutdownAll)} />
      <${Entry} icon="🔔" label="Notifications" onClick=${() => openNotifications(go)}
        end=${snap && html`<span class="phone-menu-end phone-menu-note">${notifying ? 'on' : 'off'}</span>`} />
      <${AccessAlertsEntry} notify=${shellToast} />
      <${Entry} icon="⬆" label="Update tclaude" disabled=${!updatable} title=${updatable ? undefined : 'This node does not let you update it (needs node.update)'}
        onClick=${() => openPhoneSheet({ label: 'Update tclaude', Body: UpdateSheet, props: { navigate: actions.navigate } })}
        end=${!updatable && html`<span class="phone-menu-end phone-menu-note">needs node.update</span>`} />
      <${Entry} icon="🖥" label="Desktop site" onClick=${() => run(actions.desktopSite)}
        end=${html`<span class="phone-menu-end phone-menu-note">this device</span>`} />
    </ul>
    ${meta && html`<p class="phone-sheet-meta">tclaude ${meta.version}</p>`}`;
}
