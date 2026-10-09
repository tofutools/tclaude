// fleet-harness-model.js — DOM-free helpers for the Fleet → Harnesses page:
// which harnesses each node has, at what version, and what an install,
// update or credential operation there means.

// HARNESS_ORDER is the column order when no node has answered yet.
export const HARNESS_ORDER = Object.freeze(['claude', 'codex', 'opencode', 'copilot', 'gemini']);

// CREDENTIAL_FILE_HARNESSES keep their login in files the daemon can copy,
// back up and restore; others (Copilot, keychain or environment logins) need
// a login on the node itself.
export const CREDENTIAL_FILE_HARNESSES = Object.freeze(['claude', 'codex', 'gemini', 'opencode']);

// SHARE_WARNING is repeated wherever credentials leave this node.
export const SHARE_WARNING = 'Agents on the target node will act as you with this provider: anything they do is billed to and attributed to your account.';

// nodeBase addresses a node's harness routes: this node's own, or a peer's
// through the local peer proxy (which checks the peer's grants).
export function nodeBase(node) {
  return node.local ? '/api' : `/api/peer/${encodeURIComponent(node.id)}`;
}

// harnessColumns orders the harness columns: the first answering node's
// order, then any extra names, then the defaults.
export function harnessColumns(availabilities) {
  const out = [];
  for (const a of availabilities) for (const h of a?.harnesses || []) if (h?.name && !out.includes(h.name)) out.push(h.name);
  for (const n of HARNESS_ORDER) if (!out.includes(n)) out.push(n);
  return out;
}

// cellView summarises one harness on one node.
export function cellView(h) {
  if (!h) return { state: 'unknown', text: '—', title: 'Not reported' };
  if (h.version_status === 'not_spawnable') return { state: 'na', text: 'n/a', title: `${h.display_name || h.name} cannot be spawned on this node` };
  if (!h.installed) return { state: 'missing', text: 'not installed', title: `${h.display_name || h.name} is not installed` };
  const version = h.version || (h.version_status === 'unknown' ? 'version ?' : 'installed');
  const update = h.update_available === true;
  const parts = [`${h.display_name || h.name} ${version}`];
  if (update && h.latest_version) parts.push(`latest ${h.latest_version}`);
  if (h.credential_present === true) parts.push('login files present');
  if (h.usable === false) parts.push('not usable');
  else if (h.usable == null) parts.push('not verified');
  if (h.path) parts.push(h.path);
  return { state: update ? 'update' : h.usable === false ? 'broken' : 'ok', text: version, update, latest: h.latest_version || '', credential: h.credential_present === true, title: parts.join(' · ') };
}

// jobActive says whether a harness job is still going (poll it).
export function jobActive(job) {
  return job?.state === 'running' || job?.state === 'waiting_idle';
}

// accessHint explains a peer refusing a harness route.
export function accessHint(error, what = 'harness availability') {
  const code = error?.code || '';
  if (error?.status === 403 || code === 'permission_denied' || code === 'not_trusted') return `${what} is not shared with you`;
  if (code === 'peer_unreachable' || code === 'peer_busy' || error?.status === 502 || error?.status === 503) return 'unreachable';
  return error?.message || 'unavailable';
}
