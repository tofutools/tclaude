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
// them. Groups is the snapshot itself, which shows every group the peer sees
// (any grant in it), each field as far as its grants reach (roster,
// presence, agents.status.read, mail; peer_snapshot.go); the map is this operator's own fleet
// view, and Terminals holds this browser's own terminals (a peer's agent
// opens there through this node's federation bridge). A tab absent here has
// no peer route at all. Usage is gated on costs: the peer serves both its
// Costs view and its Quota history under that one grant.
// Messages is the peer's mail, read under node.messages.read; what the
// reader may do there follows the peer's further grants (mail-island.js).
// Jobs (cron, standing orders, triggers) and Access (this node's pending
// approvals) have no peer grant of their own: unrestricted trust reaches them
// as the local operator does (local_dashboard).
const TAB_FEATURES = Object.freeze({ groups: 'groups', usage: 'costs', costs: 'costs', activity: 'audit', messages: 'node.messages', jobs: 'local_dashboard', access: 'local_dashboard', map: 'local', fleet: 'local', 'fleet-admin': 'local', terminals: 'local' });

// SUBTAB_FEATURES narrows a tab's feature per subtab. Activity is offered on a
// peer view for its audit log; the daemon log and poll timings have no peer
// grant, so only unrestricted trust reaches them.
const SUBTAB_FEATURES = Object.freeze({
  activity: Object.freeze({ audit: 'audit', logs: 'local_dashboard', performance: 'local_dashboard' }),
});

// READ_ONLY_ACTS are data-act controls that only change this browser's view
// or show snapshot data. The ⚙ menus open (their items are gated one by one).
// (The status dot is a power control — wake / shut down — so it is not here.)
export const READ_ONLY_ACTS = new Set(['copy-generation-id', 'sandbox-details', 'group-menu', 'row-menu']);

// GROUP_GRANT_ACTS are group controls a peer serves through its peer-granted
// routes (peer_grant_routes.go), with the peer grants they need in their
// group (or, as feature:<name>, a node-wide feature the peer shares: the
// profile pickers read its spawn catalog). The peer names the grants it
// holds per group in peer_view.groups; unrestricted trust holds them all. A
// control listed here runs its normal handler, whose requests reach the peer
// (remote-node.js, node-api.js). A group's default directory and sandbox
// profile reach every spawn into it, so they need the matching spawn grant.
export const GROUP_GRANT_ACTS = Object.freeze({
  'set-group-descr': ['groups.settings'],
  'set-group-dir': ['groups.settings', 'groups.members.spawn.cwd'],
  'set-group-max-members': ['groups.settings'],
  'set-group-profile': ['groups.settings', 'feature:spawn.catalog'],
  'set-group-sandbox-profile': ['groups.settings', 'groups.members.spawn.sandbox', 'feature:spawn.catalog'],
  'group-attachment': ['groups.settings'],
  'rename-group': ['groups.settings'],
  // The ⚙ menu's group settings (nesting is board layout only).
  'group-settings': ['groups.settings'],
  'set-group-context': ['groups.settings'],
  'toggle-group-notify': ['groups.settings'],
  'set-group-remote-control': ['groups.settings'],
  'nest-group': ['groups.settings'],
  'unnest-group': ['groups.settings'],
  // Shut down / power on every member: the member lifecycle grants.
  'shutdown-group': ['groups.members.stop'],
  'power-on-group': ['groups.members.resume'],
  // Fields of the settings dialog with no control of their own: the
  // environment reaches every spawned agent's process, and the default saved
  // prompts every spawn's brief.
  'set-group-environment': ['groups.settings.environment'],
  'set-group-prompts': ['groups.settings', 'groups.members.spawn.prompts'],
  // A member's role belongs to the group (its description cell also edits
  // the agent's tags: an agent control).
  'edit-role': ['groups.members.manage'],
  // A clone's members spawn with the node's launch settings, and delete and
  // cleanup remove what no peer grant hands out: unrestricted trust only (a
  // feature no restricted peer shares).
  'clone-group': ['feature:groups.create.full'],
  'delete-group': ['feature:groups.manage.full'],
  'cleanup-group': ['feature:groups.manage.full'],
  // Removing a member from the group: unrestricted trust only.
  'remove-member': ['feature:agents.manage.full'],
  // What no peer grant covers, which unrestricted trust reaches as this
  // node's operator does (the local_dashboard routes): its process, force
  // and ownership controls, the auto-join default, its member permissions, its
  // links within its node, new cron jobs, its standing orders, adding
  // members, its cross-harness spawn policy and saving it as a template there.
  'advance-phase': ['feature:local_dashboard'],
  'rebrief-force': ['feature:local_dashboard'],
  'stand-down-force': ['feature:local_dashboard'],
  'grant-owner': ['feature:local_dashboard'],
  'revoke-owner': ['feature:local_dashboard'],
  'set-default-spawn-group': ['feature:local_dashboard'],
  'set-group-permissions': ['feature:local_dashboard'],
  'link-new': ['feature:local_dashboard'],
  'link-edit': ['feature:local_dashboard'],
  'link-delete': ['feature:local_dashboard'],
  'cron-new': ['feature:local_dashboard'],
  'standing-orders': ['feature:local_dashboard'],
  'template-from-group': ['feature:local_dashboard'],
  'add-member': ['feature:local_dashboard'],
  // A subgroup, through the regular create dialog on the group's node.
  'create-subgroup': ['feature:groups.create'],
  // A pending spawn's 🗑, on the group it is pending in.
  'delete-pending': ['feature:local_dashboard'],
  // A queued spawn's row (queued-spawns.js): its dialog or discard, on the
  // queue's node.
  'queued-spawn-launch': ['feature:local_dashboard'],
  'queued-spawn-edit': ['feature:local_dashboard'],
  'queued-spawn-discard': ['feature:local_dashboard'],
  // Its archive downloads from its node (dashboard_peer_downloads.go).
  'export-group': ['feature:local_dashboard'],
  // Unrestricted trust messages a group with the regular dialog; a restricted
  // peer's messaging grant sends one agent at a time (PEER_ACTS).
  'message-new': ['feature:local_dashboard'],
  'spawn-harness-policy': ['feature:local_dashboard'],
  // Long operations, which run as peer view jobs (peer_view_jobs.go).
  'pull-repositories-group': ['feature:local_dashboard'],
  'sync-repositories-group': ['feature:local_dashboard'],
  'cleanup-worktrees-group': ['feature:local_dashboard'],
  // Not listed, so local to their own node: the links manager,
  // messages, the template manager, terminals and showing a
  // window (hiding one is an agent control); and export, whose binary archive the JSON peer
  // view does not carry.
});

