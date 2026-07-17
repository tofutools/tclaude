// Pure external-head awareness for the process template editor. The dashboard's
// existing snapshot cadence observes bounded template heads; this reducer
// turns an exact ref+source generation into editor review state without owning
// a poller or another authoritative representation of template content.

export const NO_EXTERNAL_CHANGE = Object.freeze({ kind: 'none', ref: '' });
export const CHANGE_SUMMARY_LIMITS = Object.freeze({
  nodeIDs: 12,
  // Every listed ID includes its shortening marker inside both limits. The
  // complete summary ceilings cover JSON.stringify(summary); the rendered
  // ceilings cover the graph and source textContent combined. They reserve
  // room for every category plus worst-case JSON escaping of bounded text.
  nodeIDCharacters: 96,
  nodeIDBytes: 192,
  sourceLinesPerSide: 4,
  sourceCharactersPerLine: 240,
  sourceBytesPerLine: 512,
  sourceCharacters: 1920,
  sourceBytes: 4096,
  serializedCharacters: 65536,
  serializedBytes: 65536,
  renderedCharacters: 8192,
  renderedBytes: 16384,
});

export const CHANGE_SUMMARY_MARKERS = Object.freeze({
  shortenedNodeID: '… [ID shortened]',
  omittedNodeIDs: 'more IDs omitted',
});

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
  const head = {
    ref: generation.ref, sourceHash: generation.sourceHash,
    actor: generation.actor, authoredAt: generation.authoredAt,
  };
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

// start and nodes[*].next are projections of the editor wire's edges array.
// Strip only those derived fields on both sides so a post-save baseline built
// from the submitted editor view compares like the canonical committed GET.
// The start pseudo-edge and every real edge remain in the separate exact edge
// comparison below, so a real topology change is still reported.
function comparisonTemplate(template = {}) {
  const { start: _start, nodes, ...metadata } = template || {};
  const normalizedNodes = Object.fromEntries(Object.entries(nodes || {}).map(([id, node]) => {
    const { next: _next, ...semantic } = node || {};
    return [id, semantic];
  }));
  return { metadata, nodes: normalizedNodes };
}

const utf8 = new TextEncoder();
const graphemes = typeof Intl.Segmenter === 'function'
  ? new Intl.Segmenter(undefined, { granularity: 'grapheme' }) : null;

function scalarAt(value, offset) {
  const first = value.charCodeAt(offset);
  if (first >= 0xD800 && first <= 0xDBFF) {
    const second = value.charCodeAt(offset + 1);
    if (second >= 0xDC00 && second <= 0xDFFF) {
      return { value: value.slice(offset, offset + 2), width: 2, bytes: 4, replaced: false };
    }
    return { value: '\uFFFD', width: 1, bytes: 3, replaced: true };
  }
  if (first >= 0xDC00 && first <= 0xDFFF) {
    return { value: '\uFFFD', width: 1, bytes: 3, replaced: true };
  }
  const scalar = value[offset];
  return {
    value: scalar,
    width: 1,
    bytes: first <= 0x7F ? 1 : first <= 0x7FF ? 2 : 3,
    replaced: false,
  };
}

// Walk only enough Unicode scalars to fill the short preview. This avoids a
// TextEncoder result (and another proportional allocation) for a near-4 MiB
// valid ID. Intl.Segmenter then selects complete extended grapheme clusters
// from that bounded prefix, covering combining marks, emoji modifiers, ZWJ
// sequences, and flags. Malformed UTF-16 is replaced so the preview itself
// always round-trips through UTF-8.
function boundedNodeID(value) {
  const id = String(value ?? '');
  const units = [];
  let offset = 0; let bytes = 0; let replaced = false;
  while (offset < id.length && units.length <= CHANGE_SUMMARY_LIMITS.nodeIDCharacters
      && bytes <= CHANGE_SUMMARY_LIMITS.nodeIDBytes) {
    const scalar = scalarAt(id, offset);
    units.push(scalar);
    bytes += scalar.bytes;
    replaced ||= scalar.replaced;
    offset += scalar.width;
  }
  if (offset === id.length && units.length <= CHANGE_SUMMARY_LIMITS.nodeIDCharacters
      && bytes <= CHANGE_SUMMARY_LIMITS.nodeIDBytes && !replaced) return id;

  const marker = CHANGE_SUMMARY_MARKERS.shortenedNodeID;
  const markerCharacters = [...marker].length;
  const markerBytes = utf8.encode(marker).length;
  const characterBudget = CHANGE_SUMMARY_LIMITS.nodeIDCharacters - markerCharacters;
  const byteBudget = CHANGE_SUMMARY_LIMITS.nodeIDBytes - markerBytes;
  // Older engines without Segmenter get a conservative marker-only preview;
  // exact in-budget IDs already returned above and remain fully visible.
  if (!graphemes) return marker;
  const candidate = units.map((scalar) => scalar.value).join('');
  const candidateEndsAtInputEnd = offset === id.length;
  let preview = ''; let previewCharacters = 0; let previewBytes = 0;
  for (const part of graphemes.segment(candidate)) {
    const end = part.index + part.segment.length;
    if (end === candidate.length && !candidateEndsAtInputEnd) break;
    const characters = [...part.segment].length;
    const segmentBytes = utf8.encode(part.segment).length;
    if (previewCharacters + characters > characterBudget || previewBytes + segmentBytes > byteBudget) break;
    preview += part.segment;
    previewCharacters += characters;
    previewBytes += segmentBytes;
  }
  return preview + marker;
}

