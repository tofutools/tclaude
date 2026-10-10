import { h, render } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { GroupsNativeList } from './groups-list.js';
import { GroupsInteractionProvider } from './groups-interactions.js';
import { dashboardState } from './snapshot-store.js';
import { shellToast } from './shell-state.js';
import { nodeHref, pollDelay, remoteNodeID, staggerOffset } from './skynet-model.js';
import { MERGED_IDLE_POLL_MS, MERGED_POLL_MS, mergeSnapshots } from './skynet-merged-model.js';
import { peerAction } from './peer-view-limits.js';
import { PeerActionDialog, createPeerActionActions } from './peer-action.js';

const html = htm.bind(h);

// VIEW_ONLY_ACTS open menus or show data already on screen. On a peer's rows
// the controls the peer shares (peerAction: stop or wake, restart, sandbox
// restart, retire, clone, spawn, message) open the peer action dialog against
// that node's peer routes, as on its per-node view; everything else waits for
// that node's own view.
// (The status dot is a power control — wake / shut down — so it is not here.)
const VIEW_ONLY_ACTS = new Set(['copy-generation-id', 'sandbox-details', 'group-menu', 'row-menu']);

// TERMINAL_ACTS open an agent's terminal; on a peer's row they open the peer's
// agent in the browser terminal (remote-terminal.js), as the peer shares it.
const TERMINAL_ACTS = new Set(['web-open-window', 'jump']);
const openRemote = (opts) => import('./remote-terminal.js').then((m) => m.openRemoteTerminal(opts));
const peerRoutes = (node) => createPeerActionActions({ node });

// ownGroup turns a merged group@node name back into the peer's own name.
function ownGroup(name, node) {
  const at = `@${node}`;
  return node && name.endsWith(at) ? name.slice(0, -at.length) : '';
}

// readOnlyActions stands in for the Groups actions: the merged overview never
// changes a node, so every action resolves without doing anything.
const readOnlyActions = new Proxy({}, {
  get: (_, key) => (key === 'reportError' ? () => {} : () => Promise.resolve()),
});

function defaultSwitchNode(id, tab = 'groups') {
  globalThis.location.assign(nodeHref(id, { pathname: `/${tab}`, search: globalThis.location.search }));
}

// usePeerSnapshots reads each peer's dashboard snapshot through the local peer
// proxy while the merged view is on screen: staggered first reads, a relaxed
// jittered interval, failure backoff, one read in flight per node, and nothing
// at all while the view is hidden or the page is in the background.
function usePeerSnapshots({ active, peers, fetchImpl, timers, now }) {
  const [entries, setEntries] = useState({});
  const entriesRef = useRef(entries);
  entriesRef.current = entries;
  // A peer's online flag is in the key so one coming back online restarts its
  // poll at once instead of waiting out the idle cadence.
  const peersKey = peers.map((p) => `${p.id}:${p.online}`).join(',');
  useEffect(() => {
    if (!active || !peers.length) return undefined;
    let disposed = false;
    const pending = new Map();
    const inflight = new Set();
    const schedule = (peer, delay) => { pending.set(peer.id, timers.setTimeout(() => tick(peer), delay)); };
    const commit = (id, patch) => {
      if (disposed) return;
      setEntries((cur) => ({ ...cur, [id]: { ...cur[id], ...patch } }));
    };
    async function tick(peer) {
      if (disposed || inflight.has(peer.id)) return;
      if (globalThis.document?.hidden) { schedule(peer, pollDelay({ base: MERGED_POLL_MS })); return; }
      // A peer the hub reports offline cannot answer: check back slowly
      // instead of sending a full snapshot read each interval.
      if (peer.online === false) { schedule(peer, pollDelay({ base: MERGED_IDLE_POLL_MS })); return; }
      inflight.add(peer.id);
      let ok = false;
      let empty = false;
      let failures = entriesRef.current[peer.id]?.failures || 0;
      try {
        const res = await fetchImpl(`/api/peer/${encodeURIComponent(peer.id)}/snapshot`, { credentials: 'same-origin', cache: 'no-store' });
        if (res.ok) {
          const snapshot = await res.json();
          commit(peer.id, { snapshot, receivedAt: now(), failure: null, failures: 0 });
          failures = 0;
          ok = true;
          empty = !(snapshot?.groups || []).length;
        } else {
          let body = null; try { body = await res.json(); } catch (_) { body = null; }
          failures += 1;
          commit(peer.id, { failure: { status: res.status, code: body?.code || '' }, failures });
        }
      } catch (error) {
        failures += 1;
        commit(peer.id, { failure: { status: 0, code: 'network' }, failures });
      } finally {
        inflight.delete(peer.id);
      }
      // A peer sharing no groups shows nothing here: re-check it at the idle
      // cadence so a newly shared group still appears.
      if (!disposed) schedule(peer, pollDelay({ base: ok && empty ? MERGED_IDLE_POLL_MS : MERGED_POLL_MS, failures: ok ? 0 : failures }));
    }
    peers.forEach((peer, i) => schedule(peer, staggerOffset(i, peers.length, MERGED_POLL_MS)));
    return () => { disposed = true; pending.forEach((t) => timers.clearTimeout(t)); };
  }, [active, peersKey]);
  return entries;
}

