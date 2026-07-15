// toolbar-profile-renderers.js — imperative painters for the two stable
// dashboard toolbar profile chips. Their one-shot picker lifecycle remains in
// row-actions.js; these painters deliberately preserve the cached DOM nodes
// that dock.js re-homes between toolbar layouts.

import { $ } from './helpers.js';
import { getDashDefaultProfile } from './profiles.js';
import { lastSnapshot } from './dashboard.js';

function renderDashDefaultProfile() {
  const el = $('#dashboard-default-profile');
  if (!el) return;
  const name = getDashDefaultProfile();
  el.classList.toggle('unset', !name);
  el.setAttribute('data-profile', name);
  el.setAttribute('aria-label', name ? `Dashboard default spawn profile: ${name}. Click to change.` : 'Set dashboard default spawn profile');
  el.textContent = '🧠' + (name ? ' ' + name : '');
  el.title = name
    ? `Dashboard default spawn profile: ${name} — pre-fills the spawn dialog when the chosen group has no default profile of its own. Click to change.`
    : 'No dashboard default spawn profile — click to set one. (Pre-fills the spawn dialog as a fallback after a group’s own default.)';
}

function renderDashSandboxProfile() {
  const el = $('#dashboard-default-sandbox-profile');
  if (!el || !lastSnapshot) return;
  const name = lastSnapshot.sandbox_profile_default || '';
  el.classList.toggle('unset', !name);
  el.setAttribute('data-sandbox-profile', name);
  el.setAttribute('aria-label', name ? `Global sandbox profile: ${name}. Click to change.` : 'Set global sandbox profile');
  el.textContent = '🛡' + (name ? ' ' + name : '');
  el.title = name
    ? `Global sandbox profile: ${name} — newly launched agents inherit it before any group or explicit assignment. Click to change.`
    : 'No global sandbox profile — click to set one. Newly launched agents inherit it unless their group adds another assignment.';
}

export { renderDashDefaultProfile, renderDashSandboxProfile };
