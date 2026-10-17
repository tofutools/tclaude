import { batch, computed, signal } from '@preact/signals';
import { rankCommands } from './palette-score.js';
import {
  commandKey, createMemoryRecentStore, rememberRecent, withRecentFirst,
} from './palette-recent.js';

// The palette's durable interaction model. Command construction is injected so
// this state stays independent of the legacy action modules that the commands
// delegate to; production passes buildCommands from palette.js together with
// dashboardState.snapshot, while tests can use small command fixtures. The
// recent-command history is likewise an injected { load, save } store
// (dashPrefs-backed in production, in-memory by default).
export function createPaletteState({
  snapshot, commandBuilder, onError = () => {}, recentStore = createMemoryRecentStore(),
} = {}) {
  if (!snapshot || !('value' in snapshot)) throw new TypeError('palette state requires a snapshot signal');
  if (typeof commandBuilder !== 'function') throw new TypeError('palette state requires a command builder');
  if (typeof onError !== 'function') throw new TypeError('palette state requires an error handler');

  const open = signal(false);
  const query = signal('');
  const selected = signal(0);
  const commands = signal([]);
  const recent = signal(recentStore.load());
  // An empty query leads with the recently run commands; once the operator
  // types, matching decides and recency only breaks score ties.
  const filtered = computed(() => {
    const keys = recent.value;
    if (!query.value.trim()) return withRecentFirst(commands.value, keys);
    const rank = new Map(keys.map((key, index) => [key, index]));
    return rankCommands(commands.value, query.value,
      (command) => rank.get(commandKey(command)) ?? Infinity);
  });
  const view = computed(() => ({
    open: open.value,
    query: query.value,
    selected: selected.value,
    commands: filtered.value,
  }));

  function rebuild() {
    batch(() => {
      commands.value = commandBuilder(snapshot.value || {});
      selected.value = 0;
    });
  }

  function show() {
    batch(() => {
      commands.value = commandBuilder(snapshot.value || {});
      recent.value = recentStore.load();
      query.value = '';
      selected.value = 0;
      open.value = true;
    });
  }

  function close() {
    if (!open.value) return false;
    open.value = false;
    return true;
  }

  function setQuery(value) {
    batch(() => {
      query.value = String(value ?? '');
      selected.value = 0;
    });
  }

  function setSelected(value) {
    const count = filtered.value.length;
    if (!count) return false;
    const next = Math.min(count - 1, Math.max(0, Number(value) || 0));
    if (selected.value === next) return false;
    selected.value = next;
    return true;
  }

  // Arrow navigation wraps, matching the legacy palette and native command
  // launchers where every option remains reachable with repeated keypresses.
  function move(delta) {
    const count = filtered.value.length;
    if (!count) return false;
    selected.value = (selected.value + delta + count) % count;
    return true;
  }

  // Page navigation deliberately clamps instead of wrapping. pageSize is read
  // from the rendered list by the island so it remains responsive to layout.
  function movePage(delta, pageSize) {
    const count = filtered.value.length;
    if (!count) return false;
    const size = Math.max(1, Number(pageSize) || 1);
    const next = selected.value + delta * size;
    selected.value = Math.min(count - 1, Math.max(0, next));
    return true;
  }

  function selectedCommand() {
    return filtered.value[selected.value] || null;
  }

  // Close before invoking the action so a command that opens another modal is
  // never stacked below the palette. beforeRun lets the Preact owner restore
  // focus synchronously before that next modal takes ownership.
  function runSelected({ beforeRun = () => {} } = {}) {
    const command = selectedCommand();
    if (!command) return false;
    if (command.enabled === false) return false;
    recent.value = rememberRecent(recent.value, commandKey(command));
    recentStore.save(recent.value);
    close();
    beforeRun();
    try {
      command.run();
    } catch (error) {
      onError('command failed: ' + ((error && error.message) || error), true);
    }
    return true;
  }

  return Object.freeze({
    open,
    query,
    selected,
    commands,
    recent,
    filtered,
    view,
    show,
    close,
    rebuild,
    setQuery,
    setSelected,
    move,
    movePage,
    selectedCommand,
    runSelected,
  });
}