// MergedGroups is the top-level "Groups · all nodes" view: today's Groups
// listing over every linked node's groups, named group@node. It is an
// overview: what a peer shares runs through its peer routes from here, the
// rest happens in that node's own view, one click on its @node away.
export function MergedGroups({
  state, host, snapshot = dashboardState.snapshot, fetchImpl = (...a) => globalThis.fetch(...a),
  timers = globalThis, now = () => Date.now(), toast = shellToast, remote = remoteNodeID(), switchNode = defaultSwitchNode, openTerminal = openRemote,
  peerActions = peerRoutes, confirm,
}) {
  const current = state.view.value;
  const fleet = current.fleet;
  const active = current.fleetActive && !!fleet && !remote;
  const peers = fleet ? fleet.peers : [];
  const entries = usePeerSnapshots({ active, peers, fetchImpl, timers, now });
  const [peerReq, setPeerReq] = useState(null);
  // The capture handlers below are bound once; they read the latest snapshots.
  const entriesRef = useRef(entries);
  entriesRef.current = entries;

  // The merged view merges from this node; a peer's page hands it over.
  useEffect(() => {
    if (current.fleetActive && remote) switchNode('', 'fleet');
  }, [current.fleetActive, remote]);

  // Read-only overview: stop node-changing controls before their handlers,
  // and open a group's node from its @node suffix.
  useEffect(() => {
    if (!host) return undefined;
    const onClick = (event) => {
      const suffix = event.target.closest?.('[data-fleet-open]');
      if (suffix && host.contains(suffix)) {
        event.preventDefault(); event.stopPropagation();
        switchNode(suffix.dataset.fleetOpen);
        return;
      }
      const act = event.target.closest?.('[data-act]');
      if (!act || !host.contains(act) || VIEW_ONLY_ACTS.has(act.dataset.act)) return;
      event.preventDefault(); event.stopImmediatePropagation();
      const group = act.closest('details[data-fleet-node]');
      const instance = group?.dataset.fleetNode || '';
      if (TERMINAL_ACTS.has(act.dataset.act) && act.dataset.agent && instance && instance !== state.view.value.fleet?.self?.id) {
        void openTerminal({ instance, agent: act.dataset.agent, peerLabel: group.dataset.fleetNodeName || '', toast });
        return;
      }
      const name = group?.dataset.fleetNodeName || '';
      if (instance && instance !== state.view.value.fleet?.self?.id) {
        const req = peerAction(act, entriesRef.current[instance]?.snapshot?.peer_view, (g) => ownGroup(g, name));
        if (req) { setPeerReq({ ...req, node: name || instance.slice(0, 13), nodeId: instance }); return; }
      }
      const where = group ? `${group.dataset.fleetNodeName}'s dashboard` : "the node's own dashboard";
      toast(`The all-nodes view is an overview — act on this from ${where} (click the @node name)`, true);
    };
    // Editable chips open on Enter/Space in their own keydown, and the row
    // context menu (Ctrl+right-click) opens terminals: guard both like clicks.
    const onKey = (event) => {
      if (event.key !== 'Enter' && event.key !== ' ') return;
      if (event.target?.closest?.('input, textarea, select')) return;
      onClick(event);
    };
    const onContext = (event) => {
      if (event.target?.closest?.('[data-act]') && host.contains(event.target)) { event.preventDefault(); event.stopImmediatePropagation(); }
    };
    const onDrag = (event) => { if (host.contains(event.target)) event.preventDefault(); };
    host.addEventListener('click', onClick, true);
    host.addEventListener('keydown', onKey, true);
    host.addEventListener('contextmenu', onContext, true);
    host.addEventListener('dragstart', onDrag, true);
    return () => {
      host.removeEventListener('click', onClick, true);
      host.removeEventListener('keydown', onKey, true);
      host.removeEventListener('contextmenu', onContext, true);
      host.removeEventListener('dragstart', onDrag, true);
    };
  }, [host]);

  if (!fleet) return html`<div class="empty">No linked nodes.</div>`;
  if (remote) return html`<div class="empty">Opening the all-nodes view on this node…</div>`;
  const t = now();
  const nodes = [
    { node: fleet.self, entry: snapshot.value ? { snapshot: snapshot.value, receivedAt: t } : null },
    ...peers.map((peer) => ({ node: peer, entry: entries[peer.id] || null })),
  ];
  const merged = mergeSnapshots(nodes, t);
  return html`<div class="skynet-merged">
    <div class="skynet-merged-nodes" aria-label="Nodes in this view">
      ${merged.fleet_nodes.map((n) => html`<span key=${n.node.id} class=${`skynet-merged-node${n.stale ? ' stale' : ''}`} style=${`--nc:${n.node.color}`}
        title=${n.node.local ? 'This node' : n.failure ? `Unreachable (${n.failure.code || `HTTP ${n.failure.status}`})` : n.loaded ? 'Shared groups of this peer' : 'Loading…'}>
        <i aria-hidden="true"></i>${n.node.local ? '⌂ ' : ''}${n.node.name}${n.label ? html` <span class="muted">· ${n.label}</span>` : !n.loaded && !n.node.local ? html` <span class="muted">· loading…</span>` : ''}
      </span>`)}
      <span class="skynet-merged-note">Overview: shared actions go to the peer; open <b>@node</b> for the rest.</span>
    </div>
    <${GroupsInteractionProvider}>
      <${GroupsNativeList} groups=${merged.groups} snapshot=${merged} actions=${readOnlyActions} />
    <//>
    ${peerReq && html`<${PeerActionDialog} req=${peerReq} peerView=${entries[peerReq.nodeId]?.snapshot?.peer_view} groups=${entries[peerReq.nodeId]?.snapshot?.groups}
      actions=${peerActions(peerReq.nodeId)} toast=${toast} timers=${timers} onClose=${() => setPeerReq(null)} ...${confirm ? { confirm } : {}} />`}
  </div>`;
}

export function mountMergedGroupsIsland({ host, state, registerCleanup }) {
  render(html`<${MergedGroups} state=${state} host=${host} />`, host);
  registerCleanup(() => render(null, host));
}
