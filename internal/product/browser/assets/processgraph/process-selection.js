// process-selection.js -- pure selection and marquee math shared by the
// process graph shell and editor. Kept DOM-free so Node tests exercise the
// exact browser implementation.

export function selectionItems(selection) {
  if (!selection) return [];
  return selection.type === 'multi' ? [...(selection.items || [])] : [selection];
}

export function selectionKey(item) {
  if (!item) return '';
  if (item.type === 'node') return `node:${item.id}`;
  if (item.type === 'edge') return item.id != null
    ? `edge-id:${item.id}`
    : `edge:${JSON.stringify([item.from, item.outcome])}`;
  return '';
}

export function makeSelection(items) {
  const unique = new Map();
  for (const item of items || []) {
    const key = selectionKey(item);
    if (key) unique.set(key, item);
  }
  const selected = [...unique.values()];
  if (!selected.length) return null;
  if (selected.length === 1) return selected[0];
  return { type: 'multi', items: selected };
}

export function selectionContains(selection, item) {
  const key = selectionKey(item);
  return !!key && selectionItems(selection).some((candidate) => selectionKey(candidate) === key);
}

export function toggleSelection(selection, item) {
  const key = selectionKey(item);
  const items = selectionItems(selection);
  return makeSelection(selectionContains(selection, item)
    ? items.filter((candidate) => selectionKey(candidate) !== key)
    : [...items, item]);
}

export function normalizeMarquee(start, end) {
  return {
    left: Math.min(start.x, end.x), right: Math.max(start.x, end.x),
    top: Math.min(start.y, end.y), bottom: Math.max(start.y, end.y),
  };
}

export function nodesInMarquee(nodes, start, end) {
  const box = normalizeMarquee(start, end);
  return (nodes || []).filter((node) => {
    const left = node.x - node.width / 2;
    const right = node.x + node.width / 2;
    const top = node.y - node.height / 2;
    const bottom = node.y + node.height / 2;
    return right >= box.left && left <= box.right && bottom >= box.top && top <= box.bottom;
  });
}
