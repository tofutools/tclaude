import { batch, computed, signal } from '@preact/signals';
import { dashPrefs } from './prefs.js';
import {
  virtualConversationsGroup,
  virtualPendingGroup,
  virtualReplacedGroup,
  virtualRetiredGroup,
  virtualUngroupedGroup,
} from './virtual-groups.js';
import { sortGroupsByPref } from './group-order.js';
import { scribeGroupVisible } from './scribe-groups.js';
import {
  hideableMemberCols,
  memberColDeviationCount,
  memberColHidden,
  setMemberColHidden,
} from './member-columns.js';
import { resetListOffsets } from './list-paging.js';
import { GROUPS_FOCUS_PREF, normalizeGroupsFocus, restoreGroupsFocus } from './groups-focus.js';
import { DEFAULT_RUN_FILTER, normalizeRunFilter } from './process-context.js';

// The run filter became a checklist (process-context.js RUN_KINDS). A fresh
// key, so a remembered single-select value never overrides the new default.
const PROCESS_FILTER_PREF = 'tclaude.dash.groups.processes.kinds';

const FILTER_KEY = 'tclaude.dash.filter.groups';

export const GROUP_VIEW_OPTIONS = Object.freeze([
  {
    key: 'offline', defaultValue: true, pref: 'tclaude.dash.offline.groups',
    label: 'show offline',
    wizardLabel: 'show slumbering',
    title: 'Show members whose tmux pane is offline. This is the tab-wide default — the ⚙ menu on each group has a per-group override.',
    wizardTitle: 'Show slumbering heroes. This is the tab-wide default — the ⚙ menu on each party has a per-party override.',
  },
  {
    key: 'ungrouped', defaultValue: true, pref: 'tclaude.dash.ungrouped.groups',
    label: 'show ungrouped',
    wizardLabel: 'show unbound',
    title: `Show the virtual "Ungrouped" group at the bottom — online agents that aren't in any group. Drag rows onto a group to add them; drag group members onto it to remove them.`,
    wizardTitle: `Show the ethereal "Unbound" party at the bottom — heroes not bound to any party. Drag them onto a party to bind them; drag party heroes back onto it to release them.`,
  },
  {
    key: 'retired', defaultValue: false, pref: 'tclaude.dash.retired.groups',
    label: 'show retired',
    wizardLabel: 'show banished',
    title: 'Show the virtual "Retired" group — agents that were demoted back to plain conversations. Click reinstate to bring one back.',
    wizardTitle: 'Show the ethereal "Banished" party — heroes returned to plain scrolls. Restore one to bring it back.',
  },
  {
    key: 'conversations', defaultValue: false, pref: 'tclaude.dash.conversations.groups',
    label: 'show conversations',
    wizardLabel: 'show plain scrolls',
    title: `Show the virtual "Conversations" group — recent conversations that aren't agents. Drag one onto a group (or click promote) to make it an agent.`,
    wizardTitle: `Show the ethereal "Plain scrolls" party — recent scrolls without heroes. Drag one onto a party (or click awaken) to awaken it as a hero.`,
  },
  {
    key: 'replaced', defaultValue: false, pref: 'tclaude.dash.replaced.groups',
    label: 'show replaced generations',
    wizardLabel: 'show past incarnations',
    title: 'Show the virtual "Replaced generations" group — superseded past generations of agents (left behind by reincarnate / /clear). Archival and read-mostly: copy a conv-id to inspect it, or delete a generation to prune it. The live agent is never affected.',
    wizardTitle: 'Show the ethereal "Past incarnations" party — superseded lives left behind by reincarnate / /clear. Copy a conv-id to scry one, or erase an incarnation to prune it. The living hero is never affected.',
  },
  {
    key: 'scribe', defaultValue: false, pref: 'tclaude.dash.scribe.groups',
    label: 'show offline scribes',
    wizardLabel: 'show slumbering scribes',
    title: 'Live scribe groups and their independently named agents are always shown. Enable this to also show dormant system groups left by offline scribes.',
    wizardTitle: 'Live scribe parties and their heroes are always shown. Enable this to also show dormant circles left by slumbering scribes.',
  },
]);

function pendingRowMatches(row, needle) {
  return ((row.label || '').toLowerCase().includes(needle)) ||
    ((row.name || '').toLowerCase().includes(needle)) ||
    ((row.role || '').toLowerCase().includes(needle)) ||
    ((row.descr || '').toLowerCase().includes(needle)) ||
    ((row.group || '').toLowerCase().includes(needle)) ||
    ((row.cwd || '').toLowerCase().includes(needle)) ||
    ((row.harness || '').toLowerCase().includes(needle));
}

