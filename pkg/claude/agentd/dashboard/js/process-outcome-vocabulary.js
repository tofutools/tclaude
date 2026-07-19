// Edge-outcome vocabulary shared by the process editor, the connection-drag
// preview, and the read-only graph renderer.
//
// These mirror model/next.go. They live in their own module because the
// renderer serves the read-only viewer too and must not pull in the edit model,
// while the editor and the drag preview must agree with the renderer about
// which outcomes are worth painting.

// UNNAMED_OUTCOME is the outcome given to a connector whose label carries no
// information: the sole way out of a node. It is a real key, never an empty
// string — model.validate rejects blank outcomes (validate.go, missing_outcome)
// and a YAML `next` map cannot hold a blank key. "next" is Go's
// model.DefaultOutcome, so a lone edge named this way marshals back to the bare
// shorthand `next: <to>` and routes through the plain-pass vocabulary.
export const UNNAMED_OUTCOME = 'next';

// PASS_OUTCOMES mirrors passOutcomeLabels in model/next.go, in the precedence
// order plan.ResolvePassEdge uses. The editor consults it so it cannot hand one
// node two competing pass edges: validateOutcomeRouting picks the first match
// in this order and the loser becomes unreachable, with no diagnostic.
export const PASS_OUTCOMES = ['pass', 'done', 'success', UNNAMED_OUTCOME];

// outcomeCarriesInformation reports whether an edge's outcome is worth drawing
// over the arrow. It is not when the edge is the only way out of its node AND
// carries the unnamed default: the runtime takes a lone edge regardless of its
// name (plan.ResolvePassEdge's single-edge fallback), so the label would state
// nothing the arrow does not already show. Once a sibling exists the outcome is
// the routing decision and is always drawn.
export function outcomeCarriesInformation(outcome, siblingCount) {
  if (!outcome) return false;
  if (siblingCount > 1) return true;
  return outcome !== UNNAMED_OUTCOME;
}
