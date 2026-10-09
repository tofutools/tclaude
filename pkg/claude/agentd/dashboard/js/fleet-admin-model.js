// fleet-admin-model.js — DOM-free view helpers for Fleet administration: which
// nodes this node trusts and what they may do. The data is this node's own
// federation state (/api/federation/*, cookie-authenticated, never served to
// peers); every trust-widening or destructive action goes through a confirm
// that names its consequence.

// UNRESTRICTED_CONSEQUENCE is repeated verbatim wherever unrestricted trust is
// granted: it implies every peer permission on every group, including groups
// created later. (Wording follows the daemon's confirmation_required error.)
export const UNRESTRICTED_CONSEQUENCE = 'Unrestricted trust grants this peer every peer permission on all groups — every live group and every group created later — plus automatic spawn and local unscoped grants towards it. Interactive terminal attach includes answering harness approval prompts.';

export function peerLabel(p) {
  return p?.label || p?.name || String(p?.instance_id || '').slice(0, 13) || 'peer';
}

// shortID abbreviates an instance ID for tables; dialogs always show it whole.
export function shortID(id) {
  const s = String(id || '');
  return s.length > 14 ? `${s.slice(0, 9)}…${s.slice(-4)}` : s;
}

// shortFingerprint abbreviates a dash-grouped fingerprint for tables.
export function shortFingerprint(fp) {
  const parts = String(fp || '').split('-');
  return parts.length > 2 ? `${parts[0]}-…-${parts[parts.length - 1]}` : String(fp || '');
}

// grantsByPeer groups the status's peer_grants per peer: total, node-wide
// (unscoped) and group-scoped counts.
export function grantsByPeer(grants) {
  const out = new Map();
  for (const g of Array.isArray(grants) ? grants : []) {
    if (!g?.peer) continue;
    const row = out.get(g.peer) || { total: 0, nodeWide: 0, scoped: 0 };
    row.total += 1;
    if (g.scope) row.scoped += 1; else row.nodeWide += 1;
    out.set(g.peer, row);
  }
  return out;
}

// poolsByPeer maps instance ID -> pool names from /api/federation/nodes/groups.
export function poolsByPeer(pools) {
  const out = new Map();
  for (const pool of Array.isArray(pools) ? pools : []) {
    for (const m of pool?.members || []) {
      const id = m?.instance_id || m?.InstanceID || m;
      if (typeof id !== 'string') continue;
      out.set(id, [...(out.get(id) || []), pool.name]);
    }
  }
  return out;
}

// adminView splits a federation status into the identity strip, trusted peers
// and the hub-visible instances waiting to be trusted.
export function adminView(status, { pools = [] } = {}) {
  if (!status?.instance_id) return null;
  const grants = grantsByPeer(status.peer_grants);
  const poolMap = poolsByPeer(pools);
  const peers = Array.isArray(status.peers) ? status.peers : [];
  const row = (p) => ({
    id: p.instance_id,
    label: peerLabel(p),
    name: p.name || '',
    fingerprint: p.fingerprint || '',
    level: p.level === 'unrestricted' ? 'unrestricted' : 'restricted',
    online: !!p.online,
    lastSeen: p.last_seen && !String(p.last_seen).startsWith('0001-') ? p.last_seen : null,
    trustedAt: p.trusted_at && !String(p.trusted_at).startsWith('0001-') ? p.trusted_at : null,
    grants: grants.get(p.instance_id) || { total: 0, nodeWide: 0, scoped: 0 },
    pools: poolMap.get(p.instance_id) || [],
  });
  const byLabel = (a, b) => a.label.localeCompare(b.label);
  return {
    self: {
      id: status.instance_id,
      name: status.name || 'this node',
      fingerprint: status.fingerprint || '',
      enabled: !!status.enabled,
      hubURL: status.hub_url || '',
      hubState: status.hub?.state || (status.enabled ? 'connecting' : 'disabled'),
      hubError: status.hub?.last_error || '',
    },
    trusted: peers.filter((p) => p?.trusted && p.instance_id).map(row).sort(byLabel),
    waiting: peers.filter((p) => p && !p.trusted && p.instance_id).map(row).sort(byLabel),
  };
}

// LABEL_RE mirrors the daemon's validFedLabel.
export const LABEL_RE = /^[a-z0-9_.-]{1,32}$/;

// trustBody builds the peers/trust request. A profile requires a preview
// token from a prior preview; unrestricted requires the exact fingerprint.
export function trustBody({ instance, label = '', level = 'restricted', profile = '', noDefaultProfile = false, preview = false, previewToken = '', confirmFingerprint = '' }) {
  const body = { instance };
  if (label) body.label = label;
  if (profile) body.profile = profile;
  else if (level) body.level = level;
  if (noDefaultProfile) body.no_default_profile = true;
  if (preview) body.preview = true;
  if (previewToken) body.preview_token = previewToken;
  if (confirmFingerprint) body.confirm_fingerprint = confirmFingerprint;
  return body;
}
