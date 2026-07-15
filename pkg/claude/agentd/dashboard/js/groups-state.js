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
    wizardLabel: 'show slumbering',
    title: 'Show members whose tmux pane is offline. This is the tab-wide default — each group header has a per-group override.',
    wizardTitle: 'Show slumbering familiars. This is the tab-wide default — each party banner has a per-party override.',
  },
  {
    key: 'ungrouped', defaultValue: true, pref: 'tclaude.dash.ungrouped.groups',
    label: 'show ungrouped',
    wizardLabel: 'show unbound',
    title: `Show the virtual "Ungrouped" group at the bottom — online agents that aren't in any group. Drag rows onto a group to add them; drag group members onto it to remove them.`,
    wizardTitle: `Show the ethereal "Unbound" party at the bottom — familiars not bound to any party. Drag them onto a party to bind them; drag party familiars back onto it to release them.`,
  },
  {
    key: 'retired', defaultValue: false, pref: 'tclaude.dash.retired.groups',
    label: 'show retired',
    wizardLabel: 'show banished',
    title: 'Show the virtual "Retired" group — agents that were demoted back to plain conversations. Click reinstate to bring one back.',
    wizardTitle: 'Show the ethereal "Banished" party — familiars returned to plain scrolls. Restore one to bring it back.',
  },
  {
    key: 'conversations', defaultValue: false, pref: 'tclaude.dash.conversations.groups',
    label: 'show conversations',
    wizardLabel: 'show plain scrolls',
    title: `Show the virtual "Conversations" group — recent conversations that aren't agents. Drag one onto a group (or click promote) to make it an agent.`,
    wizardTitle: `Show the ethereal "Plain scrolls" party — recent scrolls without familiars. Drag one onto a party (or click awaken) to awaken it as a familiar.`,
  },
  {
    key: 'replaced', defaultValue: false, pref: 'tclaude.dash.replaced.groups',
    label: 'show replaced generations',
    wizardLabel: 'show past incarnations',
    title: 'Show the virtual "Replaced generations" group — superseded past generations of agents (left behind by reincarnate / /clear). Archival and read-mostly: copy a conv-id to inspect it, or delete a generation to prune it. The live agent is never affected.',
    wizardTitle: 'Show the ethereal "Past incarnations" party — superseded lives left behind by reincarnate / /clear. Copy a conv-id to scry one, or erase an incarnation to prune it. The living familiar is never affected.',
  },
  {
    key: 'scribe', defaultValue: false, pref: 'tclaude.dash.scribe.groups',
    label: 'show offline scribes',
    wizardLabel: 'show slumbering scribes',
    title: 'Live scribe groups and their independently named agents are always shown. Enable this to also show dormant system groups left by offline scribes.',
    wizardTitle: 'Live scribe parties and their familiars are always shown. Enable this to also show dormant circles left by slumbering scribes.',
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
  const memberEditor = signal(null);
  const renderRevision = signal(0);
  let initialized = false;
  let nextMemberEditorLaunchID = 0;

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

  function openMemberEditor(member, group, focus = 'title') {
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
    };
    return true;
  }

  function closeMemberEditor() {
    memberEditor.value = null;
  }

  return Object.freeze({
    snapshot, query, visibility, viewOpen, memberEditor, renderRevision,
    view, columnOptions, deviationCount,
    initialize, publish, setQuery, setVisible, setColumnShown, rerender,
    openMemberEditor, closeMemberEditor,
  });
}

export const groupsState = createGroupsState();
