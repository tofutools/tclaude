import { computed, signal } from '@preact/signals';
import { dashPrefs } from './prefs.js';
import { normalizeSeed } from './terminals-core.js';

export const TERMINAL_PANE_ORDER_KEY = 'tclaude.dash.terminals.order';
export const TERMINAL_TAB_GROUP_KEY = 'tclaude.dash.terminals.groups';
export const MAX_REMEMBERED_TERMINAL_PANES = 512;
export const MAX_TERMINAL_PANE_ORDER_BYTES = 60 * 1024;
export const MAX_TERMINAL_TAB_GROUPS = 24;
export const MAX_TERMINAL_GROUP_NAME_LENGTH = 40;
export const MAX_TERMINAL_TAB_GROUP_BYTES = 60 * 1024;

// Named palette slots rather than raw colours: mux.css owns the actual values
// for both the ordinary and the wizard theme, so a stored group keeps its
// identity when either palette is retuned.
export const TERMINAL_GROUP_COLORS = Object.freeze([
  'blue', 'purple', 'green', 'amber', 'red', 'teal', 'pink', 'slate',
]);

function boundPreferredOrder(keys) {
  const bounded = [];
  const encoder = new TextEncoder();
  let bytes = 2; // JSON array brackets.
  for (const key of keys) {
    if (bounded.length >= MAX_REMEMBERED_TERMINAL_PANES) break;
    const entryBytes = encoder.encode(JSON.stringify(key)).byteLength + (bounded.length ? 1 : 0);
    if (bytes + entryBytes > MAX_TERMINAL_PANE_ORDER_BYTES) continue;
    bounded.push(key);
    bytes += entryBytes;
  }
  return bounded;
}

// Group names are operator text rendered into the tab strip and into the
// accessible announcements, so they are normalized the same way on the way in
// from a dialog and on the way back off persistence: no control characters, no
// runaway length, never empty.
export function sanitizeGroupName(name, fallback = 'group') {
  const cleaned = String(name ?? '')
    .replace(/[\u0000-\u001f\u007f]+/g, ' ')
    .replace(/\s+/g, ' ')
    .trim()
    .slice(0, MAX_TERMINAL_GROUP_NAME_LENGTH);
  return cleaned || fallback;
}

function normalizeColor(color, index = 0) {
  return TERMINAL_GROUP_COLORS.includes(color)
    ? color
    : TERMINAL_GROUP_COLORS[index % TERMINAL_GROUP_COLORS.length];
}

// segmentsFor turns the flat pane order into what the strip actually renders:
// a sequence of standalone tabs and group stacks. It relies on the contiguity
// invariant that normalizeGrouping maintains, but tolerates a violated one by
// folding stray members into the stack's first appearance rather than emitting
// the same group twice.
export function segmentsFor(panes, groupIdOf, groupById) {
  const segments = [];
  const emitted = new Set();
  for (const pane of panes) {
    const group = groupById.get(groupIdOf(pane.key));
    if (!group) {
      segments.push(Object.freeze({ type: 'pane', key: pane.key, pane, panes: [pane] }));
      continue;
    }
    if (emitted.has(group.id)) continue;
    emitted.add(group.id);
    segments.push(Object.freeze({
      type: 'group',
      key: `group:${group.id}`,
      group,
      panes: Object.freeze(panes.filter((candidate) => groupIdOf(candidate.key) === group.id)),
    }));
  }
  return Object.freeze(segments);
}

// normalizeGrouping pulls every member of a group next to the group's first
// member without otherwise disturbing the operator's order. Every mutation
// funnels through it, so "a group is a contiguous run of tabs" holds for
// rendering, drag geometry and keyboard movement alike.
export function normalizeGrouping(panes, groupIdOf) {
  const out = [];
  const emitted = new Set();
  for (const pane of panes) {
    if (emitted.has(pane.key)) continue;
    const groupID = groupIdOf(pane.key);
    if (!groupID) {
      out.push(pane);
      emitted.add(pane.key);
      continue;
    }
    for (const member of panes) {
      if (emitted.has(member.key) || groupIdOf(member.key) !== groupID) continue;
      out.push(member);
      emitted.add(member.key);
    }
  }
  return out;
}

export function terminalSeedKey(seed) {
  return seed.key || seed.ws;
}

