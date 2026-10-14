// automations-subnav.js — the Automations sub-navigation shared by the Jobs
// island (kind views) and the Processes island. Processes is presented as an
// Automations subtab but keeps its own internal tab id and #tab-processes
// section, so both islands render this one row and a cross-island pick goes
// through the hidden nav anchors, which keeps the router's single recording
// path (nav click → tclaude:navigated) intact.
import { h } from 'preact';
import htm from 'htm';
import { $, isModifiedClick } from './helpers.js';
import { featureState } from './feature-state-registry.js';
import { toPath } from './nav-history-core.js';
import { JOBS_KINDS, jobsLocation } from './jobs-state.js';

const html = htm.bind(h);

export const PROCESSES_SUBTAB = 'processes';

const LABELS = {
  all: 'All',
  'export': 'Exports',
  cron: 'Cron jobs',
  'standing-order': 'Standing orders',
  trigger: 'Triggers',
  [PROCESSES_SUBTAB]: 'Processes',
};

// automationsSubnavEntries lists the visible entries in display order. A
// snapshot that has not arrived yet (null) shows Triggers but not Processes:
// Triggers is the established default, while Processes is dark-launched and
// must not flash before the server confirms its flag.
export function automationsSubnavEntries(snapshot) {
  const known = snapshot != null;
  const kinds = !known || snapshot.triggers_enabled === true
    ? JOBS_KINDS : JOBS_KINDS.filter((kind) => kind !== 'trigger');
  return snapshot?.processes_enabled ? [...kinds, PROCESSES_SUBTAB] : kinds;
}

function entryHref(key) {
  return toPath(key === PROCESSES_SUBTAB ? { tab: 'processes' } : jobsLocation(key));
}

// openAutomationsEntry switches to `key` from whichever island is showing.
// Selecting a kind sets the Jobs state BEFORE clicking the Automations anchor
// so the router records the destination once, not the previous kind first.
export function openAutomationsEntry(key) {
  if (key === PROCESSES_SUBTAB) {
    $('nav [data-tab="processes"]')?.click();
    return;
  }
  featureState('jobs')?.setKind?.(key);
  $('nav [data-tab="jobs"]')?.click();
}

// AutomationsSubnav renders the row. `onSelect(key)` handles a plain
// activation; modified clicks fall through to the anchor's href so the view
// opens in a new browser tab.
export function AutomationsSubnav({ entries, active, onSelect }) {
  const activate = (event, key) => {
    if (isModifiedClick(event)) return;
    event.preventDefault();
    onSelect(key);
  };
  const keyDown = (event, key) => {
    if (event.key !== ' ' && event.key !== 'Spacebar') return;
    event.preventDefault();
    onSelect(key);
  };
  return html`<div class="jobs-subnav" role="tablist" aria-label="Automation views">
    ${entries.map((key) => html`<a href=${entryHref(key)}
      class=${`jobs-subtab${active === key ? ' active' : ''}`}
      data-automations-subtab=${key}
      role="tab" aria-selected=${active === key ? 'true' : 'false'}
      onClick=${(event) => activate(event, key)}
      onKeyDown=${(event) => keyDown(event, key)}>${LABELS[key]}</a>`)}
  </div>`;
}
