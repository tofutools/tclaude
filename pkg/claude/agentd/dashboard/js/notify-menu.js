// notify-menu.js — the top-bar bell's notification-settings popover.
//
// The bell glyph (🔔/🔕) is painted by renderNotifyGlobal (render.js) from
// every snapshot poll — it mirrors config.notifications.enabled, the
// master switch ABOVE the per-group / per-agent filters. This module owns
// the popover that drops from the bell on click:
//
//   • the master on/off (everything muted when off);
//   • the per-type checklist — which session-state transitions raise a
//     desktop banner (idle / needs-permission / awaits-input / error /
//     exited). Each box is one wildcard transition rule server-side;
//   • the human-message knob (a `tclaude agent notify-human` ping also
//     raising a desktop banner — it always lands in the Messages tab).
//   • the access-request knob (an agent `--ask-human` request also raising
//     a desktop banner — it always lands in the Messages tab).
//
// All of it is backed by GET/POST /api/notifications — the lightweight
// twin of the Config tab's full-config editor for the same notifications
// block. State is fetched on every open (so a Config-tab edit, another
// browser, or a CLI change is reflected) and the POST response re-seeds
// the widgets after each change, so the popover never drifts from disk.
//
// NB: the per-type checklist only governs the events tclaude turns into
// *desktop banners*. The underlying state transitions are still recorded
// — unchecking "Exits" silences the banner, it does not stop tclaude
// noticing the agent exited.

import { $, $$ } from './helpers.js';
import { toast } from './refresh.js';

function popover() { return $('#notify-pop'); }
function bell() { return $('#notify-global'); }

function isOpen() {
  const pop = popover();
  return !!pop && pop.classList.contains('open');
}

function setOpen(open) {
  const pop = popover();
  const btn = bell();
  if (!pop || !btn) return;
  pop.classList.toggle('open', open);
  btn.setAttribute('aria-expanded', open ? 'true' : 'false');
}

// paint reflects a /api/notifications state object onto the widgets.
function paint(state) {
  if (!state || typeof state !== 'object') return;
  const enabled = !!state.enabled;
  const master = $('#notify-pop-enabled');
  if (master) master.checked = enabled;

  const types = state.types || {};
  $$('#notify-pop [data-notify-type]').forEach(cb => {
    cb.checked = !!types[cb.getAttribute('data-notify-type')];
  });

  const human = $('#notify-pop-human');
  // human_messages defaults ON within an enabled block — only an explicit
  // false unchecks it.
  if (human) human.checked = state.human_messages !== false;
  const access = $('#notify-pop-access');
  if (access) access.checked = !!state.access_requests;

  // With the master off nothing notifies; dim the per-type rows so the
  // popover reads honestly, but leave them editable — you can arrange the
  // selection before flipping the master on.
  const pop = popover();
  if (pop) pop.classList.toggle('master-off', !enabled);
}

async function load() {
  try {
    const r = await fetch('/api/notifications', { credentials: 'same-origin' });
    if (!r.ok) throw new Error('HTTP ' + r.status);
    paint(await r.json());
  } catch (e) {
    toast('Could not load notification settings: ' + (e.message || e), true);
  }
}

// post sends a partial update and re-paints from the authoritative
// response. On failure it re-loads, so a rejected toggle never leaves a
// checkbox showing a value that did not persist.
async function post(body, okMsg) {
  try {
    const r = await fetch('/api/notifications', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (!r.ok) throw new Error((await r.text()) || ('HTTP ' + r.status));
    paint(await r.json());
    if (okMsg) toast(okMsg);
  } catch (e) {
    toast('Notification update failed: ' + (e.message || e), true);
    load();
  }
}

export function bindNotifyMenu() {
  const btn = bell();
  const pop = popover();
  if (!btn || !pop) return;

  btn.addEventListener('click', () => {
    const next = !isOpen();
    setOpen(next);
    if (next) load(); // fresh state on every open
  });

  // Dismiss on an outside click or Escape — the same manners as the slop
  // mixer and the filter-bar view menus.
  document.addEventListener('pointerdown', (e) => {
    if (!isOpen()) return;
    if (pop.contains(e.target) || btn.contains(e.target)) return;
    setOpen(false);
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && isOpen()) setOpen(false);
  });

  // Master on/off — sits above every other filter.
  const master = $('#notify-pop-enabled');
  if (master) master.addEventListener('change', () => {
    post({ enabled: master.checked },
      master.checked ? 'OS notifications ON' : 'OS notifications OFF (everything muted)');
  });

  // Per-type checklist — each box maps to one {from:"*", to:<state>} rule.
  $$('#notify-pop [data-notify-type]').forEach(cb => {
    cb.addEventListener('change', () => {
      post({ types: { [cb.getAttribute('data-notify-type')]: cb.checked } });
    });
  });

  // Human-message desktop banner.
  const human = $('#notify-pop-human');
  if (human) human.addEventListener('change', () => {
    post({ human_messages: human.checked });
  });

  // Access-request desktop banner.
  const access = $('#notify-pop-access');
  if (access) access.addEventListener('change', () => {
    post({ access_requests: access.checked });
  });

  // "Config tab ↗" — jump to the full editor. Clicking its nav button
  // both switches the tab and triggers the Config island's lazy-load listener.
  const cfgLink = $('#notify-pop-config');
  if (cfgLink) cfgLink.addEventListener('click', () => {
    setOpen(false);
    const nav = $('nav [data-tab="config"]');
    if (nav) nav.click();
  });
}