export function filterGroups(groups, query) {
  if (!query) return groups;
  const needle = query.toLowerCase();
  const result = [];
  for (const group of groups) {
    const groupMatch = (group.name || '').toLowerCase().includes(needle) ||
      (group.descr || '').toLowerCase().includes(needle);
    const members = (group.members || []).filter((member) => {
      const state = member.state || {};
      return ((member.title || '').toLowerCase().includes(needle)) ||
        ((member.agent_id || '').toLowerCase().includes(needle)) ||
        ((member.conv_id || '').toLowerCase().includes(needle)) ||
        ((member.role || '').toLowerCase().includes(needle)) ||
        ((member.descr || '').toLowerCase().includes(needle)) ||
        ((member.branch || '').toLowerCase().includes(needle)) ||
        ((member.startup_branch || '').toLowerCase().includes(needle)) ||
        ((state.cwd || '').toLowerCase().includes(needle)) ||
        ((member.startup_dir || '').toLowerCase().includes(needle)) ||
        ((member.current_dir || '').toLowerCase().includes(needle)) ||
        ((member.actor_title || '').toLowerCase().includes(needle)) ||
        ((member.actor_conv_id || '').toLowerCase().includes(needle)) ||
        ((member.reason || '').toLowerCase().includes(needle));
    });
    const pending = (group.pending || []).filter((row) => pendingRowMatches(row, needle));
    if (groupMatch) result.push(group);
    else if (members.length || pending.length) result.push({ ...group, members, pending });
  }
  return result;
}

export function distributePendingToGroups(groups, pendingRows) {
  const byGroup = new Map();
  const fallback = [];
  for (const row of pendingRows || []) {
    const groupName = (row.group || '').trim();
    if (!groupName) {
      fallback.push(row);
      continue;
    }
    const rows = byGroup.get(groupName) || [];
    rows.push(row);
    byGroup.set(groupName, rows);
  }
  const withPending = groups.map((group) => {
    const rows = byGroup.get(group.name);
    if (!rows?.length) return group;
    byGroup.delete(group.name);
    return { ...group, pending: rows };
  });
  for (const rows of byGroup.values()) fallback.push(...rows);
  return { groups: withPending, fallback };
}

// VIRTUAL_KINDS are the virtual groups the view menu shows, in list order.
const VIRTUAL_KINDS = [
  ['ungrouped', (s) => virtualUngroupedGroup(s.ungrouped || [])],
  ['retired', (s) => virtualRetiredGroup(s.retired || [], s.paging?.retired)],
  ['conversations', (s) => virtualConversationsGroup(s.conversations || [], s.paging?.conversations)],
  ['replaced', (s) => virtualReplacedGroup(s.replaced || [], s.paging?.replaced)],
];

// nodeVirtualGroup names one node's virtual group in the fused view, as its
// real groups are (group@node); its rows name their node, so their controls
// reach it. The page's node's lists (this node's, or the ?node= peer's) come
// with the page's snapshot and page through list-paging.js; another peer's
// page when its lists came through usePeerLists (their paging names the
// node), and otherwise show their first page.
function nodeVirtualGroup(group, node) {
  return {
    ...group,
    name: `${group.name}@${node.name}`,
    key: `${node.id}/${group.key}`,
    fleet_node: node,
    members: (group.members || []).map((m) => ({ ...m, fleet_node: node })),
    ...(node.page || group.paging?.node === node.id ? {} : { paging: null }),
  };
}

export function buildGroupsView(snapshot, options, query, reorder = sortGroupsByPref) {
  if (!snapshot) return { groups: [], total: 0, shownReal: 0, query: query || '' };
  const realGroups = (snapshot.groups || [])
    .filter((group) => scribeGroupVisible(group, options.scribe));
  const distributed = distributePendingToGroups(realGroups, snapshot.pending || []);
  const list = reorder(distributed.groups.slice());
  if (distributed.fallback.length) list.unshift(virtualPendingGroup(distributed.fallback));
  // The fused Groups view lists each node's virtual groups (virtual_nodes,
  // fleet-merged-model.js) under their group@node names; one node's view
  // lists its own.
  const sources = snapshot.virtual_nodes || [{ node: null, lists: snapshot }];
  for (const [key, make] of VIRTUAL_KINDS) {
    if (!options[key]) continue;
    for (const { node, lists } of sources) list.push(node ? nodeVirtualGroup(make(lists), node) : make(lists));
  }
  const groups = filterGroups(list, query || '');
  return {
    groups,
    total: realGroups.length,
    shownReal: groups.filter((group) => !group.virtual).length,
    query: query || '',
  };
}

