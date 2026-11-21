// agent-group-prefix.js -- which group an agent belongs to, as a short prefix
// on its name where an agent's terminal shows it (tk_c501cda3): "t/lead" for
// the lead of tclaude, "c/lead" for the lead of counterslop. Two agents of the
// same name in different groups are told apart at a glance; the full
// "tclaude/lead" is in the tooltip.
//
// A group's prefix is the shortest start of its name that no other group's
// name on this node starts with, so it stays the same whichever tabs are open:
// "web" and "wor" where webshop and workers both start with w. A name that
// another starts with (web beside webshop) is given in full. Pure:
// jstest/agent-group-prefix.test.mjs exercises it directly. The dashboard's
// terminal tabs and title bars, the overlay windows' tabs and the phone
// terminal all use it.

// groupPrefixes maps each group name to its prefix.
export function groupPrefixes(names) {
  const unique = [...new Set((names || []).filter((name) => typeof name === 'string' && name))];
  const folded = unique.map((name) => name.toLowerCase());
  const out = new Map();
  unique.forEach((name, i) => {
    let length = 1;
    for (; length < name.length; length += 1) {
      const start = folded[i].slice(0, length);
      if (!folded.some((other, j) => j !== i && other.startsWith(start))) break;
    }
    out.set(name, name.slice(0, length));
  });
  return out;
}

// The prefixes are worked out once per snapshot.
const prefixesOf = new WeakMap();

function snapshotPrefixes(snapshotValue) {
  if (!snapshotValue || typeof snapshotValue !== 'object') return new Map();
  let prefixes = prefixesOf.get(snapshotValue);
  if (!prefixes) {
    const names = (Array.isArray(snapshotValue.groups) ? snapshotValue.groups : []).map((group) => group?.name);
    // An agent's groups count too, should the group list leave one out.
    for (const agent of Array.isArray(snapshotValue.agents) ? snapshotValue.agents : []) {
      if (Array.isArray(agent?.groups)) names.push(...agent.groups);
    }
    prefixes = groupPrefixes(names);
    prefixesOf.set(snapshotValue, prefixes);
  }
  return prefixes;
}

function findAgent(snapshotValue, selector) {
  if (!selector) return null;
  const agents = Array.isArray(snapshotValue?.agents) ? snapshotValue.agents : [];
  return agents.find((agent) => agent && (agent.agent_id === selector || agent.conv_id === selector)) || null;
}

// agentGroupMark is how an agent's terminal names its group: the group (the
// one the terminal was opened from when the agent is in it, else the group it
// was spawned into, else its first), that group's prefix, and all its groups.
// null for an agent in no group, or one this node does not know.
export function agentGroupMark(snapshotValue, selector, openedFrom = '') {
  const agent = findAgent(snapshotValue, selector);
  const groups = Array.isArray(agent?.groups) ? agent.groups.filter((group) => typeof group === 'string' && group) : [];
  if (!groups.length) return null;
  const group = [openedFrom, agent.spawn_group].find((candidate) => candidate && groups.includes(candidate)) || groups[0];
  const prefix = snapshotPrefixes(snapshotValue).get(group) || group;
  return { group, prefix, groups };
}

// markedName is the name with its group's prefix: "t/lead".
export function markedName(name, mark) {
  return mark ? `${mark.prefix}/${name}` : name;
}

// markTitle is the name with its full group, for a tooltip: "tclaude/lead",
// and its other groups when it is in more than one.
export function markTitle(name, mark) {
  if (!mark) return name;
  const others = mark.groups.filter((group) => group !== mark.group);
  return `${mark.group}/${name}${others.length ? ` (also in ${others.join(', ')})` : ''}`;
}

// fitPrefix is the prefix as it fits in room (a width, measured by width()):
// whole when it fits, else as many of its first letters as fit before an
// ellipsis, at least one. Cut here rather than by CSS text-overflow, which
// leaves the unused end of the box as a gap before the slash ("websh… /lead").
export function fitPrefix(prefix, room, width) {
  if (prefix.length <= 1 || Number.isNaN(room) || width(prefix) <= room) return prefix;
  for (let n = prefix.length - 1; n > 1; n -= 1) {
    const cut = `${prefix.slice(0, n)}…`;
    if (width(cut) <= room) return cut;
  }
  return `${prefix.slice(0, 1)}…`;
}

// seedGroupMark is a mark a popped-out terminal was handed in its seed: that
// window reads only its own agent, not every group's name, so the dashboard
// works the prefix out and passes it on. It is shown as text only, and a
// malformed one is dropped.
export function seedGroupMark(value) {
  const text = (item) => typeof item === 'string' && item.length > 0 && item.length <= 128;
  if (!value || !text(value.group) || !text(value.prefix) || !Array.isArray(value.groups)) return null;
  const groups = value.groups.filter(text).slice(0, 32);
  return groups.includes(value.group) ? { group: value.group, prefix: value.prefix, groups } : null;
}