// groupGrantHeld reports whether the peer lets a group control act on group
// (its own name for the group).
export function groupGrantHeld(act, group, peerView) {
  const need = GROUP_GRANT_ACTS[act];
  if (!need || !group) return false;
  if (peerView?.level === 'unrestricted') return true;
  const held = peerView?.groups?.[group];
  return Array.isArray(held) && need.every((slug) => (slug.startsWith('feature:')
    ? featureState(slug.slice('feature:'.length), peerView) === 'shared'
    : held.includes(slug)));
}

// missingGrants lists what a refused control lacks: the grants of need not
// in held, and for a feature:<name> the grant the peer names for it.
function missingGrants(need, held, peerView) {
  const out = need.flatMap((slug) => (slug.startsWith('feature:')
    ? (featureState(slug.slice('feature:'.length), peerView) === 'shared' ? [] : [featureNeeds(slug.slice('feature:'.length), peerView)])
    : (held.includes(slug) ? [] : [slug])));
  return [...new Set(out.filter(Boolean))];
}

// AGENT_GRANT_ACTS are an agent's controls a peer serves through its
// peer-granted routes (peer_member_routes.go), with the peer grants they
// need. An agent's own settings follow it into every group it is in, so the
// peer names per agent the grants it holds in all of them
// (peer_view.agents); unrestricted trust holds them all. A control listed
// here runs its normal handler, whose requests reach the agent's node.
export const AGENT_GRANT_ACTS = Object.freeze({
  'rename-name': ['groups.members.manage'],
  'edit-member': ['groups.members.manage'],
  'edit-descr': ['groups.members.manage'],
  'edit-task': ['groups.members.manage'],
  'toggle-agent-notify': ['groups.members.manage'],
  // Remote access, fast mode, the sandbox implementation, the debug export,
  // reincarnation and permanent delete reach what no peer grant hands out:
  // unrestricted trust only (a feature no restricted peer shares).
  'toggle-remote-control': ['feature:agents.manage.full'],
  'sandbox-impl': ['feature:agents.manage.full'],
  'debug-export': ['feature:agents.manage.full'],
  'fast-mode-set': ['feature:agents.manage.full'],
  'fast-mode-disable': ['feature:agents.manage.full'],
  'reincarnate': ['feature:agents.manage.full'],
  'delete-agent': ['feature:agents.manage.full'],
  // Its lifecycle, through the regular dialogs (peer_actions.go refuses
  // their local-only options). A restart stops the agent first.
  'restart': ['groups.members.stop', 'groups.members.resume'],
  'retire-agent': ['groups.members.retire'],
  'clone': ['groups.members.clone'],
  // Unrestricted trust only (the local_dashboard routes).
  'promote-agent': ['feature:local_dashboard'],
  'reinstate-agent': ['feature:local_dashboard'],
  'delete-generation': ['feature:local_dashboard'],
  'hide': ['feature:local_dashboard'],
  'perm-edit': ['feature:local_dashboard'],
  'sudo-grant': ['feature:local_dashboard'],
  // Its node's snapshot library and PAF export; the file downloads from
  // there (dashboard_peer_downloads.go).
  'snapshot-agent': ['feature:local_dashboard'],
  'export-paf': ['feature:local_dashboard'],
});

// AGENT_GRANT_VARIANTS are agent controls whose grants depend on which way
// they act, as [selector qualifier, dataset test, grants]: the status dot
// stops a running agent and wakes a stopped one, a sandbox restart that
// turns the sandbox off takes unrestricted trust, and of "Send elsewhere"
// only a move brings another node's agent here (which retires it there).
const STOP_AND_RESUME = Object.freeze(['groups.members.stop', 'groups.members.resume']);
// PULL_GRANTS are what a peer must grant on its agent for this node to bring
// it here: a move, a teleport or a clone all go through its move route.
export const PULL_GRANTS = Object.freeze(['agent.move', 'groups.members.retire']);
export const AGENT_GRANT_VARIANTS = Object.freeze({
  'dot-toggle': Object.freeze([
    ['[data-online="1"]', (d) => d.online === '1', ['groups.members.stop']],
    [':not([data-online="1"])', (d) => d.online !== '1', ['groups.members.resume']],
  ]),
  'sandbox-restart': Object.freeze([
    ['[data-action="restore"]', (d) => d.action === 'restore', STOP_AND_RESUME],
    [':not([data-action="restore"])', (d) => d.action !== 'restore', [...STOP_AND_RESUME, 'feature:agents.manage.full']],
  ]),
  'send-elsewhere': Object.freeze([
    ['[data-mode="move"]', (d) => d.mode === 'move', PULL_GRANTS],
  ]),
});

// isAgentGrantAct reports whether act is an agent control of either table.
export function isAgentGrantAct(act) {
  return !!(AGENT_GRANT_ACTS[act] || AGENT_GRANT_VARIANTS[act]);
}

function agentNeedHeld(need, agent, peerView) {
  if (peerView?.level === 'unrestricted') return true;
  const held = peerView?.agents?.[agent] || [];
  return need.every((slug) => (slug.startsWith('feature:')
    ? featureState(slug.slice('feature:'.length), peerView) === 'shared'
    : held.includes(slug)));
}

