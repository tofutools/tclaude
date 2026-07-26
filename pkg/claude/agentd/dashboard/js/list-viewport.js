// list-viewport.js — shared viewport-to-row-count measurement.
//
// Keyboard choosers use this for PageUp / PageDown. DOM shims and hidden
// lists report no row height, so callers get a stable fallback rather than a
// dead key.

export const DEFAULT_PAGE_SIZE = 10;

export function visiblePageSize(container, rowSelector, fallback = DEFAULT_PAGE_SIZE) {
  const first = container?.querySelector?.(rowSelector);
  const rowHeight = first?.offsetHeight || 0;
  if (!rowHeight) return fallback;
  return Math.max(1, Math.floor(container.clientHeight / rowHeight));
}
