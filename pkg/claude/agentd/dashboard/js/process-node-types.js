// Canonical process node-type chooser data. Both the editor dock and the
// command palette and connector-drop chooser consume this list without
// importing the edit model or editor DOM.
export const PROCESS_NODE_TYPES = [
  { type: 'task', label: 'Task', wizardLabel: 'Task rune', hint: 'A unit of work with a performer', requiresConfiguration: true },
  { type: 'decision', label: 'Decision', wizardLabel: 'Forking rune', hint: 'Branch on an explicit outcome', requiresConfiguration: true },
  { type: 'wait', label: 'Wait / timer', wizardLabel: 'Waiting rune', hint: 'Pause for a duration or signal', requiresConfiguration: true },
  { type: 'start', label: 'Start', wizardLabel: 'Opening rune', hint: 'Entry marker' },
  { type: 'end', label: 'End', wizardLabel: 'Closing rune', hint: 'Terminal node with a result' },
];
