// Shared imperative hover identity for the native Groups shell and its
// delegated pointer binder. A live binding lets reconciliation re-stamp the
// class without pulling the Groups component into refresh.js's legacy graph.
export let hoveredGroupKey = null;

export function setHoveredGroupKey(key) {
  hoveredGroupKey = key || null;
}
