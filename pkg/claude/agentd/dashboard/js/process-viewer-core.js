// Pure projection helpers for the live process viewer. Topology and overlays
// deliberately read only viewerV2 (exact template + current checkpoint).
// Sanitized report evidence is flattened separately for the timeline and can
// never influence graph shape or state decoration.

export const VIEWER_PAGE_LIMIT = 25;

export const VIEWER_DETAIL_TABS = Object.freeze([
  { key: 'generations', label: 'Generations', columns: ['Node', 'Generation', 'Policy', 'State', 'Winner'] },
  { key: 'scopes', label: 'Scopes', columns: ['Scope', 'Generation', 'State', 'Join', 'Close reason'] },
  { key: 'closures', label: 'Closures', columns: ['Reservation', 'Candidate', 'Terminal', 'Cause digest'] },
  { key: 'causeSets', label: 'Cause sets', columns: ['Digest', 'Cause IDs'] },
  { key: 'causes', label: 'Causes', columns: ['Cause', 'Terminal', 'Reason', 'Sequence'] },
  { key: 'detachments', label: 'Detachments', columns: ['Reservation', 'Candidate', 'Winner', 'Reason'] },
  { key: 'detachedSinks', label: 'Detached sinks', columns: ['Path', 'Source', 'Reservation', 'State', 'Reason'] },
]);

export const ROUTING_UNAVAILABLE = Object.freeze({
  legacy_schema: {
    title: 'Legacy run: routing overlay unavailable',
    detail: 'The exact pinned template is shown without reconstructing path state from legacy evidence.',
  },
  routing_absent: {
    title: 'Checkpoint routing is absent',
    detail: 'The exact pinned template is shown without an inferred overlay.',
  },
  unsupported_schema: {
    title: 'Unsupported run-state schema',
    detail: 'This dashboard cannot safely interpret the run topology or routing state.',
  },
  unsupported_protocol: {
    title: 'Unsupported routing protocol',
    detail: 'The exact template remains visible, but checkpoint routing is not interpreted.',
  },
  over_budget: {
    title: 'Viewer budget exceeded',
    detail: 'The dashboard failed closed instead of rendering a partial or misleading routing view.',
  },
  inconsistent: {
    title: 'Run and template are inconsistent',
    detail: 'No routing claims are rendered until the checkpoint and exact pinned template agree.',
  },
});

function countSummary(counts = []) {
  return counts
    .filter((entry) => entry?.count > 0)
    .map((entry) => `${entry.state} ${entry.count}`)
    .join(' · ');
}

function edgeOverlaySummary(items = []) {
  return items
    .filter((entry) => entry?.count > 0)
    .sort((a, b) => String(a.state).localeCompare(String(b.state), 'en'))
    .map((entry) => `${entry.state} ${entry.count}`)
    .join(' · ');
}

function overlaySeverity(items = []) {
  if (items.some((entry) => entry?.state === 'failed')) return 'error';
  if (items.some((entry) => ['impossible', 'canceled'].includes(entry?.state))) return 'warning';
  return 'info';
}

