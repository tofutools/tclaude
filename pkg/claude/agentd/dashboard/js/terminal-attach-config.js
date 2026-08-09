// Effective browser-terminal attachment geometry. The dashboard snapshot
// refreshes this module before newly opened widgets read it. A detached
// terminal carries a copy in its hash seed so the standalone page preserves
// the strategy without running the dashboard poller.

const DEFAULTS = Object.freeze({
  mode: 'repair',
  initial_resize_delay_ms: 0,
  repair_delay_ms: 250,
  pre_attach_delay_ms: 250,
});

let current = DEFAULTS;

function delay(value, fallback) {
  const number = Number(value);
  if (!Number.isFinite(number)) return fallback;
  return Math.max(0, Math.min(10000, Math.trunc(number)));
}

export function normalizeTerminalAttachConfig(value) {
  const source = value && typeof value === 'object' ? value : {};
  const mode = ['repair', 'initial', 'pre_attach'].includes(source.mode)
    ? source.mode : DEFAULTS.mode;
  return Object.freeze({
    mode,
    initial_resize_delay_ms: delay(source.initial_resize_delay_ms, DEFAULTS.initial_resize_delay_ms),
    repair_delay_ms: delay(source.repair_delay_ms, DEFAULTS.repair_delay_ms),
    pre_attach_delay_ms: delay(source.pre_attach_delay_ms, DEFAULTS.pre_attach_delay_ms),
  });
}

export function setTerminalAttachConfig(value) {
  current = normalizeTerminalAttachConfig(value);
}

export function terminalAttachConfig() {
  return current;
}

export function terminalAttachWidgetOptions(seedConfig = null) {
  const config = seedConfig ? normalizeTerminalAttachConfig(seedConfig) : current;
  return {
    attachResizeMode: config.mode,
    initialResizeDelayMs: config.initial_resize_delay_ms,
    postAttachResizeDelayMs: config.repair_delay_ms,
    preAttachDelayMs: config.pre_attach_delay_ms,
  };
}
