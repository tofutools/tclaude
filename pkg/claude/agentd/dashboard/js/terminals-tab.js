// terminals-tab.js is the DOM-free compatibility facade for every terminal
// launcher outside the Preact Terminal Shell feature. Callers keep stable
// functions while the mounted feature owns descriptors, chrome and lifecycle.

import { revealInCompanion } from './companion-facade.js';
import { agentSessionSeed } from './terminal-window-model.js';
import { createAgentRosterReconciler, normalizeSeed } from './terminals-core.js';
import { terminalAttachConfig } from './terminal-attach-config.js';
import { detachWindowFeatures } from './terminal-drag-out.js';
import { encodeTerminalOpenHash } from './terminal-handoff.js';
import { isWizardActive } from './slop.js';

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

// The overlay windows answer for a terminal floating in one, which this facade
// leaves to them to stay free of Preact.
let floatingTerminals = () => false;
export function setFloatingTerminals(lookup) {
  floatingTerminals = lookup;
}

// An agent is in one place at a time, and every way of showing it comes through
// here. A window holds it first. A live pane in this tab beats a companion tab
// that was left pinned: that pane is what has the session.
function revealAgentPlace(selector) {
  if (revealInCompanion(selector, { tabs: false })) return true;
  return !controller?.hasAgentPane?.([selector]) && revealInCompanion(selector);
}

// revealHeldSeed shows a terminal somewhere other than a tab here, and reports
// whether it was: a floating window, or a place that holds the agent.
export function revealHeldSeed(seed) {
  return floatingTerminals(seed.key || seed.ws) || (agentSessionSeed(seed) && revealAgentPlace(seed.agent));
}

export function openTerminalPane(seedOrPromise, { reveal = true, activate = true } = {}) {
  // activate rides along only when a caller opts out, so every ordinary open
  // keeps the plain { reveal } contract.
  const paneOptions = activate ? { reveal } : { reveal, activate };
  return Promise.resolve(seedOrPromise).then((rawSeed) => {
    const normalized = normalizeSeed(rawSeed);
    const seed = normalized && { ...normalized, initialRetry: normalized.initialRetry !== false };
    if (!seed) return null;
    const key = seed.key || seed.ws;
    if (revealHeldSeed(seed)) return { key, floating: true };
    const current = controller;
    if (!current) return null;
    // A board pane has no xterm to load.
    if (!prepareRuntime || seed.board) return current.openPane(seed, paneOptions);
    return prepareRuntime().then(
      () => controller === current ? current.openPane(seed, paneOptions) : null,
      (error) => {
        console.error('terminal runtime load failed:', error);
        return null;
      },
    );
  });
}

// Returns openTerminalPane's promise (the opened pane, or null). Every existing
// caller is fire-and-forget; the deep-link restore in terminal-nav-route.js
// needs to know whether the agent's terminal actually came up.
export function openWebWindowPane(agent, label, options) {
  const { harness = '', ...paneOptions } = options || {};
  return openTerminalPane({
    ws: `/api/open-window-ws/${encodeURIComponent(agent)}`,
    label,
    key: `window:${agent}`,
    hideConv: agent,
    agent,
    harness,
  }, paneOptions);
}

// detachSeedWindow opens a seed in a browser window of its own, the page a
// detached Terminals tab lands on. It returns the window, or null when pop-ups
// are blocked.
export function detachSeedWindow(seed, { size = null, at = null } = {}) {
  const features = size ? detachWindowFeatures({ size, at, screen: globalThis.screen }) : '';
  let target = null;
  try { target = globalThis.open('about:blank', '_blank', features || undefined); } catch { target = null; }
  if (!target) return null;
  const solo = { ...seed, initialRetry: true, wizard: isWizardActive(), terminalAttach: terminalAttachConfig() };
  try { target.location.replace(`/terminals?solo=1${encodeTerminalOpenHash(solo)}`); } catch { return null; }
  return target;
}

// detachWebWindow opens an agent's terminal in a browser window of its own.
export function detachWebWindow(agent, label, { harness = '', size = null, at = null } = {}) {
  return detachSeedWindow({
    ws: `/api/open-window-ws/${encodeURIComponent(agent)}`, label, key: `window:${agent}`, hideConv: agent, agent, harness, floats: true,
  }, { size, at });
}

export function openWebTermPane(agent, label, whichOrPromise, { group = '', ...paneOptions } = {}) {
  return openTerminalPane(
    Promise.resolve(whichOrPromise).then((which) => (which
      ? {
        ws: `/api/term-ws/${encodeURIComponent(agent)}?which=${encodeURIComponent(which)}${group ? `&group=${encodeURIComponent(group)}` : ''}`,
        label,
        key: `term:${agent}:${which}:${group}`,
        agent,
      }
      : null)),
    paneOptions,
  );
}

export function openGroupWebTermPane(group, label, options) {
  return openTerminalPane({
    ws: `/api/group-term-ws/${encodeURIComponent(group)}`,
    label,
    key: `groupterm:${group}`,
  }, options);
}

// waitForTerminalSettled resolves once the terminal opened under key has
// attached and drawn its first screen (true), or false on timeout / when no
// terminal shell is mounted.
export function waitForTerminalSettled(key, options) {
  return controller?.waitForPaneSettled?.(key, options) || Promise.resolve(false);
}

// focusTerminalForConv shows an agent wherever it already is, and reports whether
// it was anywhere: a caller opens a place for it only when not.
export function focusTerminalForConv(selectors, options) {
  if ((selectors || []).some(revealAgentPlace)) return true;
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
