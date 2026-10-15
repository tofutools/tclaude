// process-loops.js -- bounded review loops (tcl-eu70x9) as the dashboard
// authors and shows them. Pure: no DOM, no fetch.
//
// A decision outcome that goes back to earlier work is bounded in the
// decision's `loops`, keyed by outcome:
//
//   loops: { changes: { maxIterations: 3, onExhausted: escalate } }
//
// maxIterations is the most rounds the work between the edge's target and
// the decision runs; past the last round the decision follows onExhausted, or
// with none the outcome is refused and the decider picks another. Only an edge
// that goes back can be bounded, and every cycle needs one (the server's
// loop_* and graph_cycle diagnostics stay the authority).

export const LOOP_DEFAULT_ROUNDS = 3;
export const LOOP_MIN_ROUNDS = 2;
export const LOOP_MAX_ROUNDS = 100;

function isRecord(value) {
  return value != null && typeof value === 'object' && !Array.isArray(value);
}

// loopBound is the bound a node declares for one outcome, or null.
export function loopBound(node, outcome) {
  const bound = isRecord(node?.loops) ? node.loops[outcome] : undefined;
  return isRecord(bound) ? { maxIterations: bound.maxIterations, onExhausted: bound.onExhausted || '' } : null;
}

// setLoopBound writes (or with a null bound removes) one outcome's bound on a
// node in place, keeping `loops` absent when nothing is bounded.
export function setLoopBound(node, outcome, bound) {
  if (!node) return;
  if (bound) {
    const entry = { maxIterations: bound.maxIterations };
    if (bound.onExhausted) entry.onExhausted = bound.onExhausted;
    node.loops = { ...(isRecord(node.loops) ? node.loops : {}), [outcome]: entry };
    return;
  }
  if (!isRecord(node.loops)) return;
  delete node.loops[outcome];
  if (!Object.keys(node.loops).length) delete node.loops;
}

// edgeGoesBack reports whether the edge (from, outcome) leads back to work
// before its source: its target reaches the source again along the other
// edges, not counting edges that are bounded themselves. A self edge is not a
// loop the server accepts, so it is not offered one either.
//
// edges are { from, outcome, to } with from '' for the start pseudo edge.
export function edgeGoesBack(nodes, edges, from, outcome) {
  const edge = edges.find((candidate) => candidate.from === from && candidate.outcome === outcome);
  if (!edge || !from || edge.to === from || !nodes?.[edge.to]) return false;
  return reachableFrom(nodes, edges, edge.to, edge).has(from);
}

// reachableFrom is every node reachable from start along unbounded edges,
// start included, leaving out one edge (skip) if given.
function reachableFrom(nodes, edges, start, skip = null) {
  const out = new Map();
  for (const candidate of edges) {
    if (!candidate.from || candidate === skip || loopBound(nodes[candidate.from], candidate.outcome)) continue;
    if (!out.has(candidate.from)) out.set(candidate.from, []);
    out.get(candidate.from).push(candidate.to);
  }
  const seen = new Set([start]);
  const stack = [start];
  while (stack.length) {
    const id = stack.pop();
    for (const next of out.get(id) || []) {
      if (!seen.has(next)) {
        seen.add(next);
        stack.push(next);
      }
    }
  }
  return seen;
}

// cycleFixEdge is the decision connector that bounds an unbounded cycle
// through nodeID: one going back whose cycle passes nodeID, or null when the
// cycle has no decision on it. Ties go to the first by (from, outcome).
export function cycleFixEdge(nodes, edges, nodeID) {
  const candidates = edges
    .filter((edge) => edge.from && !loopBound(nodes[edge.from], edge.outcome)
      && loopableEdge(nodes, edges, edge.from, edge.outcome))
    .filter((edge) => {
      const ahead = reachableFrom(nodes, edges, edge.to, edge);
      return ahead.has(nodeID) && reachableFrom(nodes, edges, nodeID, edge).has(edge.from);
    })
    .sort((a, b) => a.from.localeCompare(b.from, 'en') || a.outcome.localeCompare(b.outcome, 'en'));
  return candidates.length ? { from: candidates[0].from, outcome: candidates[0].outcome } : null;
}

// loopableEdge reports whether the editor offers loop controls on an edge:
// a decision's edge that goes back.
export function loopableEdge(nodes, edges, from, outcome) {
  return nodes?.[from]?.type === 'decision' && edgeGoesBack(nodes, edges, from, outcome);
}

// exhaustionChoices are the outcomes a used-up loop may follow instead: the
// decision's other outcomes that do not go back themselves.
export function exhaustionChoices(node, outcomes, outcome) {
  return outcomes.filter((candidate) => candidate !== outcome && !loopBound(node, candidate));
}

// normalizeRounds parses an authored round count: an integer within the
// server's range, or null for a blank or out-of-range entry.
export function normalizeRounds(value) {
  const text = String(value ?? '').trim();
  if (!/^\d+$/.test(text)) return null;
  const rounds = Number(text);
  return rounds >= LOOP_MIN_ROUNDS && rounds <= LOOP_MAX_ROUNDS ? rounds : null;
}

// loopEdgeSuffix is the label suffix of a bounded edge: "≤3×" on a template,
// "round 2 of 3" when a run's round is known.
export function loopEdgeSuffix(maxIterations, round = 0) {
  if (!maxIterations) return '';
  return round > 0 ? `round ${round} of ${maxIterations}` : `≤${maxIterations}×`;
}

// loopSummary is the read-only wording of a bound: "max 3 rounds, then
// escalate", or "max 3 rounds" when a used-up loop refuses the outcome.
export function loopSummary(bound) {
  if (!bound) return '';
  const rounds = `max ${bound.maxIterations} round${bound.maxIterations === 1 ? '' : 's'}`;
  return bound.onExhausted ? `${rounds}, then ${bound.onExhausted}` : rounds;
}