// missingPullGrants names the PULL_GRANTS a peer withholds on its agent;
// without the peer's metadata nothing is known, so nothing is reported. A
// peer that grants no agent controls omits peer_view.agents: none held, as
// the ⚙ menu's move gating reads it.
export function missingPullGrants(peerView, agent) {
  if (!peerView || unrestricted(peerView)) return [];
  return missingGrants(PULL_GRANTS, peerView.agents?.[agent] || [], peerView);
}

// agentGrantHeld reports whether the peer lets an agent control act on agent
// (its agent ID); dataset picks a variant's direction.
export function agentGrantHeld(act, agent, peerView, dataset = {}) {
  const need = AGENT_GRANT_ACTS[act] || AGENT_GRANT_VARIANTS[act]?.find(([, test]) => test(dataset))?.[2];
  if (!need || !agent) return false;
  return agentNeedHeld(need, agent, peerView);
}

// grantHeld reports whether the peer lets a granted control run: a group's
// (GROUP_GRANT_ACTS, on its group, which groupOf maps to the peer's own name)
// or an agent's (AGENT_GRANT_ACTS, on its agent). It is null for any other
// control.
export function grantHeld(act, peerView, groupOf = (g) => g) {
  const name = act?.dataset?.act;
  if (GROUP_GRANT_ACTS[name]) return groupGrantHeld(name, groupOf(act.dataset.group || ''), peerView);
  // The retire icon stays conv-keyed and names its agent in
  // data-stable-agent.
  if (isAgentGrantAct(name)) return agentGrantHeld(name, act.dataset.stableAgent || act.dataset.agent || '', peerView, act.dataset);
  return null;
}

// grantMissing lists the grants a refused group or agent control (see
// grantHeld) lacks on the peer, for the hint that names them; empty for any
// other control, or when nothing is missing.
export function grantMissing(act, peerView, groupOf = (g) => g) {
  const name = act?.dataset?.act;
  if (unrestricted(peerView)) return [];
  if (GROUP_GRANT_ACTS[name]) {
    const group = groupOf(act.dataset.group || '');
    return missingGrants(GROUP_GRANT_ACTS[name], peerView?.groups?.[group] || [], peerView);
  }
  if (!isAgentGrantAct(name)) return [];
  const need = AGENT_GRANT_ACTS[name] || AGENT_GRANT_VARIANTS[name]?.find(([, test]) => test(act.dataset))?.[2];
  return need ? missingGrants(need, peerView?.agents?.[act.dataset.stableAgent || act.dataset.agent || ''] || [], peerView) : [];
}

// groupFieldMissing is grantMissing for a group dialog's field.
export function groupFieldMissing(act, group, peerView) {
  if (!peerView || unrestricted(peerView) || !GROUP_GRANT_ACTS[act]) return [];
  return missingGrants(GROUP_GRANT_ACTS[act], peerView?.groups?.[group] || [], peerView);
}

// groupFieldHeld is groupGrantHeld for a dialog that may be on this node's
// own group: without a peer view (this node's snapshot) everything is held.
export function groupFieldHeld(act, group, peerView) {
  return !peerView || groupGrantHeld(act, group, peerView);
}

// cssString quotes a value for an attribute selector: group names come from
// the peer, so they never reach the stylesheet unescaped.
function cssString(value) {
  return `"${String(value).replace(/[\\"]/g, '\\$&').replace(/[\n\r\f]/g, '')}"`;
}

// groupGrantSelectors lists the group controls the peer lets run: every one
// under unrestricted trust, otherwise each group's own. The default
// directory chip's text opens its editor (its terminal link needs Ctrl), so
// it counts as the chip. groupName maps the peer's own group name to the
// one its controls carry (the fused view's group@node).
export function groupGrantSelectors(peerView, groupName = (g) => g) {
  const out = [];
  const add = (act, qualifier) => {
    out.push(`[data-act="${act}"]${qualifier}`);
    if (act === 'set-group-dir') out.push(`[data-act="${act}"]${qualifier} [data-act="group-web-term"]`);
  };
  if (peerView?.level === 'unrestricted') {
    for (const act of Object.keys(GROUP_GRANT_ACTS)) add(act, '');
    return out;
  }
  for (const group of Object.keys(peerView?.groups || {})) {
    for (const act of Object.keys(GROUP_GRANT_ACTS)) {
      if (groupGrantHeld(act, group, peerView)) add(act, `[data-group=${cssString(groupName(group))}]`);
    }
  }
  return out;
}

// grantSelectors is groupGrantSelectors with the agent controls the peer lets
// run: every one under unrestricted trust, otherwise each agent's own.
export function grantSelectors(peerView, groupName = (g) => g) {
  const out = groupGrantSelectors(peerView, groupName);
  const controls = [
    ...Object.entries(AGENT_GRANT_ACTS).map(([act, need]) => [`[data-act="${act}"]`, need]),
    ...Object.entries(AGENT_GRANT_VARIANTS).flatMap(([act, variants]) => variants.map(([qualifier, , need]) => [`[data-act="${act}"]${qualifier}`, need])),
  ];
  const agents = peerView?.level === 'unrestricted' ? [''] : Object.keys(peerView?.agents || {});
  for (const agent of agents) {
    for (const [sel, need] of controls) {
      if (!agentNeedHeld(need, agent, peerView)) continue;
      if (!agent) { out.push(sel); continue; }
      out.push(`${sel}[data-agent=${cssString(agent)}]`, `${sel}[data-stable-agent=${cssString(agent)}]`);
    }
  }
  return out;
}

// actOf is the control a click or key press acts on: the closest data-act,
// except that a plain click on the default directory chip's text opens the
// chip's editor (only Ctrl/Cmd-click opens its terminal).
export function actOf(target, event = null) {
  const act = target?.closest?.('[data-act]');
  if (act?.dataset.act === 'group-web-term' && !(event?.ctrlKey || event?.metaKey)) {
    return act.parentElement?.closest?.('[data-act="set-group-dir"]') || act;
  }
  return act;
}

