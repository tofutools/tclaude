// peer-view-limits.js — what a peer's per-node view (remote-node.js) offers.
//
// The peer decides what it shares (the snapshot's peer_view metadata) and
// refuses everything else; this module makes that visible instead of letting
// an operator click into a 403. Unavailable tabs, actions and management
// controls stay in place (no layout change) but are greyed with a hover
// explanation, and a click is stopped before any handler runs.

import { effect } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';
import { shellToast } from './shell-state.js';

// TAB_FEATURES maps the per-node tabs to the peer-view feature that serves
// them. Groups is the snapshot itself; the map is this operator's own fleet
// view. A tab absent here has no peer route at all.
const TAB_FEATURES = Object.freeze({ groups: 'agents.status', costs: 'costs', audit: 'audit', map: 'local', fleet: 'local' });

// READ_ONLY_ACTS are data-act controls that only change this browser's view
// or show snapshot data. The ⚙ menus open (their items are gated one by one).
// (The status dot is a power control — wake / shut down — so it is not here.)
const READ_ONLY_ACTS = new Set(['copy-generation-id', 'sandbox-details', 'group-menu', 'row-menu']);

// GATED_CONTROLS are mutation controls without a data-act: group creation and
// the management overlays, which edit the shown node's configuration.
const GATED_CONTROLS = [
  '#group-create-open', '#group-import-open', '#cleanup-all-open', '#delete-retired-open',
  '#profiles-manage-open', '#templates-manage-open', '#roles-manage-open',
  '#sandbox-profiles-manage-open', '#spawn-harness-policy-open',
];

// featureState reports whether the peer shares a feature: 'shared', 'omitted'
// (the peer could share it with a grant), or 'local' (not offered to any peer).
export function featureState(feature, peerView) {
  if (feature === 'local') return 'shared';
  if (!feature) return 'local';
  const included = Array.isArray(peerView?.included) ? peerView.included : null;
  const omitted = (Array.isArray(peerView?.omitted) ? peerView.omitted : []).map((o) => (typeof o === 'string' ? o : o?.feature));
  if (omitted.includes(feature)) return 'omitted';
  // Without metadata (an unrestricted peer's full snapshot still carries it;
  // before the first answer there is none) assume the snapshot-backed view.
  if (!included) return feature === 'agents.status' ? 'shared' : 'omitted';
  return included.includes(feature) ? 'shared' : 'omitted';
}

export function tabState(tab, peerView) {
  return featureState(TAB_FEATURES[tab] || '', peerView);
}

function currentPeerView() {
  return dashboardState.snapshot.value?.peer_view || null;
}

// peerViewTabUsable tells tab cycling and history routing whether a tab can
// be shown on this page: always on this node, and on a peer view only when the
// peer offers it.
export function peerViewTabUsable(tab, { remote = globalThis.__tclaudeRemoteNode, peerView = currentPeerView() } = {}) {
  if (!remote?.id) return true;
  // Before the peer first answers, only tabs without any peer route are
  // known to be unusable; a deep link to one the peer may share waits.
  if (!peerView) return Boolean(TAB_FEATURES[tab]);
  return tabState(tab, peerView) === 'shared';
}

// limitPeerViewCommands disables the command palette's node-changing commands
// on a peer view: only view commands, tabs the peer offers, and messaging when
// the peer shares it stay runnable.
export function limitPeerViewCommands(cmds, snap, remote = globalThis.__tclaudeRemoteNode) {
  if (!remote?.id) return cmds;
  const pv = snap?.peer_view || null;
  const name = globalThis.document?.documentElement?.dataset.remoteNodeName || remote.id.slice(0, 13);
  return cmds.map((cmd) => {
    let state = 'local';
    let what = 'This command';
    if (cmd.peerView === 'view') state = 'shared';
    else if (cmd.peerView === 'tab') { state = tabState(cmd.tab, pv); what = tabLabel(cmd.tab); }
    else if (cmd.peerView === 'messaging') { state = featureState('messaging', pv); what = 'messaging'; }
    if (state === 'shared') return cmd;
    return { ...cmd, enabled: false, disabledReason: limitHint(state, what, name) };
  });
}

// tabLabel names a tab in hints; the visible label varies by theme skin.
function tabLabel(tab) {
  return tab === 'costs' ? 'costs' : tab === 'audit' ? 'its audit log' : `the ${tab} tab`;
}

export function limitHint(state, what, node) {
  if (state === 'attach') return `${node}'s terminals open from the CLI — click to copy the attach command`;
  if (state === 'omitted') return `${node} does not share ${what} with you`;
  return `${what} is not available in a peer view`;
}

// ATTACH_ACTS open an agent's terminal. A peer's terminals are reachable only
// from the CLI (remote attach refuses browser origins by design), so on a peer
// view these copy the attach command instead.
const ATTACH_ACTS = new Set(['web-open-window', 'jump', 'term-dir']);

// SAFE_AGENT_ID admits only a plain agent ID into a command the operator will
// paste into a shell: the ID comes from the peer's snapshot, and a hostile
// peer must not be able to smuggle shell syntax into it.
const SAFE_AGENT_ID = /^agt_[A-Za-z0-9]{4,64}$/;

