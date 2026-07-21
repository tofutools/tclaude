import { h, Fragment } from 'preact';
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ProcessGraph } from './process-graph.js';
import {
  VIEWER_DETAIL_TABS, VIEWER_PAGE_LIMIT, buildViewerGraph, detailPage,
  detailRowCells, epochV8Summary, sanitizedTimeline, shortViewerID, viewerStateChips, viewerUnavailable,
} from './process-viewer-core.js';

const html = htm.bind(h);
export const VIEWER_REFRESH_MS = 5000;

function detailTabID(key) {
  return `process-viewer-detail-tab-${key}`;
}

function detailPanelID(key) {
  return `process-viewer-detail-panel-${key}`;
}

function ViewerGraph({ graph, runID }) {
  const hostRef = useRef(null);
  const widgetRef = useRef(null);
  const widgetRunRef = useRef('');
  const graphRef = useRef(null);
  useLayoutEffect(() => {
    if (!hostRef.current || !graph) return undefined;
    if (!widgetRef.current || widgetRunRef.current !== runID) {
      widgetRef.current?.destroy();
      widgetRef.current = new ProcessGraph(hostRef.current, graph, {
        ariaLabel: `Exact template and checkpoint routing graph for run ${runID}`,
        colorScheme: 'dark', wheelPan: true, marqueeSelect: false, fitOnRender: false,
      });
      globalThis.requestAnimationFrame?.(() => {
        if (widgetRef.current && widgetRunRef.current === runID) widgetRef.current.fitToView();
      });
      widgetRunRef.current = runID;
      graphRef.current = graph;
    } else if (graphRef.current !== graph) {
      widgetRef.current.setGraph(graph);
      graphRef.current = graph;
    }
    return undefined;
  });
  useLayoutEffect(() => () => {
    widgetRef.current?.destroy();
    widgetRef.current = null;
    widgetRunRef.current = '';
    graphRef.current = null;
  }, []);
  return html`<div ref=${hostRef} class="process-viewer-graph" data-process-viewer-graph></div>`;
}

function ViewerCell({ value }) {
  const text = String(value ?? '—');
  if (text.length < 20) return text;
  return html`<span class="process-viewer-id" title=${text}>${shortViewerID(text)}</span>`;
}

function DetailTable({ tab, routing, onPage }) {
  const detail = detailPage(routing, tab.key);
  const page = detail.page;
  const start = page.total ? Math.min(page.offset + 1, page.total) : 0;
  const end = Math.min(page.offset + detail.items.length, page.total);
  return html`<${Fragment}>
    <div class="process-viewer-detail-summary">
      <span>${page.total ? `${start}–${end} of ${page.total}` : 'No records'}</span>
      <span class="spacer"></span>
      <button class="process-action" type="button" disabled=${page.offset <= 0} onClick=${() => onPage(Math.max(0, page.offset - page.limit))}>← previous</button>
      <button class="process-action" type="button" disabled=${!page.hasMore} onClick=${() => onPage(page.offset + page.limit)}>next →</button>
    </div>
    <div class="process-viewer-table-wrap">
      <table class="process-viewer-table"><thead><tr>${tab.columns.map((column) => html`<th key=${column}>${column}</th>`)}</tr></thead><tbody>
        ${detail.items.length ? detail.items.map((row, index) => html`<tr key=${row.id || row.digest || row.pathId || `${tab.key}-${index}`}>${detailRowCells(tab.key, row).map((value, cell) => html`<td key=${cell}><${ViewerCell} value=${value} /></td>`)}</tr>`) : html`<tr><td colspan=${tab.columns.length} class="process-viewer-empty">No ${tab.label.toLowerCase()} in this checkpoint page.</td></tr>`}
      </tbody></table>
    </div>
  </${Fragment}>`;
}

