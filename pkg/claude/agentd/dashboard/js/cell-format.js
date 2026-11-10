// cell-format.js -- the small text formatters that dashboard table cells
// share: an error's message, a local timestamp, a byte size and a group
// scope label. Pure functions with no Preact and no imports, so any module
// can use them without pulling in a component tree.
//
// Each one keeps the output its callers showed before they shared it. A
// table that showed something different on purpose (a date-only stamp, an
// empty cell instead of a dash) asks for it through an option.

// errText is the text of a failed request or a thrown error.
export function errText(error) { return error?.message || String(error); }

// timeOf parses a timestamp (an RFC3339 string, epoch ms or a Date) into a
// Date, or null when it is missing, unparseable, or Go's zero time (year 1,
// which the daemon sends for a time that was never set).
export function timeOf(value) {
  if (value == null || value === '') return null;
  const date = value instanceof Date ? value : new Date(value);
  if (Number.isNaN(date.getTime()) || date.getUTCFullYear() <= 1) return null;
  return date;
}

// isZeroTime reports whether a timestamp is Go's zero time (year 1). A
// missing or unparseable value is not a zero time; timeOf covers those too.
export function isZeroTime(value) {
  if (value == null || value === '') return false;
  const date = value instanceof Date ? value : new Date(value);
  return !Number.isNaN(date.getTime()) && date.getUTCFullYear() <= 1;
}

// localStamp is a timestamp in the viewer's locale ("6/16/2026, 10:30:00
// AM"), or empty when there is no real time to show. dateOnly drops the time
// of day, for a dense list where the day is enough.
export function localStamp(value, { empty = '—', dateOnly = false } = {}) {
  const date = timeOf(value);
  if (!date) return empty;
  return dateOnly ? date.toLocaleDateString() : date.toLocaleString();
}

// binarySize is a byte count in binary units: "512 B", "3.4 KiB", "12.0 MiB".
// A missing or negative count is empty.
export function binarySize(n, empty = '—') {
  if (!(n >= 0)) return empty;
  if (n < 1024) return `${n} B`;
  if (n < 1 << 20) return `${(n / 1024).toFixed(1)} KiB`;
  return `${(n / (1 << 20)).toFixed(1)} MiB`;
}

// groupScopeLabel names a group-scoped job or rule: "group:<name>", or the
// group's id when its name is not known.
export function groupScopeLabel(name, id) {
  return `group:${name || ('#' + id)}`;
}