function boundedNodeIDs(ids) {
  const total = ids.length;
  return {
    ids: ids.slice(0, CHANGE_SUMMARY_LIMITS.nodeIDs).map(boundedNodeID),
    total,
    truncated: total > CHANGE_SUMMARY_LIMITS.nodeIDs,
  };
}

function truncateSourceLine(value, characterBudget, byteBudget) {
  const line = String(value || '');
  const characterLimit = Math.max(0, Math.min(CHANGE_SUMMARY_LIMITS.sourceCharactersPerLine, characterBudget));
  const byteLimit = Math.max(0, Math.min(CHANGE_SUMMARY_LIMITS.sourceBytesPerLine, byteBudget));
  let preview = line.slice(0, characterLimit);
  if (preview && /[\uD800-\uDBFF]$/.test(preview)) preview = preview.slice(0, -1);
  let byteTruncated = false;
  if (utf8.encode(preview).length > byteLimit) {
    let low = 0; let high = preview.length;
    while (low < high) {
      const middle = Math.ceil((low + high) / 2);
      if (utf8.encode(preview.slice(0, middle)).length <= byteLimit) low = middle;
      else high = middle - 1;
    }
    preview = preview.slice(0, low);
    if (preview && /[\uD800-\uDBFF]$/.test(preview)) preview = preview.slice(0, -1);
    byteTruncated = true;
  }
  return {
    value: preview,
    characters: preview.length,
    bytes: utf8.encode(preview).length,
    characterTruncated: line.length > characterLimit,
    byteTruncated,
    truncated: preview.length < line.length,
  };
}

function boundedSourcePreview(beforeLines, afterLines) {
  let characters = CHANGE_SUMMARY_LIMITS.sourceCharacters;
  let bytes = CHANGE_SUMMARY_LIMITS.sourceBytes;
  let characterTruncated = false; let byteTruncated = false;
  const take = (lines) => {
    const result = [];
    for (const line of lines.slice(0, CHANGE_SUMMARY_LIMITS.sourceLinesPerSide)) {
      const bounded = truncateSourceLine(line, characters, bytes);
      result.push(bounded.value);
      characters -= bounded.characters;
      bytes -= bounded.bytes;
      characterTruncated ||= bounded.characterTruncated;
      byteTruncated ||= bounded.byteTruncated;
    }
    return result;
  };
  const before = take(beforeLines); const after = take(afterLines);
  return {
    before, after,
    lineTruncated: beforeLines.length > before.length || afterLines.length > after.length,
    characterTruncated,
    byteTruncated,
  };
}

export function summarizeTemplateChange(before = {}, after = {}) {
  const oldComparison = comparisonTemplate(before.template); const newComparison = comparisonTemplate(after.template);
  const oldNodes = oldComparison.nodes; const newNodes = newComparison.nodes;
  const oldIDs = new Set(Object.keys(oldNodes)); const newIDs = new Set(Object.keys(newNodes));
  const added = boundedNodeIDs([...newIDs].filter((id) => !oldIDs.has(id)).sort());
  const removed = boundedNodeIDs([...oldIDs].filter((id) => !newIDs.has(id)).sort());
  const changedSet = boundedNodeIDs([...newIDs].filter((id) => oldIDs.has(id) && changed(oldNodes[id], newNodes[id])).sort());
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
    const preview = boundedSourcePreview(beforeChanged, afterChanged);
    source = {
      firstLine: prefix + 1, removedLines: beforeChanged.length, addedLines: afterChanged.length,
      before: preview.before, after: preview.after,
      truncated: preview.lineTruncated || preview.characterTruncated || preview.byteTruncated,
      truncation: {
        lines: preview.lineTruncated,
        characters: preview.characterTruncated,
        bytes: preview.byteTruncated,
      },
    };
  }
  return {
    addedNodes: added.ids, addedNodeCount: added.total, addedNodesTruncated: added.truncated,
    removedNodes: removed.ids, removedNodeCount: removed.total, removedNodesTruncated: removed.truncated,
    changedNodes: changedSet.ids, changedNodeCount: changedSet.total, changedNodesTruncated: changedSet.truncated,
    addedEdges, removedEdges, source,
    metadataChanged: changed(oldComparison.metadata, newComparison.metadata),
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
