// profiles.js — the spawn-profile DATA layer (JOH-210 inc3).
//
// Spawn profiles are named, reusable bundles of the spawn-agent dialog's
// fields (harness / model / effort / sandbox / trust-dir / name / role /
// descr / initial message + the dialog toggles — NOT cwd/worktree), stored
// server-side behind /api/spawn-profiles (the loopback twins of the daemon's
// /v1/spawn-profiles surface; see dashboard_spawn_profiles.go). A profile
// pre-fills the spawn dialog, and a group's default profile (set via the
// group 🧠 picker) is resolved server-side to fill blank launch fields for
// non-dialog spawns.
//
// This module is pure data + cache — no DOM. The spawn dialog
// (modal-spawn.js), the profile editor (modal-profiles.js) and the
// group/dashboard default-profile pickers (row-actions.js) all read through
// here so there is one fetch path and one cache.

import { dashPrefs } from './prefs.js';

const API = '/api/spawn-profiles';

// The dashboard-level default profile is a pure client preference (it only
// pre-fills the spawn dialog as a fallback), so it lives in dashPrefs — the
// generic key→string store — not on the server. Exported so the editor and
// the pickers share the one key.
const DASH_DEFAULT_PROFILE_KEY = 'tclaude.dash.default_profile';

// In-memory cache of the full profile list (each entry is a complete
// spawnProfileJSON — GET /api/spawn-profiles returns full objects, so a
// single list fetch is enough; getProfile reads from here rather than
// round-tripping /{name}). null = not loaded yet; a mutation invalidates it
// so the next loadProfiles re-fetches.
let cache = null;

// loadProfiles fetches the profile list (cached). Pass force=true to bypass
// the cache after a known mutation or when a fresh listing is required (the
// manage modal). Throws on a non-OK response so callers can surface the error.
async function loadProfiles(force = false) {
  if (cache && !force) return cache;
  const r = await fetch(API, { credentials: 'same-origin' });
  if (!r.ok) throw new Error((await r.text()) || `HTTP ${r.status}`);
  const list = await r.json();
  cache = Array.isArray(list) ? list : [];
  return cache;
}

// cachedProfiles returns the last-loaded list without a fetch (or [] when
// nothing has been loaded yet) — for synchronous render paths that have
// already ensured a load elsewhere.
function cachedProfiles() {
  return cache || [];
}

// invalidateProfiles drops the cache so the next loadProfiles re-fetches.
// Called after every create/update/delete.
function invalidateProfiles() {
  cache = null;
}

// getProfile returns the full profile object for `name`, or null. Reads from
// the (lazily loaded) list cache — the list already carries every field.
async function getProfile(name) {
  const list = await loadProfiles();
  return list.find(p => p.name === name) || null;
}

// createProfile POSTs a new profile. `body` is a spawnProfileJSON (name +
// whatever optional fields are set). Invalidates the cache on success.
async function createProfile(body) {
  const r = await fetch(API, {
    method: 'POST', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!r.ok) throw new Error((await r.text()) || `HTTP ${r.status}`);
  invalidateProfiles();
  return r.json().catch(() => ({}));
}

// updateProfile PATCHes an existing profile by its current name. The server's
// PATCH is a FULL replace (post the whole desired state), so `body` must carry
// every field the profile should keep — renaming is a PATCH whose body carries
// the new name. Invalidates the cache on success.
async function updateProfile(name, body) {
  const r = await fetch(`${API}/${encodeURIComponent(name)}`, {
    method: 'PATCH', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!r.ok) throw new Error((await r.text()) || `HTTP ${r.status}`);
  invalidateProfiles();
  return r.json().catch(() => ({}));
}

// deleteProfile DELETEs a profile by name. Treats 204 (the daemon's success
// code) and any 2xx as success. Invalidates the cache.
async function deleteProfile(name) {
  const r = await fetch(`${API}/${encodeURIComponent(name)}`, {
    method: 'DELETE', credentials: 'same-origin',
  });
  if (!r.ok && r.status !== 204) throw new Error((await r.text()) || `HTTP ${r.status}`);
  invalidateProfiles();
}

// getDashDefaultProfile reads the dashboard-level default profile name from
// dashPrefs ("" when none). Used as the spawn dialog's pre-fill fallback
// (after a group's own default_profile).
function getDashDefaultProfile() {
  return dashPrefs.getItem(DASH_DEFAULT_PROFILE_KEY) || '';
}

// setDashDefaultProfile records (or, with a blank name, clears) the
// dashboard-level default profile.
function setDashDefaultProfile(name) {
  if (name) dashPrefs.setItem(DASH_DEFAULT_PROFILE_KEY, name);
  else dashPrefs.removeItem(DASH_DEFAULT_PROFILE_KEY);
}

// profileSummary builds a compact one-line summary of a profile's set fields
// for the manage-modal cards — only the fields that are actually set show, so
// a sparse profile reads cleanly. The launch fields lead (harness/model/…),
// then the identity bits. Mirrors the terse meta style of the template cards.
function profileSummary(p) {
  const parts = [];
  // Claude is the default harness — only name a non-default one.
  if (p.harness && p.harness !== 'claude') parts.push(p.harness);
  if (p.model) parts.push(p.model);
  if (p.effort) parts.push(`effort ${p.effort}`);
  if (p.sandbox) parts.push(`sandbox ${p.sandbox}`);
  if (p.agent_name) parts.push(`name ${p.agent_name}`);
  if (p.role) parts.push(p.role);
  // The *bool toggles read as on/off only when explicitly set (an absent
  // toggle leaves the dialog's own default, so it isn't worth a chip).
  if (p.trust_dir != null) parts.push(`trust-dir ${p.trust_dir ? 'on' : 'off'}`);
  if (p.remote_control != null) parts.push(`remote-control ${p.remote_control ? 'on' : 'off'}`);
  if (p.sync_worktree != null) parts.push(`sync-wt ${p.sync_worktree ? 'on' : 'off'}`);
  if (p.auto_focus != null) parts.push(`focus ${p.auto_focus ? 'on' : 'off'}`);
  if (p.include_group_default_context != null) {
    parts.push(`group-ctx ${p.include_group_default_context ? 'on' : 'off'}`);
  }
  if (p.auto_review != null) parts.push(`auto-review ${p.auto_review ? 'on' : 'off'}`);
  if (p.approval) parts.push(`approval ${p.approval}`);
  return parts.join(' · ');
}

export {
  loadProfiles, cachedProfiles, invalidateProfiles, getProfile,
  createProfile, updateProfile, deleteProfile,
  getDashDefaultProfile, setDashDefaultProfile,
  DASH_DEFAULT_PROFILE_KEY, profileSummary,
};
