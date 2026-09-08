// The one legacy authoring-only loop. It never enables runtime retries.
export function processEscalations(graph) {
  const nodes = new Map(graph.Nodes.map(node => [node.ID, node]));
  const compound = node => node?.Kind === 'task' && node.Stages &&
    (node.Stages.Plan || node.Stages.PlanApproval || node.Stages.Checks?.length || node.Stages.Review);
  const failure = verdict => ['fail', 'failed', 'failure', 'error'].includes(verdict);
  const decisions = new Map(), sources = new Map(), retries = new Set(), errors = [];
  for (const edge of graph.Edges || []) {
    if (!compound(nodes.get(edge.From)) || !failure(edge.Verdict) || nodes.get(edge.To)?.Kind !== 'decision') continue;
    if ((decisions.has(edge.To) && decisions.get(edge.To) !== edge.From) ||
        (sources.has(edge.From) && sources.get(edge.From) !== edge.To)) {
      errors.push('Each compound escalation must have its own single decision.');
    }
    decisions.set(edge.To, edge.From); sources.set(edge.From, edge.To);
  }
  for (const [id, source] of decisions) {
    const node = nodes.get(id), answers = node.Decision?.PermittedAnswers || [];
    const outgoing = (graph.Edges || []).map((edge, index) => ({edge, index})).filter(item => item.edge.From === id);
    const retry = outgoing.find(item => item.edge.Verdict === 'retry');
    const cancel = outgoing.find(item => item.edge.Verdict === 'cancel');
    const end = nodes.get(cancel?.edge.To);
    const incoming = (graph.Edges || []).filter(edge => edge.To === id);
    if (id === graph.EntryNodeID || answers.length !== 2 || !answers.includes('retry') || !answers.includes('cancel') ||
        outgoing.length !== 2 || !retry || !cancel || retry.edge.To !== source ||
        end?.Kind !== 'end' || end.End?.Outcome !== 'cancelled' ||
        incoming.some(edge => edge.From !== source || !failure(edge.Verdict))) {
      errors.push(`${node.Name || id}: escalation needs exactly retry back to its compound task and cancel to a cancelled end, with no other incoming route.`);
      continue;
    }
    retries.add(retry.index);
  }
  return {retries: errors.length ? new Set() : retries, errors};
}
