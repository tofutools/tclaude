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

// phoneAgentActions takes onComposeMessage(seed), the ✉ Message the page
// wires (the phone's message sheet), and navigate(path), how the page goes
// to a tab (Now's leaves its own view first).
export function phoneAgentActions({ onComposeMessage = null, navigate = navigateToPath, notify = shellToast } = {}) {
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
    // grant opens Access › + Grant on this agent's step (phone-grant.js).
    grant: (row) => {
      grantFor.value = row.member?.agent_id || row.agent;
      navigate('/access');
    },
    // copy puts text on the clipboard; what names it in the toast.
    copy: async (text, what) => {
      if (await writeClipboard(text)) notify(`Copied ${what}`);
      else notify(`${what}: ${text}`);
    },
  });
}
