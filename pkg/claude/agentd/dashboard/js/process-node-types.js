// Canonical process node-type chooser data. Both the editor dock and the
// command palette and connector-drop chooser consume this list without
// importing the edit model or editor DOM.
export const PROCESS_NODE_TYPES = [
  { type: 'task', label: 'Task', wizardLabel: 'Task rune', hint: 'A unit of work with a performer', requiresConfiguration: true },
  { type: 'decision', label: 'Decision', wizardLabel: 'Forking rune', hint: 'Branch on an explicit outcome', requiresConfiguration: true },
  { type: 'parallel', label: 'Parallel', wizardLabel: 'Splitting rune', hint: 'Fan out through every outgoing branch' },
  { type: 'subprocess', label: 'Subprocess', wizardLabel: 'Nested circle', hint: 'Run a nested graph as one node; double-click to enter' },
  // A wait step is stored but cannot run yet (the engine refuses it at run
  // creation): offered, dimmed, so the planned feature stays visible.
  { type: 'wait', label: 'Wait / timer', wizardLabel: 'Waiting rune', hint: "Pause for a duration or signal. Can't run yet: a process with one can't start a run", requiresConfiguration: true, cantRunYet: true },
  { type: 'start', label: 'Start', wizardLabel: 'Opening rune', hint: 'Entry marker' },
  { type: 'end', label: 'End', wizardLabel: 'Closing rune', hint: 'Terminal node with a result' },
];