function EvidenceTimeline({ envelope }) {
  const timeline = sanitizedTimeline(envelope);
  return html`<section class="process-viewer-timeline" aria-labelledby="process-viewer-evidence-title">
    <div class="process-viewer-section-head"><h4 id="process-viewer-evidence-title">Sanitized evidence timeline</h4><span class="process-viewer-authority">Timeline only — never topology or overlay authority</span></div>
    ${timeline.length ? html`<ol>${timeline.map((entry, index) => html`<li key=${`${entry.node}-${entry.seq}-${index}`}><span class="process-viewer-seq">#${entry.seq || '—'}</span><strong>${entry.node}</strong><span>${entry.event || entry.kind || 'event'}</span>${entry.outcome && html`<span>outcome ${entry.outcome}</span>`}${entry.verdict && html`<span>verdict ${entry.verdict}</span>`}${entry.evidenceRef && html`<${ViewerCell} value=${entry.evidenceRef} />`}</li>`)}</ol>` : html`<p class="process-viewer-empty">No sanitized evidence timeline is available for this run.</p>`}
  </section>`;
}

// EpochSummaryPanel renders the schema-8 safe summary: lineage, structural
// totals, per-state authority counts, the outstanding owner-epoch work
// entries, and the bounded history timeline. Everything shown is counts,
// refs, and bounded labels from the safe envelope — exact diffs and reasons
// stay behind the permissioned artifact route and are never fetched here.
// ExactArtifactViewer fetches one restricted diff/reason artifact on explicit
// request. Content lives only in component state (never persisted, never in
// title attributes) and every denial renders as an explicit bounded state.
function ExactArtifactViewer({ runId, epochs, actions }) {
  const [open, setOpen] = useState(null);
  const [busy, setBusy] = useState(false);
  const applied = epochs.filter((entry) => entry.ordinal > 0 && entry.epochId);
  if (!applied.length || !actions.loadExactArtifact) return null;
  const fetchArtifact = async (entry, kind) => {
    setBusy(true);
    try {
      const result = await actions.loadExactArtifact(runId, entry.epochId, kind);
      let error = '';
      if (!result.ok) {
        if (result.status === 401 || result.status === 403) error = 'Restricted: reading exact artifacts requires the process.runs.unlock.read permission.';
        else if (result.status === 404) error = 'This epoch has no such artifact.';
        else if (result.status === 409) error = 'Artifact is not coherent with the run checkpoint.';
        else if (result.status === 413) error = 'Artifact exceeds its read budget.';
        else error = `Artifact read failed (${result.status}).`;
      }
      setOpen({ ordinal: entry.ordinal, kind, text: error ? '' : result.text, error });
    } catch (fetchError) {
      setOpen({ ordinal: entry.ordinal, kind, text: '', error: `Artifact read failed: ${fetchError.message}` });
    } finally { setBusy(false); }
  };
  return html`<div class="process-epoch-artifacts">
    <h5 class="process-epoch-subhead" id="process-epoch-artifacts-title">Exact artifacts (restricted)</h5>
    <div class="process-epoch-artifact-actions" role="group" aria-labelledby="process-epoch-artifacts-title">
      ${applied.map((entry) => html`<span key=${entry.ordinal} class="process-epoch-artifact-row">epoch ${entry.ordinal}:
        <button class="process-action" type="button" disabled=${busy} onClick=${() => fetchArtifact(entry, 'diff')}>exact diff</button>
        <button class="process-action" type="button" disabled=${busy} onClick=${() => fetchArtifact(entry, 'reason')}>reason</button>
      </span>`)}
    </div>
    ${open && html`<div class="process-epoch-artifact-view">
      <div class="process-viewer-section-head"><h6>epoch ${open.ordinal} · ${open.kind}</h6><button class="process-action" type="button" onClick=${() => setOpen(null)}>close</button></div>
      ${open.error ? html`<p class="island-error" role="alert">${open.error}</p>` : html`<pre class="process-epoch-artifact-pre">${open.text}</pre>`}
    </div>`}
  </div>`;
}

