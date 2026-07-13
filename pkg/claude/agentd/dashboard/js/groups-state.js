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

const FILTER_KEY = 'tclaude.dash.filter.groups';

export const GROUP_VIEW_OPTIONS = Object.freeze([
  {
    key: 'offline', defaultValue: true, pref: 'tclaude.dash.offline.groups',
    label: 'show offline',
    title: 'Show members whose tmux pane is offline. This is the tab-wide default — each group header has a per-group override.',
  },
  {
    key: 'ungrouped', defaultValue: true, pref: 'tclaude.dash.ungrouped.groups',
    label: 'show ungrouped',
    title: `Show the virtual "Ungrouped" group at the bottom — online agents that aren't in any group. Drag rows onto a group to add them; drag group members onto it to remove them.`,
  },
  {
    key: 'retired', defaultValue: true, pref: 'tclaude.dash.retired.groups',
    label: 'show retired',
    title: 'Show the virtual "Retired" group — agents that were demoted back to plain conversations. Retired agents land here instead of vanishing off the tab; click reinstate to bring one back.',
  },
  {
    key: 'conversations', defaultValue: false, pref: 'tclaude.dash.conversations.groups',
    label: 'show conversations',
    title: `Show the virtual "Conversations" group — recent conversations that aren't agents. Drag one onto a group (or click promote) to make it an agent.`,
  },
  {
    key: 'replaced', defaultValue: false, pref: 'tclaude.dash.replaced.groups',
    label: 'show replaced generations',
    title: 'Show the virtual "Replaced generations" group — superseded past generations of agents (left behind by reincarnate / /clear). Archival and read-mostly: copy a conv-id to inspect it, or delete a generation to prune it. The live agent is never affected.',
  },
  {
    key: 'scribe', defaultValue: false, pref: 'tclaude.dash.scribe.groups',
    label: 'show offline scribes',
    title: 'Live scribe groups and their independently named agents are always shown. Enable this to also show dormant system groups left by offline scribes.',
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

export function buildGroupsView(snapshot, options, query, reorder = sortGroupsByPref) {
  if (!snapshot) return { groups: [], total: 0, shownReal: 0, query: query || '' };
  const realGroups = (snapshot.groups || [])
    .filter((group) => scribeGroupVisible(group, options.scribe));
  const distributed = distributePendingToGroups(realGroups, snapshot.pending || []);
  const list = reorder(distributed.groups.slice());
  if (distributed.fallback.length) list.unshift(virtualPendingGroup(distributed.fallback));
  if (options.ungrouped) list.push(virtualUngroupedGroup(snapshot.ungrouped || []));
  if (options.retired) {
    list.push(virtualRetiredGroup(snapshot.retired || [], snapshot.paging?.retired));
  }
  if (options.conversations) {
    list.push(virtualConversationsGroup(
      snapshot.conversations || [], snapshot.paging?.conversations,
    ));
  }
  if (options.replaced) {
    list.push(virtualReplacedGroup(snapshot.replaced || [], snapshot.paging?.replaced));
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
  const renderRevision = signal(0);
  let initialized = false;

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

  return Object.freeze({
    snapshot, query, visibility, viewOpen, renderRevision,
    view, columnOptions, deviationCount,
    initialize, publish, setQuery, setVisible, setColumnShown, rerender,
  });
}

export const groupsState = createGroupsState();
