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
// (agent-spawn-island.js), the profile editor (modal-profiles.js) and the
// group/dashboard default-profile pickers (row-actions.js) all read through
// here so there is one fetch path and one cache.

import { dashPrefs } from './prefs.js';

const API = '/api/spawn-profiles';

// The dashboard-level default profile lives in dashPrefs' server-backed SQLite
// store. It pre-fills dashboard forms AND is the daemon's global spawn fallback
// after a group's own default, so the picker, CLI and raw spawn API share one
// value. Exported so the editor and pickers share the one key.
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
  return findProfileByHandle(list, name);
}

// findProfileByHandle resolves the primary name or any alias without another
// request. The server guarantees one shared namespace, so the first match is
// unambiguous.
function findProfileByHandle(profiles, handle) {
  const name = String(handle || '').trim();
  return (profiles || []).find(p => p.name === name || (p.aliases || []).includes(name)) || null;
}

// profileChoices flattens each stored profile into one canonical option and
// one option per alias. Alias entries remain visibly tied to their canonical
// profile instead of looking like duplicate profiles.
function profileChoices(profiles) {
  const out = [];
  for (const profile of profiles || []) {
    const disabled = profile.disabled ? ` [🚫 disabled: ${String(profile.disabled_reason).replace(/\s+/g, ' ').trim()}]` : '';
    out.push({ value: profile.name, label: profile.name + disabled, profile, alias: false });
    for (const alias of profile.aliases || []) {
      out.push({ value: alias, label: `${alias} → ${profile.name}${disabled}`, profile, alias: true });
    }
  }
  return out;
}

