// config-access.js — what the viewer may change in the Config tab.
//
// /api/config answers with an access block (config_remote.go): the caller's
// level (local, unrestricted, restricted), write or read per key class, the
// grant each class takes, and the class of every config key. On a peer's
// per-node page the tab reads the peer's config through it: a row whose keys
// the viewer may not change is read-only and says why, and a secret the peer
// withholds shows only whether it is set, with Replace and Clear where its
// key is writable. On this node's own page every row stays as it was.

import { signal } from '@preact/signals';

// configAccess is the loaded {access, redacted} block, or null before a load.
export const configAccess = signal(null);
// secretEdits are the withheld secrets the viewer chose to replace or clear
// since the last load.
export const secretEdits = signal({ replace: {}, clear: {} });

export function setConfigAccess(data) {
  configAccess.value = data?.access ? { access: data.access, redacted: Array.isArray(data.redacted) ? data.redacted : [] } : null;
  secretEdits.value = { replace: {}, clear: {} };
}

// remoteAccess reports whether the access block restricts anything: it is a
// peer's view (any level but local).
export function remoteAccess(model = configAccess.value) {
  return !!model?.access && model.access.level !== 'local';
}

// rowPaths expands a row's key label into config paths: "a.b · c" names
// a.b and a.c, and a trailing "*" matches every key with that prefix.
export function rowPaths(k) {
  if (!k) return [];
  const parts = String(k).split('·').map(s => s.trim()).filter(Boolean);
  if (!parts.length) return [];
  const first = parts[0];
  const parent = first.includes('.') ? first.slice(0, first.lastIndexOf('.') + 1) : '';
  return parts.map((p, i) => (i === 0 || p.includes('.') ? p : parent + p));
}

// keysFor lists the classified keys a path covers: the key itself, the keys
// under it, the key it lies inside, or every key matching a "prefix*".
export function keysFor(path, keys = []) {
  if (path.endsWith('*')) {
    const prefix = path.slice(0, -1);
    return keys.filter(k => k.path.startsWith(prefix));
  }
  return keys.filter(k => k.path === path || k.path.startsWith(path + '.') || path.startsWith(k.path + '.') || path.startsWith(k.path + '['));
}

// rowAccess is what the viewer may do with a row: state write, partial or
// read, and the note a read-only row shows.
export function rowAccess(k, model = configAccess.value) {
  if (!remoteAccess(model)) return { state: 'write', note: '' };
  const { access } = model;
  const keys = rowPaths(k).flatMap(p => keysFor(p, access.keys || []));
  if (!keys.length) return { state: 'read', note: k ? 'Not changed from a peer’s page' : '' };
  const classes = [...new Set(keys.map(key => key.class))];
  const writable = classes.filter(c => access.classes?.[c] === 'write');
  const state = writable.length === classes.length ? 'write' : writable.length ? 'partial' : 'read';
  if (state === 'write') return { state, note: '' };
  return { state, note: accessNote(classes.filter(c => access.classes?.[c] !== 'write'), access) };
}

// accessNote names what a key class the viewer may not change takes.
export function accessNote(classes, access) {
  if (classes.includes('local')) return 'Change on the node itself';
  const slugs = [...new Set(classes.map(c => access.slugs?.[c]).filter(Boolean))];
  if (slugs.length) return `Needs ${slugs.join(', ')} on this node`;
  return 'Needs unrestricted trust';
}

// panelAccess folds a panel's rows: write when every row is writable, read
// when none is, partial otherwise; '' on this node's own page.
export function panelAccess(rowKeys, model = configAccess.value) {
  if (!remoteAccess(model) || !rowKeys?.length) return '';
  const states = rowKeys.map(k => rowAccess(k, model).state);
  if (states.every(s => s === 'write')) return 'write';
  if (states.every(s => s === 'read')) return 'read';
  return 'partial';
}

// secretOf is the withheld secret a row edits ({path, set}), or null.
export function secretOf(k, model = configAccess.value) {
  if (!remoteAccess(model)) return null;
  const paths = rowPaths(k);
  return model.redacted.find(r => paths.includes(r.path)) || null;
}

export function replaceSecret(path) {
  const { replace, clear } = secretEdits.value;
  const nextClear = { ...clear };
  delete nextClear[path];
  secretEdits.value = { replace: { ...replace, [path]: true }, clear: nextClear };
}

export function clearSecret(path) {
  const { replace, clear } = secretEdits.value;
  const nextReplace = { ...replace };
  delete nextReplace[path];
  secretEdits.value = { replace: nextReplace, clear: { ...clear, [path]: true } };
}

export function keepSecret(path) {
  const { replace, clear } = secretEdits.value;
  const nextReplace = { ...replace };
  const nextClear = { ...clear };
  delete nextReplace[path];
  delete nextClear[path];
  secretEdits.value = { replace: nextReplace, clear: nextClear };
}

// clearedSecrets are the paths a save asks the peer to clear.
export function clearedSecrets() {
  return Object.keys(secretEdits.value.clear);
}

// panelKeys collects each panel's row keys as the rows render, for the
// rail's per-panel access tag.
const panelKeys = new Map();
export function registerPanelKey(panel, k) {
  if (!panel) return;
  if (!panelKeys.has(panel)) panelKeys.set(panel, new Set());
  panelKeys.get(panel).add(k || '');
}
export function panelRowKeys(panel) {
  return [...(panelKeys.get(panel) || [])].filter(Boolean);
}
