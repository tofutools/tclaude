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
function EpochSummaryPanel({ epoch }) {
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
    <h5 class="process-epoch-subhead" id="process-epoch-timeline-title">History</h5>
    ${epoch.timeline.length ? html`<ol class="process-epoch-timeline" aria-labelledby="process-epoch-timeline-title">${epoch.timeline.map((event) => html`<li key=${event.revision}>rev ${event.revision} · ${event.kind} · epoch ${event.epochOrdinal}${event.reasonCode ? ` · ${event.reasonCode}` : ''}${event.actorClass ? ` · by ${event.actorClass}` : ''}${event.appliedAt ? ` · ${event.appliedAt}` : ''}</li>`)}</ol>` : html`<p class="process-viewer-empty">No recorded history events.</p>`}
    ${epoch.timelineTruncated && html`<p class="process-secondary">Showing the newest ${epoch.timeline.length} of ${epoch.timelineTotal} events.</p>`}
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
    ${epoch && html`<${EpochSummaryPanel} epoch=${epoch} />`}
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
