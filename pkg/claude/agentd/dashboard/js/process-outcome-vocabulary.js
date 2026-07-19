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
// over the arrow.
//
// It is not, when the edge is the only way out of its node AND its outcome is
// one of the generic pass-vocabulary names. The runtime takes a lone edge
// regardless of what it is called (plan.ResolvePassEdge's single-edge
// fallback), so such a label states nothing the arrow does not already show.
//
// The whole vocabulary is covered, not just UNNAMED_OUTCOME, because templates
// authored before unnamed connectors existed spell this exact situation
// 'pass' — see docs/examples/code-change-with-review.yaml. Hiding only the new
// default would declutter new work and leave every existing process as noisy as
// before, which is the complaint this exists to answer.
//
// A name outside the vocabulary is always drawn even on a lone edge: the author
// typed something specific, so it is documentation rather than filler. And once
// a sibling exists ANY outcome is the routing decision, and is always drawn.
export function outcomeCarriesInformation(outcome, siblingCount) {
  if (!outcome) return false;
  if (siblingCount > 1) return true;
  return !PASS_OUTCOMES.includes(outcome);
}