// UnlockPanel is the memory-only Draft → Preview → Apply surface. Candidate
// source, reason, handoff choices, blockers, and tokens live exclusively in
// component state: never dashPrefs, storage, URLs, titles, or notices. A
// binding change invalidates the preview and every token while preserving
// the dirty draft verbatim; apply is reachable only through an explicit
// confirmation that restates the preview.
// The server rejects candidate sources over 4 MiB; the panel refuses them
// client-side with the same bound, before reading or serializing anything.
export const MAX_UNLOCK_SOURCE_BYTES = 4 * 1024 * 1024;

function encodedByteLength(text) {
  return new TextEncoder().encode(text).length;
}

function UnlockPanel({ runId, epoch, actions }) {
  const [draft, setDraft] = useState({ source: '', reason: '' });
  const [handoffs, setHandoffs] = useState({});
  const [base, setBase] = useState(null);
  const [preview, setPreview] = useState(null);
  const [stale, setStale] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [confirming, setConfirming] = useState(false);
  const [applied, setApplied] = useState(null);
  const dirty = draft.source.trim() !== '' || draft.reason.trim() !== '';
  const bindingDigest = epoch.binding.digest;
  // The latest observed binding and a request generation couple every
  // in-flight preview to the state it was captured against: a response that
  // resolves after the binding moved (or after a newer preview started) is
  // discarded instead of installing stale tokens.
  const bindingRef = useRef(epoch.binding);
  bindingRef.current = epoch.binding;
  const generationRef = useRef(0);
  useEffect(() => {
    if (preview && preview.baseDigest !== bindingDigest) {
      setPreview(null);
      setConfirming(false);
      setStale(`Run binding moved to revision ${epoch.binding.revision} — the preview and its tokens were discarded. Your draft is unchanged; re-preview to continue.`);
    }
  }, [bindingDigest]);
  if (!actions.previewUnlock) return null;
  const invalidate = (currentBinding, message) => {
    setPreview(null);
    setConfirming(false);
    if (currentBinding) setBase(currentBinding);
    setStale(message);
  };
  const runPreview = async () => {
    if (encodedByteLength(draft.source) > MAX_UNLOCK_SOURCE_BYTES) {
      setError('Candidate source exceeds the 4 MiB ceiling; nothing was sent.');
      return;
    }
    setBusy(true); setError(''); setStale(''); setApplied(null);
    const generation = ++generationRef.current;
    const captured = base || { revision: epoch.binding.revision, digest: epoch.binding.digest };
    setBase(captured);
    const payload = { baseBinding: captured, candidateSource: draft.source, handoffs: handoffValues(handoffs) };
    if (draft.reason.trim() !== '') payload.reason = draft.reason;
    try {
      const result = await actions.previewUnlock(runId, payload);
      if (generation !== generationRef.current) return; // superseded by a newer preview
      if (captured.digest !== bindingRef.current.digest) {
        invalidate(bindingRef.current, `Run binding moved to revision ${bindingRef.current.revision} while the preview was in flight — its result and tokens were discarded. Your draft is unchanged; re-preview to continue.`);
        setBusy(false);
        return;
      }
      if (result.status === 409 && result.body?.status === 'stale') {
        invalidate(result.body.currentBinding, `Run binding moved to revision ${result.body.currentBinding?.revision} — re-preview against the new binding. Your draft is unchanged.`);
      } else if (result.ok || result.status === 422) {
        if (result.body?.blockers || result.body?.applyToken || result.body?.status) {
          setPreview({ body: result.body, baseDigest: captured.digest });
          const next = {};
          for (const blocker of result.body.blockers || []) {
            if (blocker.token) next[blocker.token] = handoffs[blocker.token] || { action: '', local: '', reservation: '', node: '' };
          }
          setHandoffs(next);
        } else {
          setError(`Preview failed (${result.status}): ${result.body?.error || 'invalid input'}`);
        }
      } else {
        setError(`Preview failed (${result.status}): ${result.body?.error || 'request rejected'}`);
      }
    } catch (previewError) { setError(`Preview failed: ${previewError.message}`); }
    setBusy(false);
  };
  const runApply = async () => {
    if (encodedByteLength(draft.source) > MAX_UNLOCK_SOURCE_BYTES) {
      setError('Candidate source exceeds the 4 MiB ceiling; nothing was sent.');
      setConfirming(false);
      return;
    }
    setBusy(true); setError(''); setConfirming(false);
    const payload = {
      baseBinding: base, applyToken: preview.body.applyToken,
      candidateSource: draft.source, handoffs: handoffValues(handoffs),
    };
    if (draft.reason.trim() !== '') payload.reason = draft.reason;
    try {
      const result = await actions.applyUnlock(runId, payload);
      if (result.ok) {
        setApplied(result.body);
        setPreview(null);
        setBase(null);
      } else if (result.status === 409 && result.body?.status === 'stale') {
        invalidate(result.body.currentBinding, `Apply was refused: the binding moved to revision ${result.body.currentBinding?.revision}. Your draft is unchanged; re-preview to continue.`);
      } else if (result.status === 401 || result.status === 403) {
        setError('Apply denied: this caller does not hold the process.runs.unlock permission.');
      } else {
        setError(`Apply failed (${result.status}): ${result.body?.error || 'request rejected'}`);
      }
    } catch (applyError) { setError(`Apply failed: ${applyError.message}`); }
    setBusy(false);
  };
  const importFile = async (event) => {
    const file = event.currentTarget.files?.[0];
    event.currentTarget.value = '';
    if (!file) return;
    if (file.size > MAX_UNLOCK_SOURCE_BYTES) {
      setError('Candidate file exceeds the 4 MiB source ceiling; it was not read.');
      return;
    }
    const text = await file.text();
    if (encodedByteLength(text) > MAX_UNLOCK_SOURCE_BYTES) {
      setError('Candidate file exceeds the 4 MiB source ceiling after decoding; it was discarded.');
      return;
    }
    setError('');
    setDraft((current) => ({ ...current, source: text }));
    setPreview(null); setStale(''); setApplied(null);
  };
  const body = preview?.body;
  const ready = body?.status === 'valid' && body.applyToken;
  return html`<div class="process-unlock-panel">
    <h5 class="process-epoch-subhead" id="process-unlock-title">Unlock (adapt this run)</h5>
    <p class="process-secondary">Draft material stays in this panel's memory only. Base binding ${base ? `rev ${base.revision}` : `rev ${epoch.binding.revision}`}${dirty ? ' · unsaved draft' : ''}</p>
    ${stale && html`<div class="process-unlock-stale" role="status">${stale}</div>`}
    ${error && html`<div class="island-error" role="alert">${error}</div>`}
    ${applied && html`<div class="process-unlock-applied" role="status">Unlock ${applied.status} at epoch ${shortViewerID(applied.epochId, 16)} (revision ${applied.currentBinding?.revision}).</div>`}
    <label class="process-unlock-label" for="process-unlock-source">Candidate template YAML</label>
    <textarea id="process-unlock-source" class="process-unlock-source" rows="8" value=${draft.source}
      onInput=${(event) => { setDraft((current) => ({ ...current, source: event.currentTarget.value })); setApplied(null); }}></textarea>
    <div class="process-unlock-row">
      <label class="process-action process-unlock-import">import file<input type="file" accept=".yaml,.yml,.txt" hidden onChange=${importFile} /></label>
      <label class="process-unlock-label" for="process-unlock-reason">Reason (optional, restricted at rest)</label>
      <input id="process-unlock-reason" type="text" value=${draft.reason}
        onInput=${(event) => setDraft((current) => ({ ...current, reason: event.currentTarget.value }))} />
      <button class="process-action" type="button" disabled=${busy || draft.source.trim() === ''} onClick=${runPreview}>${preview ? 're-preview' : 'preview'}</button>
    </div>
    ${body && html`<div class="process-unlock-preview" role="status" aria-label="Unlock preview result">
      <p><strong>${body.status}</strong>${body.classification ? ` · ${body.classification}` : ''} · candidate ${body.graphSummary?.candidate?.nodes ?? '—'} nodes / ${body.graphSummary?.candidate?.edges ?? '—'} edges${body.graphSummary?.changed ? ' · changes template' : ''}</p>
      ${(body.blockers || []).length > 0 && html`<ul class="process-unlock-blockers">${body.blockers.map((blocker) => html`<li key=${blocker.token || blocker.code}>
        <code>${blocker.code}</code> ${blocker.nodeId && html`<strong>${blocker.nodeId}</strong>`} ${blocker.ownerEpochOrdinal !== undefined && blocker.ownerEpochOrdinal !== null && html`<span class="wl-epoch-badge">◈ epoch ${blocker.ownerEpochOrdinal}</span>`} ${blocker.stateClass || ''}
        ${blocker.token && html`<span class="process-unlock-handoff">
          <label>handoff <select value=${handoffs[blocker.token]?.action || ''} onChange=${(event) => setHandoffs((current) => ({ ...current, [blocker.token]: { ...current[blocker.token], action: event.currentTarget.value } }))}>
            <option value="">choose…</option>
            ${(blocker.allowedActions || []).map((action) => html`<option key=${action} value=${action}>${action === 'transfer_verified_unclaimed' ? 'transfer' : 'retain'}</option>`)}
          </select></label>
          ${handoffs[blocker.token]?.action === 'transfer_verified_unclaimed' && html`<span class="process-unlock-transfer">
            <label>local <input type="text" value=${handoffs[blocker.token]?.local || ''} onInput=${(event) => setHandoffs((current) => ({ ...current, [blocker.token]: { ...current[blocker.token], local: event.currentTarget.value } }))} /></label>
            <label>reservation <input type="text" value=${handoffs[blocker.token]?.reservation || ''} onInput=${(event) => setHandoffs((current) => ({ ...current, [blocker.token]: { ...current[blocker.token], reservation: event.currentTarget.value } }))} /></label>
            <label>node <input type="text" value=${handoffs[blocker.token]?.node || ''} onInput=${(event) => setHandoffs((current) => ({ ...current, [blocker.token]: { ...current[blocker.token], node: event.currentTarget.value } }))} /></label>
          </span>`}
        </span>`}
      </li>`)}</ul>`}
      ${body.guidance && html`<p class="process-secondary">Guidance: ${body.guidance.action} (requires ${body.guidance.permission})${body.guidance.repreviewRequired ? ' · re-preview required after settlement' : ''}</p>`}
      ${ready && html`<button class="process-action process-unlock-apply" type="button" disabled=${busy} onClick=${() => setConfirming(true)}>apply…</button>`}
    </div>`}
    ${confirming && ready && html`<div class="process-unlock-confirm" role="alertdialog" aria-modal="false" aria-labelledby="process-unlock-confirm-title">
      <strong id="process-unlock-confirm-title">Apply this unlock?</strong>
      <p>Run ${runId} at binding rev ${base?.revision}; classification ${body.classification || 'n/a'}; ${Object.keys(handoffs).length} handoff${Object.keys(handoffs).length === 1 ? '' : 's'}. This mutation requires process.runs.unlock.</p>
      <div class="wl-action-row">
        <button class="process-action" type="button" autofocus onClick=${runApply}>apply now</button>
        <button class="process-action" type="button" onClick=${() => setConfirming(false)}>cancel</button>
      </div>
    </div>`}
  </div>`;
}