// defaultCopy fails when the page has no clipboard API (an insecure-context
// dashboard over plain http), so the caller shows the command instead of
// claiming it was copied.
function defaultCopy(text) {
  const clipboard = globalThis.navigator?.clipboard;
  if (!clipboard?.writeText) throw new Error('clipboard unavailable');
  return clipboard.writeText(text);
}

// attachCommand is the CLI command that opens agent's pane on the peer.
export function attachCommand(agentID, remoteID) {
  return `tclaude federation attach ${agentID}@${remoteID}`;
}

// blockedControl returns the element a click must not reach, and why.
export function blockedControl(target, peerView) {
  const tab = target?.closest?.('nav [data-tab]');
  if (tab) {
    const state = tabState(tab.dataset.tab, peerView);
    return state === 'shared' ? null : { el: tab, state, what: tabLabel(tab.dataset.tab) };
  }
  const act = target?.closest?.('[data-act]');
  if (act && ATTACH_ACTS.has(act.dataset.act) && SAFE_AGENT_ID.test(act.dataset.agent || '')) return { el: act, state: 'attach', what: 'This terminal', agent: act.dataset.agent };
  if (act && !READ_ONLY_ACTS.has(act.dataset.act)) return { el: act, state: 'local', what: 'This action' };
  const ctl = target?.closest?.(GATED_CONTROLS.join(','));
  if (ctl) return { el: ctl, state: 'local', what: 'This action' };
  return null;
}

// installPeerViewLimits wires the gating for the page's lifetime. It is a
// no-op unless this page is a peer view.
export function installPeerViewLimits({ doc = document, snapshot = dashboardState.snapshot, toast = shellToast, remote = globalThis.__tclaudeRemoteNode, copy = defaultCopy } = {}) {
  if (!remote?.id) return () => {};
  const root = doc.documentElement;
  let peerView = snapshot.value?.peer_view || null;
  const nodeName = () => root.dataset.remoteNodeName || remote.id.slice(0, 13);

  // titled remembers each greyed control's own title so the reason can be
  // prefixed, refreshed and removed again.
  const titled = new Map();
  const setReason = (el, reason) => {
    if (!titled.has(el)) titled.set(el, el.getAttribute('title'));
    const own = titled.get(el);
    el.setAttribute('title', `${reason}${own ? `\n\n${own}` : ''}`);
  };
  const clearReason = (el) => {
    if (!titled.has(el)) return;
    const own = titled.get(el);
    titled.delete(el);
    if (own == null) el.removeAttribute('title'); else el.setAttribute('title', own);
  };

  // Tabs carry their state as a class so CSS greys them, with the reason as
  // their title; recomputed whenever a snapshot changes what the peer shares
  // (and so once the marker has named the node).
  const stop = effect(() => {
    peerView = snapshot.value?.peer_view || null;
    for (const tab of doc.querySelectorAll('nav [data-tab]')) {
      const state = tabState(tab.dataset.tab, peerView);
      tab.classList.toggle('pv-off', state !== 'shared');
      if (state === 'shared') clearReason(tab);
      else setReason(tab, limitHint(state, tabLabel(tab.dataset.tab), nodeName()));
    }
  });

  const refuse = (event) => {
    const blocked = blockedControl(event.target, peerView);
    if (!blocked) return;
    event.preventDefault();
    event.stopImmediatePropagation();
    if (blocked.state === 'attach') {
      const cmd = attachCommand(blocked.agent, remote.id);
      Promise.resolve().then(() => copy(cmd))
        .then(() => toast(`Copied: ${cmd} — run it in a terminal to watch or type into this agent on ${nodeName()}`, false))
        .catch(() => toast(`Run in a terminal: ${cmd}`, false));
      return;
    }
    toast(limitHint(blocked.state, blocked.what, nodeName()), true);
  };
  // role=button chips open their editors on Enter/Space in their own keydown,
  // without a click; guard the keyboard path the same way.
  const onKey = (event) => {
    if (event.key !== 'Enter' && event.key !== ' ') return;
    if (event.target?.closest?.('input, textarea, select, [contenteditable="true"], [contenteditable=""]')) return;
    refuse(event);
  };
  // Hover explanation for controls (tabs get theirs from the effect).
  const onOver = (event) => {
    const blocked = blockedControl(event.target, peerView);
    if (blocked && !blocked.el.matches('nav [data-tab]')) setReason(blocked.el, limitHint(blocked.state, blocked.what, nodeName()));
  };
  // Drags spawn, move and reorder on the node — never on a peer view.
  const onDrag = (event) => { event.preventDefault(); };
  doc.addEventListener('click', refuse, true);
  doc.addEventListener('keydown', onKey, true);
  doc.addEventListener('pointerover', onOver, true);
  doc.addEventListener('dragstart', onDrag, true);
  return () => {
    stop();
    doc.removeEventListener('click', refuse, true);
    doc.removeEventListener('keydown', onKey, true);
    doc.removeEventListener('pointerover', onOver, true);
    doc.removeEventListener('dragstart', onDrag, true);
  };
}
