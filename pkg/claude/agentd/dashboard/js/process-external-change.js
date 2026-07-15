// Pure external-head awareness for the process template editor. The dashboard's
// existing snapshot cadence observes bounded template heads; this reducer
// turns an exact ref+source generation into editor review state without owning
// a poller or another authoritative representation of template content.

export const NO_EXTERNAL_CHANGE = Object.freeze({ kind: 'none', ref: '' });

function normalizedHead(head = {}) {
  return {
    ref: String(head.ref || head.currentRef || ''),
    sourceHash: String(head.sourceHash || ''),
    actor: String(head.actor || ''),
    authoredAt: String(head.authoredAt || ''),
  };
}

export function sameTemplateGeneration(a, b) {
  const left = normalizedHead(a); const right = normalizedHead(b);
  return !!left.ref && !!left.sourceHash
    && left.ref === right.ref && left.sourceHash === right.sourceHash;
}

export function reconcileExternalChange(previous, {
  loadedRef, loadedSourceHash, currentRef, currentSourceHash, actor, authoredAt, dirty,
} = {}) {
  const prior = previous || NO_EXTERNAL_CHANGE;
  const loaded = String(loadedRef || '');
  const loadedSource = String(loadedSourceHash || '');
  const current = String(currentRef || '');
  const currentSource = String(currentSourceHash || '');
  if (!loaded || !loadedSource || !current || !currentSource
      || (loaded === current && loadedSource === currentSource)) return NO_EXTERNAL_CHANGE;
  if (prior.kind === 'kept' && prior.ref === current && prior.sourceHash === currentSource) return prior;
  const kind = dirty ? 'dirty' : 'clean';
  if (prior.ref === current && prior.sourceHash === currentSource) {
    const nextActor = String(actor || prior.actor || '');
    const nextAuthoredAt = String(authoredAt || prior.authoredAt || '');
    if (prior.kind === kind && nextActor === (prior.actor || '') && nextAuthoredAt === (prior.authoredAt || '')) return prior;
    return { ...prior, kind, actor: nextActor, authoredAt: nextAuthoredAt };
  }
  return {
    kind, ref: current, sourceHash: currentSource,
    actor: String(actor || ''), authoredAt: String(authoredAt || ''),
  };
}

export function keepExternalChange(change) {
  const ref = String(change?.ref || '');
  const sourceHash = String(change?.sourceHash || '');
  return ref && sourceHash ? { ...change, kind: 'kept', ref, sourceHash } : NO_EXTERNAL_CHANGE;
}

// The edit view carries append-only authorship. Select only the event matching
// its exact ref + sourceHash; absence stays unknown rather than falling back to
// whichever actor happened to author another source-only generation.
export function templateHeadFromEditView(view = {}) {
  const generation = normalizedHead(view);
  const head = { ref: generation.ref, sourceHash: generation.sourceHash, actor: '', authoredAt: '' };
  const authorship = Array.isArray(view.authorship) ? view.authorship : [];
  for (let i = authorship.length - 1; i >= 0; i -= 1) {
    const event = authorship[i] || {};
    if (String(event.ref || '') !== head.ref || String(event.sourceHash || '') !== head.sourceHash) continue;
    head.actor = String(event.actor || '');
    head.authoredAt = String(event.authoredAt || '');
    break;
  }
  return head;
}

function stable(value) {
  if (Array.isArray(value)) return value.map(stable);
  if (!value || typeof value !== 'object') return value;
  return Object.fromEntries(Object.keys(value).sort().map((key) => [key, stable(value[key])]));
}

function changed(a, b) {
  return JSON.stringify(stable(a)) !== JSON.stringify(stable(b));
}

function edgeKey(edge = {}) { return `${edge.from || ''}\u0000${edge.outcome || ''}\u0000${edge.to || ''}`; }

export function summarizeTemplateChange(before = {}, after = {}) {
  const oldNodes = before.template?.nodes || {}; const newNodes = after.template?.nodes || {};
  const oldIDs = new Set(Object.keys(oldNodes)); const newIDs = new Set(Object.keys(newNodes));
  const addedNodes = [...newIDs].filter((id) => !oldIDs.has(id)).sort();
  const removedNodes = [...oldIDs].filter((id) => !newIDs.has(id)).sort();
  const changedNodes = [...newIDs].filter((id) => oldIDs.has(id) && changed(oldNodes[id], newNodes[id])).sort();
  const oldEdges = new Map((before.edges || []).map((edge) => [edgeKey(edge), edge]));
  const newEdges = new Map((after.edges || []).map((edge) => [edgeKey(edge), edge]));
  const addedEdges = [...newEdges.keys()].filter((key) => !oldEdges.has(key)).length;
  const removedEdges = [...oldEdges.keys()].filter((key) => !newEdges.has(key)).length;

  const oldLines = typeof before.source === 'string' ? before.source.split('\n') : null;
  const newLines = typeof after.source === 'string' ? after.source.split('\n') : null;
  let source = null;
  if (oldLines && newLines) {
    let prefix = 0;
    while (prefix < oldLines.length && prefix < newLines.length && oldLines[prefix] === newLines[prefix]) prefix += 1;
    let suffix = 0;
    while (suffix < oldLines.length - prefix && suffix < newLines.length - prefix
        && oldLines[oldLines.length - 1 - suffix] === newLines[newLines.length - 1 - suffix]) suffix += 1;
    const beforeChanged = oldLines.slice(prefix, oldLines.length - suffix);
    const afterChanged = newLines.slice(prefix, newLines.length - suffix);
    source = {
      firstLine: prefix + 1, removedLines: beforeChanged.length, addedLines: afterChanged.length,
      before: beforeChanged.slice(0, 6), after: afterChanged.slice(0, 6),
      truncated: beforeChanged.length > 6 || afterChanged.length > 6,
    };
  }
  return {
    addedNodes, removedNodes, changedNodes, addedEdges, removedEdges, source,
    metadataChanged: changed(
      { ...before.template, nodes: undefined }, { ...after.template, nodes: undefined },
    ),
  };
}

export function attachExternalReview(change, view, baseline) {
  if (!sameTemplateGeneration(change, view)) return change;
  return { ...change, review: { view, summary: summarizeTemplateChange(baseline, view) } };
}

export function templateHeadSignature(heads) {
  return JSON.stringify((heads || []).map((head) => ({
    id: String(head?.id || ''), ref: String(head?.ref || ''), sourceHash: String(head?.sourceHash || ''),
    actor: String(head?.actor || ''), authoredAt: String(head?.authoredAt || ''),
  })).filter((head) => head.id).sort((a, b) => a.id.localeCompare(b.id, 'en')));
}
