// phone-agent-actions.js -- what a phone can do to an agent, through the
// desktop's own entry points (the row action handler, the member editor, the
// spawn dialog), so the confirmations and transactions are the desktop's.
// The Agents view's rows and the terminal's ⋯ open the same agent sheet
// (phone-agents-island.js AgentSheet) with these.

import { handleRowAction } from './row-action-handler.js';
import { openWebWindowPane } from './terminals-tab.js';
import { openAgentSpawnModal } from './agent-spawn-controller.js';
import { featureState } from './feature-state-registry.js';

// phoneAgentActions takes onComposeMessage(seed), the ✉ Message the page
// wires (the phone's message sheet).
export function phoneAgentActions({ onComposeMessage = null } = {}) {
  return Object.freeze({
    openTerminal: (row) => openWebWindowPane(row.agent, row.name, { harness: row.harness }),
    message: (row) => onComposeMessage?.({ agent: row.agent, label: row.name }),
    // rowAction takes the same data a desktop row's data-* attributes carry.
    rowAction: (act, row) => handleRowAction({ data: {
      act, conv: row.conv, agent: row.agent, label: row.name, harness: row.harness,
      group: row.group, online: row.online ? '1' : '0',
    } }),
    rename: (row, group) => featureState('groups-actions')?.openMemberEditor(row.member, group.group, 'title'),
    spawn: (groupName) => openAgentSpawnModal({ groupName }),
  });
}