export function createTerminalShellState({ prefs = dashPrefs, persistOrder = true } = {}) {
  const panes = signal([]);
  const activeKey = signal(null);
  const modal = signal(null);
  const revealRequest = signal(0);
  // Group descriptors and pane→group membership are two signals because they
  // change independently: renaming or collapsing a stack touches no pane, and
  // moving a tab between stacks touches no descriptor.
  const groups = signal([]);
  const membership = signal(new Map());
  const groupIndex = computed(() => new Map(groups.value.map((group) => [group.id, group])));
  const segments = computed(() => segmentsFor(
    panes.value,
    (key) => membership.value.get(key) || null,
    groupIndex.value,
  ));
  const view = computed(() => ({
    panes: panes.value,
    activeKey: activeKey.value,
    modal: modal.value,
    count: panes.value.length,
    revealRequest: revealRequest.value,
    groups: groups.value,
    segments: segments.value,
  }));
  let paneSequence = 0;
  let modalSequence = 0;
  let groupSequence = 0;
  let preferredOrder = null;
  let groupsLoaded = false;

  function readPreferredOrder() {
    if (preferredOrder) return preferredOrder;
    try {
      const parsed = JSON.parse(prefs.getItem(TERMINAL_PANE_ORDER_KEY) || '[]');
      preferredOrder = Array.isArray(parsed)
        ? boundPreferredOrder([...new Set(parsed.filter((key) => typeof key === 'string' && key))])
        : [];
    } catch (_) {
      preferredOrder = [];
    }
    return preferredOrder;
  }

  function persistPreferredOrder(visibleKeys = panes.value.map((pane) => pane.key)) {
    const visible = new Set(visibleKeys);
    preferredOrder = boundPreferredOrder([
      ...visibleKeys,
      ...readPreferredOrder().filter((key) => !visible.has(key)),
    ]);
    if (!persistOrder) return;
    try { prefs.setItem(TERMINAL_PANE_ORDER_KEY, JSON.stringify(preferredOrder)); } catch (_) {}
  }

  // Membership is remembered for keys that are not currently open, exactly like
  // the pane order is: closing every tab of a stack and reopening one later
  // restores it to its stack instead of dropping it into the ungrouped run.
  function loadGroups() {
    if (groupsLoaded) return;
    groupsLoaded = true;
    let parsed = null;
    try { parsed = JSON.parse(prefs.getItem(TERMINAL_TAB_GROUP_KEY) || 'null'); } catch (_) { parsed = null; }
    if (!parsed || typeof parsed !== 'object') return;
    const loaded = [];
    const ids = new Set();
    for (const raw of Array.isArray(parsed.groups) ? parsed.groups : []) {
      if (loaded.length >= MAX_TERMINAL_TAB_GROUPS) break;
      const id = typeof raw?.id === 'string' && raw.id && !ids.has(raw.id) ? raw.id : null;
      if (!id) continue;
      ids.add(id);
      loaded.push(Object.freeze({
        id,
        name: sanitizeGroupName(raw.name),
        color: normalizeColor(raw.color, loaded.length),
        collapsed: raw.collapsed === true,
      }));
      const suffix = Number.parseInt(/^group-(\d+)$/.exec(id)?.[1] ?? '', 10);
      if (Number.isInteger(suffix) && suffix > groupSequence) groupSequence = suffix;
    }
    const members = new Map();
    const source = parsed.members && typeof parsed.members === 'object' ? parsed.members : {};
    for (const [key, groupID] of Object.entries(source)) {
      if (members.size >= MAX_REMEMBERED_TERMINAL_PANES) break;
      if (typeof key === 'string' && key && ids.has(groupID)) members.set(key, groupID);
    }
    groups.value = loaded;
    membership.value = members;
  }

  function persistGroups() {
    if (!persistOrder) return;
    // Bound the same way the order pref is: prefer membership for tabs that are
    // open right now, then the remembered tail, and stop before the byte cap.
    const openKeys = new Set(panes.value.map((pane) => pane.key));
    const ordered = [...membership.value.entries()]
      .sort((a, b) => Number(openKeys.has(b[0])) - Number(openKeys.has(a[0])));
    const members = {};
    const encoder = new TextEncoder();
    let bytes = encoder.encode(JSON.stringify({ groups: groups.value, members: {} })).byteLength;
    for (const [key, groupID] of ordered) {
      const entryBytes = encoder.encode(JSON.stringify(key) + JSON.stringify(groupID)).byteLength + 2;
      if (bytes + entryBytes > MAX_TERMINAL_TAB_GROUP_BYTES) break;
      members[key] = groupID;
      bytes += entryBytes;
    }
    try {
      prefs.setItem(TERMINAL_TAB_GROUP_KEY, JSON.stringify({ groups: groups.value, members }));
    } catch (_) {}
  }

  function groupIDFor(key) {
    loadGroups();
    const groupID = membership.value.get(key) || null;
    return groupID && groupIndex.value.has(groupID) ? groupID : null;
  }

  function groupFor(key) {
    const groupID = groupIDFor(key);
    return groupID ? groupIndex.value.get(groupID) : null;
  }

  function sortByPreferredOrder(items) {
    const rank = new Map(readPreferredOrder().map((key, index) => [key, index]));
    return items
      .map((pane, index) => ({ pane, index }))
      .sort((a, b) => (rank.get(a.pane.key) ?? Number.MAX_SAFE_INTEGER)
        - (rank.get(b.pane.key) ?? Number.MAX_SAFE_INTEGER) || a.index - b.index)
      .map(({ pane }) => pane);
  }

  function commitPaneOrder(next) {
    const normalized = normalizeGrouping(next, groupIDFor);
    if (normalized.every((pane, index) => panes.value[index] === pane)
      && normalized.length === panes.value.length) return null;
    panes.value = normalized;
    persistPreferredOrder();
    return Object.freeze({
      pane: normalized.find((pane) => pane.key === activeKey.value) || null,
      panes: normalized,
    });
  }

  // setMembership is the single write path for pane→group edges. It drops the
  // entry entirely for the ungrouped case so a stack that is later dissolved
  // does not leave dead keys behind in the persisted map.
  function setMembership(assignments) {
    const next = new Map(membership.value);
    let changed = false;
    for (const [key, groupID] of assignments) {
      const current = next.get(key) || null;
      const wanted = groupID && groupIndex.value.has(groupID) ? groupID : null;
      if (current === wanted) continue;
      changed = true;
      if (wanted) next.set(key, wanted);
      else next.delete(key);
    }
    if (!changed) return false;
    membership.value = next;
    // A stack whose LAST member left is gone — there is nothing to render, drop
    // onto, or name. Closing every tab of a stack is not that case: membership
    // is remembered for closed keys, so reopening one restores its stack.
    const alive = new Set(next.values());
    if (groups.value.some((group) => !alive.has(group.id))) {
      groups.value = groups.value.filter((group) => alive.has(group.id));
    }
    return true;
  }

  function commitGrouping(assignments, { order = null } = {}) {
    const changed = setMembership(assignments);
    const next = order || panes.value;
    const normalized = normalizeGrouping(next, groupIDFor);
    const orderChanged = normalized.length !== panes.value.length
      || !normalized.every((pane, index) => panes.value[index] === pane);
    if (orderChanged) {
      panes.value = normalized;
      persistPreferredOrder();
    }
    if (changed || orderChanged) persistGroups();
    return changed || orderChanged;
  }

  function requestReveal() {
    revealRequest.value += 1;
  }

  function openPane(raw, { reveal = true } = {}) {
    const seed = normalizeSeed(raw);
    if (!seed) return null;
    loadGroups();
    const key = terminalSeedKey(seed);
    const existing = panes.value.find((pane) => pane.key === key);
    if (existing) {
      activeKey.value = key;
      if (reveal) requestReveal();
      return existing;
    }
    paneSequence += 1;
    const pane = Object.freeze({
      id: `terminal-pane-${paneSequence}`,
      key,
      label: seed.label || 'terminal',
      seed: Object.freeze({ ...seed }),
    });
    const preferred = readPreferredOrder();
    if (!preferred.includes(key)) preferred.push(key);
    panes.value = normalizeGrouping(sortByPreferredOrder([...panes.value, pane]), groupIDFor);
    activeKey.value = key;
    // A pane that lands in a collapsed stack must be visible to be usable, so
    // the stack opens rather than the activation being silently invisible.
    expandGroupFor(key);
    if (reveal) requestReveal();
    return pane;
  }

  function activatePane(key, { reveal = true } = {}) {
    if (!panes.value.some((pane) => pane.key === key)) return false;
    activeKey.value = key;
    expandGroupFor(key);
    if (reveal) requestReveal();
    return true;
  }

  function removePanes(keys) {
    const current = panes.value;
    const wanted = new Set(keys || []);
    const removed = current.filter((candidate) => wanted.has(candidate.key));
    if (!removed.length) return [];
    const next = current.filter((candidate) => !wanted.has(candidate.key));
    const previousActive = activeKey.value;
    panes.value = next;
    if (!next.some((candidate) => candidate.key === previousActive)) {
      const previousIndex = current.findIndex((candidate) => candidate.key === previousActive);
      const successor = previousIndex < 0 ? null : current.slice(previousIndex + 1)
        .find((candidate) => !wanted.has(candidate.key));
      const predecessor = previousIndex < 0 ? null : current.slice(0, previousIndex).reverse()
        .find((candidate) => !wanted.has(candidate.key));
      activeKey.value = successor?.key || predecessor?.key || next[0]?.key || null;
      if (activeKey.value) expandGroupFor(activeKey.value);
    }
    return removed;
  }

  function removePane(key) {
    return removePanes([key])[0] || null;
  }

  function movePane(key, toIndex) {
    const current = panes.value;
    const fromIndex = current.findIndex((pane) => pane.key === key);
    if (fromIndex < 0 || current.length < 2 || !Number.isInteger(toIndex)) return null;
    const destination = Math.max(0, Math.min(current.length - 1, toIndex));
    if (destination === fromIndex) return null;
    const next = [...current];
    const [pane] = next.splice(fromIndex, 1);
    next.splice(destination, 0, pane);
    if (!commitPaneOrder(next)) return null;
    return Object.freeze({
      pane,
      index: panes.value.findIndex((candidate) => candidate.key === key),
      count: panes.value.length,
      group: groupFor(key),
    });
  }

  // reorderPane is the drop path: landing a tab on another tab adopts that
  // tab's stack membership. That is the whole "drag into / out of a group"
  // gesture — dropping between two ungrouped tabs leaves the stack, dropping
  // between two members joins it — with no separate drop zone to discover.
  function reorderPane(key, targetKey, { after = false } = {}) {
    if (key === targetKey) return null;
    loadGroups();
    const current = panes.value;
    const pane = current.find((candidate) => candidate.key === key);
    if (!pane || !current.some((candidate) => candidate.key === targetKey)) return null;
    const next = current.filter((candidate) => candidate.key !== key);
    const targetIndex = next.findIndex((candidate) => candidate.key === targetKey);
    const destination = targetIndex + (after ? 1 : 0);
    next.splice(destination, 0, pane);
    const targetGroup = groupIDFor(targetKey);
    if (!commitGrouping([[key, targetGroup]], { order: next })) return null;
    return Object.freeze({
      pane,
      index: panes.value.findIndex((candidate) => candidate.key === key),
      count: panes.value.length,
      group: groupFor(key),
    });
  }

  // movePaneByOffset is the keyboard mirror of the drag gesture and moves in
  // strip terms, not raw array terms. Inside a stack it steps between siblings
  // and then steps OUT of the stack at either edge; outside one it hops whole
  // stacks rather than tunnelling through them, which would otherwise be
  // undone by the contiguity normalization on the very next keypress.
  function movePaneByOffset(key, offset) {
    loadGroups();
    if (!Number.isInteger(offset) || offset === 0) return null;
    const direction = offset > 0 ? 1 : -1;
    const steps = Math.abs(offset);
    let moved = null;
    for (let step = 0; step < steps; step += 1) {
      const outcome = stepPane(key, direction);
      if (!outcome) break;
      moved = outcome;
    }
    return moved;
  }

  function stepPane(key, direction) {
    const current = panes.value;
    const pane = current.find((candidate) => candidate.key === key);
    if (!pane || current.length < 2) return null;
    const groupID = groupIDFor(key);
    if (groupID) {
      const members = current.filter((candidate) => groupIDFor(candidate.key) === groupID);
      const index = members.findIndex((candidate) => candidate.key === key);
      const destination = index + direction;
      if (destination >= 0 && destination < members.length) {
        return reorderPane(key, members[destination].key, { after: direction > 0 });
      }
      // At the edge of its stack: the next step leaves the stack and parks the
      // tab immediately outside it, keeping the gesture reversible. The anchor
      // is the outermost REMAINING member, so the tab lands clear of the stack
      // it just left instead of back inside its own old span.
      const others = members.filter((member) => member.key !== key);
      const next = current.filter((candidate) => candidate.key !== key);
      if (others.length) {
        const anchor = direction > 0 ? others.at(-1) : others[0];
        const anchorIndex = next.findIndex((candidate) => candidate.key === anchor.key);
        next.splice(anchorIndex + (direction > 0 ? 1 : 0), 0, pane);
      } else {
        // Last member out: the tab keeps its position, and the now-memberless
        // stack is dropped by setMembership.
        next.splice(current.indexOf(pane), 0, pane);
      }
      if (!commitGrouping([[key, null]], { order: next })) return null;
      return Object.freeze({
        pane,
        index: panes.value.findIndex((candidate) => candidate.key === key),
        count: panes.value.length,
        group: null,
        leftGroup: groupIndex.value.get(groupID) || null,
      });
    }
    const strip = segments.value;
    const segmentIndex = strip.findIndex((segment) => segment.type === 'pane' && segment.key === key);
    const neighbour = strip[segmentIndex + direction];
    if (segmentIndex < 0 || !neighbour) return null;
    const anchor = direction > 0 ? neighbour.panes.at(-1) : neighbour.panes[0];
    const next = current.filter((candidate) => candidate.key !== key);
    const anchorIndex = next.findIndex((candidate) => candidate.key === anchor.key);
    next.splice(anchorIndex + (direction > 0 ? 1 : 0), 0, pane);
    if (!commitPaneOrder(next)) return null;
    return Object.freeze({
      pane,
      index: panes.value.findIndex((candidate) => candidate.key === key),
      count: panes.value.length,
      group: null,
    });
  }

  function createGroup({ name = '', color = null, keys = [] } = {}) {
    loadGroups();
    if (groups.value.length >= MAX_TERMINAL_TAB_GROUPS) return null;
    groupSequence += 1;
    const group = Object.freeze({
      id: `group-${groupSequence}`,
      name: sanitizeGroupName(name, `group ${groups.value.length + 1}`),
      color: normalizeColor(color, groups.value.length),
      collapsed: false,
    });
    groups.value = [...groups.value, group];
    const wanted = (keys || []).filter((key) => panes.value.some((pane) => pane.key === key));
    if (!wanted.length) {
      // An empty stack has nothing to render and nothing to drop onto, so it is
      // never created: the caller always names it with at least one tab.
      groups.value = groups.value.filter((candidate) => candidate.id !== group.id);
      return null;
    }
    commitGrouping(wanted.map((key) => [key, group.id]));
    return group;
  }

  function updateGroup(id, patch) {
    loadGroups();
    const index = groups.value.findIndex((group) => group.id === id);
    if (index < 0) return null;
    const current = groups.value[index];
    const next = Object.freeze({ ...current, ...patch, id: current.id });
    if (next.name === current.name && next.color === current.color
      && next.collapsed === current.collapsed) return current;
    const list = [...groups.value];
    list[index] = next;
    groups.value = list;
    persistGroups();
    return next;
  }

  function renameGroup(id, name) {
    return updateGroup(id, { name: sanitizeGroupName(name, groupIndex.value.get(id)?.name || 'group') });
  }

  function setGroupColor(id, color) {
    const index = Math.max(0, groups.value.findIndex((group) => group.id === id));
    return updateGroup(id, { color: normalizeColor(color, index) });
  }

  // Collapsing a stack that owns the active terminal moves activation to the
  // nearest tab outside it, so a collapse never hides the terminal the operator
  // is looking at. With nothing outside to move to, the active member stays
  // visible in the collapsed pill instead — the island renders it — rather than
  // refusing a collapse the operator explicitly asked for.
  function setGroupCollapsed(id, collapsed) {
    loadGroups();
    if (!groupIndex.value.has(id)) return null;
    if (collapsed && groupIDFor(activeKey.value) === id) {
      const outside = panes.value.filter((pane) => groupIDFor(pane.key) !== id);
      const index = panes.value.findIndex((pane) => pane.key === activeKey.value);
      const successor = panes.value.slice(index + 1).find((pane) => groupIDFor(pane.key) !== id)
        || panes.value.slice(0, index).reverse().find((pane) => groupIDFor(pane.key) !== id)
        || outside[0];
      if (successor) activeKey.value = successor.key;
    }
    return updateGroup(id, { collapsed: collapsed === true });
  }

  function toggleGroupCollapsed(id) {
    const group = groupIndex.value.get(id);
    return group ? setGroupCollapsed(id, !group.collapsed) : null;
  }

  function expandGroupFor(key) {
    const group = groupFor(key);
    if (group?.collapsed) updateGroup(group.id, { collapsed: false });
  }

  // dissolveGroup drops the stack and returns its members to the ungrouped run
  // in place. The terminals themselves are untouched — this is the "ungroup"
  // command, not a close.
  function dissolveGroup(id) {
    loadGroups();
    if (!groupIndex.value.has(id)) return false;
    const members = [...membership.value.entries()]
      .filter(([, groupID]) => groupID === id)
      .map(([key]) => key);
    groups.value = groups.value.filter((group) => group.id !== id);
    const next = new Map(membership.value);
    for (const key of members) next.delete(key);
    membership.value = next;
    panes.value = normalizeGrouping(panes.value, groupIDFor);
    persistPreferredOrder();
    persistGroups();
    return true;
  }

  function assignPaneToGroup(key, groupID) {
    loadGroups();
    if (!panes.value.some((pane) => pane.key === key)) return false;
    if (groupID && !groupIndex.value.has(groupID)) return false;
    if (!groupID) return commitGrouping([[key, null]]);
    // Joining a stack lands the tab at the end of it, which is where a drop on
    // the stack's pill puts it too.
    const members = panes.value.filter((pane) => groupIDFor(pane.key) === groupID && pane.key !== key);
    const order = panes.value.filter((pane) => pane.key !== key);
    const pane = panes.value.find((candidate) => candidate.key === key);
    const anchorIndex = members.length
      ? order.findIndex((candidate) => candidate.key === members.at(-1).key) + 1
      : order.length;
    order.splice(anchorIndex, 0, pane);
    const changed = commitGrouping([[key, groupID]], { order });
    if (changed && groupIndex.value.get(groupID)?.collapsed && activeKey.value === key) {
      updateGroup(groupID, { collapsed: false });
    }
    return changed;
  }

  function groupMembers(id) {
    return panes.value.filter((pane) => groupIDFor(pane.key) === id);
  }

  function openModal({ wsPath, ws, label = '', hideConv = null, initialRetry = false } = {}) {
    const seed = normalizeSeed({
      ws: wsPath || ws, label, hideConv: hideConv || null, initialRetry: initialRetry === true,
    });
    if (!seed) return null;
    modalSequence += 1;
    const descriptor = Object.freeze({
      id: `terminal-modal-${modalSequence}`,
      label: seed.label || '',
      seed: Object.freeze({ ...seed }),
    });
    modal.value = descriptor;
    return descriptor;
  }

  function closeModal(id) {
    if (!modal.value || (id && modal.value.id !== id)) return null;
    const descriptor = modal.value;
    modal.value = null;
    return descriptor;
  }

  function findPaneKey(selectors) {
    const wanted = new Set(selectors || []);
    if (!wanted.size) return null;
    return panes.value.find((pane) => wanted.has(pane.seed.agent))?.key || null;
  }

  function dispose() {
    panes.value = [];
    activeKey.value = null;
    modal.value = null;
  }

  return Object.freeze({
    panes,
    activeKey,
    modal,
    revealRequest,
    groups,
    membership,
    segments,
    view,
    openPane,
    activatePane,
    removePane,
    removePanes,
    movePane,
    reorderPane,
    movePaneByOffset,
    createGroup,
    renameGroup,
    setGroupColor,
    setGroupCollapsed,
    toggleGroupCollapsed,
    dissolveGroup,
    assignPaneToGroup,
    groupFor,
    groupMembers,
    openModal,
    closeModal,
    findPaneKey,
    requestReveal,
    dispose,
  });
}

export const terminalShellState = createTerminalShellState();
