import { h, render } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import { loadTriggerRules } from './group-triggers.js';
import htm from 'htm';
import { GroupsInteractionProvider } from './groups-interactions.js';
import {
  BrokerRefusalNotice, GroupsLensList, useGroupsHover, useGroupsTableControls, useWizardTheme,
} from './groups-island.js';
import { GROUP_VIEW_OPTIONS, buildGroupsView, groupsState } from './groups-state.js';
import { GroupsProcessScope } from './groups-process-scope.js';
import { syncBotAnimations, syncWizardOrbit } from './helpers.js';
import { dashboardState } from './snapshot-store.js';
import { shellToast } from './shell-state.js';
import { PEER_POLL_HEADER, nodeHref, pollDelay, remoteNodeID, staggerOffset } from './fleet-model.js';
import { tickedNodes, withFused } from './fleet-scope.js';
import { MERGED_IDLE_POLL_MS, MERGED_POLL_MS, mergeSnapshots, uniqueNodeNames } from './fleet-merged-model.js';
import {
  ATTACH_ACTS, SHELL_ACTS, shellControl, limitHint, READ_ONLY_ACTS as VIEW_ONLY_ACTS, actOf, grantHeld, grantMissing, grantSelectors, missingPullGrants, needsSuffix, peerAction, refusedActsRule, usableActSelectors,
  reasonTitles,
} from './peer-view-limits.js';
import { announceNodeChanged, isPageNode, nodeFetch, onNodeChanged, setNodeSnapshotSource } from './node-api.js';
import { clearDropBox, dndGesture, markDropBox, nodeAllowsActing } from './dnd-ops.js';
import { FUSED_GROUP_ORDER_KEY, setFusedGroupListing, sortGroupsByPref } from './group-order.js';
import { dashPrefs } from './prefs.js';
import { PeerActionDialog, createPeerActionActions } from './peer-action.js';
import { dropPlan } from './fleet-move-drop.js';
import { LaunchTargetDialog, createLaunchTargetApi, grantWhy, launchAgent } from './launch-target-dialog.js';
import { openCloneAgentDialog } from './action-dialog-controller.js';
import { createFleetAdminActions } from './fleet-admin-actions.js';
import { featureState } from './feature-state-registry.js';
import { GroupStandingOrdersDialog } from './groups-standing-orders-dialog.js';
import { publishFusedDock } from './dock-nodes.js';

const html = htm.bind(h);

// VIEW_ONLY_ACTS (peer-view-limits.js) open menus or show data already on
// screen. This node's own groups (home) run every control as on its own
// Groups view. On a peer's rows a control whose grant the peer holds
// (GROUP_GRANT_ACTS, AGENT_GRANT_ACTS) runs its regular dialog against that
// node (node-api.js); without it, a spawn or message the peer's action routes
// take (peerAction) opens the peer action dialog; anything else stays out of
// reach, as on its per-node view. ATTACH_ACTS on a peer's row open the peer's
// agent in the browser terminal (remote-terminal.js), and SHELL_ACTS its
// shells under its sessions.shell grant.
const openRemote = (opts) => import('./remote-terminal.js').then((m) => m.openRemoteTerminal(opts));
const openRemoteShell = (opts) => import('./remote-terminal.js').then((m) => m.openRemoteShell(opts));
const peerRoutes = (node) => createPeerActionActions({ node });
const MOVE_MIME = 'application/x-tclaude-fleet-move';
const PROCESS_DRAGS = '[data-saved-process-drag], [data-run-dock-agent]';
const ROOT = '#fleetview-fused-groups-root';
const NODE_ID = /^inst_[A-Za-z0-9]{4,64}$/;