function handoffValues(handoffs) {
  return Object.entries(handoffs)
    .filter(([, choice]) => choice.action)
    .map(([token, choice]) => {
      const directive = { token, action: choice.action };
      if (choice.action === 'transfer_verified_unclaimed') {
        directive.target = { localId: choice.local || '', reservationId: choice.reservation || '', nodeId: choice.node || '' };
      }
      return directive;
    });
}

function EpochSummaryPanel({ epoch, runId, actions }) {
  return html`<section class="process-epoch-summary" aria-labelledby="process-epoch-summary-title">
    <div class="process-viewer-section-head"><h4 id="process-epoch-summary-title">Adaptation summary</h4><span>binding rev ${epoch.binding.revision}</span></div>
    <dl class="process-epoch-lineage">
      <div><dt>Original template</dt><dd><span class="process-hash" title=${epoch.originalTemplateRef}>${shortViewerID(epoch.originalTemplateRef, 36)}</span></dd></div>
      <div><dt>Current template</dt><dd><span class="process-hash" title=${epoch.currentTemplateRef}>${shortViewerID(epoch.currentTemplateRef, 36)}</span></dd></div>
      <div><dt>Epochs</dt><dd>${epoch.totalEpochs}${epoch.lineageTruncated ? ' (oldest and newest shown)' : ''}</dd></div>
      <div><dt>Current structure</dt><dd>${epoch.structural.nodes} nodes · ${epoch.structural.edges} edges${epoch.structural.changedFromOriginal ? ' · changed from original' : ''}</dd></div>
    </dl>
    ${epoch.adapted && html`<ol class="process-epoch-list" aria-label="Epoch lineage">${epoch.epochs.map((entry) => html`<li key=${entry.ordinal}><strong>epoch ${entry.ordinal}</strong> <span class="process-hash" title=${entry.templateRef}>${shortViewerID(entry.templateRef, 28)}</span></li>`)}</ol>`}
    <div class="process-viewer-state-chips" aria-label="Authority state counts">${epoch.stateChips.map(([label, value]) => html`<span key=${label}><strong>${label}</strong> ${value}</span>`)}</div>
    <h5 class="process-epoch-subhead" id="process-epoch-work-title">Outstanding work</h5>
    ${epoch.entries.length ? html`<ul class="process-epoch-entries" aria-labelledby="process-epoch-work-title">${epoch.entries.map((entry) => html`<li key=${entry.id}><span class="wl-epoch-badge">◈ epoch ${entry.ownerEpochOrdinal}</span> <strong>${entry.nodeId}</strong> ${entry.kind} · ${entry.status}${entry.attempt > 1 ? ` · attempt ${entry.attempt}` : ''}</li>`)}</ul>` : html`<p class="process-viewer-empty">No outstanding owner-epoch work.</p>`}
    ${epoch.entriesTruncated && html`<p class="process-secondary">Showing the first ${epoch.entries.length} of ${epoch.entriesTotal} outstanding items — the Worklist tab lists them all.</p>`}
    <h5 class="process-epoch-subhead" id="process-epoch-timeline-title">History</h5>
    ${epoch.timeline.length ? html`<ol class="process-epoch-timeline" aria-labelledby="process-epoch-timeline-title">${epoch.timeline.map((event) => html`<li key=${event.revision}>rev ${event.revision} · ${event.kind} · epoch ${event.epochOrdinal}${event.reasonCode ? ` · ${event.reasonCode}` : ''}${event.actorClass ? ` · by ${event.actorClass}` : ''}${event.appliedAt ? ` · ${event.appliedAt}` : ''}</li>`)}</ol>` : html`<p class="process-viewer-empty">No recorded history events.</p>`}
    ${epoch.timelineTruncated && html`<p class="process-secondary">Showing the newest ${epoch.timeline.length} of ${epoch.timelineTotal} events.</p>`}
    <${ExactArtifactViewer} runId=${runId} epochs=${epoch.epochs} actions=${actions} />
    <${UnlockPanel} runId=${runId} epoch=${epoch} actions=${actions} />
  </section>`;
}

