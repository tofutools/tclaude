// tabs.js — the legacy Groups / Links tab renderers.
//
// Builds the listing tables for the Groups and Links
// tabs from snapshot data, each with its text-filter helper.
// Extracted from dashboard.js as part of the Stage 2 module split.

import { featureState } from './feature-state-registry.js';

// lastSnapshot still lives in dashboard.js — the snapshot-refresh
// cluster is not extracted yet. Importing it back forms a deliberate,
// benign cycle (dashboard.js <-> tabs.js): it is safe because tabs.js
// runs no top-level code that reads it — the render*Tab functions
// touch it only when called, long after both modules finish
// evaluating (it is a read-only live binding here). This import
// re-points to the proper module once the snapshot cluster is
// extracted in a later PR.
import { lastSnapshot } from './dashboard.js';

function renderGroupsTab() {
  if (!lastSnapshot) return;
  // Preact owns the Groups render. This adapter remains for legacy mutations
  // and drag/drop modules that already call renderGroupsTab after updating the
  // shared snapshot; publishing a shallow copy also catches in-place updates.
  featureState('groups')?.publish(lastSnapshot);
}

function fmtRemaining(secs) {
  if (!secs || secs <= 0) return 'expired';
  if (secs < 60) return secs + 's';
  if (secs < 3600) {
    const m = Math.floor(secs / 60);
    const s = secs % 60;
    return s > 0 ? `${m}m${s}s` : `${m}m`;
  }
  const h = Math.floor(secs / 3600);
  const m = Math.floor((secs % 3600) / 60);
  return m > 0 ? `${h}h${m}m` : `${h}h`;
}

function renderLinksTab() {
  if (!lastSnapshot) return;
  featureState('links')?.publish(lastSnapshot);
}

export {
  renderGroupsTab, renderLinksTab, fmtRemaining,
};
