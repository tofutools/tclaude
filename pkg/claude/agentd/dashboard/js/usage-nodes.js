// usage-nodes.js — which node's quota the header usage shows (tcl-ut0w37).
// A fused view ticks several nodes; the header bars show one node's harness
// quota at a time (quota percentages do not add up across machines), this
// node by default. A node pill before the bars switches them; its menu lists
// every ticked node's peak quota so a tight node shows without switching. A
// peer's usage comes from its peer snapshot, under the costs share.
import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { usageView } from './shell-model.js';
import { featureNeeds, featureState, needsSuffix } from './peer-view-limits.js';
import { LockReason } from './lock-reason.js';
import { dashPrefs } from './prefs.js';
import { useDismissablePopover } from './hover-popover.js';

const html = htm.bind(h);
export const USAGE_NODE_PREF = 'tclaude.dash.headerUsageNode';
export const PEER_USAGE_POLL_MS = 60000;
const MENU_FRESH_MS = 30000;

// usagePeak is the highest quota percentage a usage payload shows, or null.
export function usagePeak(usage) {
  let peak = null;
  for (const line of usageView(usage).lines || []) {
    for (const token of line.tokens || []) {
      const pct = Number(token.pct);
      if (token.kind !== 'window' || token.hidden || !Number.isFinite(pct)) continue;
      peak = peak == null ? pct : Math.max(peak, pct);
    }
  }
  return peak;
}

// peerUsageState reads a peer snapshot entry as what the header can show:
// { usage } when it shares costs, else { why }.
export function peerUsageState(node, entry) {
  if (!entry) return { why: `loading ${node.name}…`, pending: true };
  if (entry.failure) return { why: `${node.name} is unreachable (${entry.failure})` };
  if (entry.peerView?.level !== 'unrestricted' && featureState('costs', entry.peerView) !== 'shared') {
    return { why: `${node.name} does not share usage or costs with you${needsSuffix([featureNeeds('costs', entry.peerView)])}` };
  }
  return { usage: entry.usage };
}

// useUsageNode picks the node the header usage shows among nodes (the
// ticked nodes, this one first) and keeps the chosen peer's usage fresh.
// It returns { nodes, selected, select, peer(id), refreshAll }.
export function useUsageNode({ nodes, selfID, fetchImpl = (...a) => globalThis.fetch(...a), timers = globalThis, now = () => Date.now() }) {
  const [pref, setPref] = useState(() => { try { return dashPrefs.getItem(USAGE_NODE_PREF) || ''; } catch (_) { return ''; } });
  const [entries, setEntries] = useState({});
  const inflight = useRef(new Set());
  const ids = nodes.map((n) => n.id);
  const selected = ids.includes(pref) ? pref : selfID;
  const load = async (id) => {
    if (id === selfID || inflight.current.has(id)) return;
    inflight.current.add(id);
    let entry;
    try {
      const response = await fetchImpl(`/api/peer/${encodeURIComponent(id)}/snapshot`);
      if (!response.ok) entry = { failure: `HTTP ${response.status}`, at: now() };
      else {
        const body = await response.json();
        entry = { usage: body?.usage || null, peerView: body?.peer_view || null, at: now() };
      }
    } catch (_) {
      entry = { failure: 'network', at: now() };
    } finally {
      inflight.current.delete(id);
    }
    setEntries((current) => ({ ...current, [id]: entry }));
  };
  useEffect(() => {
    if (selected === selfID) return undefined;
    void load(selected);
    const timer = timers.setInterval(() => { void load(selected); }, PEER_USAGE_POLL_MS);
    return () => timers.clearInterval(timer);
  }, [selected, selfID]);
  const select = (id) => {
    setPref(id);
    try { dashPrefs.setItem(USAGE_NODE_PREF, id); } catch (_) { /* a view preference only */ }
  };
  const refreshAll = () => {
    for (const id of ids) if (id !== selfID && !(entries[id]?.at > now() - MENU_FRESH_MS)) void load(id);
  };
  return { selected, select, peer: (id) => entries[id] || null, refreshAll };
}

