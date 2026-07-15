// terminals-tab.js is the DOM-free compatibility facade for every terminal
// launcher outside the Preact Terminal Shell feature. Callers keep stable
// functions while the mounted feature owns descriptors, chrome and lifecycle.

import { createAgentRosterReconciler } from './terminals-core.js';

let controller = null;
const reconcileAgentRoster = createAgentRosterReconciler();

export function registerTerminalShellController(next) {
  if (controller) throw new Error('terminal shell controller already registered');
  controller = next;
  return () => { if (controller === next) controller = null; };
}

export function openTermModal(options) {
  return controller?.openModal(options) || null;
}

export function openTerminalPane(seedOrPromise) {
  Promise.resolve(seedOrPromise).then((seed) => controller?.openPane(seed));
}

export function openWebWindowPane(agent, label) {
  openTerminalPane({
    ws: `/api/open-window-ws/${encodeURIComponent(agent)}`,
    label,
    key: `window:${agent}`,
    hideConv: agent,
    agent,
  });
}

export function openWebTermPane(agent, label, whichOrPromise) {
  openTerminalPane(
    Promise.resolve(whichOrPromise).then((which) => (which
      ? {
        ws: `/api/term-ws/${encodeURIComponent(agent)}?which=${encodeURIComponent(which)}`,
        label,
        key: `term:${agent}:${which}`,
        agent,
      }
      : null)),
  );
}

export function openGroupWebTermPane(group, label) {
  openTerminalPane({
    ws: `/api/group-term-ws/${encodeURIComponent(group)}`,
    label,
    key: `groupterm:${group}`,
  });
}

export function focusTerminalForConv(selectors) {
  return controller?.focusForSelectors(selectors) || false;
}

export function closeTerminalsForConvs(selectors) {
  controller?.closeForHide(selectors);
}

export function closeTerminalsForWindowOp(agents) {
  if (!Array.isArray(agents)) return;
  const selectors = [];
  for (const outcome of agents) {
    if (outcome?.outcome !== 'detached') continue;
    if (outcome.agent_id) selectors.push(outcome.agent_id);
    if (outcome.conv_id) selectors.push(outcome.conv_id);
  }
  if (selectors.length) closeTerminalsForConvs(selectors);
}

export function reconcileTerminalsForAgentRoster(nextAgents, authoritative) {
  const departed = reconcileAgentRoster(nextAgents, authoritative);
  if (departed.length) controller?.closeForAgents(departed);
}
