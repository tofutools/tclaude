// dashboard.close_terminal_tabs vocabulary for the Config tab. Mirrors the Go
// config.TerminalTab{Event,Target} values and the defaults of
// (*Config).CloseTerminalTab; the daemon applies the setting, so the browser
// only needs it to render and persist the matrix.
export const CLOSE_TAB_EVENTS = Object.freeze([
  ['session_ended', 'Session ended'],
  ['detached', 'Detached'],
]);

export const CLOSE_TAB_TARGETS = Object.freeze([
  ['group_terminal', 'Group terminal'],
  ['agent_terminal', 'Agent-dir terminal'],
  ['agent_window', 'Agent window'],
]);

export function closeTabDefault(event, target) {
  return event === 'session_ended' && (target === 'group_terminal' || target === 'agent_terminal');
}

// closeTabsChecked resolves every cell of a stored config value.
export function closeTabsChecked(stored) {
  const out = {};
  for (const [event] of CLOSE_TAB_EVENTS) {
    for (const [target] of CLOSE_TAB_TARGETS) {
      const value = stored?.[event]?.[target];
      out[`${event}.${target}`] = typeof value === 'boolean' ? value : closeTabDefault(event, target);
    }
  }
  return out;
}

// closeTabsStored folds the matrix back into config, storing only the cells
// that differ from their default; null when nothing does.
export function closeTabsStored(checked) {
  const out = {};
  for (const [event] of CLOSE_TAB_EVENTS) {
    for (const [target] of CLOSE_TAB_TARGETS) {
      const value = !!checked[`${event}.${target}`];
      if (value === closeTabDefault(event, target)) continue;
      (out[event] ||= {})[target] = value;
    }
  }
  return Object.keys(out).length ? out : null;
}
