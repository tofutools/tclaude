// phone-nav-toggle.js -- the ▾/▴ toggle that hides or shows the phone's
// bottom nav (Now · Agents · Mail · Board · More) where a view owns the
// bottom edge: a terminal (phone-terminal-island.js) and the mail reader
// (phone-mail-island.js). Everywhere else the nav is always shown.
//
// The toggle sits left of the view's own field (✉ Message / › Type into…,
// Reply to…), in the same row, so it costs no height; hiding the nav gives
// its 56 px to the terminal or the mail. It starts shown. Each view kind
// remembers its own choice (dash prefs, so it follows the operator across
// restarts and devices): hiding the nav in one terminal hides it in every
// terminal, while the mail reader keeps its own. While the nav is hidden the
// toggle carries the count the nav's badges would show (Now + Mail), live.

import { h } from 'preact';
import { signal } from '@preact/signals';
import htm from 'htm';
import { dashPrefs } from './prefs.js';

const html = htm.bind(h);

// NAV_KINDS are the views with a toggle.
export const NAV_KINDS = ['terminal', 'mail'];

const prefKey = (kind) => `tclaude.dash.phone.nav.${kind}`;

// shown holds each kind's choice, read from the prefs on first use.
const shown = new Map();

function shownSignal(kind, prefs) {
  if (!shown.has(kind)) {
    let hidden = false;
    try { hidden = prefs.getItem(prefKey(kind)) === 'hidden'; } catch { /* no prefs: shown */ }
    shown.set(kind, signal(!hidden));
  }
  return shown.get(kind);
}

// navShown says whether kind's view shows the nav (subscribes a render).
export function navShown(kind, prefs = dashPrefs) {
  return shownSignal(kind, prefs).value;
}

// setNavShown sets kind's choice and remembers it.
export function setNavShown(kind, on, prefs = dashPrefs) {
  shownSignal(kind, prefs).value = !!on;
  try {
    if (on) prefs.removeItem(prefKey(kind));
    else prefs.setItem(prefKey(kind), 'hidden');
  } catch { /* not remembered; the choice still holds this session */ }
}

// resetNavToggleForTest forgets every kind's cached choice.
export function resetNavToggleForTest() {
  shown.clear();
}

// navBadge is the count the nav's badges add up to (Now's needs and Mail's
// unread), which the phone shell keeps current; the toggle shows it while
// the nav is hidden.
export const navBadge = signal(0);

// terminalDock is the terminal docked over the dashboard, for the shell:
// null with none, else { keyboard, close }. While the phone's keyboard is
// up, or something covers the whole terminal (Select, whose Copy buttons
// sit at the bottom), the nav stays away whatever the choice, and
// picking a view in the nav closes the terminal first.
export const terminalDock = signal(null);

// NavToggle is the toggle button for kind's view.
export function NavToggle({ kind, prefs = dashPrefs, onMouseDown }) {
  const on = navShown(kind, prefs);
  const count = on ? 0 : navBadge.value;
  const label = on ? 'Hide page selector' : 'Show page selector';
  return html`<button data-ui-exempt type="button" class=${`phone-nav-toggle${on ? '' : ' off'}`}
    aria-label=${count ? `${label} (${count} waiting)` : label} aria-expanded=${on ? 'true' : 'false'} title=${label}
    onMouseDown=${onMouseDown} onClick=${() => setNavShown(kind, !on, prefs)}>
    <span aria-hidden="true">${on ? '▾' : '▴'}</span>
    ${count > 0 && html`<span class="ui-badge ui-badge-danger phone-nav-toggle-badge" aria-hidden="true">${count > 99 ? '99+' : count}</span>`}
  </button>`;
}