// GATED_CONTROLS are mutation controls without a data-act: group creation and
// the management overlays, which edit the shown node's configuration.
const GATED_CONTROLS = [
  '#group-create-open', '#group-import-open', '#snapshots-open', '#cleanup-all-open', '#delete-retired-open',
  '#profiles-manage-open', '#templates-manage-open', '#roles-manage-open',
  '#sandbox-profiles-manage-open', '#spawn-harness-policy-open',
];

// MANAGED_CONTROLS are the node-configuration controls a peer serves under a
// grant (node.spawn.defaults.manage, node.groups.create; unrestricted trust
// holds both): they run their regular local dialogs, routed to the peer. The
// default pickers also list the peer's profiles, so they need its catalogs
// (node.spawn.read) too.
const MANAGED_CONTROLS = Object.freeze({
  '[data-act="set-dash-profile"]': ['spawn.defaults', 'spawn.catalog'],
  '[data-act="set-dash-sandbox-profile"]': ['spawn.defaults', 'spawn.catalog'],
  '#group-create-open': ['groups.create'],
});

// managedState is 'shared' when the peer shares every feature, else the
// first missing one's state.
function managedState(features, peerView) {
  for (const feature of features) {
    const state = featureState(feature, peerView);
    if (state !== 'shared') return state;
  }
  return 'shared';
}

const managedWhat = (features) => (features[0] === 'groups.create' ? 'group creation' : 'its global spawn defaults');
const managedNeeds = (features, peerView) => features.filter((f) => featureState(f, peerView) !== 'shared').map((f) => featureNeeds(f, peerView));

// A managed control carrying data-node acts on that node, not the page's (the
// Groups toolbar following a fused view's node, toolbar-node.js): its own
// component gates it by that node's grants, so the page's limits leave it be
// (own).
function managedControl(target, peerView) {
  for (const [sel, features] of Object.entries(MANAGED_CONTROLS)) {
    const el = target?.closest?.(sel);
    if (el?.hasAttribute?.('data-node')) return { el, own: true };
    if (el) return { el, state: managedState(features, peerView), what: managedWhat(features), needs: managedNeeds(features, peerView) };
  }
  return null;
}

// managedHint is why the managed control sel is refused on the node whose
// peerView is given (named node), or '' when that node shares it.
export function managedHint(sel, peerView, node) {
  const features = MANAGED_CONTROLS[sel];
  if (!features) return '';
  const state = managedState(features, peerView);
  return state === 'shared' ? '' : limitHint(state, managedWhat(features), node, managedNeeds(features, peerView));
}

// managedControlsRule greys the managed controls the peer does not share.
export function managedControlsRule(peerView) {
  const off = Object.entries(MANAGED_CONTROLS).filter(([, f]) => managedState(f, peerView) !== 'shared').map(([sel]) => `html.remote-node ${sel}:not([data-node])`);
  return off.length ? `${off.join(', ')} { opacity: .45; cursor: not-allowed; }` : '';
}

// featureState reports whether the peer shares a feature: 'shared', 'omitted'
// (the peer could share it with a grant), or 'local' (not offered to any peer).
export function featureState(feature, peerView) {
  if (feature === 'local') return 'shared';
  // The routes without a peer grant answer only unrestricted trust.
  if (feature === 'local_dashboard') return unrestricted(peerView) ? 'shared' : 'local';
  if (!feature) return 'local';
  const included = Array.isArray(peerView?.included) ? peerView.included : null;
  const omissions = Array.isArray(peerView?.omitted) ? peerView.omitted : [];
  const omission = omissions.find((o) => (typeof o === 'string' ? o : o?.feature) === feature);
  // A feature no grant opens (only unrestricted trust) is not requestable.
  if (omission) return omission?.requires === 'local_only' ? 'local' : 'omitted';
  // Without metadata (an unrestricted peer's full snapshot still carries it;
  // before the first answer there is none) assume the snapshot-backed view.
  if (!included) return feature === 'groups' ? 'shared' : 'omitted';
  return included.includes(feature) ? 'shared' : 'omitted';
}

// humanReadShared reports whether the peer lets this operator mark its human
// notifications read or unread (node.messages.mark-read; unrestricted trust
// holds it), which opening one does as it does locally. A peer on an older
// build served that under human.inbox.answer.
export function humanReadShared(peerView) {
  return featureState('node.messages.mark-read', peerView) === 'shared' || featureState('human.inbox.answer', peerView) === 'shared';
}

// UNRESTRICTED_TRUST names what a feature no peer grant opens takes.
const UNRESTRICTED_TRUST = 'unrestricted trust';

// featureNeeds names the grant the peer requires for a feature it does not
// share (its omission's requires), 'unrestricted trust' for one no grant
// opens, or '' when there is nothing to name (shared, or no peer route).
export function featureNeeds(feature, peerView) {
  if (feature === 'local_dashboard') return unrestricted(peerView) ? '' : UNRESTRICTED_TRUST;
  const omissions = Array.isArray(peerView?.omitted) ? peerView.omitted : [];
  const omission = omissions.find((o) => typeof o === 'object' && o?.feature === feature);
  const requires = typeof omission?.requires === 'string' ? omission.requires : '';
  if (!requires || requires === 'local_only') return '';
  return omission.unrestricted_only ? UNRESTRICTED_TRUST : requires;
}

export function tabState(tab, peerView) {
  return featureState(TAB_FEATURES[tab] || '', peerView);
}

function tabNeeds(tab, peerView) {
  return featureNeeds(TAB_FEATURES[tab] || '', peerView);
}

function subtabNeeds(tab, subtab, peerView) {
  const subs = SUBTAB_FEATURES[tab];
  if (!subs || !Object.hasOwn(subs, subtab)) return tabNeeds(tab, peerView);
  return featureNeeds(subs[subtab], peerView);
}

