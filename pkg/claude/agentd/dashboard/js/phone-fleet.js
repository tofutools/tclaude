// phone-fleet.js -- Fleet on a phone (data-layout="phone"), read-only.
//
// The fleet-admin tab shows, in place of the desktop's Fleet admin: this node
// and its hub connection, the trusted peers (online, or when last seen, and
// the access level they have here) and the instances asking to be trusted.
// Trusting, grants, the hub settings and every other Fleet admin section stay
// on the desktop: a link to one of them (/fleet-admin/<section>) gets the
// desktop-only page. It reads fleetState, which the fleet island's status
// poller keeps fresh on a phone too, and mutates nothing.

import { h, render } from 'preact';
import htm from 'htm';
import { fleetAdminPage } from './fleet-admin-page.js';
import { hubOffReason } from './fleet-model.js';
import { relTime } from './helpers.js';
import { DesktopOnlyNote, DesktopOnlyPage } from './phone-desktop-only.js';

const html = htm.bind(h);

// PHONE_SECTIONS are the Fleet admin sections a phone shows: the landing
// and Peers, both as this view.
const PHONE_SECTIONS = new Set(['', 'peers']);

const HUB_TEXT = Object.freeze({
  connected: 'hub connected',
  connecting: 'connecting to the hub…',
  disconnected: 'hub unreachable, retrying',
  refused: 'the hub refused the connection',
  off: 'federation is off',
});

const hostOf = (url) => {
  try { return new URL(url).host; } catch { return url || ''; }
};

function ThisNode({ node }) {
  const hub = node.hub;
  const text = HUB_TEXT[hub] || hub;
  const url = hub !== 'off' ? hostOf(node.details?.url) : '';
  return html`<div class="phone-fleet-self ui-card" data-key="fleet-self">
    <div class="phone-fleet-name">${node.self.name} <span class="ui-tag">this node</span></div>
    <div class="phone-fleet-id" title=${node.self.id}>${node.self.id}</div>
    <div class=${`phone-fleet-hub hub-${hub}`} title=${hubOffReason(node) || undefined}>
      ${text}${url ? ` · ${url}` : ''}${hub === 'refused' && node.error ? ` (${node.error})` : ''}
    </div>
  </div>`;
}

function Peer({ peer }) {
  return html`<li class="phone-fleet-row" data-key=${`peer-${peer.id}`}>
    <span class=${`phone-fleet-dot ${peer.online ? 'online' : ''}`} aria-hidden="true"></span>
    <span class="phone-fleet-text">
      <span class="phone-fleet-name">${peer.name}</span>
      <span class="phone-fleet-sub">${peer.level}${peer.partTime ? ' · part-time' : ''}</span>
    </span>
    <span class="phone-fleet-when">${peer.online ? 'online' : peer.lastSeen ? relTime(peer.lastSeen) : 'offline'}</span>
  </li>`;
}

function Asking({ stranger }) {
  const at = stranger.request?.receivedAt;
  const note = stranger.request?.note || '';
  return html`<li class="phone-fleet-row" data-key=${`asking-${stranger.id}`}>
    <span class=${`phone-fleet-dot asking`} aria-hidden="true"></span>
    <span class="phone-fleet-text">
      <span class="phone-fleet-name">${stranger.label}</span>
      <span class="phone-fleet-sub">${note ? `“${note}”` : 'asks to be trusted'}${stranger.request?.trustsYou ? ' · trusts this node' : ''}</span>
    </span>
    ${at && html`<span class="phone-fleet-when">${relTime(new Date(at).toISOString())}</span>`}
  </li>`;
}

export function PhoneFleet({ state, page = fleetAdminPage }) {
  if (!PHONE_SECTIONS.has(page.value)) return html`<${DesktopOnlyPage} what="Fleet admin" />`;
  const current = state.view.value;
  const node = current.node;
  if (!node) {
    return html`<div class="phone-fleet">
      <p class="ui-empty phone-empty">${current.statusLoaded ? 'This node is not set up for a fleet.' : 'Loading…'}</p>
      <${DesktopOnlyNote} what="Setting up a fleet is" />
    </div>`;
  }
  const peers = current.fleet?.peers || [];
  const asking = (current.strangers || []).filter((stranger) => stranger.request);
  return html`<div class="phone-fleet">
    <${ThisNode} node=${node} />
    <div class="phone-section" role="group" aria-label="Trusted peers">
      <h2 class="phone-section-head ui-eyebrow">Trusted peers · ${peers.length}</h2>
      ${peers.length
        ? html`<ul class="phone-list">${peers.map((peer) => html`<${Peer} key=${peer.id} peer=${peer} />`)}</ul>`
        : html`<p class="ui-empty phone-empty">No trusted peers yet.</p>`}
    </div>
    ${asking.length > 0 && html`<div class="phone-section" role="group" aria-label="Waiting to be trusted">
      <h2 class="phone-section-head ui-eyebrow">Waiting to be trusted · ${asking.length}</h2>
      <ul class="phone-list">${asking.map((stranger) => html`<${Asking} key=${stranger.id} stranger=${stranger} />`)}</ul>
    </div>`}
    <${DesktopOnlyNote} what="Trust, grants, hub settings and the rest of Fleet admin are" />
  </div>`;
}

export function mountPhoneFleetIsland({ host, state, registerCleanup }) {
  render(html`<${PhoneFleet} state=${state} />`, host);
  registerCleanup(() => render(null, host));
}
