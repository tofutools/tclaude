export const AUDIT_PAGE_SIZES = [50, 100, 250, 500];
export const AUDIT_COLUMNS = [
  { label: 'When', key: 'time' }, { label: 'Actor', key: 'actor' },
  { label: 'Action', key: 'verb' }, { label: 'Target', key: 'target' },
  { label: 'Detail', key: null }, { label: 'Outcome', key: 'status' },
];
export const AUDIT_SORT_KEYS = new Set(AUDIT_COLUMNS.map((column) => column.key).filter(Boolean));

export function verbClass(verb) {
  const value = verb || '';
  if (/(^|\.)(delete|retire|remove|stop|deny|revoke|prune|wipe|shutdown)(\.|$)/.test(value)) return 'audit-verb danger';
  if (/^(permissions|sudo|owner|approval|remote-access)/.test(value)) return 'audit-verb elevate';
  if (/^(spawn|clone|reincarnate|group\.create|member\.add|template\.instantiate|power\.on)/.test(value)) return 'audit-verb create';
  return 'audit-verb';
}

export function statusView(status) {
  const value = Number(status) || 0;
  if (value >= 200 && value < 300) return { className: 'state-pill state-working', label: 'ok', title: String(value) };
  if (value === 401 || value === 403) return { className: 'state-pill state-awaiting', label: 'denied', title: `${value} — permission denied` };
  if (value >= 400 && value < 500) return { className: 'state-pill state-awaiting', label: 'rejected', title: String(value) };
  if (value >= 500) return { className: 'state-pill state-offline', label: 'err', title: `${value} — error` };
  return { className: 'state-pill', label: value ? String(value) : '—', title: String(value) };
}

export function fmtAuditTime(iso) {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return iso || '—';
  const pad = (value) => String(value).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}

export function actorTitle(entry, shortID = '') {
  if (entry.actor_kind === 'human') return 'the human operator';
  if (entry.actor_kind === 'agent') return `${entry.actor_label || '(agent)'}${shortID ? ` ${shortID}` : ''}`;
  if (entry.actor_kind === 'system') return 'tclaude system observer';
  return entry.actor_label || 'unknown';
}
export function targetTitle(entry) { return [entry.group_name, entry.target_label, entry.target_agent || entry.target_conv].filter(Boolean).join(' ') || '—'; }
export function auditPageCount(total, size) { return Math.max(1, Math.ceil((Number(total) || 0) / (Number(size) || 100))); }

export function auditParams(view) {
  const params = new URLSearchParams({ page: String(view.page), page_size: String(view.pageSize), sort: view.sort, dir: view.dir });
  const query = String(view.query || '').trim();
  if (query) params.set('q', query);
  if (view.outcome) params.set('outcome', view.outcome);
  if (view.source) params.set('source', view.source);
  return params;
}
