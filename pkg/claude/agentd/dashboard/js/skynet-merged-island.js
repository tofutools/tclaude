import { h, render } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { GroupsNativeList } from './groups-list.js';
import { GroupsInteractionProvider } from './groups-interactions.js';
import { dashboardState } from './snapshot-store.js';
import { shellToast } from './shell-state.js';
import { nodeHref, pollDelay, remoteNodeID, staggerOffset } from './skynet-model.js';
import { tickedNodes, withFused } from './skynet-scope.js';
import { MERGED_IDLE_POLL_MS, MERGED_POLL_MS, mergeSnapshots } from './skynet-merged-model.js';
import { peerAction } from './peer-view-limits.js';
import { PeerActionDialog, createPeerActionActions } from './peer-action.js';
import { MoveDropDialog, dropPlan } from './skynet-move-drop.js';
import { createFleetAdminActions } from './fleet-admin-actions.js';

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
const MOVE_MIME = 'application/x-tclaude-fleet-move';

// dragSource reads a dragged member row in the merged view: the agent and the
// node and group it is in. Only real group members can move.
function dragSource(row) {
  const group = row?.closest?.('details[data-fleet-node]');
  const agent = row?.getAttribute?.('data-dnd-agent');
  const merged = row?.getAttribute?.('data-dnd-source-group');
  if (!group || !agent || !merged || !/^agt_[A-Za-z0-9]+$/.test(agent)) return null;
  const name = group.dataset.fleetNodeName || '';
  return { agent: { id: agent, name: row.getAttribute('data-dnd-label') || agent }, source: { node: group.dataset.fleetNode, name, group: ownGroup(merged, name) || merged } };
}

// dropTarget reads the group a drag is over (the innermost one).
function dropTarget(el) {
  const group = el?.closest?.('details[data-fleet-node]');
  if (!group) return null;
  const name = group.dataset.fleetNodeName || '';
  return { el: group, node: group.dataset.fleetNode, name, group: ownGroup(group.dataset.groupKey || '', name) };
}

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

// defaultSwitchNode shows one node's own Groups view: it leaves the fused
// view, as picking a node alone does.
function defaultSwitchNode(id) {
  globalThis.location.assign(nodeHref(id, { pathname: '/groups', search: withFused(globalThis.location.search, null) }));
}

// MAX_IN_FLIGHT caps concurrent snapshot reads: the peer proxy allows 4
// streams per peer and 16 overall, and a wide fused view must leave room for
// everything else.
export const MAX_IN_FLIGHT = 4;

// snapshotURL addresses a node's snapshot: the peer proxy for a peer, this
// node's own route for this node (read only from a peer's page).
function snapshotURL(node) {
  return node.local ? '/api/snapshot' : `/api/peer/${encodeURIComponent(node.id)}/snapshot`;
}

