// toolbar-profile-renderers.js — snapshot bridges for the two Preact-owned
// dashboard toolbar profile controls. The stable host nodes are what dock.js
// re-homes between toolbar layouts.

import { getDashDefaultProfile } from './profiles.js';
import { lastSnapshot } from './dashboard.js';
import { updateToolbarProfileValue } from './toolbar-profile-picker.js';

function renderDashDefaultProfile() {
  updateToolbarProfileValue('profile', getDashDefaultProfile());
}

function renderDashSandboxProfile() {
  if (lastSnapshot) updateToolbarProfileValue('sandbox', lastSnapshot.sandbox_profile_default || '');
}

export { renderDashDefaultProfile, renderDashSandboxProfile };
