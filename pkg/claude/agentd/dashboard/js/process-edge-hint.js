// process-edge-hint.js -- the "this label routes the run" hint shown against a
// labelled connector, plus its pin state.
//
// Why it exists: an outcome label looks like decoration but is the key of the
// node's `next` map, so renaming it re-points the run. The editor hides the
// label entirely when it cannot matter (a lone connector, see
// process-outcome-vocabulary.js), which means every label still on screen IS
// load-bearing — and worth explaining once.
//
// Pin model: shown without a hover until the author dismisses it, hover-only
// afterwards. The dismissal is one editor-wide flag in localStorage, not
// per-edge: the message is identical on every connector, so asking for it to be
// dismissed once per arrow would be a chore, and it is an author preference
// rather than template content (it must never reach the saved YAML).

export const EDGE_HINT_STORAGE_KEY = 'tclaude.processEditor.edgeLabelHint';
const DISMISSED = 'dismissed';

export function edgeHintText(outcome, siblingCount) {
  const quoted = `"${outcome}"`;
  return siblingCount > 1
    ? `${quoted} is this connector's outcome key: when the run leaves this node it takes the connector whose key matches the result. Renaming it changes which results come this way.`
    : `${quoted} is this connector's outcome key. It is the only way out of this node, so the run takes it either way — but adding a second connector makes the key decide.`;
}

// readEdgeHintDismissed tolerates a storage object that throws on access:
// Safari in private mode and embedded webviews both do, and a hint preference
// is never worth breaking the editor over.
export function readEdgeHintDismissed(storage) {
  try {
    return storage?.getItem?.(EDGE_HINT_STORAGE_KEY) === DISMISSED;
  } catch {
    return false;
  }
}

export function writeEdgeHintDismissed(storage, dismissed) {
  try {
    if (dismissed) storage?.setItem?.(EDGE_HINT_STORAGE_KEY, DISMISSED);
    else storage?.removeItem?.(EDGE_HINT_STORAGE_KEY);
    return true;
  } catch {
    return false;
  }
}

// resolveEdgeHint decides whether a hint is showing, for which edge, and
// whether its pin affordance is offered. Pure so the visibility rule is unit
// testable without a DOM: the interesting cases are "pinned but nothing
// selected" and "dismissed and merely hovered".
//
//   dismissed=false -> the selected edge shows it; a hovered edge shows it too
//   dismissed=true  -> only a hovered edge (or the hovered hint itself) shows it
//
// At most one hint exists at a time; selection wins over hover so the hint does
// not jump away while the author moves toward it.
export function resolveEdgeHint({
  dismissed = false, selected = null, hovered = null, hintHovered = false, labelled = () => false,
} = {}) {
  const pick = (candidate) => (candidate && labelled(candidate) ? candidate : null);
  const selectedEdge = pick(selected);
  const hoveredEdge = pick(hovered);
  if (!dismissed) {
    const edge = selectedEdge || hoveredEdge;
    return edge ? { open: true, edge, pinned: true } : { open: false, edge: null, pinned: true };
  }
  // Once dismissed the hint is hover-only. hintHovered keeps it alive while the
  // pointer is over the bubble itself, which otherwise sits off the arrow's hit
  // path and would dismiss on approach.
  const edge = hoveredEdge || (hintHovered ? selectedEdge : null);
  return edge ? { open: true, edge, pinned: false } : { open: false, edge: null, pinned: false };
}