export function buildViewerGraph(envelope) {
  const viewer = envelope?.viewerV2 || {};
  const topology = viewer.exactTopology;
  if (!topology || !Array.isArray(topology.nodes) || !Array.isArray(topology.edges)) return null;
  const routing = viewer.routingAvailable ? viewer.routing : null;
  const byEdge = new Map();
  for (const overlay of routing?.edges || []) {
    if (!byEdge.has(overlay.edgeId)) byEdge.set(overlay.edgeId, []);
    byEdge.get(overlay.edgeId).push(overlay);
  }
  const nodeActivity = new Map();
  for (const edge of topology.edges) {
	if (!edge.from) continue; // the exact genesis edge is represented by topology.start
    const overlays = byEdge.get(edge.id) || [];
    if (!overlays.length) continue;
    const count = overlays.reduce((total, entry) => total + (entry.count || 0), 0);
    const target = nodeActivity.get(edge.to) || { count: 0, states: new Set() };
    target.count += count;
    overlays.forEach((entry) => target.states.add(entry.state));
    nodeActivity.set(edge.to, target);
  }
  const joins = new Map((routing?.joins || []).map((join) => [join.nodeId, join]));
  return {
    nodes: topology.nodes.map((node) => {
      const join = joins.get(node.id);
      const activity = nodeActivity.get(node.id);
      let overlay;
      if (join) {
        const glyph = join.policy === 'any' ? '∨' : '∧';
        const badges = [];
        if (join.winnerPathId) badges.push('winner selected');
        if (join.detached) badges.push(`${join.detached} detached`);
        overlay = {
          glyph, label: `${join.policy} · ${join.state}`,
          badge: badges.join(' · '),
          progress: `${join.arrived}/${join.arrived + join.open + join.impossible + join.failed + join.skipped + join.canceled}`,
        };
      } else if (activity) {
        const states = [...activity.states].sort((a, b) => String(a).localeCompare(String(b), 'en'));
        overlay = { glyph: '●', label: states.join(' + '), badge: `${activity.count} path${activity.count === 1 ? '' : 's'}` };
      }
      return { id: node.id, label: node.id, type: node.type || 'task', ...(overlay ? { overlay } : {}) };
    }),
    edges: topology.edges.filter((edge) => edge.from).map((edge) => {
      const overlays = byEdge.get(edge.id) || [];
      const summary = edgeOverlaySummary(overlays);
      const targetJoin = joins.get(edge.to);
      return {
        id: edge.id, from: edge.from, outcome: edge.outcome, to: edge.to,
        ...(targetJoin ? { joinOnTarget: targetJoin.policy } : {}),
        ...(summary ? { badge: summary, badgeSeverity: overlaySeverity(overlays), issues: [summary] } : {}),
      };
    }),
  };
}

export function viewerUnavailable(viewerV2) {
  if (viewerV2?.routingAvailable) return null;
  const reason = viewerV2?.routingUnavailableReason || 'unsupported_schema';
  return { reason, ...(ROUTING_UNAVAILABLE[reason] || ROUTING_UNAVAILABLE.unsupported_schema) };
}

export function viewerStateChips(routing) {
  if (!routing) return [];
  const counts = routing.stateCounts || {};
  return [
    ['Paths', countSummary(counts.paths)],
    ['Scopes', countSummary(counts.scopes)],
    ['Reservations', countSummary(counts.reservations)],
    ['Propagation', countSummary(counts.propagation)],
    ['Detached paths', String(counts.detachedPathCount || 0)],
    ['Detached sinks', String(counts.detachedSinkCount || 0)],
  ].filter(([, value]) => value && value !== '0');
}

export function detailPage(routing, key) {
  const value = routing?.details?.[key];
  return value && Array.isArray(value.items) && value.page
    ? value : { page: { offset: 0, limit: VIEWER_PAGE_LIMIT, total: 0, hasMore: false }, items: [] };
}

export function detailRowCells(key, row) {
  switch (key) {
    case 'generations': return [row.nodeId, row.generation, row.policy, `${row.reservationState}${row.receiptResult ? ` / ${row.receiptResult}` : ''}`, row.winnerPathId || '—'];
    case 'scopes': return [row.id, row.generation, row.state, row.joinNodeId || '—', row.closeReason || '—'];
    case 'closures': return [row.reservationId, row.candidateId, row.terminalKind, row.causeDigest];
    case 'causeSets': return [row.digest, (row.causeIds || []).join(', ') || '—'];
    case 'causes': return [row.id, row.terminalKind, row.dispositionReason, row.eventSeq];
    case 'detachments': return [row.reservationId, row.candidateId, row.winnerPathId, row.reasonCode];
    case 'detachedSinks': return [row.pathId, row.sourceActivationId, row.targetReservationId, row.state, row.reasonCode];
    default: return [];
  }
}

export function sanitizedTimeline(envelope) {
  const rows = [];
  for (const [node, report] of Object.entries(envelope?.report?.nodes || {})) {
    for (const entry of report?.timeline || []) rows.push({ node, ...entry });
  }
  return rows.sort((a, b) => (a.seq || 0) - (b.seq || 0) || String(a.node).localeCompare(String(b.node), 'en'));
}

export function shortViewerID(value, width = 12) {
  const text = String(value ?? '');
  return text.length > width + 1 ? `${text.slice(0, width)}…` : text || '—';
}
