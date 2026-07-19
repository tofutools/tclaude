// process-edge-hint.js -- the "this label routes the run" hint shown against a
// labelled connector, plus its pin state.
//
// Why it exists: an outcome label looks like decoration but is the key of the
// node's `next` map, so renaming it re-points the run. The editor hides the
// label entirely when it cannot matter (a lone connector, see
// process-outcome-vocabulary.js), which means every label still on screen IS
// load-bearing — and worth explaining once.
//
// Pin model: shown against the selected connector until the author dismisses it
// with the pin button, then never again. The dismissal is one editor-wide flag,
// not per-edge: the message is identical on every connector, so dismissing it
// once per arrow would be a chore.
//
// It is an author preference, never template content -- it must never reach the
// saved YAML -- so it goes through dashPrefs (-> SQLite), which is where editor
// prefs live in this dashboard. The store is injected rather than imported so
// this module stays DOM-free and unit testable.

// Namespaced under tclaude.dash.* per the convention documented in prefs.js.
export const EDGE_HINT_STORAGE_KEY = 'tclaude.dash.processEditor.edgeLabelHint';
const DISMISSED = 'dismissed';

export function edgeHintText(outcome, siblingCount) {
  const quoted = `"${outcome}"`;
  return siblingCount > 1
    ? `${quoted} is this connector's outcome key: when the run leaves this node it takes the connector whose key matches the result. Renaming it changes which results come this way.`
    : `${quoted} is this connector's outcome key. It is the only way out of this node, so the run takes it either way — but adding a second connector makes the key decide.`;
}

// Both accessors tolerate a store that throws or is absent. dashPrefs is backed
// by a network round trip to SQLite, so a failure here is a real possibility --
// and a hint preference is never worth breaking the editor over.
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

// resolveEdgeHint decides whether a hint is showing and for which edge. Pure so
// the rule is unit testable without a DOM.
//
// The hint is tied to selection alone: it appears against the selected
// connector, and only while that connector actually carries a label (an unnamed
// lone connector has no key to explain and nothing to anchor to). Dismissing it
// is editor-wide and permanent — the message is identical on every connector,
// so once it has been read it is noise everywhere.
export function resolveEdgeHint({ dismissed = false, selected = null, labelled = () => false } = {}) {
  if (dismissed) return { open: false, edge: null };
  if (!selected || !labelled(selected)) return { open: false, edge: null };
  return { open: true, edge: selected };
}
