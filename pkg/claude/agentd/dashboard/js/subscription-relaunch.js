// An agent on a peer operator's own Claude subscription runs on a token its
// node holds in memory only; once that node's tclaude restarts, the agent
// cannot be relaunched until its owner sends the token again, or until it is
// switched to the node's own login (claude_subscription_agent.go). Before a
// resume, restart or reincarnate, ensureClaudeSubscription offers the one
// that fits who is looking.
import { dashboardState } from './snapshot-store.js';
import { fleetState } from './fleet-state.js';
import { isPageNode, nodeFetch, nodeSnapshot, selfNodeID } from './node-api.js';
import { nodeRef } from './agent-presence.js';
import { confirmModal, refresh, toast } from './refresh.js';

// subscriptionOf finds an agent's subscription marker in its node's snapshot.
export function subscriptionOf(agent, node = '', snapshot = null) {
  const snap = snapshot || (isPageNode(node) ? dashboardState.snapshot.value : nodeSnapshot(node));
  for (const g of snap?.groups || []) {
    for (const m of g.members || []) {
      if (m.agent_id === agent || m.conv_id === agent) return m.claude_subscription || null;
    }
  }
  return null;
}

async function call(node, agent, method) {
  const init = { method, credentials: 'same-origin', headers: {} };
  // Only the owner's own node fills in the token (its peer proxy); the page
  // sends just the choice.
  if (method === 'POST') { init.headers['Content-Type'] = 'application/json'; init.body = JSON.stringify({ claude_subscription: true }); }
  const r = await nodeFetch(node)(`/api/agents/${encodeURIComponent(agent)}/claude-subscription`, init);
  if (!r.ok) {
    let detail = await r.text();
    try { detail = JSON.parse(detail).error || detail; } catch (_) { /* plain text */ }
    throw new Error(detail);
  }
  return r.json().catch(() => ({}));
}

// subscriptionChoice words the dialog for an agent whose token is gone: its
// owner sends it again; anyone else may switch it to the node's own login.
export function subscriptionChoice({ sub, label, nodeName, verb = 'start it', fleet = fleetState.fleet.value, self = fleet?.self?.id || selfNodeID() }) {
  const owner = nodeRef(sub.peer, fleet);
  const mine = !!self && sub.peer === self;
  const ownerName = mine ? 'you' : owner.name || sub.label || 'its owner';
  if (mine) {
    return {
      method: 'POST',
      title: `Send your Claude subscription to ${nodeName} again?`,
      body: `${label} runs on your Claude subscription, and ${nodeName} no longer holds the token (its tclaude restarted, which forgets it). This sends your stored token again in an encrypted request, then can ${verb}. To stop using your subscription there instead, pick "Use ${nodeName}'s own login" in the agent's ⚙ menu.`,
      okLabel: 'Send & continue', done: `${label}: your subscription is back on ${nodeName}`,
    };
  }
  return {
    method: 'DELETE',
    title: `Switch ${label} to ${nodeName}'s own login?`,
    body: `${label} runs on ${ownerName}'s Claude subscription, and ${nodeName} no longer holds the token; only ${ownerName} can send it again. Switching makes it use ${nodeName}'s own Claude login from now on, billed there, and then can ${verb}.`,
    okLabel: 'Switch & continue', done: `${label} now uses ${nodeName}'s own login`,
  };
}

// ensureClaudeSubscription resolves true when the agent may be relaunched:
// it does not run on a lost token, or the operator just restored or switched
// it. false means cancelled or failed (already said).
// lost says the daemon just refused for a missing token, which a snapshot
// taken before may not show yet.
export async function ensureClaudeSubscription({ agent, label, node = '', verb, snapshot = null, lost = false }) {
  const sub = subscriptionOf(agent, node, snapshot);
  if (!sub) return !lost;
  if (sub.held && !lost) return true;
  const nodeName = isPageNode(node) ? (globalThis.document?.documentElement?.dataset?.remoteNodeName || 'this node') : nodeRef(node, fleetState.fleet.value).name;
  const choice = subscriptionChoice({ sub, label, nodeName, verb });
  try {
    const ok = await confirmModal({ title: choice.title, body: choice.body, okLabel: choice.okLabel, busyLabel: 'Working…', action: () => call(node, agent, choice.method) });
    if (!ok) return false;
  } catch (e) {
    toast(`${choice.okLabel.replace(' & continue', '')} failed: ${e?.message || e}`, true);
    return false;
  }
  toast(choice.done);
  refresh();
  return true;
}

// switchToOwnLogin is the ⚙ menu's explicit "Use <node>'s own login".
export async function switchToOwnLogin({ agent, label, node = '' }) {
  const nodeName = isPageNode(node) ? 'this node' : nodeRef(node, fleetState.fleet.value).name;
  try {
    const ok = await confirmModal({
      title: `Use ${nodeName}'s own login for ${label}?`,
      body: `${label} stops using the Claude subscription it was spawned on: ${nodeName} forgets the token, and from its next start it runs on ${nodeName}'s own Claude login, billed there. A running agent keeps its current session until it restarts.`,
      okLabel: 'Use own login', busyLabel: 'Switching…', action: () => call(node, agent, 'DELETE'),
    });
    if (!ok) return;
  } catch (e) {
    toast(`Switch failed: ${e?.message || e}`, true);
    return;
  }
  toast(`${label} now uses ${nodeName}'s own login`);
  refresh();
}
