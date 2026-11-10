// tab-group-model.js -- a tab group, shared by the Terminals strip and overlay
// windows: its name, its colour, and how a flat row of tabs falls into stacks.
// Tabs here are { key }, as the Terminals strip's panes are.

export const MAX_TERMINAL_TAB_GROUPS = 24;
export const MAX_TERMINAL_GROUP_NAME_LENGTH = 40;

// Named palette slots rather than raw colours: mux.css owns the actual values
// for both the ordinary and the wizard theme.
export const TERMINAL_GROUP_COLORS = Object.freeze([
  'blue', 'purple', 'green', 'amber', 'red', 'teal', 'pink', 'slate',
]);

// Group names are operator text rendered into the tab strip and into the
// accessible announcements, so they are normalized on the way in from a dialog:
// nothing invisible, no runaway length, never empty.
//
// Stripped: C0/DEL, the C1 range, the Unicode line/paragraph separators, and
// the invisible formatting characters — zero-width joiners/spaces, the soft
// hyphen, the BOM, and the bidi overrides/isolates. The last group is the one
// that matters beyond tidiness: U+202E and friends reverse the rendering of the
// text after them, in the strip AND in the aria-label, which would let a name
// misrepresent itself. Everything here is invisible, so a name made only of it
// collapses to empty and takes the fallback.
const INVISIBLE_NAME_CHARS = /[\u0000-\u001f\u007f-\u009f\u00ad\u061c\u180e\u200b-\u200f\u2028\u2029\u202a-\u202e\u2060-\u2064\u2066-\u2069\ufeff]+/g;

export function sanitizeGroupName(name, fallback = 'group') {
  const cleaned = String(name ?? '')
    .replace(INVISIBLE_NAME_CHARS, ' ')
    .replace(/\s+/g, ' ')
    .trim()
    .slice(0, MAX_TERMINAL_GROUP_NAME_LENGTH)
    .trim();
  return cleaned || fallback;
}

export function normalizeColor(color, index = 0) {
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
