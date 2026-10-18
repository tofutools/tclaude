// fused-sections.js — a section per ticked peer on the Plugins and Processes
// tabs of a fused view (tcl-9fzv3k). Plugins are installed per machine and a
// process library belongs to its node's groups, so neither merges: this node's
// set shows as before, then each ticked peer's under its node pill, read
// through its peer routes (served to a peer that trusts this node
// unrestricted). Actions in a section act on that section's node; a section
// that is not shared stays collapsed, with the reason.
import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { nodeFetch } from './node-api.js';
import { nodeHref } from './fleet-model.js';
import { tickedNodes, withFused } from './fleet-scope.js';
import { toPath } from './nav-history-core.js';
import { LockReason } from './lock-reason.js';

const html = htm.bind(h);
export const PEER_SECTION_POLL_MS = 30000;

// fusedPeers are the ticked peers of a fused view, or [] outside one (a
// peer's own view never fuses).
export function fusedPeers(fleet, remote = globalThis.__tclaudeRemoteNode) {
  const value = fleet.fleet.value;
  const fused = fleet.fused.value;
  return value && fused && !remote ? tickedNodes(value, fused).filter((n) => !n.local) : [];
}

// peerSectionRefusal words a refused peer read of what ("its plugins").
export function peerSectionRefusal(node, status, what) {
  if (status === 403) return `${node.name} does not share ${what} with you (needs unrestricted trust from ${node.name})`;
  if (status === 502 || status === 503 || status === 504) return `${node.name} is unreachable`;
  return `${node.name} did not answer (HTTP ${status})`;
}

// openOnNode opens a node's own view of where (a tab, or a location as
// toPath takes it), leaving the fused set.
export function openOnNode(node, where, loc = globalThis.location, assign = (url) => globalThis.location.assign(url)) {
  const to = typeof where === 'string' ? { tab: where } : where;
  assign(nodeHref(node.id, { pathname: toPath(to), search: withFused(loc.search, null) }));
}

// peerRequestMutation is a requestMutation bound to a peer's routes, as the
// tabs' own actions take it; a write re-reads that peer's section.
export function peerRequestMutation(node, { reload, fetchFor = nodeFetch }) {
  return async (path, { method = 'POST', body, refreshAfter = true } = {}) => {
    const init = { method, headers: { Accept: 'application/json' }, credentials: 'same-origin' };
    if (body !== undefined) {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }
    const response = await fetchFor(node.id)(path, init);
    const text = await response.text();
    let result = {};
    try { result = text ? JSON.parse(text) : {}; } catch (_) { result = text; }
    if (!response.ok) {
      const error = new Error(`${node.name}: HTTP ${response.status}`);
      error.status = response.status;
      error.body = result;
      throw error;
    }
    if (refreshAfter && method !== 'GET') reload();
    return result;
  };
}

// usePeerSections reads path on each peer while active, every
// PEER_SECTION_POLL_MS. read turns a reply body into the section's data. It
// returns { byNode: id -> { data } | { why }, reload(id) }.
export function usePeerSections({ peers, path, what, read, active, fetchFor = nodeFetch, timers = globalThis }) {
  const [byNode, setByNode] = useState({});
  const live = useRef(true);
  const loaders = useRef({});
  const key = peers.map((p) => p.id).join(',');
  useEffect(() => {
    live.current = true;
    if (!active || !peers.length) return () => { live.current = false; };
    const load = async (node) => {
      let entry;
      try {
        const response = await fetchFor(node.id)(path, { headers: { Accept: 'application/json' } });
        entry = response.ok ? { data: read(await response.json()) } : { why: peerSectionRefusal(node, response.status, what) };
      } catch (_) {
        entry = { why: `${node.name} is unreachable` };
      }
      if (live.current) setByNode((current) => ({ ...current, [node.id]: entry }));
    };
    loaders.current = Object.fromEntries(peers.map((p) => [p.id, () => load(p)]));
    for (const p of peers) void load(p);
    const timer = timers.setInterval(() => { for (const p of peers) void load(p); }, PEER_SECTION_POLL_MS);
    return () => { live.current = false; timers.clearInterval(timer); };
  }, [key, path, active]);
  return { byNode, reload: (id) => { void loaders.current[id]?.(); } };
}

// NodeSection is one node's collapsible section: its pill and summary, then
// its rows. A refused section is collapsed and locked.
export function NodeSection({ node, entry, summary, onOpen, openLabel, children }) {
  const label = `${node.local ? '⌂ ' : ''}${node.name}`;
  const refused = !!entry?.why;
  return html`<details class="fused-section" open=${!refused} style=${`--nc:${node.color}`} data-node=${node.id}>
    <summary class="fused-section-head"><span class="scope-pill" style=${`--nc:${node.color}`}>${label}</span>
      ${!entry ? html`<span class="muted">loading…</span>`
        : refused ? html`<span class="muted">not shared</span><${LockReason} why=${entry.why} />`
        : html`<span class="muted">${summary}</span>`}
      ${onOpen && !refused ? html`<span class="spacer"></span><button type="button" class="tool fused-section-open" onClick=${(event) => { event.preventDefault(); onOpen(); }}>${openLabel || `open on ${node.name}`} ↗</button>` : null}
    </summary>
    ${entry && !refused ? children : null}
  </details>`;
}
