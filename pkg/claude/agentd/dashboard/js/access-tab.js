// Legacy snapshot renderer for the bounded Access surface. Keeping this slice
// feature-local lets its Preact migration retire Access ownership without
// editing the authoritative poll's internals.
import { $ } from './helpers.js';
import { morphInto } from './morph.js';
import { renderPermissions, renderSlugs } from './render.js';
import { renderSudoTab } from './tabs.js';
import { bindFilter } from './refresh.js';

export function bindAccessTab() {
  bindFilter('sudo', renderSudoTab);
}

export function renderAccessListSnapshot() {
  renderSudoTab();
}

export function renderAccessRegistrySnapshot(data) {
  // Reconcile rather than replace so selection in either roster survives the
  // 2-second poll. Access migration removes these morph consumers in one file.
  morphInto($('#permissions-body'), renderPermissions(data.permissions, data.agents));
  morphInto($('#slugs-body'), renderSlugs(data.slugs));
}
