import { h, render } from 'preact';
import { useEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { NOTIFY_TYPES, NOTIFY_DELIVERIES } from './notify-state.js';
import { browserNotifyPermission } from './browser-notify.js';

const html = htm.bind(h);

const TYPE_LABELS = Object.freeze({
  idle: 'Goes idle / finishes',
  awaiting_permission: 'Needs permission',
  awaiting_input: 'Awaits input',
  error: 'Errors',
  exited: 'Exits',
});

const DELIVERY_LABELS = Object.freeze({
  os: 'Desktop (this machine)',
  browser: 'Browser (this dashboard)',
  both: 'Both',
});

// browserPermissionNote returns the one-line caveat shown under the
// delivery selector when a browser channel is chosen but the browser is
// not (yet) able to raise notifications — otherwise the human picks
// "Browser" and silently sees nothing. Empty string = nothing to warn about.
function browserPermissionNote(delivery, permission) {
  if (delivery !== 'browser' && delivery !== 'both') return '';
  switch (permission) {
    case 'granted':
      return '';
    case 'denied':
      return 'Browser notifications are blocked — allow them in your browser’s site settings.';
    case 'unsupported':
      return 'This browser can’t raise notifications here (needs https or localhost).';
    default:
      return 'Your browser hasn’t granted permission yet — pick this to be asked.';
  }
}

export function NotifyApp({ state, actions, documentRef = document }) {
  const rootRef = useRef(null);
  const current = state.view.value;
  const { settings } = current;

  // These listeners are document-scoped because pointerdown outside the island
  // and Escape must dismiss it. The effect cleanup is essential: shell islands
  // can be unmounted independently during a failed mount rollback or pagehide.
  useEffect(() => {
    const onPointerDown = (event) => {
      if (!state.open.value || rootRef.current?.contains(event.target)) return;
      actions.close();
    };
    const onKeyDown = (event) => {
      if (event.key === 'Escape' && state.open.value) actions.close();
    };
    documentRef.addEventListener('pointerdown', onPointerDown);
    documentRef.addEventListener('keydown', onKeyDown);
    return () => {
      documentRef.removeEventListener('pointerdown', onPointerDown);
      documentRef.removeEventListener('keydown', onKeyDown);
    };
  }, [actions, documentRef, state]);

  const bellTitle = current.bellEnabled
    ? 'Notifications ON — click to choose which notifications you want'
    : 'Notifications OFF — nothing notifies, regardless of group/agent bells. Click to configure.';
  // Re-read live each render (not cached): the human may grant/revoke
  // permission in another tab or the browser UI while the popover is open.
  const deliveryNote = browserPermissionNote(settings.delivery, browserNotifyPermission(globalThis));
  const popClass = [current.open && 'open', !settings.enabled && 'master-off'].filter(Boolean).join(' ');

  return html`<span class="notify-bell-wrap" ref=${rootRef}>
    <button class=${`notify-bell${current.bellEnabled ? '' : ' muted'}`} id="notify-global"
      type="button" aria-haspopup="true" aria-expanded=${current.open ? 'true' : 'false'}
      aria-controls="notify-pop" data-enabled=${current.bellEnabled ? '1' : '0'}
      hidden=${!current.bellReady} title=${bellTitle} onClick=${() => { void actions.toggle(); }}>
      ${current.bellEnabled ? '🔔' : '🔕'}
    </button>
    <div id="notify-pop" class=${popClass || undefined} role="group" aria-label="Notification settings">
      <label class="notify-pop-master" title="The master switch — off means nothing notifies, regardless of the per-type or per-group/agent settings.">
        <input type="checkbox" id="notify-pop-enabled" checked=${settings.enabled}
          onChange=${(event) => { void actions.setEnabled(event.currentTarget.checked); }} /> <b>All notifications</b>
      </label>
      <div class="notify-pop-sep"></div>
      <div class="notify-pop-hint" id="notify-pop-types-hint">Notify me when an agent…</div>
      ${NOTIFY_TYPES.map((type) => html`<label class="notify-pop-row" key=${type}>
        <input type="checkbox" data-notify-type=${type} checked=${settings.types[type]}
          onChange=${(event) => { void actions.setType(type, event.currentTarget.checked); }} /> ${TYPE_LABELS[type]}
      </label>`)}
      <div class="notify-pop-sep"></div>
      <label class="notify-pop-row" title="A \`tclaude agent notify-human\` message also raises a desktop banner (it always lands in the Messages tab regardless).">
        <input type="checkbox" id="notify-pop-human" checked=${settings.humanMessages}
          onChange=${(event) => { void actions.setHumanMessages(event.currentTarget.checked); }} /> Sends me a message
      </label>
      <label class="notify-pop-row" title="An agent \`--ask-human\` access request also raises a desktop banner (it always lands in the Messages tab regardless).">
        <input type="checkbox" id="notify-pop-access" checked=${settings.accessRequests}
          onChange=${(event) => { void actions.setAccessRequests(event.currentTarget.checked); }} /> Requests access
      </label>
      <div class="notify-pop-sep"></div>
      <label class="notify-pop-row notify-pop-delivery" title="Where a notification is raised. Desktop uses this machine's notifier; Browser raises it from any open dashboard tab (reaches you when you're remote); Both does each.">
        <span>Deliver via</span>
        <select id="notify-pop-delivery" value=${settings.delivery}
          onChange=${(event) => { void actions.setDelivery(event.currentTarget.value); }}>
          ${NOTIFY_DELIVERIES.map((value) => html`<option key=${value} value=${value}
            selected=${settings.delivery === value}>${DELIVERY_LABELS[value]}</option>`)}
        </select>
      </label>
      ${deliveryNote && html`<div class="notify-pop-hint" id="notify-pop-delivery-note">${deliveryNote}</div>`}
      <div class="notify-pop-sep"></div>
      <button class="notify-pop-config" id="notify-pop-config" type="button"
        title="Open the Config tab for the full notifications settings (cooldown, custom command, advanced rules)."
        onClick=${actions.openConfig}>Config tab ↗</button>
    </div>
  </span>`;
}

export function mountNotifyIsland({
  host,
  state,
  actions,
  registerCleanup,
  documentRef = document,
}) {
  if (!host) throw new TypeError('notify island requires host');
  if (!state?.view) throw new TypeError('notify island requires state');
  if (!actions || typeof actions.toggle !== 'function') throw new TypeError('notify island requires actions');
  if (typeof registerCleanup !== 'function') throw new TypeError('notify island requires registerCleanup');
  registerCleanup(() => {
    state.setOpen(false);
    render(null, host);
  });
  render(html`<${NotifyApp} state=${state} actions=${actions} documentRef=${documentRef} />`, host);
}
