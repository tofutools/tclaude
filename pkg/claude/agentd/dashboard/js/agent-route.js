// agent-route.js -- an agent's place in the Agents view: /groups?agent=<id>
// (an agent id, or a conversation id for an agent with none). Opening it
// shows the agent in its group: the phone's Agents view (phone-agents-
// island.js) opens that group up, Show offline on when the agent is offline,
// scrolls to its row and makes the row glow a moment. It is a one-shot
// overlay route (overlay-routes.js): once the view has found the agent, or
// said it is not here, the location drops the param in place, so Back goes
// to where the link was tapped and a reload does not find it again.
//
// Generic on purpose: the Mail reader's Find › uses it today; Now or a
// board can link an agent the same way.

import { signal } from '@preact/signals';
import { announceOverlay, registerOverlayRoute } from './overlay-routes.js';

// The ids an agent goes by (agt_…, a conversation's uuid) and nothing a URL
// could smuggle more into.
export const AGENT_ROUTE = /^[A-Za-z0-9_-]{1,128}$/;

// agentFocus is the agent the location asks the view to find, { id, n }
// (n tells one ask from the next for the same agent), or null.
export const agentFocus = signal(null);
let asks = 0;

// agentPath is the link to an agent in the Agents view. On a peer's page
// the link keeps node=, as every location there does.
export function agentPath(id) {
  const query = new URLSearchParams({ agent: String(id || '') });
  const remote = globalThis.__tclaudeRemoteNode?.id;
  if (remote) query.set('node', remote);
  return `/groups?${query}`;
}

// findAgentRoute asks the view to find `id` (a link tapped in this page).
export async function findAgentRoute(id) {
  const { navigateToPath } = await import('./nav-history.js');
  return navigateToPath(agentPath(id));
}

// doneAgentFocus lets go of the ask: the view found the agent or said it is
// not here. The location drops the param in place.
export function doneAgentFocus() {
  if (!agentFocus.value) return;
  agentFocus.value = null;
  announceOverlay({ replace: true });
}

registerOverlayRoute('agent', {
  value: () => agentFocus.value?.id || '',
  restore: (value) => {
    agentFocus.value = AGENT_ROUTE.test(value || '') ? { id: value, n: ++asks } : null;
  },
});
