// morph.js — a minimal keyed DOM reconciler for the dashboard's 2-second poll.
//
// The dashboard re-renders every tab wholesale on each snapshot poll by
// replacing a container's innerHTML with a freshly-built HTML string. That
// destroys and recreates every node under the container, which:
//   - drops any text selection the user was making (copy-paste is impossible —
//     the selection is wiped every 2s);
//   - restarts CSS animations from 0% (patched by helpers.syncBotAnimations);
//   - snaps :hover reveals shut under a stationary cursor (patched by the
//     hoveredGroupKey re-stamping in refresh.js / render.js);
//   - bounces keyboard focus to <body> (patched by captureFocus/restoreFocus).
//
// morphInto() replaces the innerHTML swap with a reconcile: it diffs the fresh
// HTML against the LIVE DOM and mutates only what actually changed. Unchanged
// nodes are never touched, so a selection, focus, hover, animation, scroll
// position or open <details> anchored in them survives the tick untouched.
// Changed text is written in place on the existing text node via nodeValue, so
// even the churny relTime() "Ns ago" strings update without tearing a
// selection down (a selection spanning static text keeps that text; only the
// changed run shifts).
//
// This is a hand-rolled ~1-file reconciler rather than a vendored morphdom
// because the dashboard's need is narrow and fixed — reconcile a STABLE
// container's children against a freshly-rendered HTML string, keyed by
// id / data-group-key / data-key, skipping unchanged subtrees — so the whole
// thing fits in one auditable, dependency-free module the cold reviewer can
// read end to end (see the PR for the vendor-vs-handroll rationale).
//
// SCOPE / SAFETY:
//   - morphInto only ever mutates a container's CHILDREN; it never replaces or
//     re-creates the container itself, so the delegated event listeners the
//     dashboard binds on the stable containers (#groups-list, #jobs-list, …)
//     survive for free.
//   - The 2s poll is already SUSPENDED (refreshSuspended() in refresh.js) while
//     an inline-rename input, a drag, or any modal is live, so the morph never
//     runs against a container that holds a live-only input value — the fresh
//     HTML string is always the full truth for what's inside these containers.
//   - Live <details open> state is PRESERVED on the existing node rather than
//     overwritten from the fresh HTML (see morphAttributes), the morphdom
//     convention for user-toggled disclosure/form state. It can't drift: the
//     open state is user-driven and persisted to dashPrefs synchronously on
//     toggle (bindDetailsPersistence), and the fresh HTML re-derives `open`
//     from that same dashPref — so live and fresh already agree every tick.

// nodeKey returns the reconcile key for a node, or '' when it is unkeyed.
// Element keys, in priority order: a stable `id`, then `data-group-key` (the
// group <details>), then `data-key` (repeated rows). Keyed siblings are matched
// by key across a reorder — a sorted/filtered/moved node is relocated with its
// whole subtree intact rather than having some other row's content morphed into
// it. Non-elements (text / comment / whitespace between rows) are never keyed.
function nodeKey(node) {
  if (node.nodeType !== 1 /* ELEMENT_NODE */) return '';
  return node.id
    || node.getAttribute('data-group-key')
    || node.getAttribute('data-key')
    || '';
}

// indexKeyed maps key → child for every keyed child of `parent`, so the main
// loop can pull a kept-but-moved keyed node to its new slot in O(1). Built
// lazily (only when the fresh side actually has keyed children).
function indexKeyed(parent) {
  const map = new Map();
  for (let c = parent.firstChild; c; c = c.nextSibling) {
    const k = nodeKey(c);
    // First occurrence wins; keys are unique among real siblings, so a
    // duplicate would only ever be stray/foreign and is safely ignored.
    if (k && !map.has(k)) map.set(k, c);
  }
  return map;
}

// collectKeys returns the set of reconcile keys among a parent's children.
function collectKeys(parent) {
  const set = new Set();
  for (let c = parent.firstChild; c; c = c.nextSibling) {
    const k = nodeKey(c);
    if (k) set.add(k);
  }
  return set;
}