// limitsCSS greys the controls a click here refuses, node by node, from the
// same rules onClick applies: ⚙ "send elsewhere" and the view-only controls
// everywhere; on a peer's groups its terminal and the actions it shares. The
// home groups (this node's, on its own page) act as on its Groups view, so
// nothing there is greyed; on a peer's page this node's groups are an
// overview. A control outside any node's group is refused.
export function limitsCSS(nodes, { self, home = '' }) {
  const always = [...VIEW_ONLY_ACTS, 'send-elsewhere'];
  const rules = [refusedActsRule(ROOT, always.map((a) => `[data-act="${a}"]`), '[data-act]:not(details[data-fleet-node] [data-act])')];
  // Controls carry each node's name as the merged view shows it.
  const names = uniqueNodeNames(nodes.map(({ id, name, local }) => ({ node: { id, name, local } })));
  for (const { id, peerView } of nodes) {
    if (!NODE_ID.test(id || '') || id === home) continue;
    const scope = `${ROOT} details[data-fleet-node="${id}"]`;
    const merged = (g) => `${g}@${names.get(id)}`;
    // This node's groups on a peer's page take its group controls directly
    // (node-api.js), as though this node trusted itself unrestricted.
    const usable = id !== self
      ? usableActSelectors(peerView, ['send-elsewhere'], merged, { fused: true })
      : [...always.map((a) => `[data-act="${a}"]`), ...grantSelectors({ level: 'unrestricted' })];
    rules.push(refusedActsRule(scope, usable));
  }
  return rules.join('\n');
}

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

// VIEW_ACTIONS change only this browser's view of the listed groups (sort,
// paging, pins and folds, error reports): a peer's group runs them on this
// page under the name it is listed by (group@node), which its pins and folds
// are kept under.
const VIEW_ACTIONS = new Set(['sort', 'page', 'setPageSize', 'toggleQuickPin', 'toggleForceFold', 'reportError']);

// createNodeGroupsActions builds the Groups actions for one node's groups in
// this view: requests through nodeFetch (this node's daemon, or the peer
// proxy), dialogs whose descriptors name the node, and a refresh that reads
// that node again now. The actions module loads on first use (it reaches the
// page's controllers). Which controls a peer's rows run is the grant table's
// (peer-view-limits.js), not these actions'.
export function createNodeGroupsActions({
  node, notify = shellToast, refresh = () => {}, fetchImpl, state = groupsState,
  load = () => import('./groups-actions.js'),
}) {
  let built = null;
  const actions = () => {
    built ||= load().then(({ createGroupsActions }) => createGroupsActions({
      state, node, notify, fetchImpl: fetchImpl || nodeFetch(node),
      refresh: async () => { announceNodeChanged(node); await refresh(); },
      openMemberPermissions: (conv, label, target) => import('./message-access-dialog-controller.js')
        .then(({ openPermEditModal }) => openPermEditModal(conv, label, target || node)),
    }));
    return built;
  };
  // Every Groups action, bound to node; never thenable itself.
  return new Proxy({}, {
    get: (_, key) => (typeof key !== 'string' || key === 'then' ? undefined
      : (...args) => actions().then((a) => (typeof a[key] === 'function' ? a[key](...args) : undefined))),
  });
}

// isMergedGroup tells a merged group from a merged member: both name their
// node (fleet_node), and only a member names its agent.
const isMergedGroup = (a) => !('agent_id' in a) && !('conv_id' in a);

// homeActions routes the Groups actions by the group (or agent) they act on: a home
// group's go to this node's Groups actions with the group as this node names
// it (ownOf), another node's to forNode(node) with the group under its own
// name (its VIEW_ACTIONS to this page's, as listed), or resolve without doing
// anything when that node cannot be addressed. A call naming no group (a
// sort, an error report) goes to this node's actions.
export function homeActions(live, ownOf, forNode = () => null) {
  return new Proxy({}, {
    get: (_, key) => (...args) => {
      const real = live();
      const group = args.find((a) => a && typeof a === 'object' && a.fleet_node);
      if (group && VIEW_ACTIONS.has(key)) return real?.[key]?.(...args);
      if (group && !group.fleet_node.home) {
        const actions = forNode(group.fleet_node);
        if (!actions) return Promise.resolve();
        const own = (a) => (isMergedGroup(a)
          ? { ...a, name: ownGroup(a.name, a.fleet_node.name) || a.name, fleet_node: undefined }
          : { ...a, fleet_node: undefined });
        return actions[key](...args.map((a) => (a && typeof a === 'object' && a.fleet_node ? own(a) : a)));
      }
      if (!real || typeof real[key] !== 'function') return key === 'reportError' ? undefined : Promise.resolve();
      return real[key](...args.map((a) => (a && typeof a === 'object' && a.fleet_node ? (isMergedGroup(a) ? ownOf(a) : { ...a, fleet_node: undefined }) : a)));
    },
  });
}

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
        // A peer read declares this poll's interval, so this node shares one
        // conditional peer read among polls within a second (peer_read_cache.go);
        // Age says how old the served copy is.
        const init = { credentials: 'same-origin', cache: 'no-store', ...(peer.local ? {} : { headers: { [PEER_POLL_HEADER]: String(MERGED_POLL_MS) } }) };
        const res = await (peer.local ? localFetch : fetchImpl)(snapshotURL(peer), init);
        if (res.ok) {
          const snapshot = await res.json();
          const age = Number(res.headers?.get?.('Age')) || 0;
          commit(peer.id, { snapshot, receivedAt: now() - age * 1000, failure: null, failures: 0 });
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
    // A control here just changed a node: read it again now.
    const stopChanged = onNodeChanged((node) => {
      const peer = peers.find((p) => p.id === node);
      if (!peer || inflight.has(peer.id)) return;
      timers.clearTimeout(pending.get(peer.id));
      schedule(peer, 0);
    });
    return () => {
      disposed = true;
      pending.forEach((t) => timers.clearTimeout(t));
      stopChanged();
    };
  }, [active, peersKey]);
  return entries;
}

