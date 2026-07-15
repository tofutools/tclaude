// Remaining Groups toolbar bindings that do not own a modal. Group creation is
// Preact-owned behind group-create-controller.js; cleanup flows retain their
// existing transaction-dialog entry points here until their toolbar migrates.

import { openCleanupModal, openDeleteRetiredPreview } from './refresh.js';
import { $ } from './helpers.js';

export function bindGroupsCleanupButtons() {
  $('#cleanup-all-open').addEventListener('click', () => openCleanupModal({ mode: 'agents' }));
  $('#delete-retired-open').addEventListener('click', () => openDeleteRetiredPreview());
}