// compatible reports whether `from` can be morphed in place into `to` (same
// node type, and for elements the same tag). Incompatible pairs are replaced
// wholesale instead. Only reached on the UNKEYED positional path, where both
// sides are unkeyed by construction, so key identity need not be checked here.
function compatible(from, to) {
  if (from.nodeType !== to.nodeType) return false;
  if (from.nodeType === 1 /* ELEMENT_NODE */) return from.tagName === to.tagName;
  return true; // text / comment: always morphable via nodeValue
}

// morphAttributes syncs `from`'s attributes to match `to`'s: add/update every
// attribute the fresh node carries, then drop any the fresh node no longer has.
function morphAttributes(from, to) {
  // <details open> (and, defensively, live form-control state) is user-driven
  // and must not be clobbered from the fresh HTML mid-interaction — preserve
  // the live node's own value. See the module header for why this can't drift.
  const preserveOpen = from.nodeName === 'DETAILS';

  const toAttrs = to.attributes;
  for (let i = 0; i < toAttrs.length; i++) {
    const { name, value } = toAttrs[i];
    if (preserveOpen && name === 'open') continue;
    if (from.getAttribute(name) !== value) from.setAttribute(name, value);
  }
  // Iterate a snapshot of the names (we mutate the live map inside the loop).
  const fromAttrs = from.attributes;
  for (let i = fromAttrs.length - 1; i >= 0; i--) {
    const name = fromAttrs[i].name;
    if (preserveOpen && name === 'open') continue;
    if (!to.hasAttribute(name)) from.removeAttribute(name);
  }
}

// morphNode reconciles one matched pair (already known to be `compatible`).
function morphNode(from, to) {
  const type = from.nodeType;

  // Text / comment: rewrite the value in place, and ONLY when it changed, so an
  // unchanged run (the static parts of a row) is never disturbed and a
  // selection anchored in it is fully preserved. The churny relTime cells are
  // the one run that actually rewrites each tick.
  if (type === 3 /* TEXT_NODE */ || type === 8 /* COMMENT_NODE */) {
    if (from.nodeValue !== to.nodeValue) from.nodeValue = to.nodeValue;
    return;
  }
  if (type !== 1 /* ELEMENT_NODE */) return;

  // Fast path: an identical subtree needs no work at all. isEqualNode compares
  // tag + attributes + descendants deeply, so a genuinely unchanged group /
  // row / empty-state is skipped entirely — nothing under it is touched, which
  // is what lets a selection inside a static region ride across the tick. Safe
  // because these containers hold no live-only property state during a poll
  // (the poll is suspended while any input/drag/modal is live).
  if (from.isEqualNode(to)) return;

  morphAttributes(from, to);
  morphChildren(from, to);
}

