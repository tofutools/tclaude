const API = '/api/sandbox-profiles';

async function request(path, options = {}) {
  const response = await fetch(path, { credentials: 'same-origin', ...options });
  if (!response.ok) {
    // Failures carry the daemon's structured {"error", "code"} body; the
    // status and typed code stay on the thrown Error so callers can key
    // recovery (e.g. break_glass_acknowledgement_required) off them instead
    // of pattern-matching message text.
    const raw = await response.text();
    let body = null;
    try { body = JSON.parse(raw); } catch (_) { body = null; }
    const error = new Error(body?.message || body?.error || raw || `HTTP ${response.status}`);
    error.status = response.status;
    if (body?.code) error.code = body.code;
    throw error;
  }
  if (response.status === 204) return null;
  return response.json().catch(() => ({}));
}

export async function loadSandboxProfiles() { const value = await request(API); return Array.isArray(value) ? value : []; }
export async function previewSandboxProfile(name, body) {
  const target = name ? `${API}/${encodeURIComponent(name)}` : API;
  return request(`${target}?dry_run=1`, { method: name ? 'PATCH' : 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
}
export async function saveSandboxProfile(name, body, revision = '') {
  const target = name ? `${API}/${encodeURIComponent(name)}?revision=${encodeURIComponent(revision)}` : API;
  return request(target, { method: name ? 'PATCH' : 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
}
export function deleteSandboxProfile(name) { return request(`${API}/${encodeURIComponent(name)}`, { method: 'DELETE' }); }
export async function exportSandboxProfiles(names) { const query = new URLSearchParams(); names.forEach((name) => query.append('name', name)); return request(`${API}/export?${query}`); }
export function inspectSandboxImport(envelope) { return request(`${API}/import/inspect`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(envelope) }); }
export function importSandboxProfiles(envelope, onConflict, breakGlassAcknowledged = false) { return request(`${API}/import`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ ...envelope, on_conflict: onConflict, apply_assignments: false, ...(breakGlassAcknowledged ? { break_glass_acknowledged: true } : {}) }) }); }
export function inspectSandboxDirectories(body) { return request('/api/sandbox-profile-directories/inspect', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }); }
export function createSandboxDirectories(body) { return request('/api/sandbox-profile-directories/create', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }); }

export function sandboxProfileSummary(profile) {
  const fs = profile.filesystem || []; const env = profile.environment || []; const inc = profile.includes || []; const own = profile.agent_directories || []; const bg = profile.break_glass_filesystem || [];
  const parts = [['read', 'read'], ['write', 'write'], ['deny', 'deny']].flatMap(([access, label]) => { const count = fs.filter((entry) => entry.access === access).length; return count ? [`${count} ${label}`] : []; });
  if (inc.length) parts.push(`${inc.length} include${inc.length === 1 ? '' : 's'}`);
  if (env.length) parts.push(`${env.length} env key${env.length === 1 ? '' : 's'}`);
  if (own.length) parts.push(`${own.length} agent dir${own.length === 1 ? '' : 's'}`);
  if (profile.network_access) parts.push(`network ${profile.network_access}`);
  if (profile.read_baseline === 'minimal') parts.push('minimal reads');
  if (bg.length) parts.unshift(`⚠ ${bg.length} break-glass`);
  return parts.join(' · ') || 'no sandbox rules';
}
