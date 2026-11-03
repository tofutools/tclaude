// Compatibility seam for the authoritative snapshot poll. Preact owns every
// descendant of #processes-root; the legacy poll only controls feature-level
// visibility and redirects an active hidden tab.
import { $, $$ } from './helpers.js';
import { dashboardState } from './snapshot-store.js';

export function applyProcessesTabVisibility(data) {
  const visible = !!data?.processes_enabled;
  document.body.classList.toggle('hide-processes', !visible);
  if (!visible && $('#tab-processes')?.classList.contains('active')) {
    $$('nav [data-tab]').forEach((button) => button.classList.toggle('active', button.dataset.tab === 'groups'));
    $$('main section').forEach((panel) => panel.classList.toggle('active', panel.id === 'tab-groups'));
    dashboardState.setActiveTab('groups');
  }
}