export function ProcessViewerBoundary({
  spec, actions, active = true,
  setTimeoutImpl = globalThis.setTimeout,
  clearTimeoutImpl = globalThis.clearTimeout,
}) {
  const [request, setRequest] = useState({ phase: 'loading', envelope: null, error: '' });
  const [tabKey, setTabKey] = useState(VIEWER_DETAIL_TABS[0].key);
  const [offset, setOffset] = useState(0);
  const [reload, setReload] = useState(0);
  const generation = useRef(0);
  useEffect(() => {
    if (!active) return undefined;
    let mounted = true;
    let timer = null;
    const load = () => {
      const token = ++generation.current;
      setRequest((current) => ({ phase: current.envelope ? 'refreshing' : 'loading', envelope: current.envelope, error: '' }));
      Promise.resolve().then(() => actions.loadRunView(spec.id, offset, VIEWER_PAGE_LIMIT))
        .then((envelope) => { if (mounted && generation.current === token) setRequest({ phase: 'ready', envelope, error: '' }); })
        .catch((error) => { if (mounted && generation.current === token) setRequest((current) => ({ phase: 'error', envelope: current.envelope, error: error.message })); })
        .finally(() => {
          if (mounted && generation.current === token) timer = setTimeoutImpl(load, VIEWER_REFRESH_MS);
        });
    };
    load();
    return () => {
      mounted = false;
      generation.current += 1;
      if (timer !== null) clearTimeoutImpl(timer);
    };
  }, [spec.key, offset, reload, actions, active, setTimeoutImpl, clearTimeoutImpl]);

  const envelope = request.envelope;
  const epoch = epochV8Summary(envelope);
  const viewer = envelope?.viewerV2;
  const routing = viewer?.routingAvailable ? viewer.routing : null;
  const graph = useMemo(() => buildViewerGraph(envelope), [envelope]);
  const unavailable = viewerUnavailable(viewer);
  const tab = VIEWER_DETAIL_TABS.find((candidate) => candidate.key === tabKey) || VIEWER_DETAIL_TABS[0];
  const selectTab = (key) => { setTabKey(key); setOffset(0); };
  const selectTabFromKeyboard = (event, key) => {
    const index = VIEWER_DETAIL_TABS.findIndex((candidate) => candidate.key === key);
    let nextIndex;
    switch (event.key) {
      case 'ArrowLeft': nextIndex = (index - 1 + VIEWER_DETAIL_TABS.length) % VIEWER_DETAIL_TABS.length; break;
      case 'ArrowRight': nextIndex = (index + 1) % VIEWER_DETAIL_TABS.length; break;
      case 'Home': nextIndex = 0; break;
      case 'End': nextIndex = VIEWER_DETAIL_TABS.length - 1; break;
      default: return;
    }
    event.preventDefault();
    const next = VIEWER_DETAIL_TABS[nextIndex];
    const page = detailPage(routing, next.key).page;
    const limit = Math.max(1, page.limit || VIEWER_PAGE_LIMIT);
    const maxOffset = page.total > 0 ? Math.floor((page.total - 1) / limit) * limit : 0;
    setTabKey(next.key);
    if (offset > maxOffset) setOffset(maxOffset);
    globalThis.document?.getElementById(detailTabID(next.key))?.focus();
  };

  if (!envelope && request.phase === 'loading') return html`<div id="process-viewer-canvas" class="process-canvas-mount process-viewer" data-process-mount="viewer"><p class="muted">Loading exact process view…</p></div>`;
  if (!envelope) return html`<div id="process-viewer-canvas" class="process-canvas-mount process-viewer" data-process-mount="viewer"><div class="island-error" role="alert">Could not load run ${spec.id}: ${request.error} <button class="process-action" type="button" onClick=${() => setReload((value) => value + 1)}>retry</button></div></div>`;

  return html`<div id="process-viewer-canvas" class="process-canvas-mount process-viewer" data-process-mount="viewer" aria-busy=${request.phase === 'refreshing'}>
    ${request.phase === 'error' && html`<div class="island-error" role="alert">Refresh failed: ${request.error}</div>`}
    <header class="process-viewer-header">
      <div><span class="process-viewer-kicker">Live process view</span><h3>${envelope.run?.id || spec.id}</h3><div class="process-viewer-ref" title=${envelope.run?.templateRef || ''}>${shortViewerID(envelope.run?.templateRef, 36)}</div></div>
      <div class="process-viewer-run-state"><span class="process-status">${envelope.run?.effectiveStatus || 'unknown'}</span>${epoch?.adapted && html`<span class="process-adapted-badge">⟳ adapted</span>`}<span>schema ${viewer?.stateSchemaVersion || '—'}</span><span>${viewer?.pathProtocol || 'no path protocol'}</span></div>
    </header>
    <div class="process-viewer-authority-strip"><strong>Authority boundary:</strong> graph topology is the exact pinned template; routing decorations and counts are the current checkpoint. Evidence is rendered only in the timeline below.</div>
    ${unavailable && html`<div class=${`process-viewer-unavailable reason-${unavailable.reason}`} role="status"><span class="process-viewer-unavailable-glyph">⚠</span><div><strong>${unavailable.title}</strong><p>${unavailable.detail}</p><code>${unavailable.reason}</code></div></div>`}
    <div class="process-viewer-state-chips" aria-label="Checkpoint state counts">${viewerStateChips(routing).map(([label, value]) => html`<span key=${label}><strong>${label}</strong> ${value}</span>`)}</div>
    ${epoch && html`<${EpochSummaryPanel} epoch=${epoch} runId=${spec.id} actions=${actions} />`}
    <div class="process-viewer-main">
      <section class="process-viewer-graph-panel" aria-label="Exact process topology">${graph ? html`<${ViewerGraph} graph=${graph} runID=${spec.id} />` : html`<div class="process-placeholder"><h3>Exact topology unavailable</h3><p>The viewer failed closed and will not fall back to evidence-derived graph data.</p></div>`}</section>
      <section class="process-viewer-details" aria-labelledby="process-viewer-details-title">
        <div class="process-viewer-section-head"><h4 id="process-viewer-details-title">Checkpoint details</h4>${routing?.aggregate && html`<span>${routing.aggregate.paths} paths · ${routing.aggregate.reservations} generations</span>`}</div>
        ${routing ? html`<${Fragment}>
          <div class="process-viewer-tabs" role="tablist" aria-label="Routing detail tables">${VIEWER_DETAIL_TABS.map((candidate) => {
            const selected = candidate.key === tab.key;
            return html`<button
              key=${candidate.key} id=${detailTabID(candidate.key)} type="button" role="tab"
              aria-controls=${detailPanelID(candidate.key)} aria-selected=${selected} tabIndex=${selected ? '0' : '-1'}
              class=${selected ? 'active' : ''} onClick=${() => selectTab(candidate.key)}
              onKeyDown=${(event) => selectTabFromKeyboard(event, candidate.key)}
            >${candidate.label}<span>${routing.details?.[candidate.key]?.page?.total || 0}</span></button>`;
          })}</div>
          ${VIEWER_DETAIL_TABS.map((candidate) => {
            const selected = candidate.key === tab.key;
            return html`<div
              key=${candidate.key} id=${detailPanelID(candidate.key)} class="process-viewer-tabpanel"
              role="tabpanel" aria-labelledby=${detailTabID(candidate.key)} tabIndex=${selected ? '0' : '-1'}
              hidden=${!selected}
            >${selected && html`<${DetailTable} tab=${candidate} routing=${routing} onPage=${setOffset} />`}</div>`;
          })}
        </${Fragment}>` : html`<p class="process-viewer-empty">Checkpoint-derived detail tables are unavailable.</p>`}
      </section>
    </div>
    <${EvidenceTimeline} envelope=${envelope} />
  </div>`;
}
