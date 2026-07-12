export const ACCESS_SUBTABS = ['permissions', 'slugs', 'sudo'];

export const SUDO_COLUMNS = [
  { label: 'Agent', key: 'conv' },
  { label: 'Slug', key: 'slug' },
  { label: 'Granted at', key: 'granted' },
  { label: 'Expires in', key: 'expires' },
  { label: 'Reason', key: 'reason' },
  { label: 'Granted by', key: 'by' },
  { label: '', key: null },
];

const sudoValue = (row, key) => ({
  conv: row.conv_title || row.agent_id || row.conv_id,
  slug: row.slug,
  granted: row.granted_at,
  expires: row.remaining_seconds,
  reason: row.reason,
  by: row.granted_by,
})[key];

function compare(left, right) {
  const a = left ?? '';
  const b = right ?? '';
  if (typeof a === 'number' && typeof b === 'number') return a - b;
  return String(a).toLowerCase().localeCompare(String(b).toLowerCase());
}

export function sortSudo(rows, sort) {
  if (!sort?.key) return rows.slice();
  const direction = sort.dir === 'desc' ? -1 : 1;
  return rows.slice().sort((left, right) => {
    const primary = compare(sudoValue(left, sort.key), sudoValue(right, sort.key));
    if (primary) return primary * direction;
    return (Number(left.id) || 0) - (Number(right.id) || 0);
  });
}

export function matchesSudo(row, query) {
  const needle = String(query || '').trim().toLowerCase();
  return !needle || [row.conv_title, row.conv_id, row.agent_id, row.slug, row.reason, row.granted_by]
    .some((value) => String(value || '').toLowerCase().includes(needle));
}

export function remainingSeconds(row, nowMs, snapshotMs) {
  const expires = Date.parse(row.expires_at || '');
  if (Number.isFinite(expires)) return Math.max(0, Math.ceil((expires - nowMs) / 1000));
  const elapsed = Number.isFinite(snapshotMs) ? Math.max(0, Math.floor((nowMs - snapshotMs) / 1000)) : 0;
  return Math.max(0, (Number(row.remaining_seconds) || 0) - elapsed);
}

export function fmtRemaining(seconds) {
  const secs = Math.max(0, Math.floor(Number(seconds) || 0));
  if (secs <= 0) return 'expired';
  if (secs < 60) return secs + 's';
  if (secs < 3600) {
    const minutes = Math.floor(secs / 60);
    const rest = secs % 60;
    return rest ? `${minutes}m${rest}s` : `${minutes}m`;
  }
  const hours = Math.floor(secs / 3600);
  const minutes = Math.floor((secs % 3600) / 60);
  return minutes ? `${hours}h${minutes}m` : `${hours}h`;
}

export function permissionRows(permissions, agents) {
  const titleByConv = new Map((agents || []).map((agent) => [agent.conv_id, agent.title]));
  const agentByConv = new Map((agents || []).map((agent) => [agent.conv_id, agent.agent_id]));
  return Object.entries(permissions?.overrides || {}).map(([convId, effects]) => {
    const granted = [];
    const denied = [];
    for (const [slug, effect] of Object.entries(effects || {})) {
      (effect === 'deny' ? denied : granted).push(slug);
    }
    granted.sort();
    denied.sort();
    return { convId, agentId: agentByConv.get(convId) || '', title: titleByConv.get(convId) || '(unknown)', granted, denied };
  }).sort((left, right) => left.convId.localeCompare(right.convId));
}
