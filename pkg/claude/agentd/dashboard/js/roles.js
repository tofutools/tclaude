// roles.js — the role-library DATA layer (JOH-240).
//
// A role is a named, reusable bundle of defaults a template roster agent can
// reference: a canonical role-brief (folded into the agent's startup context),
// a default launch shape (harness / model / effort / sandbox / approval or a
// spawn-profile reference), and a default permission set. Stored server-side
// behind /api/roles (the loopback twin of the daemon's /v1/roles surface; see
// dashboard_roles.go).
//
// This module is pure data + cache — no DOM. The role editor (modal-roles.js)
// and the template editor's per-agent role dropdown (modal-templates.js) read
// through here so there is one fetch path and one cache.

const API = '/api/roles';

// In-memory cache of the full role list (each entry is a complete roleJSON —
// GET /api/roles returns full objects). null = not loaded yet; a mutation
// invalidates it so the next loadRoles re-fetches.
let cache = null;

// loadRoles fetches the role list (cached). Pass force=true to bypass the cache
// after a known mutation or when a fresh listing is required (the manage
// modal). Throws on a non-OK response so callers can surface the error.
async function loadRoles(force = false) {
  if (cache && !force) return cache;
  const r = await fetch(API, { credentials: 'same-origin' });
  if (!r.ok) throw new Error((await r.text()) || `HTTP ${r.status}`);
  const list = await r.json();
  cache = Array.isArray(list) ? list : [];
  return cache;
}

// cachedRoles returns the last-loaded list without a fetch (or [] when nothing
// has been loaded yet) — for synchronous render paths that ensured a load
// elsewhere.
function cachedRoles() {
  return cache || [];
}

// invalidateRoles drops the cache so the next loadRoles re-fetches. Called
// after every create/update/delete.
function invalidateRoles() {
  cache = null;
}

// createRole POSTs a new role. `body` is a roleJSON (name + whatever optional
// fields are set). Invalidates the cache on success.
async function createRole(body) {
  const r = await fetch(API, {
    method: 'POST', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!r.ok) throw new Error((await r.text()) || `HTTP ${r.status}`);
  invalidateRoles();
  return r.json().catch(() => ({}));
}

// updateRole PATCHes an existing role by its current name. The server's PATCH
// is a FULL replace (post the whole desired state), so `body` must carry every
// field the role should keep — renaming is a PATCH whose body carries the new
// name. Invalidates the cache on success.
async function updateRole(name, body) {
  const r = await fetch(`${API}/${encodeURIComponent(name)}`, {
    method: 'PATCH', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!r.ok) throw new Error((await r.text()) || `HTTP ${r.status}`);
  invalidateRoles();
  return r.json().catch(() => ({}));
}

// deleteRole DELETEs a role by name. Treats 204 (the daemon's success code) and
// any 2xx as success. Invalidates the cache.
async function deleteRole(name) {
  const r = await fetch(`${API}/${encodeURIComponent(name)}`, {
    method: 'DELETE', credentials: 'same-origin',
  });
  if (!r.ok && r.status !== 204) throw new Error((await r.text()) || `HTTP ${r.status}`);
  invalidateRoles();
}

// roleSummary builds a compact one-line summary of a role's set fields for the
// manage-modal cards — only the fields actually set show. The launch fields
// lead (profile/harness/model/…), then the permission count.
function roleSummary(rl) {
  const parts = [];
  if (rl.spawn_profile) parts.push(`⚙ ${rl.spawn_profile}`);
  if (rl.harness && rl.harness !== 'claude') parts.push(rl.harness);
  if (rl.model) parts.push(rl.model);
  if (rl.effort) parts.push(`effort ${rl.effort}`);
  if (rl.sandbox) parts.push(`sandbox ${rl.sandbox}`);
  if (rl.approval) parts.push(`approval ${rl.approval}`);
  if (rl.tools) parts.push(`tools ${rl.tools}`);
  const nPerms = (rl.permissions || []).length;
  if (nPerms) parts.push(`${nPerms} perm${nPerms === 1 ? '' : 's'}`);
  return parts.join(' · ');
}

export {
  loadRoles, cachedRoles, invalidateRoles,
  createRole, updateRole, deleteRole, roleSummary,
};
