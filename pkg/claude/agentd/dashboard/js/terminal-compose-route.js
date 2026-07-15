import { isComposeMessageShortcut } from './terminal-interactions.js';

// Decide what the integrated Terminals tab should do with a keydown before
// touching DOM focus. "consume" is reserved for repeats while the operator
// composer is already open: they must not reopen it or leak to macOS Cmd+M.
export function terminalComposeShortcutAction(event, {
  tabActive = false,
  operatorModalOpen = false,
  blockingOverlayOpen = false,
  eligiblePane = false,
} = {}) {
  if (!isComposeMessageShortcut(event) || !tabActive) return 'ignore';
  if (operatorModalOpen) return 'consume';
  if (blockingOverlayOpen || !eligiblePane) return 'ignore';
  return 'open';
}