// NODE_ONLY_OFF keeps the node-only virtual groups (Ungrouped, Retired, …)
// out of a listing of several nodes, whatever this node's view shows.
const NODE_ONLY_OFF = Object.fromEntries(GROUP_VIEW_OPTIONS.filter((o) => o.nodeOnly).map((o) => [o.key, false]));

// fusedOrder sorts the merged groups by the fused view's own remembered order
// (group@node names; group-reorder.js keeps it), else node order.
const fusedOrder = (list) => sortGroupsByPref(list, dashPrefs, FUSED_GROUP_ORDER_KEY);

// MergedGroups is the fused Groups view: the Groups listing (GroupsLensList,
// as on one node's view, under the same toolbar and its filter, view and
// lens state in `groups`) over the ticked nodes' groups, named group@node. This node's own groups (home, on
// this node's page) work as in its own Groups view; what a peer shares runs
// through its peer routes from here, as on that peer's per-node view. The
// page's own node (this node, or the peer of a ?node= page) comes from the
// page's snapshot; every other ticked node is read here.
export function MergedGroups({
  state, host, snapshot = dashboardState.snapshot, fetchImpl = (...a) => globalThis.fetch(...a),
  timers = globalThis, now = () => Date.now(), toast = shellToast, remote = remoteNodeID(), switchNode = defaultSwitchNode, openTerminal = openRemote, openShell = openRemoteShell,
  peerActions = peerRoutes, confirm, openSpawn, openMessage, moveActions = null, localFetch = globalThis.__tclaudeRemoteNode?.localFetch || fetchImpl,
  groupsActions = () => featureState('groups-actions'), groupsFeature = () => featureState('groups'), groups = groupsState, processSource = undefined,
}) {
  const current = state.view.value;
  const fleet = current.fleet;
  const active = current.activeTab === 'groups' && !!current.fused && !!fleet;
  const pageNode = fleet ? remote || fleet.self.id : '';
  // home is the node whose groups this page's own Groups actions reach: this
  // node, on its own page. A ?node= page's actions go to that peer (and its
  // peer-view limits); this node's groups there wait for this node's page.
  const home = fleet && !remote ? fleet.self.id : '';
  const ticked = fleet ? tickedNodes(fleet, current.fused) : [];
  const polled = ticked.filter((n) => n.id !== pageNode);
  const entries = usePeerSnapshots({ active, peers: polled, fetchImpl, localFetch, timers, now });
  const [peerReq, setPeerReq] = useState(null);
  // launch is the open launch-target dialog: from a drop (pre-filled
  // target) or from a row's ⚙ "Send elsewhere" item.
  const [launch, setLaunch] = useState(null);
  const fleetActions = useRef(moveActions);
  if (!fleetActions.current) fleetActions.current = createFleetAdminActions({ fetchImpl });
  const launchApi = useRef(null);
  if (!launchApi.current) launchApi.current = createLaunchTargetApi({ fetchImpl: localFetch });
  // The capture handlers below are bound once; they read the latest snapshots.
  // snapOf reads a node's snapshot, the page's own node included.
  const snapOf = (id) => (id === pageNode ? snapshot.value : entries[id]?.snapshot) || null;
  const groupNames = (id) => (snapOf(id)?.groups || []).map((g) => g.name).filter(Boolean).sort();
  // relays: a peer that trusts this node unrestricted sends its agent on to a
  // third node for it (the peer action's "to").
  const snapRef = useRef(snapOf);
  snapRef.current = snapOf;
  const relays = (id) => id !== state.view.value.fleet?.self?.id && snapRef.current(id)?.peer_view?.level === 'unrestricted';
  // A dialog on another node's group reads that node's snapshot from here.
  useEffect(() => setNodeSnapshotSource((id) => snapRef.current(id)), []);
  // A fold/unfold persisted by refresh.js moves which header owns subgroup
  // activity: re-render now rather than on the next poll.
  const [, setDisclosures] = useState(0);
  useEffect(() => {
    if (!host) return undefined;
    const bump = () => setDisclosures((n) => n + 1);
    host.addEventListener('tclaude:group-disclosure', bump);
    return () => host.removeEventListener('tclaude:group-disclosure', bump);
  }, [host]);
  const homeRef = useRef(home);
  homeRef.current = home;
  // ownOf turns a merged home group back into this node's own group.
  const ownOf = (g) => {
    const key = g.key?.startsWith(`${home}/`) ? g.key.slice(home.length + 1) : '';
    const found = (snapshot.value?.groups || []).find((o) => (o.key || o.name) === key);
    return found || { ...g, name: ownGroup(g.name, g.fleet_node.name) || g.name, parent: g.parent && (ownGroup(g.parent, g.fleet_node.name) || g.parent), fleet_node: undefined };
  };
  const ownOfRef = useRef(ownOf);
  ownOfRef.current = ownOf;
  // Another node's groups run their group controls through actions bound to
  // that node, one set per node.
  const nodeActions = useRef(new Map());
  const forNode = (node) => {
    if (!NODE_ID.test(node?.id || '')) return null;
    if (!nodeActions.current.has(node.id)) {
      nodeActions.current.set(node.id, createNodeGroupsActions({
        node: node.id, notify: toast, fetchImpl: nodeFetch(node.id, { self: state.view.value.fleet?.self?.id || '', fetchImpl, remote: remote ? { id: remote, localFetch } : null }),
      }));
    }
    return nodeActions.current.get(node.id);
  };
  // The right panel lists the ticked nodes' catalogs (dock-nodes.js).
  const tickedKey = ticked.map((n) => n.id).join(',');
  useEffect(() => {
    if (!active) { publishFusedDock(null); return; }
    const names = uniqueNodeNames(ticked.map((node) => ({ node })));
    const self = state.view.value.fleet?.self?.id || '';
    const fetchFor = (id) => nodeFetch(id, { self, fetchImpl, remote: remote ? { id: remote, localFetch } : null });
    publishFusedDock({
      page: pageNode,
      nodes: ticked.map((n) => ({ id: n.id, name: names.get(n.id), color: n.color })),
      snapshots: Object.fromEntries(polled.map((n) => [n.id, entries[n.id]?.snapshot || null])),
      failures: Object.fromEntries(polled.map((n) => [n.id, entries[n.id]?.failure || null])),
      fetchFor,
    });
  }, [active, tickedKey, pageNode, entries]);
  useEffect(() => () => publishFusedDock(null), []);
  const routed = useRef(null);
  if (!routed.current) routed.current = homeActions(() => groupsActions(), (g) => ownOfRef.current(g), forNode);
  // processScope resolves a listed group to the group the run context names:
  // a home group to this node's own, any other to none (its node's runs are
  // not read here; see groups-process-scope.js).
  const processScope = useRef(null);
  if (!processScope.current) processScope.current = (g) => (g?.fleet_node?.home ? ownOfRef.current(g) : null);

  const t = now();
  const nodes = ticked.map((node) => ({
    node: node.id === home ? { ...node, home: true } : node,
    entry: node.id === pageNode ? (snapshot.value ? { snapshot: snapshot.value, receivedAt: t } : null) : entries[node.id] || null,
  }));
  const merged = current.fused && fleet ? mergeSnapshots(nodes, t, snapshot.value) : null;
  // The toolbar's filter and view menu narrow this listing as they do one
  // node's (renderRevision: a column or order change re-renders).
  groups.renderRevision.value;
  const view = buildGroupsView(merged, { ...groups.visibility.value, ...NODE_ONLY_OFF }, groups.query.value, fusedOrder);
  // A group drag here (group-reorder.js) plans against this listing.
  const mergedRef = useRef(merged);
  mergedRef.current = merged;
  useEffect(() => setFusedGroupListing(() => mergedRef.current), []);
  // Triggers are read from each node that lets this one act there (this
  // node, or a peer trusting it unrestricted) and has them on; each rule is
  // tagged with its node, its group under the merged name.
  const [triggers, setTriggers] = useState([]);
  const selfID = fleet?.self?.id || '';
  const triggerNodes = (merged?.fleet_nodes || []).map(({ node }) => node)
    .filter((node) => snapOf(node.id)?.triggers_enabled === true && (node.id === selfID || snapOf(node.id)?.peer_view?.level === 'unrestricted'));
  const triggerKey = triggerNodes.map((node) => `${node.id}=${node.name}`).join(',');
  // Each node is read once while it stays listed (its rules shown as they
  // arrive), not again as other nodes' snapshots load.
  const triggerCache = useRef(new Map());
  const mountedRef = useRef(true);
  useEffect(() => () => { mountedRef.current = false; }, []);
  useEffect(() => {
    const self = state.view.value.fleet?.self?.id || '';
    const keys = new Set(triggerNodes.map((node) => `${node.id}=${node.name}`));
    for (const key of [...triggerCache.current.keys()]) if (!keys.has(key)) triggerCache.current.delete(key);
    const publish = () => setTriggers([...triggerCache.current.values()].flat());
    publish();
    for (const node of triggerNodes) {
      const key = `${node.id}=${node.name}`;
      if (triggerCache.current.has(key)) continue;
      triggerCache.current.set(key, []);
      loadTriggerRules(nodeFetch(node.id, { self, fetchImpl, remote: remote ? { id: remote, localFetch } : null }))
        .then((rules) => rules.map((rule) => ({ ...rule, fleet_node: node.id, ...(rule.group ? { group: `${rule.group}@${node.name}` } : {}) })))
        .catch(() => [])
        .then((rules) => { if (mountedRef.current && triggerCache.current.has(key)) { triggerCache.current.set(key, rules); publish(); } });
    }
  }, [triggerKey]);
  const wizard = useWizardTheme();
  const hoveredGroupKey = useGroupsHover(host, view.groups);
  useGroupsTableControls(host, groups, routed.current);
  // The toolbar counts what this view lists while it is on screen.
  const listedKey = `${view.shownReal}/${view.total}/${view.groups.map((g) => `${g.key || g.name}:${(g.members || []).map((m) => m.agent_id || m.conv_id).join('+')}`).join(',')}`;
  useEffect(() => {
    groups.listed.value = active ? { groups: view.groups, total: view.total, shownReal: view.shownReal } : null;
  }, [active, listedKey]);
  useEffect(() => () => { groups.listed.value = null; }, []);
  useEffect(() => {
    syncBotAnimations();
    syncWizardOrbit();
  });

  // Reasons put on refused controls (hover/focus), re-decided after each
  // render, as polls change what each node shares.
  const titles = useMemo(() => reasonTitles(), []);
  const refusalRef = useRef(null);
  useEffect(() => {
    titles.sweep((el) => { const why = refusalRef.current?.(el); return why ? [why, true] : null; });
  });

  // Let this node's own groups' controls through to their usual handlers, send
  // a peer's shared controls to its peer routes, stop the rest before their
  // handlers, and open a group's node from its @node suffix.
  useEffect(() => {
    if (!host) return undefined;
    // refusal says why a control on another node's group does not run from
    // here, or null when it runs (its own handler, its node's routes, a peer
    // action or a terminal). The click guard toasts it; hover and focus put
    // it on the control (reasonTitles), as a peer's own page does.
    const refusal = (act) => {
      if (!act || !host.contains(act) || VIEW_ONLY_ACTS.has(act.dataset.act) || act.dataset.act === 'send-elsewhere') return null;
      const group = act.closest('details[data-fleet-node]');
      const instance = group?.dataset.fleetNode || '';
      const name = group?.dataset.fleetNodeName || '';
      const self = state.view.value.fleet?.self?.id;
      if (!instance || instance === homeRef.current) return null;
      const peerView = snapRef.current(instance)?.peer_view;
      const held = grantHeld(act, peerView, (g) => ownGroup(g, name));
      if (held !== null && (instance === self || held)) return null;
      if (instance !== self) {
        if (SHELL_ACTS.has(act.dataset.act)) {
          const shell = shellControl(act, peerView, (g) => ownGroup(g, name));
          return shell.state === 'shell' ? null : limitHint(shell.state, shell.what, name || 'This node', shell.needs);
        }
        if (ATTACH_ACTS.has(act.dataset.act) && act.dataset.agent) return null;
        if (peerAction(act, peerView, (g) => ownGroup(g, name))) return null;
        return `${name || 'This node'} does not share this action with you${needsSuffix(grantMissing(act, peerView, (g) => ownGroup(g, name)))}`;
      }
      return `Act on this node's groups from ${name ? `${name}'s own dashboard (click @${name})` : "the node's own dashboard"}`;
    };
    refusalRef.current = refusal;
    const { setReason, clearReason } = titles;
    const onOver = (event) => {
      const act = actOf(event.target, event);
      if (!act || !host.contains(act)) return;
      const why = refusal(act);
      if (why) setReason(act, why); else clearReason(act);
    };
    const onClick = (event) => {
      const suffix = event.target.closest?.('[data-fleet-open]');
      if (suffix && host.contains(suffix)) {
        event.preventDefault(); event.stopPropagation();
        switchNode(suffix.dataset.fleetOpen);
        return;
      }
      const act = actOf(event.target, event);
      if (!act || !host.contains(act) || VIEW_ONLY_ACTS.has(act.dataset.act)) return;
      const group = act.closest('details[data-fleet-node]');
      const instance = group?.dataset.fleetNode || '';
      // ⚙ "Send elsewhere" opens the same launch-target dialog as a drop.
      if (act.dataset.act === 'send-elsewhere' && group) {
        event.preventDefault(); event.stopImmediatePropagation();
        const name = group.dataset.fleetNodeName || '';
        const data = { ...act.dataset, ownGroup: ownGroup(act.dataset.group || '', name) };
        setLaunch({ mode: act.dataset.mode || 'move', agent: launchAgent(data, { id: instance, name }) });
        return;
      }
      // A group or agent control another node lets run goes to its own
      // handler, whose actions are bound to that node (forNode); this node's
      // always may.
      if (group && instance !== homeRef.current) {
        const name = group.dataset.fleetNodeName || '';
        const held = grantHeld(act, snapRef.current(instance)?.peer_view, (g) => ownGroup(g, name));
        if (held !== null && (instance === state.view.value.fleet?.self?.id || held)) return;
      }
      // A home group's control runs as on this node's Groups view (the
      // document's data-act handler maps its group@node names back).
      if (instance && instance === homeRef.current) return;
      event.preventDefault(); event.stopImmediatePropagation();
      if (ATTACH_ACTS.has(act.dataset.act) && act.dataset.agent && instance && instance !== state.view.value.fleet?.self?.id) {
        // Ctrl/Cmd opens it without switching to it, as on this node.
        const background = !!(event.ctrlKey || event.metaKey);
        void openTerminal({ instance, agent: act.dataset.agent, peerLabel: group.dataset.fleetNodeName || '', toast, ...(background ? { background } : {}) });
        return;
      }
      if (SHELL_ACTS.has(act.dataset.act) && instance && instance !== state.view.value.fleet?.self?.id) {
        const name = group.dataset.fleetNodeName || '';
        const shell = shellControl(act, snapRef.current(instance)?.peer_view, (g) => ownGroup(g, name));
        if (shell.state === 'shell') {
          void openShell({ instance, ...shell.shell, peerLabel: name, toast });
          return;
        }
      }
      const name = group?.dataset.fleetNodeName || '';
      if (instance && instance !== state.view.value.fleet?.self?.id) {
        const req = peerAction(act, snapRef.current(instance)?.peer_view, (g) => ownGroup(g, name));
        if (req) { setPeerReq({ ...req, node: name || instance.slice(0, 13), nodeId: instance }); return; }
      }
      toast(refusal(act) || `Act on this node's groups from ${name ? `${name}'s own dashboard (click @${name})` : "the node's own dashboard"}`, true);
    };
    // Editable chips open on Enter/Space in their own keydown, and the row
    // context menu (Ctrl+right-click) opens terminals: guard both like clicks.
    const onKey = (event) => {
      if (event.key !== 'Enter' && event.key !== ' ') return;
      if (event.target?.closest?.('input, textarea, select')) return;
      onClick(event);
    };
    const onContext = (event) => {
      if (homeRef.current && event.target?.closest?.('details[data-fleet-node]')?.dataset.fleetNode === homeRef.current) return;
      if (!event.target?.closest?.('[data-act]') || !host.contains(event.target)) return;
      event.preventDefault(); event.stopImmediatePropagation();
      // macOS turns Ctrl-click into this event: a peer row's attach still
      // opens, in the background (row-actions.js does so on this node).
      if ((event.ctrlKey || event.metaKey) && ATTACH_ACTS.has(actOf(event.target, event)?.dataset.act)) onClick(event);
    };
    // A member drag is the Groups drag-and-drop (dnd.js) on every node: a
    // drop on a group on the agent's own node moves, adds or clones it there
    // as on that node's own Groups view. A drop on a group on another node is
    // a move between nodes: these handlers take it (the move dialog), and
    // dnd.js never sees it.
    let dragging = null;
    let marked = null;
    // The same whole-box highlight as a drop on the agent's own node.
    const mark = (el, ok) => {
      if (marked && marked !== el) clearDropBox(marked);
      marked = el;
      markDropBox(el, ok ? 'move' : 'refused');
    };
    const unmark = () => { clearDropBox(marked); marked = null; };
    const self = () => state.view.value.fleet?.self?.id || '';
    const onDragStart = (event) => {
      if (!host.contains(event.target)) return;
      const src = dragSource(event.target.closest?.('tr.dnd-draggable'));
      // A home group's process drags (a saved process, a run's performer)
      // run as on this node's own view. The single-run focus replaces the
      // tree and only ever shows a home group (a peer's has no runs here).
      if (!src && homeRef.current && event.target.closest?.(PROCESS_DRAGS)
        && (event.target.closest('[data-run-dock]')
          || event.target.closest('details[data-fleet-node]')?.dataset.fleetNode === homeRef.current)) return;
      if (!src) {
        // A group header drags as on its node's Groups view (group-reorder.js)
        // where this node may act there.
        const header = event.target.closest?.('summary[data-group-reorder]');
        const box = header?.closest('details[data-fleet-node]');
        if (header && box && nodeAllowsActing(isPageNode(box.dataset.fleetNode) ? '' : box.dataset.fleetNode)) return;
        // A pending or queued spawn drags to the bin only (dnd.js), on a node
        // this one may act on.
        const spawn = event.target.closest?.('tr.dnd-draggable[data-dnd-pending]')?.closest('details[data-fleet-node]');
        if (spawn && nodeAllowsActing(isPageNode(spawn.dataset.fleetNode) ? '' : spawn.dataset.fleetNode)) return;
        event.preventDefault(); return;
      }
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
      const target = dropTarget(event.target);
      if (!target) { unmark(); return; }
      const plan = dropPlan({ source: dragging.source, target, self: self() });
      // The agent's own node: dnd.js.
      if (plan.kind === 'same') { unmark(); return; }
      event.stopPropagation();
      // dnd.js no longer sees this drag's hover: its clone must not finish.
      dndGesture.cancelClone();
      if (plan.kind === 'none') { unmark(); return; }
      // A third-node drop is accepted only to say why not, unless the agent's
      // node is unrestricted and relays it on; so is an ungranted pull.
      event.preventDefault();
      const ok = plan.kind === 'push' || (plan.kind === 'pull' && !pullMissing(dragging).length) || (plan.kind === 'third' && relays(dragging.source.node));
      if (event.dataTransfer) event.dataTransfer.dropEffect = ok ? 'move' : 'none';
      mark(target.el, ok);
    };
    const onDrop = (event) => {
      if (!dragging) return;
      const target = dropTarget(event.target);
      const plan = target ? dropPlan({ source: dragging.source, target, self: self() }) : { kind: 'none' };
      if (plan.kind === 'same') { dragging = null; unmark(); return; }
      event.preventDefault(); event.stopPropagation();
      dndGesture.cancelClone();
      const src = dragging; dragging = null; unmark();
      if (!target) return;
      if (plan.kind === 'third' && !relays(src.source.node)) { toast(`Neither ${src.source.name} nor ${target.name} is this node: move ${src.agent.name} from ${src.source.name}'s dashboard`, true); return; }
      const missing = plan.kind === 'pull' ? pullMissing(src) : [];
      if (missing.length) { toast(grantWhy(src.source.name, missing).replace('this agent', src.agent.name), true); return; }
      if (plan.kind === 'push' || plan.kind === 'pull' || plan.kind === 'third') {
        setLaunch({ mode: 'move', fromDrop: true, target: { node: target.node, name: target.name, group: target.group },
          agent: { id: src.agent.id, conv: '', name: src.agent.name, node: src.source.node, nodeName: src.source.name, group: src.source.group } });
      }
    };
    const onDragEnd = () => { dragging = null; unmark(); };
    const pullMissing = (src) => missingPullGrants(snapRef.current(src.source.node)?.peer_view, src.agent.id);
    host.addEventListener('click', onClick, true);
    host.addEventListener('pointerover', onOver, true);
    host.addEventListener('focusin', onOver, true);
    host.addEventListener('keydown', onKey, true);
    host.addEventListener('contextmenu', onContext, true);
    host.addEventListener('dragstart', onDragStart, true);
    host.addEventListener('dragover', onDragOver, true);
    host.addEventListener('drop', onDrop, true);
    host.addEventListener('dragend', onDragEnd, true);
    return () => {
      host.removeEventListener('click', onClick, true);
      host.removeEventListener('pointerover', onOver, true);
      host.removeEventListener('focusin', onOver, true);
      host.removeEventListener('keydown', onKey, true);
      host.removeEventListener('contextmenu', onContext, true);
      host.removeEventListener('dragstart', onDragStart, true);
      host.removeEventListener('dragover', onDragOver, true);
      host.removeEventListener('drop', onDrop, true);
      host.removeEventListener('dragend', onDragEnd, true);
    };
  }, [host]);

  if (!current.fused) return null;
  if (!fleet) return html`<div class="empty">Loading the fused nodes…</div>`;
  const limits = limitsCSS(nodes.map(({ node, entry }) => ({ id: node.id, name: node.name, local: !!node.local, peerView: entry?.snapshot?.peer_view || null })), { self: fleet.self.id, home });
  // This node's standing-orders dialog renders with its Groups view, hidden
  // while fused: open it here, for a home group with its actions and for
  // another node's (descriptor.node) with that node's.
  const standingOrders = groupsFeature()?.standingOrdersDialog?.value || null;
  const ordersActions = standingOrders?.node ? forNode({ id: standingOrders.node }) : home && groupsActions();
  return html`<div class="fleet-merged">
    <style id="fleet-merged-limits">${limits}</style>
    <${BrokerRefusalNotice} snapshot=${snapshot.value} />
    <${GroupsProcessScope} value=${processScope.current}>
      <${GroupsInteractionProvider}>
        <${GroupsLensList} state=${groups} groups=${view.groups} rosters=${home ? snapshot.value?.groups : []} snapshot=${merged}
          actions=${routed.current} triggers=${triggers} hoveredGroupKey=${hoveredGroupKey} processSource=${processSource} wizard=${wizard} />
      <//>
    <//>
    ${standingOrders && ordersActions && html`<${GroupStandingOrdersDialog}
      key=${standingOrders.launchID} descriptor=${standingOrders} actions=${ordersActions} />`}
    ${peerReq && html`<${PeerActionDialog} req=${peerReq} peerView=${snapOf(peerReq.nodeId)?.peer_view} groups=${snapOf(peerReq.nodeId)?.groups} snapshot=${snapOf(peerReq.nodeId)}
      actions=${peerActions(peerReq.nodeId)} toast=${toast} onClose=${() => setPeerReq(null)} ...${confirm ? { confirm } : {}} ...${openSpawn ? { openSpawn } : {}} ...${openMessage ? { openMessage } : {}} />`}
    ${launch && html`<${LaunchTargetDialog} agent=${launch.agent} initialMode=${launch.mode} target=${launch.target || null} fromDrop=${!!launch.fromDrop}
      peerView=${snapOf(launch.agent.node)?.peer_view || null}
      self=${fleet.self} fleet=${fleet} api=${launchApi.current} actions=${fleetActions.current} peerActions=${peerActions} timers=${timers} now=${now}
      localGroups=${groupNames(fleet.self.id)} relay=${relays(launch.agent.node)}
      nodeGroups=${Object.fromEntries((fleet.peers || []).map((p) => [p.id, groupNames(p.id)]))}
      onClose=${() => setLaunch(null)}
      onLocalClone=${() => { const a = launch.agent; setLaunch(null); openCloneAgentDialog(a.conv || a.id, a.name, a.cwd || ''); }} />`}
  </div>`;
}

export function mountMergedGroupsIsland({ host, state, registerCleanup }) {
  render(html`<${MergedGroups} state=${state} host=${host} />`, host);
  registerCleanup(() => render(null, host));
}