// UsageNodePill is the pill before the bars and its node menu.
export function UsageNodePill({ nodes, selected, onSelect, peakOf, onOpen }) {
  const [open, setOpen] = useState(false);
  const root = useRef(null);
  useDismissablePopover(root, open, () => setOpen(false));
  const node = nodes.find((n) => n.id === selected) || nodes[0];
  const label = (n) => `${n.local ? '⌂ ' : ''}${n.name}`;
  return html`<span class="usage-node" ref=${root}>
    <button type="button" class="usage-node-pill" style=${`--nc:${node.color}`} aria-haspopup="menu" aria-expanded=${open ? 'true' : 'false'}
      title=${`Usage of ${node.name}: pick another node`} onClick=${() => { if (!open) onOpen?.(); setOpen(!open); }}>
      <span class="usage-node-dot" aria-hidden="true"></span>${label(node)} ▾
    </button>
    ${open ? html`<span class="usage-node-menu" role="menu">
      ${nodes.map((n) => {
        const peak = peakOf(n.id);
        return html`<button key=${n.id} type="button" role="menuitemradio" aria-checked=${n.id === selected ? 'true' : 'false'}
          class=${`usage-node-item${n.id === selected ? ' on' : ''}${peak.why && !peak.pending ? ' locked' : ''}`} style=${`--nc:${n.color}`}
          disabled=${!!peak.why && !peak.pending} onClick=${() => { onSelect(n.id); setOpen(false); }}>
          <span class="usage-node-dot" aria-hidden="true"></span>${label(n)}
          <span class="usage-node-peak">${peak.why ? (peak.pending ? '…' : html`<${LockReason} why=${peak.why} align="end" />`) : peak.value == null ? 'no quota' : `peak ${peak.value}%`}</span>
        </button>`;
      })}
    </span>` : null}
  </span>`;
}

// usePeerUsages reads every listed peer's usage (its peer snapshot) while
// active, refreshed each PEER_USAGE_POLL_MS. It returns id -> entry, as
// peerUsageState reads it.
export function usePeerUsages({ ids, active, fetchImpl = (...a) => globalThis.fetch(...a), timers = globalThis, now = () => Date.now() }) {
  const [entries, setEntries] = useState({});
  const key = ids.join(',');
  useEffect(() => {
    if (!active || !ids.length) return undefined;
    let live = true;
    const load = async (id) => {
      let entry;
      try {
        const response = await fetchImpl(`/api/peer/${encodeURIComponent(id)}/snapshot`);
        if (!response.ok) entry = { failure: `HTTP ${response.status}`, at: now() };
        else {
          const body = await response.json();
          entry = { usage: body?.usage || null, peerView: body?.peer_view || null, at: now() };
        }
      } catch (_) {
        entry = { failure: 'network', at: now() };
      }
      if (live) setEntries((current) => ({ ...current, [id]: entry }));
    };
    const all = () => { for (const id of ids) void load(id); };
    all();
    const timer = timers.setInterval(all, PEER_USAGE_POLL_MS);
    return () => { live = false; timers.clearInterval(timer); };
  }, [key, active]);
  return entries;
}

// fusedUsageRows is the fused Usage summary: a row per node (this node
// first) with its cost today and month to date and its peak quota, plus the
// cost total over the nodes that share it. Quota is per node only: quota
// percentages do not add up across machines.
export function fusedUsageRows(nodes, selfUsage, entries) {
  const rows = nodes.map((node) => {
    const st = node.local ? { usage: selfUsage } : peerUsageState(node, entries[node.id]);
    if (st.why) return { node, why: st.why, pending: !!st.pending };
    const usage = st.usage || {};
    return { node, today: Number(usage.today_cost_usd || 0), mtd: Number(usage.total_cost_usd || 0), peak: usagePeak(usage) };
  });
  const shared = rows.filter((r) => !r.why);
  return {
    rows,
    total: { today: shared.reduce((s, r) => s + r.today, 0), mtd: shared.reduce((s, r) => s + r.mtd, 0), nodes: shared.length },
  };
}

// FusedUsageSummary tops the Usage tab in a fused view: each ticked node's
// spend and peak quota, and the spend total. The views below stay this
// node's.
export function FusedUsageSummary({ nodes, selfUsage, entries, fmt }) {
  const { rows, total } = fusedUsageRows(nodes, selfUsage, entries);
  const label = (n) => `${n.local ? '⌂ ' : ''}${n.name}`;
  return html`<section class="fused-usage" aria-label="Usage on the fused nodes">
    <table class="fused-usage-table">
      <thead><tr><th>Node</th><th class="n">Today</th><th class="n">Month to date</th><th class="n">Peak quota</th></tr></thead>
      <tbody>
        ${rows.map((r) => html`<tr key=${r.node.id} style=${`--nc:${r.node.color}`}>
          <td><span class="usage-node-dot" aria-hidden="true"></span> ${label(r.node)}</td>
          ${r.why
            ? html`<td class="n muted" colspan="3">${r.pending ? 'loading…' : html`not shared <${LockReason} why=${r.why} align="end" />`}</td>`
            : html`<td class="n">${fmt(r.today)}</td><td class="n">${fmt(r.mtd)}</td><td class="n">${r.peak == null ? '—' : `${r.peak}%`}</td>`}
        </tr>`)}
      </tbody>
      <tfoot><tr><th>Total (${total.nodes} ${total.nodes === 1 ? 'node' : 'nodes'})</th><th class="n">${fmt(total.today)}</th><th class="n">${fmt(total.mtd)}</th>
        <th class="n muted" title="Quota percentages do not add up across machines">per node</th></tr></tfoot>
    </table>
    <p class="fused-usage-note">The views below show this node, ⌂ ${nodes.find((n) => n.local)?.name || ''}. Switch the header's quota bars to another node from their node pill.</p>
  </section>`;
}