// morphChildren reconciles the child lists of `fromParent` and `toParent` in a
// single left-to-right pass, matching keyed children by key (moving them when
// reordered) and unkeyed children positionally.
function morphChildren(fromParent, toParent) {
  // Pre-pass: drop every live keyed child whose key is GONE from the fresh
  // render (a retired agent, a completed job, a filtered-out group, …). This
  // is load-bearing, not an optimisation: the main loop's unkeyed positional
  // path steps OVER keyed live nodes (they are claimed by key, not position),
  // and the end-of-loop cleanup only reaches the trailing run after the final
  // cursor. A surplus keyed node the cursor stepped past would otherwise be
  // stranded forever (rows are separated by whitespace text nodes, so this
  // fires on essentially every removal). Removing them up front restores the
  // invariant the rest of the loop relies on: every keyed live node that
  // remains WILL be claimed by a target, so nothing keyed can be left behind.
  const freshKeys = collectKeys(toParent);
  for (let c = fromParent.firstChild; c; ) {
    const next = c.nextSibling;
    if (nodeKey(c) && !freshKeys.has(nodeKey(c))) fromParent.removeChild(c);
    c = next;
  }

  let fromKeyed = null; // built lazily on the first keyed target

  let fromChild = fromParent.firstChild;
  let toChild = toParent.firstChild;

  while (toChild) {
    const toNext = toChild.nextSibling;
    const key = nodeKey(toChild);

    if (key) {
      // Keyed target — find its live counterpart by key regardless of position.
      // The dashboard's keyed templates are type-stable (a given key always
      // denotes the same element tag — a member/row key is always a <tr>, a
      // group key always a <details>), so a matched keyed node is always
      // tag-compatible and can be morphed in place; we don't re-check the tag.
      if (fromKeyed === null) fromKeyed = indexKeyed(fromParent);
      const matched = fromKeyed.get(key);
      if (matched) {
        if (matched === fromChild) {
          // Already in place: morph and advance past it.
          morphNode(fromChild, toChild);
          fromChild = fromChild.nextSibling;
        } else {
          // Kept but out of order: move it to the cursor (subtree + any
          // selection/focus inside it comes along), morph, and do NOT advance
          // fromChild — the node now at the cursor still awaits its own target.
          fromParent.insertBefore(matched, fromChild);
          morphNode(matched, toChild);
        }
      } else {
        // New keyed node: insert a deep clone before the cursor.
        fromParent.insertBefore(toChild.cloneNode(true), fromChild);
      }
      toChild = toNext;
      continue;
    }

    // Unkeyed target — match positionally, but step over any KEYED live node:
    // after the pre-pass every remaining keyed node is claimed by some target,
    // so it must not be consumed by an unkeyed slot.
    while (fromChild && nodeKey(fromChild)) fromChild = fromChild.nextSibling;

    if (fromChild) {
      if (compatible(fromChild, toChild)) {
        morphNode(fromChild, toChild);
        fromChild = fromChild.nextSibling;
      } else {
        // Different tag / node type: replace this live node with a fresh clone.
        const clone = toChild.cloneNode(true);
        fromParent.replaceChild(clone, fromChild);
        fromChild = clone.nextSibling;
      }
    } else {
      // Ran out of live nodes: append the rest.
      fromParent.appendChild(toChild.cloneNode(true));
    }
    toChild = toNext;
  }

  // Anything left over on the live side is surplus UNKEYED tail (e.g. trailing
  // whitespace) — remove it. Surplus keyed nodes were already dropped by the
  // pre-pass, and moved-and-kept keyed nodes were relocated BEFORE the cursor,
  // so neither can be in this tail.
  while (fromChild) {
    const next = fromChild.nextSibling;
    fromParent.removeChild(fromChild);
    fromChild = next;
  }
}

// morphInto reconciles `container`'s children to match `html` (a freshly
// rendered HTML string), mutating only what changed. The container element
// itself is never touched, so listeners delegated on it stay bound.
//
// The fresh HTML is parsed inside a clone of the container's OWN tag, so
// context-sensitive fragments parse correctly — a bare <tr>/<td>/<tbody> only
// parses as such inside a table-family element, and the six live containers are
// plain <div>s that legitimately hold <table>…/<details>… fragments.
export function morphInto(container, html) {
  // First paint (empty container): nothing to preserve, so the plain innerHTML
  // assignment is both simpler and cheaper than reconciling against nothing.
  if (!container.firstChild) {
    container.innerHTML = html;
    return;
  }
  const fresh = container.cloneNode(false); // same tag, no children, detached
  fresh.innerHTML = html;
  morphChildren(container, fresh);
}

// Internals exported ONLY for the reconciler unit test (jstest/morph.test.mjs),
// which drives them against a mock DOM to avoid needing a browser. The app
// imports morphInto alone. (Same "export the internals for the suite" pattern
// as group-activity.js.)
export { nodeKey, morphNode, morphChildren, morphAttributes };
