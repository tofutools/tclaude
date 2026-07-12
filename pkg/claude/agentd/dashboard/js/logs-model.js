export const LOG_PAGE_SIZES = [50, 100, 250, 500];
export const LOG_LEVELS = ['', 'debug', 'info', 'warn', 'error'];
export const LOG_RANGES = [0, 900000, 3600000, 21600000, 86400000, 604800000];

export function levelKey(level) {
  return ({ debug: 'debug', info: 'info', warn: 'warn', error: 'error' })[String(level || '').toLowerCase()] || 'raw';
}

export function fmtAbsTime(iso) {
  if (!iso) return '—';
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return iso;
  const pad = (value, width = 2) => String(value).padStart(width, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} `
    + `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}.${pad(date.getMilliseconds(), 3)}`;
}

export function fieldsText(fields) {
  if (!fields) return '';
  return Object.entries(fields).map(([key, value]) =>
    `${key}=${value !== null && typeof value === 'object' ? JSON.stringify(value) : value}`).join('  ');
}

export function fmtInt(value) { return (Number(value) || 0).toLocaleString(); }
export function fmtBytes(value) {
  const bytes = Number(value) || 0;
  if (bytes >= 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  if (bytes >= 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${bytes} B`;
}

export function pageCount(total, pageSize) {
  return Math.max(1, Math.ceil((Number(total) || 0) / (Number(pageSize) || 100)));
}

// Log records have no server id and byte-identical records can coexist. Count
// duplicate fingerprints from the oldest end, so prepending a new identical
// tail record does not change the keys of already-rendered duplicates.
export function keyedLogRows(rows) {
  const seen = new Map();
  const result = Array(rows?.length || 0);
  for (let index = (rows?.length || 0) - 1; index >= 0; index -= 1) {
    const row = rows[index];
    const fingerprint = JSON.stringify(row);
    const occurrence = (seen.get(fingerprint) || 0) + 1;
    seen.set(fingerprint, occurrence);
    result[index] = { row, key: `${fingerprint}:${occurrence}` };
  }
  return result;
}

export function logsParams(view, now = Date.now()) {
  const params = new URLSearchParams({ page: String(view.page), page_size: String(view.pageSize) });
  const query = String(view.query || '').trim();
  if (query) params.set('q', query);
  if (view.level) params.set('level', view.level);
  if (view.rangeMs > 0) params.set('from', String(now - view.rangeMs));
  if (view.includeRotated) params.set('include_rotated', '1');
  if (view.hideRaw) params.set('hide_raw', '1');
  return params;
}
