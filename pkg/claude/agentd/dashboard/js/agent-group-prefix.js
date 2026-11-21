// agent-group-prefix.js -- which group an agent's terminal belongs to, where
// the terminal names its agent (tk_c501cda3, tk_0a10cbcb).
//
// Everywhere with room for it -- a pane's title bar, a popped-out window's
// title, the phone terminal's header -- the name follows its full group,
// dimmed: "tclaude/lead". A tab has no room for that, so it shows the bare
// name, and a group prefix only when another open tab shows an agent of the
// same name: "t/lead" beside "c/lead". That prefix is the shortest start that
// tells those tabs' groups apart, worked out among them alone, so it is
// usually one letter. The full "tclaude/lead" is always in the tooltip.
// Pure: jstest/agent-group-prefix.test.mjs exercises it directly.

// agentGroupMark is the group an agent's terminal names: the one the terminal
// was opened from when the agent is in it, else the group it was spawned
// into, else its first; with all its groups, for the tooltip. null for an
// agent in no group, or one this node does not know.
export function agentGroupMark(snapshotValue, selector, openedFrom = '') {
  if (!selector) return null;
  const agents = Array.isArray(snapshotValue?.agents) ? snapshotValue.agents : [];
  const agent = agents.find((candidate) => candidate && (candidate.agent_id === selector || candidate.conv_id === selector));
  const groups = Array.isArray(agent?.groups) ? agent.groups.filter((group) => typeof group === 'string' && group) : [];
  if (!groups.length) return null;
  const group = [openedFrom, agent.spawn_group].find((candidate) => candidate && groups.includes(candidate)) || groups[0];
  return { group, groups };
}

// groupName is the name after its full group: "tclaude/lead".
export function groupName(name, mark) {
  return mark ? `${mark.group}/${name}` : name;
}

// markTitle is the full group and name for a tooltip, and the agent's other
// groups when it is in more than one.
export function markTitle(name, mark) {
  if (!mark) return name;
  const others = mark.groups.filter((group) => group !== mark.group);
  return `${groupName(name, mark)}${others.length ? ` (also in ${others.join(', ')})` : ''}`;
}

const wordChar = /[\p{L}\p{N}]/u;

// distinctStarts maps each of a few group names to the shortest start no
// other of them starts with, compared without case: "t" and "c" for tclaude
// and counterslop. A name another starts with is given whole (tclaude beside
// tclaude-config), and a start never ends on a separator: "tclaude-c", not
// "tclaude-".
export function distinctStarts(names) {
  const unique = [...new Set(names.filter((name) => typeof name === 'string' && name))];
  const folded = unique.map((name) => name.toLowerCase());
  const out = new Map();
  unique.forEach((name, i) => {
    let length = 1;
    for (; length < name.length; length += 1) {
      const start = folded[i].slice(0, length);
      if (!folded.some((other, j) => j !== i && other.startsWith(start))) break;
    }
    while (length < name.length && !wordChar.test(name[length - 1])) length += 1;
    out.set(name, name.slice(0, length));
  });
  return out;
}

// clashPrefixes is the group prefix each tab shows: entries are the open tabs
// as { key, name, group } (group '' for an agent in no group, or a tab that is
// not a local agent's). A tab gets a prefix only when another tab has the
// same name, compared without case, and they are not all of one group; the
// prefixes tell apart the groups of those tabs alone. Keyed by entry key.
export function clashPrefixes(entries) {
  const byName = new Map();
  for (const entry of entries) {
    if (!entry?.name) continue;
    const name = entry.name.toLowerCase();
    if (!byName.has(name)) byName.set(name, []);
    byName.get(name).push(entry);
  }
  const out = new Map();
  for (const clashing of byName.values()) {
    if (clashing.length < 2 || clashing.every((entry) => (entry.group || '') === (clashing[0].group || ''))) continue;
    const starts = distinctStarts(clashing.map((entry) => entry.group));
    for (const entry of clashing) {
      if (entry.group) out.set(entry.key, starts.get(entry.group));
    }
  }
  return out;
}

// fitGroup is a group name as it fits in room (a width, measured by width()):
// whole when it fits, else as many of its first letters as fit before an
// ellipsis, at least one. Cut here rather than by CSS text-overflow, which
// leaves the unused end of the box as a gap before the slash ("websh… /lead").
export function fitGroup(group, room, width) {
  if (group.length <= 1 || Number.isNaN(room) || width(group) <= room) return group;
  for (let n = group.length - 1; n > 1; n -= 1) {
    const cut = `${group.slice(0, n)}…`;
    if (width(cut) <= room) return cut;
  }
  return `${group.slice(0, 1)}…`;
}

// seedGroupMark is a mark a popped-out terminal was handed in its seed: that
// window reads only its own agent, so the dashboard passes its group on. It is
// shown as text only, and a malformed one is dropped.
export function seedGroupMark(value) {
  const text = (item) => typeof item === 'string' && item.length > 0 && item.length <= 128;
  if (!value || !text(value.group) || !Array.isArray(value.groups)) return null;
  const groups = value.groups.filter(text).slice(0, 32);
  return groups.includes(value.group) ? { group: value.group, groups } : null;
}
