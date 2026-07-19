// Edge-outcome vocabulary shared by the process editor, the connection-drag
// preview, and the read-only graph renderer.
//
// These mirror model/next.go. They live in their own module because the
// renderer serves the read-only viewer too and must not pull in the edit model,
// while the editor and the drag preview must agree with the renderer about
// which outcomes are worth painting.

// UNNAMED_OUTCOME is the outcome a connector gets when it is a node's only way
// out. It is 'pass' -- deliberately the FIRST entry in PASS_OUTCOMES, not the
// tidier-looking model.DefaultOutcome ('next').
//
// 'next' is the LAST pass alias in precedence order. A lone edge named 'next'
// that later gains a 'pass' sibling loses the pass routing TO that sibling
// (validate.go emits ambiguous_pass_edge), silently re-pointing a run away from
// the target the author drew first. Naming the lone edge 'pass' keeps it the
// precedence winner forever, so drawing a second connector cannot disturb the
// first. It also means this feature changes no stored outcome at all: 'pass' is
// exactly what the editor already minted. Hiding the label is pure
// presentation.
export const UNNAMED_OUTCOME = 'pass';

// PASS_OUTCOMES mirrors passOutcomeLabels in model/next.go, in the precedence
// order plan.ResolvePassEdge uses.
export const PASS_OUTCOMES = ['pass', 'done', 'success', 'next'];

// defaultPinned is the label-visibility rule for a connector whose author has
// expressed no opinion. It answers whether the outcome is worth drawing.
//
// It is not, when the edge is the only way out of its node AND its outcome is
// one of the generic pass-vocabulary names. The runtime takes a lone edge
// regardless of what it is called (plan.ResolvePassEdge's single-edge
// fallback), so such a label states nothing the arrow does not already show.
//
// The whole vocabulary is covered, not just UNNAMED_OUTCOME, because a lone
// exit is spelled 'pass' by the editor and may be spelled 'done' or 'success'
// by hand-written YAML.
//
// DECISION NODES ARE NEVER HIDDEN. plan.DecisionEdge matches a verdict exactly
// and has NO single-edge fallback, so a decision's outcome is load-bearing even
// when it is the only one -- hiding it would leave the author unable to see the
// verdict the run must produce, and validateChoiceOutcomes does not cross-check
// choices against next keys to catch the mismatch.
//
// A name outside the vocabulary is always drawn even on a lone edge: the author
// typed something specific, so it is documentation rather than filler. And once
// a sibling exists ANY outcome is the routing decision, and is always drawn.
export function defaultPinned(outcome, siblingCount, nodeType = '') {
  if (!outcome) return false;
  if (nodeType === 'decision') return true;
  if (siblingCount > 1) return true;
  return !PASS_OUTCOMES.includes(outcome);
}

// edgeLabelVisible is the single authority on whether a connector's outcome is
// drawn. Layered so each rule has one place to live:
//
//   selected           -> always drawn, alongside the pin button. Selecting a
//                         connector is how you inspect and change its key, so it
//                         must be readable even when pinned off.
//   explicit pin state -> the author's opinion wins over the default.
//   otherwise          -> defaultPinned decides, so an untouched template
//                         declutters itself without any per-edge clicking.
export function edgeLabelVisible({
  outcome, siblingCount = 2, nodeType = '', pinned, selected = false,
} = {}) {
  if (!outcome) return false;
  if (selected) return true;
  if (pinned !== undefined) return !!pinned;
  return defaultPinned(outcome, siblingCount, nodeType);
}
