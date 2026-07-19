// terminals-tab.js is the DOM-free compatibility facade for every terminal
// launcher outside the Preact Terminal Shell feature. Callers keep stable
// functions while the mounted feature owns descriptors, chrome and lifecycle.

import { createAgentRosterReconciler, normalizeSeed } from './terminals-core.js';

let controller = null;
let prepareRuntime = null;
const reconcileAgentRoster = createAgentRosterReconciler();

export function registerTerminalShellController(next, runtimeLoader = null) {
  if (controller) throw new Error('terminal shell controller already registered');
  controller = next;
  prepareRuntime = runtimeLoader;
  return () => {
    if (controller !== next) return;
    controller = null;
    prepareRuntime = null;
  };
}

export function openTermModal(options) {
  const current = controller;
  if (!current) return null;
  if (!normalizeSeed({ ws: options?.wsPath || options?.ws })) return null;
  const descriptor = { ...options, initialRetry: options?.initialRetry !== false };
  if (!prepareRuntime) return current.openModal(descriptor);
  return prepareRuntime().then(
    () => controller === current ? current.openModal(descriptor) : null,
    (error) => {
      console.error('terminal runtime load failed:', error);
      return null;
    },
  );
}

export function openTerminalPane(seedOrPromise, { reveal = true } = {}) {
  return Promise.resolve(seedOrPromise).then((rawSeed) => {
    const normalized = normalizeSeed(rawSeed);
    const seed = normalized && { ...normalized, initialRetry: normalized.initialRetry !== false };
    if (!seed) return null;
    const current = controller;
    if (!current) return null;
    if (!prepareRuntime) return current.openPane(seed, { reveal });
    return prepareRuntime().then(
      () => controller === current ? current.openPane(seed, { reveal }) : null,
      (error) => {
        console.error('terminal runtime load failed:', error);
        return null;
      },
    );
  });
}

export function openWebWindowPane(agent, label, options) {
  openTerminalPane({
    ws: `/api/open-window-ws/${encodeURIComponent(agent)}`,
    label,
    key: `window:${agent}`,
    hideConv: agent,
    agent,
  }, options);
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

export function focusTerminalForConv(selectors, options) {
  return controller?.focusForSelectors(selectors, options) || false;
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
