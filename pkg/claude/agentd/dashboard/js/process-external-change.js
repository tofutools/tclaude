// Pure external-head awareness for the process template editor. The dashboard's
// existing snapshot cadence refreshes the templates list; this reducer turns
// the list's latest ref into an editor banner state without owning a poller.

export const NO_EXTERNAL_CHANGE = Object.freeze({ kind: 'none', ref: '' });

export function reconcileExternalChange(previous, {
  loadedRef, loadedSourceHash, currentRef, currentSourceHash, dirty,
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
  if (prior.kind === kind && prior.ref === current && prior.sourceHash === currentSource) return prior;
  return { kind, ref: current, sourceHash: currentSource };
}

export function keepExternalChange(change) {
  const ref = String(change?.ref || '');
  const sourceHash = String(change?.sourceHash || '');
  return ref && sourceHash ? { kind: 'kept', ref, sourceHash } : NO_EXTERNAL_CHANGE;
}

export function templateHeadSignature(heads) {
  return JSON.stringify((heads || []).map((head) => ({
    id: String(head?.id || ''), ref: String(head?.ref || ''), sourceHash: String(head?.sourceHash || ''),
  })).filter((head) => head.id).sort((a, b) => a.id.localeCompare(b.id, 'en')));
}
