// phone-config-island.js -- the phone's Settings: a list of what there is to
// set, where each config section opens as a page of its own.
//
// "This phone" holds what belongs to the browser rather than config.json: the
// layout (Desktop site) and the theme. A few config.json sections are edited
// here (PHONE_EDITABLE), in the desktop's own form and through its own Save
// and diff, so the round-trip is the desktop's. Every other section opens read
// only, with Desktop site one tap away: they set up a machine (install, daemon,
// git credentials, launch policy) and want a keyboard and a large screen.
//
// A section opened from the list is a location (/config/<section>, a history
// entry), so ‹ and the phone's Back return to the list. A section reached
// by a link has no list under it: ‹ shows the list in place.

import { h, render } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { configPanel, configSection } from './config-panel.js';
import { usePhonePage } from './phone-page.js';
import { Button, Switch } from './ui-components.js';

const html = htm.bind(h);

// The config.json sections a phone edits: notifications, and settings a
// phone can sensibly judge. The rest set up the machine.
export const PHONE_EDITABLE = Object.freeze(['notifications', 'steward', 'costs']);

// sectionMode is how a phone shows a section: edit, read (read-only) or
// list (no section open).
export function sectionMode(id) {
  if (!id) return 'list';
  return PHONE_EDITABLE.includes(configSection(id)) ? 'edit' : 'read';
}

function SectionRow({ panel, onOpen, readOnly }) {
  return html`<li><button data-ui-exempt type="button" class="phone-config-row" onClick=${() => onOpen(panel.id)}>
    <span class="phone-config-icon" aria-hidden="true">${panel.icon}</span>
    <span class="phone-config-text">
      <span class="phone-config-title">${panel.title}${readOnly && html` <span class="ui-tag phone-config-tag">view</span>`}</span>
      <span class="phone-config-lede">${panel.lede}</span>
    </span>
    <span class="phone-config-chevron" aria-hidden="true">›</span>
  </button></li>`;
}

function useWizard(actions) {
  const [wizard, setWizard] = useState(() => actions.isWizard());
  useEffect(() => {
    const sync = () => setWizard(actions.isWizard());
    document.addEventListener('tclaude:wizard', sync);
    return () => document.removeEventListener('tclaude:wizard', sync);
  }, [actions]);
  return wizard;
}

function SectionList({ panels, actions, onOpen }) {
  const wizard = useWizard(actions);
  const editable = panels.filter((panel) => PHONE_EDITABLE.includes(panel.id));
  const others = panels.filter((panel) => !PHONE_EDITABLE.includes(panel.id));
  return html`<div class="phone-config">
    <div class="phone-section" role="group" aria-label="This phone">
      <h2 class="phone-section-head ui-eyebrow">This phone</h2>
      <ul class="phone-list">
        <li class="phone-config-device">
          <span class="phone-config-text">
            <span class="phone-config-title">Layout</span>
            <span class="phone-config-lede">The phone layout. Desktop site shows the full dashboard in this browser.</span>
          </span>
          <${Button} size="sm" onClick=${actions.desktopSite}>Desktop site<//>
        </li>
        <li class="phone-config-device">
          <label class="phone-config-text" for="phone-config-wizard">
            <span class="phone-config-title">Wizard theme 🧙</span>
            <span class="phone-config-lede">Re-skins the dashboard and renames things (Parties, Scrying…).</span>
          </label>
          <${Switch} id="phone-config-wizard" checked=${wizard} onChange=${() => actions.toggleWizard()} />
        </li>
      </ul>
    </div>
    <div class="phone-section" role="group" aria-label="Settings">
      <h2 class="phone-section-head ui-eyebrow">Settings</h2>
      <ul class="phone-list">${editable.map((panel) => html`<${SectionRow} key=${panel.id} panel=${panel} onOpen=${onOpen} />`)}</ul>
    </div>
    <div class="phone-section" role="group" aria-label="On the desktop">
      <h2 class="phone-section-head ui-eyebrow">On the desktop</h2>
      <p class="phone-config-note">These set up the machine tclaude runs on. Read them here; change them on the desktop site.</p>
      <ul class="phone-list">${others.map((panel) => html`<${SectionRow} key=${panel.id} panel=${panel} onOpen=${onOpen} readOnly />`)}</ul>
    </div>
  </div>`;
}

// SectionPage is the top bar over the desktop form's section (which shows
// below this island, phone.css), and for a read-only one, why and the way
// to the desktop.
function SectionPage({ panel, mode, actions, back, active }) {
  usePhonePage({ title: panel?.title || 'Settings', onBack: back, hideNav: mode === 'edit' }, active);
  if (mode !== 'read') return null;
  return html`<div class="phone-config-readonly" role="note">
    <span>Read-only on a phone.</span>
    <${Button} size="sm" onClick=${actions.desktopSite}>Desktop site<//>
  </div>`;
}

export function PhoneConfig({ state, panels, actions, initialOpen = '' }) {
  const active = state.activeTab.value === 'config';
  const panel = configPanel.value;
  // open is the section the operator opened (or a link named); the list
  // shows whenever the location moved off it (Back, another section).
  const [open, setOpen] = useState(initialOpen);
  const pushed = useRef('');
  const shown = open && open === panel ? open : '';
  const mode = sectionMode(shown);
  useEffect(() => { actions.remember?.(shown); }, [shown]);

  useEffect(() => {
    const root = document.documentElement;
    if (active) root.dataset.phoneConfig = mode;
    else delete root.dataset.phoneConfig;
    return () => { delete root.dataset.phoneConfig; };
  }, [active, mode]);

  const openSection = (id) => {
    pushed.current = actions.showSection(id) ? id : '';
    setOpen(id);
  };
  const back = () => {
    const wasPushed = pushed.current === shown;
    pushed.current = '';
    setOpen('');
    if (wasPushed) actions.back();
  };
  if (!shown) return html`<${SectionList} panels=${panels} actions=${actions} onOpen=${openSection} />`;
  const meta = panels.find((item) => item.id === configSection(shown));
  return html`<${SectionPage} panel=${meta} mode=${mode} actions=${actions} back=${back} active=${active} />`;
}

export function mountPhoneConfigIsland({ host, state, panels, actions, initialOpen, registerCleanup }) {
  render(html`<${PhoneConfig} state=${state} panels=${panels} actions=${actions} initialOpen=${initialOpen} />`, host);
  registerCleanup(() => render(null, host));
}
