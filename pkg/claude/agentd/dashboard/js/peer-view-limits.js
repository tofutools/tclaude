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
const TAB_FEATURES = Object.freeze({ groups: 'agents.status', costs: 'costs', audit: 'audit', map: 'local' });

// READ_ONLY_ACTS are data-act controls that only change this browser's view.
const READ_ONLY_ACTS = new Set(['dot-toggle', 'copy-generation-id']);

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

// tabLabel names a tab in hints; the visible label varies by theme skin.
function tabLabel(tab) {
  return tab === 'costs' ? 'costs' : tab === 'audit' ? 'its audit log' : `the ${tab} tab`;
}

export function limitHint(state, what, node) {
  if (state === 'omitted') return `${node} does not share ${what} with you`;
  return `${what} is not available in a peer view`;
}

// blockedControl returns the element a click must not reach, and why.
export function blockedControl(target, peerView) {
  const tab = target?.closest?.('nav [data-tab]');
  if (tab) {
    const state = tabState(tab.dataset.tab, peerView);
    return state === 'shared' ? null : { el: tab, state, what: tabLabel(tab.dataset.tab) };
  }
  const act = target?.closest?.('[data-act]');
  if (act && !READ_ONLY_ACTS.has(act.dataset.act)) return { el: act, state: 'local', what: 'This action' };
  const ctl = target?.closest?.(GATED_CONTROLS.join(','));
  if (ctl) return { el: ctl, state: 'local', what: 'This action' };
  return null;
}

// installPeerViewLimits wires the gating for the page's lifetime. It is a
// no-op unless this page is a peer view.
export function installPeerViewLimits({ doc = document, snapshot = dashboardState.snapshot, toast = shellToast, remote = globalThis.__tclaudeRemoteNode } = {}) {
  if (!remote?.id) return () => {};
  const root = doc.documentElement;
  let peerView = snapshot.value?.peer_view || null;
  const nodeName = () => root.dataset.remoteNodeName || remote.id.slice(0, 13);

  // Tabs carry their state as a class so CSS greys them; recomputed whenever
  // a snapshot changes what the peer shares.
  const stop = effect(() => {
    peerView = snapshot.value?.peer_view || null;
    for (const tab of doc.querySelectorAll('nav [data-tab]')) {
      const state = tabState(tab.dataset.tab, peerView);
      tab.classList.toggle('pv-off', state !== 'shared');
      if (state === 'shared') tab.removeAttribute('data-pv-hint');
      else tab.setAttribute('data-pv-hint', limitHint(state, tabLabel(tab.dataset.tab), nodeName()));
    }
  });

  const onClick = (event) => {
    const blocked = blockedControl(event.target, peerView);
    if (!blocked) return;
    event.preventDefault();
    event.stopImmediatePropagation();
    toast(limitHint(blocked.state, blocked.what, nodeName()), true);
  };
  // Hover explanation: set the title the first time the pointer reaches a
  // blocked control, keeping its own title after it.
  const onOver = (event) => {
    const blocked = blockedControl(event.target, peerView);
    if (!blocked || blocked.el.dataset.pvTitled) return;
    blocked.el.dataset.pvTitled = '1';
    const own = blocked.el.getAttribute('title');
    blocked.el.setAttribute('title', `${limitHint(blocked.state, blocked.what, nodeName())}${own ? `\n\n${own}` : ''}`);
  };
  // Drags spawn, move and reorder on the node — never on a peer view.
  const onDrag = (event) => { event.preventDefault(); };
  doc.addEventListener('click', onClick, true);
  doc.addEventListener('pointerover', onOver, true);
  doc.addEventListener('dragstart', onDrag, true);
  return () => {
    stop();
    doc.removeEventListener('click', onClick, true);
    doc.removeEventListener('pointerover', onOver, true);
    doc.removeEventListener('dragstart', onDrag, true);
  };
}