export function subtabState(tab, subtab, peerView) {
  const subs = SUBTAB_FEATURES[tab];
  if (!subs || !Object.hasOwn(subs, subtab)) return tabState(tab, peerView);
  return featureState(subs[subtab], peerView);
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

// peerViewSubtabUsable is peerViewTabUsable for one subtab of a tab.
export function peerViewSubtabUsable(tab, subtab, { remote = globalThis.__tclaudeRemoteNode, peerView = currentPeerView() } = {}) {
  if (!remote?.id) return true;
  const subs = SUBTAB_FEATURES[tab];
  if (!subs || !Object.hasOwn(subs, subtab)) return peerViewTabUsable(tab, { remote, peerView });
  if (!peerView) return Boolean(subs[subtab]);
  return subtabState(tab, subtab, peerView) === 'shared';
}

// limitPeerViewCommands disables the command palette's node-changing commands
// on a peer view. Under unrestricted trust every command runs on the peer as
// on its own dashboard, except the ones that act on this machine's own
// windows, terminals or files (peerView 'own'). Commands only unrestricted
// trust reaches (peerView 'operator', what: the reason's subject) are off
// on any other peer. Otherwise only view commands,
// tabs the peer offers, messaging and spawning when it shares them, and an
// agent's lifecycle its grants cover (grant: the act, as its row's control)
// stay runnable.
export function limitPeerViewCommands(cmds, snap, remote = globalThis.__tclaudeRemoteNode) {
  if (!remote?.id) return cmds;
  const pv = snap?.peer_view || null;
  // A command naming its node (the fused view's other nodes') carries that
  // node's gate, not this page's peer's.
  const own = cmds.filter((cmd) => !cmd.node);
  if (own.length !== cmds.length) {
    const limited = limitPeerViewCommands(own, snap, remote);
    let i = 0;
    return cmds.map((cmd) => (cmd.node ? cmd : limited[i++]));
  }
  const name = globalThis.document?.documentElement?.dataset.remoteNodeName || remote.id.slice(0, 13);
  if (unrestricted(pv)) {
    return cmds.map((cmd) => (cmd.peerView === 'own'
      ? { ...cmd, enabled: false, disabledReason: `${name}'s windows, terminals and files stay on ${name}: use its own dashboard` }
      : cmd));
  }
  return cmds.map((cmd) => {
    let state = 'local';
    let what = 'This command';
    let needs = '';
    if (cmd.grant) {
      const { act, agent, dataset = {} } = cmd.grant;
      state = agentGrantHeld(act, agent, pv, dataset) ? 'shared' : 'omitted';
      what = "this agent's lifecycle";
      const need = AGENT_GRANT_ACTS[act] || AGENT_GRANT_VARIANTS[act]?.find(([, test]) => test(dataset))?.[2] || [];
      needs = state === 'shared' ? '' : missingGrants(need, pv?.agents?.[agent] || [], pv).join(' + ');
    } else if (cmd.peerView === 'spawn') { state = featureState('spawn.request', pv); what = 'spawning'; needs = featureNeeds('spawn.request', pv); }
    else if (cmd.peerView === 'view') state = 'shared';
    else if (cmd.peerView === 'tab') { state = tabState(cmd.tab, pv); what = tabLabel(cmd.tab); needs = tabNeeds(cmd.tab, pv); }
    else if (cmd.peerView === 'messaging') { state = featureState('messaging', pv); what = 'messaging'; needs = featureNeeds('messaging', pv); }
    else if (cmd.peerView === 'operator') { state = 'omitted'; what = cmd.what || what; needs = 'unrestricted trust'; }
    if (state === 'shared') return cmd;
    return { ...cmd, enabled: false, disabledReason: limitHint(state, what, name, [needs]) };
  });
}

// tabLabel names a tab in hints; the visible label varies by theme skin.
function tabLabel(tab) {
  return tab === 'costs' || tab === 'usage' ? 'costs' : tab === 'activity' ? 'its audit log' : tab === 'messages' ? 'its mail' : `the ${tab} tab`;
}

function subtabLabel(subtab) {
  return subtab === 'logs' ? 'The daemon log' : subtab === 'performance' ? 'Poll performance' : `The ${subtab} view`;
}

// limitHint words why a peer view refuses something. needs names what the
// peer would have to grant (featureNeeds, grantMissing); a refusal that only
// unrestricted trust lifts reads as not shared rather than unavailable.
// OPENS_HERE are the states whose control opens a terminal here, not a
// refusal: they keep their hover hint without being greyed.
const OPENS_HERE = new Set(['attach', 'shell']);

export function limitHint(state, what, node, needs = []) {
  if (state === 'attach') return `${node}'s agent terminal — opens here, watch-only or interactive as ${node} shares it`;
  if (state === 'shell') return `A shell on ${node} — opens here, in the browser`;
  const named = [...new Set((needs || []).filter(Boolean))];
  if (state === 'omitted' || named.length) {
    const subject = what.replace(/^(The|This) /, (m) => m.toLowerCase());
    return `${node} does not share ${subject} with you${needsSuffix(named)}`;
  }
  return `${what} is not available in a peer view`;
}

// needsSuffix is the " (needs a + b)" a hint ends with, or ''.
export function needsSuffix(needs) {
  const named = [...new Set((needs || []).filter(Boolean))];
  return named.length ? ` (needs ${named.join(' + ')})` : '';
}

// ATTACH_ACTS open an agent's terminal. On a peer view they open the peer's
// agent in the browser terminal, bridged by this node's daemon over the
// federation terminal stream (remote-terminal.js); the peer authorises it.
// (term-dir opens a shell in a directory, not the agent: see SHELL_ACTS.)
// The ⚙ "terminals & windows…" chooser goes straight to that attach, the only
// one of its options a peer can serve.
export const ATTACH_ACTS = new Set(['web-open-window', 'jump', 'terminal-chooser']);

// OWN_NODE_ACTS work on the node's own machine (a native window there, its
// desktop folder picker, a shell behind a directory picker): never from a
// peer view, even under unrestricted trust, where every other control runs
// as on the node itself.
const OWN_NODE_ACTS = new Set(['term', 'web-term', 'open-window', 'window-modal-group', 'window-modal-all', 'pick-group-dir']);

// SHELL_ACTS open a tclaude shell: a member's directory (its cwd path) or the
// group's default directory. On a peer view they open the peer's shell in the
// browser over the federation terminal stream (remote-terminal.js
// openRemoteShell), under the peer's sessions.shell grant on the group: host
// shell access, a grant of its own, which unrestricted trust holds.
export const SHELL_ACTS = new Set(['term-dir', 'group-web-term']);
const SHELL_WHICH = new Set(['start', 'current', 'worktree']);
export const SHELL_GRANT = 'sessions.shell';

// shellControl is how a peer view takes a SHELL_ACTS click: state 'shell'
// opens the peer's shell (shell: {group, shell, agent?, label}), 'omitted'
// says which grant it lacks, 'local' a control naming no usable target.
// groupOf maps a listed group name to the peer's own (fused group@node).
export function shellControl(act, peerView, groupOf = (g) => g) {
  const d = act.dataset;
  const group = groupOf(d.group || '');
  let shell = null;
  if (SAFE_GROUP.test(group) && !group.includes('@')) {
    if (d.act === 'group-web-term') shell = { group, shell: 'group', label: group };
    else if (SAFE_AGENT_ID.test(d.agent || '')) shell = { group, shell: SHELL_WHICH.has(d.which) ? d.which : 'current', agent: d.agent, label: d.label || d.agent };
  }
  if (!shell) return { el: act, state: 'local', what: 'This action' };
  const held = unrestricted(peerView) || (peerView?.groups?.[group] || []).includes(SHELL_GRANT);
  return held ? { el: act, state: 'shell', what: 'This terminal', shell } : { el: act, state: 'omitted', what: "this group's shells", needs: [SHELL_GRANT] };
}

// shellSelectors are the SHELL_ACTS controls a peer view runs: every one
// under unrestricted trust, else those of the groups granting sessions.shell.
// groupName maps the peer's group name to the listed one.
export function shellSelectors(peerView, groupName = (g) => g) {
  const acts = [...SHELL_ACTS];
  if (unrestricted(peerView)) return acts.map((a) => `[data-act="${a}"]`);
  const out = [];
  for (const [group, grants] of Object.entries(peerView?.groups || {})) {
    if (!grants.includes(SHELL_GRANT)) continue;
    for (const a of acts) out.push(`[data-act="${a}"][data-group=${cssString(groupName(group))}]`);
  }
  return out;
}

// unrestricted reports a peer that trusts this operator unrestricted: its
// dashboard routes all answer as they do for its own operator.
function unrestricted(peerView) {
  return peerView?.level === 'unrestricted';
}
// SAFE_AGENT_ID admits only a plain agent ID into a command the operator will
// paste into a shell: the ID comes from the peer's snapshot, and a hostile
// peer must not be able to smuggle shell syntax into it.
const SAFE_AGENT_ID = /^agt_[A-Za-z0-9]{4,64}$/;

// attachCommand is the CLI command that opens agent's pane on the peer.
export function attachCommand(agentID, remoteID) {
  return `tclaude federation attach ${agentID}@${remoteID}`;
}

// PEER_ACTS are controls a peer may grant through its action routes (spawn
// into a group; message one of its members). When the peer shares the
// feature, the click opens the peer action dialog (peer-action.js) instead
// of the local one. (An agent's lifecycle runs its regular dialogs:
// AGENT_GRANT_ACTS.)
const PEER_ACTS = Object.freeze({
  // Any visible group takes a spawn request (spawn.request; without a grant
  // it waits for the peer's operator). An older peer only offers 'spawn'.
  'spawn-agent': { action: 'spawn', feature: 'spawn.request', legacy: 'spawn' },
  'message-new': { action: 'message', feature: 'messaging' },
});
// PEER_ACTION_EVENT carries such a click to peer-action.js.
export const PEER_ACTION_EVENT = 'tclaude:peer-action';
const SAFE_GROUP = /^[A-Za-z0-9._@:-]{1,128}$/;

// peerAction maps a clicked control to the peer action request it stands for,
// or null when the peer does not share it. The merged all-nodes view
// (fleet-merged-island.js) uses it for a peer's rows too, with groupOf
// turning its group@node names back into the peer's own.
export function peerAction(act, peerView, groupOf = (g) => g) {
  const spec = PEER_ACTS[act.dataset.act];
  if (!spec || (featureState(spec.feature, peerView) !== 'shared' && !(spec.legacy && featureState(spec.legacy, peerView) === 'shared'))) return null;
  let group = act.dataset.group || '';
  if (spec.action === 'message') {
    try { group = JSON.parse(act.dataset.prefill || '{}')?.groupName; } catch (_) { group = ''; }
  }
  group = typeof group === 'string' ? groupOf(group) : '';
  return SAFE_GROUP.test(group) ? { action: spec.action, group } : null;
}

// usableActSelectors lists the data-act controls a click on a peer's row
// really runs: the view-only ones, the terminal attach, the granted ones
// (grantSelectors) and the PEER_ACTS the peer shares. The
// greying below is built from it, so it can never disagree with the guard.
// fused: the fused Groups view, whose guard (fleet-merged-island.js) runs
// only the tables' controls on another node, unrestricted or not; a peer's
// own page runs every control there under unrestricted trust.
export function usableActSelectors(peerView, extra = [], groupName = (g) => g, { fused = false } = {}) {
  if (unrestricted(peerView) && !fused) return [`[data-act]:not(${[...OWN_NODE_ACTS].map((a) => `[data-act="${a}"]`).join(', ')})`];
  const out = [...READ_ONLY_ACTS, ...ATTACH_ACTS, ...extra].map((a) => `[data-act="${a}"]`);
  out.push(...grantSelectors(peerView, groupName), ...shellSelectors(peerView, groupName));
  const shared = (feature) => featureState(feature, peerView) === 'shared';
  for (const [sel, features] of Object.entries(MANAGED_CONTROLS)) if (sel.startsWith('[data-act') && managedState(features, peerView) === 'shared') out.push(sel);
  for (const [act, spec] of Object.entries(PEER_ACTS)) {
    if (shared(spec.feature) || (spec.legacy && shared(spec.legacy))) out.push(`[data-act="${act}"]`);
  }
  return out;
}

// refusedActsRule greys every control matching target under scope that is
// not in usable: dimmed, with the not-allowed cursor. Conditions on the
// control itself belong in target, not scope (a scope :not() would be tested
// on an ancestor).
export function refusedActsRule(scope, usable, target = '[data-act]') {
  return `${scope} ${target}:not(${usable.join(', ')}) { opacity: .45; cursor: not-allowed; }`;
}

// FUSED_ROOT is the all-nodes Groups view: its rows belong to several nodes,
// so fleet-merged-island.js guards and greys them per node, not this module.
const FUSED_ROOT = '#fleetview-fused-groups-root';

// setLimitStyle keeps one generated <style> per id in the document head;
// empty css removes it. installPeerViewLimits' disposer removes it.
// dashboard-imperative-boundary: platform-layout
export function setLimitStyle(doc, id, css) {
  let el = doc.getElementById(id);
  if (!css) { el?.remove(); return; }
  if (!el) {
    el = doc.createElement('style');
    el.id = id;
    (doc.head || doc.documentElement).appendChild(el);
  }
  if (el.textContent !== css) el.textContent = css;
}

// blockedControl returns the element a click must not reach, and why.
export function blockedControl(target, peerView, event = null) {
  if (target?.closest?.(FUSED_ROOT)) return null;
  const tab = target?.closest?.('nav [data-tab]');
  if (tab) {
    const state = tabState(tab.dataset.tab, peerView);
    return state === 'shared' ? null : { el: tab, state, what: tabLabel(tab.dataset.tab), needs: [tabNeeds(tab.dataset.tab, peerView)] };
  }
  const sub = target?.closest?.('.activity-subtab');
  if (sub) {
    const state = subtabState('activity', sub.dataset.subtab, peerView);
    return state === 'shared' ? null : { el: sub, state, what: subtabLabel(sub.dataset.subtab), needs: [subtabNeeds('activity', sub.dataset.subtab, peerView)] };
  }
  const managed = managedControl(target, peerView);
  if (managed) return managed.own || managed.state === 'shared' ? null : { el: managed.el, state: managed.state, what: managed.what, needs: managed.needs };
  const act = actOf(target, event);
  if (act && SHELL_ACTS.has(act.dataset.act)) return shellControl(act, peerView);
  if (unrestricted(peerView)) {
    if (act && ATTACH_ACTS.has(act.dataset.act) && SAFE_AGENT_ID.test(act.dataset.agent || '')) return { el: act, state: 'attach', what: 'This terminal', agent: act.dataset.agent };
    return act && OWN_NODE_ACTS.has(act.dataset.act) ? { el: act, state: 'local', what: 'This action' } : null;
  }
  const held = grantHeld(act, peerView);
  // A control the peer's action routes take (PEER_ACTS: message one member)
  // opens the peer action dialog when its grant is not held.
  const pa = act && !held && peerAction(act, peerView);
  if (pa) return { el: act, state: 'peer-action', what: 'This action', request: pa };
  if (held !== null) {
    return held ? null : { el: act, state: 'omitted', what: GROUP_GRANT_ACTS[act.dataset.act] ? "this group's settings" : 'This action', needs: grantMissing(act, peerView) };
  }
  if (act && ATTACH_ACTS.has(act.dataset.act) && SAFE_AGENT_ID.test(act.dataset.agent || '')) return { el: act, state: 'attach', what: 'This terminal', agent: act.dataset.agent };
  if (act && !READ_ONLY_ACTS.has(act.dataset.act)) return { el: act, state: 'local', what: 'This action' };
  const ctl = target?.closest?.(GATED_CONTROLS.join(','));
  if (ctl) return { el: ctl, state: 'local', what: 'This action' };
  return null;
}

// reasonTitles puts why a greyed control is refused on it: its title (hover)
// and aria-disabled, so a keyboard or screen-reader user gets the same
// reason. It remembers the control's own title and aria-disabled, to prefix,
// refresh and restore them (re-reading the title when its component changed
// it since). sweep re-decides every marked control after what a node shares
// changed, and forgets ones no longer on the page. The peer page's limits
// and the fused view share it.
const TAB_CONTROLS = 'nav [data-tab], .activity-subtab';
// REASON_CONTROLS are the controls a reason may sit on.
const REASON_CONTROLS = ['[data-act]', TAB_CONTROLS, ...GATED_CONTROLS, ...Object.keys(MANAGED_CONTROLS)].join(', ');

export function reasonTitles() {
  const marked = new Map();
  const setReason = (el, reason, disabled = true) => {
    let mark = marked.get(el);
    if (!mark || el.getAttribute('title') !== mark.written) {
      mark = { own: el.getAttribute('title'), aria: mark ? mark.aria : el.getAttribute('aria-disabled') };
      marked.set(el, mark);
    }
    mark.written = `${reason}${mark.own ? `\n\n${mark.own}` : ''}`;
    el.setAttribute('title', mark.written);
    if (disabled) el.setAttribute('aria-disabled', 'true');
  };
  const clearReason = (el) => {
    const mark = marked.get(el);
    if (!mark) return;
    marked.delete(el);
    const own = el.getAttribute('title') === mark.written ? mark.own : el.getAttribute('title');
    if (own == null) el.removeAttribute('title'); else el.setAttribute('title', own);
    if (mark.aria == null) el.removeAttribute('aria-disabled'); else el.setAttribute('aria-disabled', mark.aria);
  };
  // reasonFor(el) is [reason, disabled] while el is refused, or null.
  const sweep = (reasonFor) => {
    for (const el of [...marked.keys()]) {
      if (!el.isConnected) { marked.delete(el); continue; }
      const why = reasonFor(el);
      if (why) setReason(el, why[0], why[1]); else clearReason(el);
    }
  };
  return { setReason, clearReason, sweep };
}

// installPeerViewLimits wires the gating for the page's lifetime. It is a
// no-op unless this page is a peer view.
const PEER_VIEW_STYLE_ID = 'peer-view-limits-style';
const PEER_VIEW_MANAGED_STYLE_ID = 'peer-view-managed-style';
const openRemote = (opts) => import('./remote-terminal.js').then((m) => m.openRemoteTerminal(opts));
const openRemoteShell = (opts) => import('./remote-terminal.js').then((m) => m.openRemoteShell(opts));

export function installPeerViewLimits({ doc = document, snapshot = dashboardState.snapshot, toast = shellToast, remote = globalThis.__tclaudeRemoteNode, openTerminal = openRemote, openShell = openRemoteShell } = {}) {
  if (!remote?.id) return () => {};
  const root = doc.documentElement;
  let peerView = snapshot.value?.peer_view || null;
  const nodeName = () => root.dataset.remoteNodeName || remote.id.slice(0, 13);

  const { setReason, clearReason, sweep } = reasonTitles();
  // A control's reason, as hover and focus give it ([reason, disabled]).
  const reasonOf = (el) => {
    const blocked = blockedControl(el, peerView);
    if (!blocked || blocked.state === 'peer-action' || blocked.el !== el) return null;
    // Tabs keep their title only: the palette still lists them, and says why
    // on use.
    return [limitHint(blocked.state, blocked.what, nodeName(), blocked.needs), !OPENS_HERE.has(blocked.state) && !el.matches(TAB_CONTROLS)];
  };

  // Tabs carry their state as a class so CSS greys them, with the reason as
  // their title; recomputed whenever a snapshot changes what the peer shares
  // (and so once the marker has named the node).
  const stop = effect(() => {
    peerView = snapshot.value?.peer_view || null;
    root.classList.toggle('pv-unrestricted', unrestricted(peerView));
    setLimitStyle(doc, PEER_VIEW_STYLE_ID, refusedActsRule('html.remote-node', usableActSelectors(peerView), `[data-act]:not(${FUSED_ROOT} [data-act])`));
    setLimitStyle(doc, PEER_VIEW_MANAGED_STYLE_ID, managedControlsRule(peerView));
    for (const tab of doc.querySelectorAll('nav [data-tab]')) {
      const state = tabState(tab.dataset.tab, peerView);
      tab.classList.toggle('pv-off', state !== 'shared');
      if (state === 'shared') clearReason(tab);
      else setReason(tab, limitHint(state, tabLabel(tab.dataset.tab), nodeName(), [tabNeeds(tab.dataset.tab, peerView)]), false);
    }
    for (const sub of doc.querySelectorAll('.activity-subtab')) {
      const state = subtabState('activity', sub.dataset.subtab, peerView);
      sub.classList.toggle('pv-off', state !== 'shared');
      if (state === 'shared') clearReason(sub);
      else setReason(sub, limitHint(state, subtabLabel(sub.dataset.subtab), nodeName(), [subtabNeeds('activity', sub.dataset.subtab, peerView)]), false);
    }
    // Controls hovered or focused before keep up with what the peer shares.
    sweep(reasonOf);
  });

  const refuse = (event) => {
    const blocked = blockedControl(event.target, peerView, event);
    if (!blocked) return;
    event.preventDefault();
    event.stopImmediatePropagation();
    if (blocked.state === 'peer-action') {
      doc.dispatchEvent(new CustomEvent(PEER_ACTION_EVENT, { detail: blocked.request }));
      return;
    }
    if (blocked.state === 'attach') {
      void openTerminal({ instance: remote.id, agent: blocked.agent, peerLabel: nodeName(), toast });
      return;
    }
    if (blocked.state === 'shell') {
      void openShell({ instance: remote.id, ...blocked.shell, peerLabel: nodeName(), toast });
      return;
    }
    toast(limitHint(blocked.state, blocked.what, nodeName(), blocked.needs), true);
  };
  // role=button chips open their editors on Enter/Space in their own keydown,
  // without a click; guard the keyboard path the same way.
  const onKey = (event) => {
    if (event.key !== 'Enter' && event.key !== ' ') return;
    if (event.target?.closest?.('input, textarea, select, [contenteditable="true"], [contenteditable=""]')) return;
    refuse(event);
  };
  // Hover or focus explanation for controls (tabs get theirs from the
  // effect).
  const onOver = (event) => {
    const blocked = blockedControl(event.target, peerView, event);
    if (blocked && blocked.state !== 'peer-action' && !blocked.el.matches('nav [data-tab], .activity-subtab')) setReason(blocked.el, limitHint(blocked.state, blocked.what, nodeName(), blocked.needs), !OPENS_HERE.has(blocked.state));
    // One the peer now shares drops a reason it showed before.
    else if (!blocked) { const el = event.target?.closest?.(REASON_CONTROLS); if (el) clearReason(el); }
  };
  // Drags spawn, move and reorder on the node: under unrestricted trust. A
  // restricted peer's agent row still drags where its grants cover a drop's
  // gesture (a Ctrl/Cmd clone, or retire on the bin; dnd-ops.js decides it).
  const onDrag = (event) => {
    if (unrestricted(peerView)) return;
    const agent = event.target?.closest?.('tr.dnd-draggable[data-dnd-agent]')?.getAttribute('data-dnd-agent') || '';
    if (agent && (agentGrantHeld('clone', agent, peerView) || agentGrantHeld('retire-agent', agent, peerView))) return;
    event.preventDefault();
  };
  doc.addEventListener('click', refuse, true);
  doc.addEventListener('keydown', onKey, true);
  doc.addEventListener('pointerover', onOver, true);
  doc.addEventListener('focusin', onOver, true);
  doc.addEventListener('dragstart', onDrag, true);
  return () => {
    stop();
    root.classList.remove('pv-unrestricted');
    setLimitStyle(doc, PEER_VIEW_STYLE_ID, '');
    setLimitStyle(doc, PEER_VIEW_MANAGED_STYLE_ID, '');
    doc.removeEventListener('click', refuse, true);
    doc.removeEventListener('keydown', onKey, true);
    doc.removeEventListener('pointerover', onOver, true);
    doc.removeEventListener('focusin', onOver, true);
    doc.removeEventListener('dragstart', onDrag, true);
  };
}
