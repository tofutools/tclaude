// phone-agent-actions.js -- what a phone can do to an agent, through the
// desktop's own entry points (the row action handler, the member editor, the
// spawn dialog), so the confirmations and transactions are the desktop's.
// An agent's menu (phone-agent-menu.js AgentMenu) runs these, wherever it is
// opened from: the Agents view's rows, Now's rows and cards, the terminal's ⋯.

import { handleRowAction } from './row-action-handler.js';
import { openWebWindowPane } from './terminals-tab.js';
import { openAgentSpawnModal } from './agent-spawn-controller.js';
import { featureState } from './feature-state-registry.js';
import { writeClipboard } from './insecure-origin.js';
import { navigateToPath } from './nav-history.js';
import { grantFor } from './phone-grant.js';
import { shellToast } from './shell-state.js';
import { openInClaudeApp } from './claude-app.js';

// phoneAgentActions takes onComposeMessage(seed), the ✉ Message the page
// wires (the phone's message sheet), and navigate(path), how the page goes
// to a tab (Now's leaves its own view first).
export function phoneAgentActions({ onComposeMessage = null, navigate = navigateToPath, notify = shellToast } = {}) {
  return Object.freeze({
    openTerminal: (row) => openWebWindowPane(row.agent, row.name, { harness: row.harness }),
    message: (row) => onComposeMessage?.({ agent: row.agent, label: row.name }),
    // openClaudeApp turns Remote Access on (turnOn) and opens the session in
    // the Claude app (claude-app.js); it opens its tab inside the tap.
    openClaudeApp: (row, turnOn) => openInClaudeApp({ conv: row.agent || row.conv, label: row.name, turnOn, notify }),
    // rowAction takes the same data a desktop row's data-* attributes carry.
    rowAction: (act, row) => handleRowAction({ data: {
      act, conv: row.conv, agent: row.agent, label: row.name, harness: row.harness,
      group: row.group, online: row.online ? '1' : '0',
    } }),
    rename: (row, group) => featureState('groups-actions')?.openMemberEditor(row.member, group.group, 'title'),
    spawn: (groupName) => openAgentSpawnModal({ groupName }),
    // groupAction is a group's own control (power-on-group, shutdown-group,
    // view-group-messages), as the desktop group header's data-* carry it:
    // the desktop's confirm, POST and toast.
    groupAction: (act, groupName) => handleRowAction({ data: { act, group: groupName } }),
    // grant opens Access › + Grant on this agent's step (phone-grant.js).
    grant: (row) => {
      grantFor.value = row.member?.agent_id || row.agent;
      navigate('/access');
    },
    // explain says why an entry is greyed (a tap on Interrupt between turns).
    explain: (text) => notify(text),
    // copy puts text on the clipboard; what names it in the toast.
    copy: async (text, what) => {
      if (await writeClipboard(text)) notify(`Copied ${what}`);
      else notify(`${what}: ${text}`);
    },
  });
}