export function createGroupsState({
  prefs = dashPrefs,
  resetOffsets = resetListOffsets,
  columns = {
    list: hideableMemberCols,
    hidden: memberColHidden,
    setHidden: setMemberColHidden,
    deviationCount: memberColDeviationCount,
  },
  reorder = sortGroupsByPref,
} = {}) {
  const snapshot = signal(null);
  const query = signal('');
  const visibility = signal(Object.fromEntries(
    GROUP_VIEW_OPTIONS.map((option) => [option.key, option.defaultValue]),
  ));
  const viewOpen = signal(false);
  const memberEditor = signal(null);
  const addMemberDialog = signal(null);
  const standingOrdersDialog = signal(null);
  const subview = signal('members');
  const routeSelection = signal('');
  // The group the Route map shows ('' = the route's, else the first).
  const routeGroup = signal('');
  // The Route map's view: 'graph' or the exact 'list' (?view=list).
  const routeView = signal('graph');
  // Both | Agents | Processes lens (groups-focus.js); restored from prefs in initialize().
  const focus = signal('both');
  // The ticked run kinds (running / planned / done) over the Processes
  // lens's run cards, as a canonical "running,planned" string.
  const processFilter = signal(DEFAULT_RUN_FILTER);
  // The single-run focus (G5): { group, runId } while one run is open in the
  // Processes lens, else null. Session-only: a reload lands on the cards.
  const focusedRun = signal(null);
  const renderRevision = signal(0);
  // listed is the view another list shows with these controls -- the fused
  // Groups view's {groups, total, shownReal} while it is on screen, else null
  // -- so the toolbar counts what is listed.
  const listed = signal(null);
  let initialized = false;
  let nextMemberEditorLaunchID = 0;
  let nextAddMemberLaunchID = 0;
  let nextStandingOrdersLaunchID = 0;

  const view = computed(() => {
    renderRevision.value;
    return buildGroupsView(snapshot.value, visibility.value, query.value, reorder);
  });
  const columnOptions = computed(() => {
    renderRevision.value;
    return columns.list().map((column) => ({ ...column, shown: !columns.hidden(column.key) }));
  });
  const deviationCount = computed(() => {
    renderRevision.value;
    let count = columns.deviationCount();
    for (const option of GROUP_VIEW_OPTIONS) {
      if (visibility.value[option.key] !== option.defaultValue) count++;
    }
    return count;
  });

  function initialize() {
    if (initialized) return false;
    initialized = true;
    const restored = {};
    for (const option of GROUP_VIEW_OPTIONS) {
      const saved = prefs.getItem(option.pref);
      restored[option.key] = saved === null ? option.defaultValue : saved === '1';
    }
    batch(() => {
      query.value = prefs.getItem(FILTER_KEY) || '';
      focus.value = restoreGroupsFocus(prefs);
      processFilter.value = normalizeRunFilter(prefs.getItem(PROCESS_FILTER_PREF));
      visibility.value = restored;
      renderRevision.value++;
    });
    return true;
  }

  function publish(value) {
    // Optimistic legacy actions sometimes mutate lastSnapshot in place. A
    // shallow copy guarantees Signals publishes that authoritative update.
    snapshot.value = value ? { ...value } : null;
  }

  function setQuery(value) {
    const next = String(value ?? '');
    query.value = next;
    if (next) prefs.setItem(FILTER_KEY, next);
    else prefs.removeItem(FILTER_KEY);
    resetOffsets();
  }

  function setVisible(key, shown) {
    const option = GROUP_VIEW_OPTIONS.find((candidate) => candidate.key === key);
    if (!option) return false;
    visibility.value = { ...visibility.value, [key]: !!shown };
    prefs.setItem(option.pref, shown ? '1' : '0');
    return true;
  }

  function setColumnShown(key, shown) {
    columns.setHidden(key, !shown);
    renderRevision.value++;
  }

  function rerender() {
    renderRevision.value++;
  }

  function setSubview(value, selection = '') {
    const next = value === 'routes' ? 'routes' : 'members';
    subview.value = next;
    routeSelection.value = next === 'routes' ? String(selection || '') : '';
  }

  function setRouteGroup(value) {
    routeGroup.value = String(value || '');
  }

  function setRouteView(value) {
    routeView.value = value === 'list' ? 'list' : 'graph';
  }

  function setFocus(value) {
    const next = normalizeGroupsFocus(value);
    if (focus.value === next) return;
    focus.value = next;
    prefs.setItem(GROUPS_FOCUS_PREF, next);
  }

  function setFocusedRun(value) {
    const next = value?.group && value?.runId ? { group: String(value.group), runId: String(value.runId) } : null;
    const prev = focusedRun.value;
    if (prev?.group === next?.group && prev?.runId === next?.runId) return;
    focusedRun.value = next;
  }

  function setProcessFilter(value) {
    const next = normalizeRunFilter(value);
    if (processFilter.value === next) return;
    processFilter.value = next;
    prefs.setItem(PROCESS_FILTER_PREF, next);
  }

  // node names another node's agent (the fused Groups view); blank is the
  // page's. The editor's requests go there.
  function openMemberEditor(member, group, focus = 'title', node = '') {
    // A live editor owns its frozen baseline until it closes. Poll publishes
    // may replace the row objects underneath it, but cannot retarget or reset
    // the draft; repeated launch gestures are ignored for the same reason.
    if (memberEditor.value || !member || !group?.name) return false;
    memberEditor.value = {
      launchID: ++nextMemberEditorLaunchID,
      conv: String(member.conv_id || ''),
      agent: String(member.agent_id || member.conv_id || ''),
      label: String(member.title || member.conv_id || ''),
      group: String(group.name),
      title: String(member.title || ''),
      role: String(member.role || ''),
      descr: String(member.descr || ''),
      tags: Array.isArray(member.tags) ? [...member.tags] : [],
      owner: !!member.owner,
      focus: ['role', 'descr'].includes(focus) ? focus : 'title',
      // Rename titles auto-correct under the same toggle as spawn names.
      normalizeTitles: snapshot.value?.spawn_name_normalize !== false,
      ...(node ? { node: String(node) } : {}),
    };
    return true;
  }

  function closeMemberEditor() {
    memberEditor.value = null;
  }

  // node names another node's group (blank: the page's).
  function openAddMember(group, node = '') {
    // The dialog owns its search/toggle/highlight transaction until it closes.
    // Poll publishes update the candidate source without retargeting the group
    // or remounting that local state.
    if (addMemberDialog.value || !group?.name) return false;
    addMemberDialog.value = {
      launchID: ++nextAddMemberLaunchID,
      group: String(group.name),
      node: String(node || ''),
    };
    return true;
  }

  function closeAddMember() {
    addMemberDialog.value = null;
  }

  // node names another node's group (the fused Groups view; blank: the
  // page's): the dialog's requests go there.
  function openStandingOrders(group, node = '') {
    if (standingOrdersDialog.value || !group?.name || group.virtual) return false;
    standingOrdersDialog.value = {
      launchID: ++nextStandingOrdersLaunchID,
      group: String(group.name),
      node: String(node || ''),
    };
    return true;
  }

  function closeStandingOrders() {
    standingOrdersDialog.value = null;
  }

  function optimisticAddMember(groupName, candidate) {
    const current = snapshot.value;
    const conv = String(candidate?.conv_id || '');
    if (!current || !conv) return false;
    let added = false;
    const groups = (current.groups || []).map((group) => {
      if (group.name !== groupName) return group;
      const members = group.members || [];
      if (members.some((member) => member.conv_id === conv)) return group;
      added = true;
      const member = {
        ...candidate,
        conv_id: conv,
        // Plain conversations do not have a stable agent id until the server
        // promotes them. Keep that field empty so interim row actions use the
        // same conv-id fallback the membership request used.
        agent_id: candidate.agent_id || '',
        role: '',
        descr: '',
      };
      return {
        ...group,
        members: [...members, member],
        online: typeof group.online === 'number' && candidate.online
          ? group.online + 1
          : group.online,
      };
    });
    if (!added) return false;
    snapshot.value = {
      ...current,
      groups,
      ungrouped: (current.ungrouped || []).filter((row) => row.conv_id !== conv),
      conversations: (current.conversations || []).filter((row) => row.conv_id !== conv),
      agents: (current.agents || []).map((row) => row.conv_id === conv
        ? { ...row, groups: [...new Set([...(row.groups || []), groupName])] }
        : row),
    };
    return true;
  }

  return Object.freeze({
    snapshot, query, visibility, viewOpen, memberEditor, addMemberDialog,
    standingOrdersDialog, subview, routeSelection, routeGroup, setRouteGroup, routeView, setRouteView, focus, processFilter, focusedRun, renderRevision, listed,
    view, columnOptions, deviationCount,
    initialize, publish, setQuery, setVisible, setColumnShown, rerender, setSubview, setFocus, setProcessFilter, setFocusedRun,
    openMemberEditor, closeMemberEditor,
    openAddMember, closeAddMember, optimisticAddMember,
    openStandingOrders, closeStandingOrders,
  });
}

export const groupsState = createGroupsState();
