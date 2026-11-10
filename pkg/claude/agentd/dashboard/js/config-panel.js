// config-panel.js — the Config tab's open settings section (/config/<section>).
//
// Dependency-free (bar Signals) so the history router can read and restore
// it without importing the Config island's module graph. The section is also
// remembered for the browser session, so a bare /config reopens it.

import { signal } from '@preact/signals';
import { KNOWN_SUBTABS } from './nav-history-core.js';

export const CONFIG_TAB = 'config';
const PANEL_STORAGE_KEY = 'tclaude.config.panel';
const DEFAULT_PANEL = 'appearance';

function storedPanel() {
  try {
    const stored = globalThis.sessionStorage?.getItem(PANEL_STORAGE_KEY);
    if (KNOWN_SUBTABS[CONFIG_TAB].has(stored)) return stored;
  } catch { /* storage unavailable — fall through to the first panel */ }
  return DEFAULT_PANEL;
}

// configPanel is the shown section. It survives tab switches, so leaving and
// re-entering Config lands on the section that was open.
export const configPanel = signal(storedPanel());

// CONFIG_PANEL_ALIASES are routes that name a part of a section rather than a
// section: /config/service and /config/updates were sections of their own
// and now open Installation scrolled to that part. The alias stays in the
// URL, so the link keeps pointing at the part.
export const CONFIG_PANEL_ALIASES = Object.freeze({ service: 'installation', updates: 'installation' });

// configSection is the rail section a route shows.
export function configSection(id) {
  return CONFIG_PANEL_ALIASES[id] || id;
}

export function configLocation(panel = configPanel.value) {
  return { tab: CONFIG_TAB, subtab: panel };
}

// setConfigPanel selects a section. Unknown names are refused (false); a
// repeat selection is a no-op (false) so callers never record a duplicate.
export function setConfigPanel(id, current = configPanel) {
  if (!KNOWN_SUBTABS[CONFIG_TAB].has(id) || current.value === id) return false;
  current.value = id;
  try { globalThis.sessionStorage?.setItem(PANEL_STORAGE_KEY, id); } catch { /* best effort */ }
  return true;
}