function profileAliasesLabel(profile) {
  const aliases = profile?.aliases || [];
  return aliases.length ? `aka ${aliases.join(' · ')}` : '';
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

async function parseAPIError(r) {
  const txt = await r.text();
  if (!txt) return `HTTP ${r.status}`;
  try {
    const j = JSON.parse(txt);
    if (j && j.error) return j.error;
  } catch (_) {}
  return txt;
}

// exportProfiles downloads a portable profile bundle for the given profile
// names. An empty names list means "all profiles" (the server default).
async function exportProfiles(names = []) {
  const q = new URLSearchParams();
  (names || []).forEach(n => {
    if (n) q.append('name', n);
  });
  const qs = q.toString();
  const r = await fetch(`${API}/export${qs ? '?' + qs : ''}`, { credentials: 'same-origin' });
  if (!r.ok) throw new Error(await parseAPIError(r));
  return r.json();
}

async function inspectProfileImport(envelope) {
  const r = await fetch(`${API}/import/inspect`, {
    method: 'POST', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(envelope),
  });
  if (!r.ok) throw new Error(await parseAPIError(r));
  return r.json();
}

async function importProfiles(envelope, decisions) {
  const body = { ...envelope, decisions: decisions || [] };
  const r = await fetch(`${API}/import`, {
    method: 'POST', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!r.ok) throw new Error(await parseAPIError(r));
  invalidateProfiles();
  return r.json().catch(() => ({}));
}

// getDashDefaultProfile reads the dashboard-level default profile name from
// dashPrefs ("" when none). Used as the spawn dialog's pre-fill fallback
// (after a group's own default_profile).
function getDashDefaultProfile() {
  return dashPrefs.getItem(DASH_DEFAULT_PROFILE_KEY) || '';
}

// setDashDefaultProfile persists (or clears) the operational global default
// through the same validated handler the CLI uses, then updates dashPrefs'
// synchronous UI cache without queuing a redundant generic-pref write. The
// immediate awaited write avoids debounce/failure swallowing for a value that
// now affects daemon spawn behavior.
async function setDashDefaultProfile(name) {
  const r = await fetch('/api/spawn-profile-default', {
    method: name ? 'PUT' : 'DELETE', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: name ? JSON.stringify({ name }) : undefined,
  });
  if (!r.ok) throw new Error((await r.text()) || `HTTP ${r.status}`);
  const response = await r.json().catch(() => ({}));
  const canonical = response.name || '';
  dashPrefs.syncItem(DASH_DEFAULT_PROFILE_KEY, canonical);
  return canonical;
}

// syncDashDefaultProfile reconciles dashPrefs' synchronous UI cache from the
// regular dashboard snapshot. The server includes this operational default in
// every poll, so CLI changes appear in an already-open dashboard without a
// separate GET /api/spawn-profile-default request.
function syncDashDefaultProfile(name) {
  dashPrefs.syncItem(DASH_DEFAULT_PROFILE_KEY, name || '');
}

// profileSummary builds a compact one-line summary of a profile's set fields
// for the manage-modal cards — only the fields that are actually set show, so
// a sparse profile reads cleanly. The launch fields lead (harness/model/…),
// then the identity bits. Mirrors the terse meta style of the template cards.
function profileSummary(p) {
  const parts = [];
  if (p.disabled) parts.push('🚫 disabled');
  // Claude is the default harness — only name a non-default one.
  if (p.harness && p.harness !== 'claude') parts.push(p.harness);
  if (p.model) parts.push(p.model);
  if (p.effort) parts.push(`effort ${p.effort}`);
  // 'inherit' is the recommended default (no per-session override), so — like an
  // absent toggle — it isn't worth a chip; only a real override (on/off) shows.
  if (p.sandbox && p.sandbox !== 'inherit') parts.push(`sandbox ${p.sandbox}`);
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
  // Same as sandbox: 'inherit' is the default permission mode, not worth a chip.
  if (p.approval && p.approval !== 'inherit') parts.push(`approval ${p.approval}`);
  // Birth-time access controls: a chip for the owner default and the
  // override count, when the profile carries them.
  if (p.is_owner != null) parts.push(`owner ${p.is_owner ? 'on' : 'off'}`);
  const nOverrides = p.permission_overrides ? Object.keys(p.permission_overrides).length : 0;
  if (nOverrides) parts.push(`${nOverrides} perm${nOverrides === 1 ? '' : 's'}`);
  return parts.join(' · ');
}

// profileDetailChips enumerates every set profile field for the dock's expanded
// hover/focus view. Unlike profileSummary, it does not hide explicit defaults
// and it expands permission overrides individually. Potentially large freeform
// startup text is represented by presence/size rather than copied into a chip.
function profileDetailChips(p) {
  const parts = [];
  if (p.disabled) parts.push(`🚫 disabled · ${String(p.disabled_reason).replace(/\s+/g, ' ').trim()}`);
  else if (p.disabled_reason) parts.push(`last disable reason · ${String(p.disabled_reason).replace(/\s+/g, ' ').trim()}`);
  const text = (label, value) => { if (value) parts.push(`${label} ${String(value).replace(/\s+/g, ' ').trim()}`); };
  const toggle = (label, value) => { if (value != null) parts.push(`${label} ${value ? 'on' : 'off'}`); };
  text('harness', p.harness);
  text('model', p.model);
  text('effort', p.effort);
  text('sandbox', p.sandbox);
  text('approval', p.approval);
  text('ask-timeout', p.ask_user_question_timeout);
  toggle('auto-review', p.auto_review);
  toggle('trust-dir', p.trust_dir);
  toggle('remote-control', p.remote_control);
  text('name', p.agent_name);
  text('role', p.role);
  text('descr', p.descr);
  if (p.initial_message) parts.push(`initial message · ${p.initial_message.length} chars`);
  toggle('sync-wt', p.sync_worktree);
  toggle('focus', p.auto_focus);
  toggle('group-ctx', p.include_group_default_context);
  toggle('owner', p.is_owner);
  for (const [slug, effect] of Object.entries(p.permission_overrides || {}).sort(([a], [b]) => a.localeCompare(b))) {
    parts.push(`perm ${slug} ${effect}`);
  }
  return parts;
}

export {
  loadProfiles, cachedProfiles, invalidateProfiles, getProfile,
  findProfileByHandle, profileChoices, profileAliasesLabel,
  createProfile, updateProfile, deleteProfile,
  exportProfiles, inspectProfileImport, importProfiles,
  getDashDefaultProfile, setDashDefaultProfile, syncDashDefaultProfile,
  DASH_DEFAULT_PROFILE_KEY, profileSummary, profileDetailChips,
};