// usePeerSnapshots reads each node's dashboard snapshot (a peer's through the
// local peer proxy) while the fused view is on screen: staggered first reads,
// a relaxed jittered interval, failure backoff, one read in flight per node
// and at most MAX_IN_FLIGHT overall, and nothing at all while the view is
// hidden or the page is in the background. A node's last good snapshot stays
// on a failed read (shown stale), except when trust is gone.
function usePeerSnapshots({ active, peers, fetchImpl, localFetch, timers, now }) {
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
      if (inflight.size >= MAX_IN_FLIGHT) { schedule(peer, 250 + Math.round(250 * Math.random())); return; }
      // A peer the hub reports offline cannot answer: check back slowly
      // instead of sending a full snapshot read each interval.
      if (!peer.local && peer.online === false) { schedule(peer, pollDelay({ base: MERGED_IDLE_POLL_MS })); return; }
      inflight.add(peer.id);
      let ok = false;
      let empty = false;
      let failures = entriesRef.current[peer.id]?.failures || 0;
      try {
        const res = await (peer.local ? localFetch : fetchImpl)(snapshotURL(peer), { credentials: 'same-origin', cache: 'no-store' });
        if (res.ok) {
          const snapshot = await res.json();
          commit(peer.id, { snapshot, receivedAt: now(), failure: null, failures: 0 });
          failures = 0;
          ok = true;
          empty = !(snapshot?.groups || []).length;
        } else {
          let body = null; try { body = await res.json(); } catch (_) { body = null; }
          failures += 1;
          // A node no longer trusted keeps nothing it shared.
          const gone = body?.code === 'not_trusted' ? { snapshot: null, receivedAt: null } : {};
          commit(peer.id, { failure: { status: res.status, code: body?.code || '' }, failures, ...gone });
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

// MergedGroups is the fused Groups view: today's Groups listing over the
// ticked nodes' groups, named group@node. It is an overview: what a peer
// shares runs through its peer routes from here, the rest happens in that
// node's own view, one click on its @node away. The page's own node (this
// node, or the peer of a ?node= page) comes from the page's snapshot; every
// other ticked node is read here.
export function MergedGroups({
  state, host, snapshot = dashboardState.snapshot, fetchImpl = (...a) => globalThis.fetch(...a),
  timers = globalThis, now = () => Date.now(), toast = shellToast, remote = remoteNodeID(), switchNode = defaultSwitchNode, openTerminal = openRemote,
  peerActions = peerRoutes, confirm, moveActions = null, localFetch = globalThis.__tclaudeRemoteNode?.localFetch || fetchImpl,
}) {
  const current = state.view.value;
  const fleet = current.fleet;
  const active = current.activeTab === 'groups' && !!current.fused && !!fleet;
  const pageNode = fleet ? remote || fleet.self.id : '';
  const ticked = fleet ? tickedNodes(fleet, current.fused) : [];
  const polled = ticked.filter((n) => n.id !== pageNode);
  const entries = usePeerSnapshots({ active, peers: polled, fetchImpl, localFetch, timers, now });
  const [peerReq, setPeerReq] = useState(null);
  const [moveDrop, setMoveDrop] = useState(null);
  const fleetActions = useRef(moveActions);
  if (!fleetActions.current) fleetActions.current = createFleetAdminActions({ fetchImpl });
  // The capture handlers below are bound once; they read the latest snapshots.
  // snapOf reads a node's snapshot, the page's own node included.
  const snapOf = (id) => (id === pageNode ? snapshot.value : entries[id]?.snapshot) || null;
  const snapRef = useRef(snapOf);
  snapRef.current = snapOf;

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
        const req = peerAction(act, snapRef.current(instance)?.peer_view, (g) => ownGroup(g, name));
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
    // Drag an agent onto a group on another node to move it there (to or from
    // this node). These handlers own the gesture here: the per-node Groups
    // drag-and-drop (dnd.js) never sees it, and nothing else drags.
    let dragging = null;
    let marked = null;
    const mark = (el, ok) => {
      if (marked && marked !== el) marked.classList.remove('skynet-drop-ok', 'skynet-drop-no');
      marked = el;
      el?.classList.toggle('skynet-drop-ok', ok);
      el?.classList.toggle('skynet-drop-no', !ok);
    };
    const unmark = () => { marked?.classList.remove('skynet-drop-ok', 'skynet-drop-no'); marked = null; };
    const self = () => state.view.value.fleet?.self?.id || '';
    const onDragStart = (event) => {
      if (!host.contains(event.target)) return;
      const src = dragSource(event.target.closest?.('tr.dnd-draggable'));
      if (!src) { event.preventDefault(); return; }
      event.stopPropagation();
      dragging = src;
      // dragend reaches its source even if a re-render detached it.
      const row = event.target.closest('tr.dnd-draggable');
      row?.addEventListener('dragend', () => { if (dragging === src) { dragging = null; unmark(); } }, { once: true });
      event.dataTransfer?.setData(MOVE_MIME, src.agent.id);
      event.dataTransfer?.setData('text/plain', src.agent.name);
      if (event.dataTransfer) event.dataTransfer.effectAllowed = 'move';
    };
    const onDragOver = (event) => {
      if (!dragging) return;
      event.stopPropagation();
      const target = dropTarget(event.target);
      if (!target) { unmark(); return; }
      const plan = dropPlan({ source: dragging.source, target, self: self() });
      if (plan.kind === 'none') { unmark(); return; }
      // Same-node and third-node drops are accepted only to say why not.
      event.preventDefault();
      const ok = plan.kind === 'push' || plan.kind === 'pull';
      if (event.dataTransfer) event.dataTransfer.dropEffect = ok ? 'move' : 'none';
      mark(target.el, ok);
    };
    const onDrop = (event) => {
      if (!dragging) return;
      event.preventDefault(); event.stopPropagation();
      const src = dragging; dragging = null; unmark();
      const target = dropTarget(event.target);
      if (!target) return;
      const plan = dropPlan({ source: src.source, target, self: self() });
      if (plan.kind === 'same') { toast(`To move ${src.agent.name} between groups on ${src.source.name}, use ${src.source.name}'s own Groups view`, true); return; }
      if (plan.kind === 'third') { toast(`Neither ${src.source.name} nor ${target.name} is this node: move ${src.agent.name} from ${src.source.name}'s dashboard`, true); return; }
      if (plan.kind === 'push' || plan.kind === 'pull') setMoveDrop({ drop: { agent: src.agent, source: src.source, target }, plan });
    };
    const onDragEnd = () => { dragging = null; unmark(); };
    host.addEventListener('click', onClick, true);
    host.addEventListener('keydown', onKey, true);
    host.addEventListener('contextmenu', onContext, true);
    host.addEventListener('dragstart', onDragStart, true);
    host.addEventListener('dragover', onDragOver, true);
    host.addEventListener('drop', onDrop, true);
    host.addEventListener('dragend', onDragEnd, true);
    return () => {
      host.removeEventListener('click', onClick, true);
      host.removeEventListener('keydown', onKey, true);
      host.removeEventListener('contextmenu', onContext, true);
      host.removeEventListener('dragstart', onDragStart, true);
      host.removeEventListener('dragover', onDragOver, true);
      host.removeEventListener('drop', onDrop, true);
      host.removeEventListener('dragend', onDragEnd, true);
    };
  }, [host]);

  if (!fleet || !current.fused) return null;
  const t = now();
  const nodes = ticked.map((node) => ({
    node,
    entry: node.id === pageNode ? (snapshot.value ? { snapshot: snapshot.value, receivedAt: t } : null) : entries[node.id] || null,
  }));
  const merged = mergeSnapshots(nodes, t, snapshot.value);
  return html`<div class="skynet-merged">
    <div class="skynet-merged-nodes" aria-label="Nodes in this view">
      ${merged.fleet_nodes.map((n) => html`<span key=${n.node.id} class=${`skynet-merged-node${n.stale ? ' stale' : ''}`} style=${`--nc:${n.node.color}`}
        title=${n.node.local ? 'This node' : n.failure ? `Unreachable (${n.failure.code || `HTTP ${n.failure.status}`})` : n.loaded ? 'Shared groups of this peer' : 'Loading…'}>
        <i aria-hidden="true"></i>${n.node.local ? '⌂ ' : ''}${n.node.name}${n.label ? html` <span class="muted">· ${n.label}</span>` : !n.loaded && !n.node.local ? html` <span class="muted">· loading…</span>` : ''}
      </span>`)}
      <span class="skynet-merged-note">Overview: shared actions go to the peer; drag an agent onto another node's group to move it; open <b>@node</b> for the rest.</span>
    </div>
    <${GroupsInteractionProvider}>
      <${GroupsNativeList} groups=${merged.groups} snapshot=${merged} actions=${readOnlyActions} />
    <//>
    ${peerReq && html`<${PeerActionDialog} req=${peerReq} peerView=${snapOf(peerReq.nodeId)?.peer_view} groups=${snapOf(peerReq.nodeId)?.groups}
      actions=${peerActions(peerReq.nodeId)} toast=${toast} timers=${timers} onClose=${() => setPeerReq(null)} ...${confirm ? { confirm } : {}} />`}
    ${moveDrop && html`<${MoveDropDialog} drop=${moveDrop.drop} plan=${moveDrop.plan} actions=${fleetActions.current} peerActions=${peerActions}
      timers=${timers} now=${now} onClose=${() => setMoveDrop(null)} />`}
  </div>`;
}

export function mountMergedGroupsIsland({ host, state, registerCleanup }) {
  render(html`<${MergedGroups} state=${state} host=${host} />`, host);
  registerCleanup(() => render(null, host));
}
