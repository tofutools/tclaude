// Compatibility seam for the authoritative snapshot poll. Preact owns every
// descendant of #processes-root; the legacy poll only controls feature-level
// visibility and redirects an active hidden tab.
import { $, $$ } from './helpers.js';
import { dashboardState } from './snapshot-store.js';
import { configPanel, setConfigPanel } from './config-panel.js';

// applyCollabVisibility shows or hides the Collab surfaces (features.collab).
// The Terminals tab stays; the shell drops its board tabs (mountTerminalShell).
export function applyCollabVisibility(data) {
  const visible = data?.collab_enabled === true;
  document.body.classList.toggle('hide-collab', !visible);
  // The palette and the scope banner read this label as text.
  const label = $('nav [data-tab="terminals"] .tab-label-regular');
  if (label) label.textContent = visible ? 'Collab' : 'Terminals';
  if (!visible && configPanel.value === 'steward') setConfigPanel('appearance');
}

export function applyProcessesTabVisibility(data) {
  const visible = !!data?.processes_enabled;
  document.body.classList.toggle('hide-processes', !visible);
  if (!visible && $('#tab-processes')?.classList.contains('active')) {
    $$('nav [data-tab]').forEach((button) => button.classList.toggle('active', button.dataset.tab === 'groups'));
    $$('main section').forEach((panel) => panel.classList.toggle('active', panel.id === 'tab-groups'));
    dashboardState.setActiveTab('groups');
  }
}
