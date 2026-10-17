// palette-recent.js — the command palette's "recently run" memory.
//
// Opening the palette with an empty query lists the operator's last few
// commands first (most recent on top, pre-selected), so Ctrl/Cmd-K ↵
// repeats the last action. Once the operator types, ranking is the plain
// match ladder from palette-score.js, with recency only breaking ties
// between equally good matches.
//
// Commands carry no stable id, so the key is `id` when a command sets one
// and its presented label otherwise. Label keys deliberately make a
// per-agent command follow its title, and a recent command that no longer
// exists (agent retired, group deleted) simply drops out of the block.
// Like the rest of this split, the module is pure apart from the injected
// store, so node:test imports it directly.

export const RECENT_PREF_KEY = 'tclaude.dash.palette.recent';
// RECENT_SHOWN is how many recent commands lead the empty-query list;
// RECENT_STORED keeps a deeper history so commands that are temporarily
// absent (an offline agent's Focus) reappear once they are back.
export const RECENT_SHOWN = 5;
export const RECENT_STORED = 20;

export function commandKey(command) {
  return String(command?.id || command?.label || '');
}

// rememberRecent moves key to the front of keys, deduplicated and capped.
export function rememberRecent(keys, key, cap = RECENT_STORED) {
  if (!key) return keys.slice(0, cap);
  return [key, ...keys.filter(k => k !== key)].slice(0, cap);
}

// withRecentFirst returns the empty-query list: up to `limit` enabled
// recent commands (copied with recent: true, the last one also marking
// recentLast for the divider) followed by everything else in build order.
// A key matches only its first command, so a duplicated label is not
// promoted twice.
export function withRecentFirst(commands, recentKeys, limit = RECENT_SHOWN) {
  if (!recentKeys.length) return commands.slice();
  const byKey = new Map();
  for (const command of commands) {
    const key = commandKey(command);
    if (key && !byKey.has(key)) byKey.set(key, command);
  }
  const promoted = new Set();
  const head = [];
  for (const key of recentKeys) {
    if (head.length >= limit) break;
    const command = byKey.get(key);
    if (!command || command.enabled === false || promoted.has(command)) continue;
    promoted.add(command);
    head.push({ ...command, recent: true });
  }
  if (!head.length) return commands.slice();
  head[head.length - 1].recentLast = true;
  return [...head, ...commands.filter(command => !promoted.has(command))];
}

// parseRecent / serializeRecent are the stored pref's wire shape: a JSON
// array of keys. Anything malformed reads as an empty history.
export function parseRecent(raw) {
  if (!raw) return [];
  try {
    const value = JSON.parse(raw);
    return Array.isArray(value)
      ? value.filter(k => typeof k === 'string' && k).slice(0, RECENT_STORED)
      : [];
  } catch (_) {
    return [];
  }
}

export function serializeRecent(keys) {
  return JSON.stringify(keys.slice(0, RECENT_STORED));
}

// createMemoryRecentStore is the default for tests and for a palette
// constructed without persistence.
export function createMemoryRecentStore(initial = []) {
  let keys = initial.slice();
  return {
    load: () => keys.slice(),
    save: (next) => { keys = next.slice(); },
  };
}

// createPrefsRecentStore persists through dashPrefs (or any getItem /
// setItem pair), best-effort like the other dashboard prefs.
export function createPrefsRecentStore(prefs, key = RECENT_PREF_KEY) {
  return {
    load: () => {
      try { return parseRecent(prefs.getItem(key)); } catch (_) { return []; }
    },
    save: (keys) => {
      try { prefs.setItem(key, serializeRecent(keys)); } catch (_) { /* best-effort */ }
    },
  };
}
