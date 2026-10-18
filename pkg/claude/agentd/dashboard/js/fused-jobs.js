// fused-jobs.js — the other ticked nodes' automations in a fused view
// (tcl-je61ja). A fused view is this node's page; Automations lists this
// node's jobs as before and then each ticked peer's, read through its peer
// routes (/api/jobs is served to a peer that trusts this node unrestricted).
// A peer's cron runs, pauses and deletes in place through that node's routes,
// the same jobs actions bound to it; editing opens on that node's own view,
// whose dialogs read its catalogs.
import { useEffect, useRef, useState } from 'preact/hooks';
import { createJobsActions } from './jobs-actions.js';
import { nodeFetch } from './node-api.js';
import { nodeHref } from './fleet-model.js';
import { withFused } from './fleet-scope.js';
import { toPath } from './nav-history-core.js';

export const PEER_JOBS_POLL_MS = 30000;

// peerJobsRefusal words a refused peer listing.
export function peerJobsRefusal(node, status) {
  if (status === 403) return `${node.name} does not share its automations with you (needs unrestricted trust from ${node.name})`;
  if (status === 502 || status === 503 || status === 504) return `${node.name} is unreachable`;
  return `${node.name} did not answer (HTTP ${status})`;
}

// openOnNode opens a node's own Automations view, leaving the fused set.
export function openOnNode(node, loc = globalThis.location, assign = (url) => globalThis.location.assign(url)) {
  assign(nodeHref(node.id, { pathname: toPath({ tab: 'jobs' }), search: withFused(loc.search, null) }));
}

// createPeerJobsActions is the jobs actions bound to a peer: mutations go to
// its routes and re-read its listing; dialogs open on its own view.
export function createPeerJobsActions(node, { confirm, notify, reload, open = openOnNode, fetchFor = nodeFetch }) {
  const requestMutation = async (path, { method = 'POST', body, refreshAfter = true } = {}) => {
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
  const onNode = () => open(node);
  const state = { upsertCron() {}, openCronEdit: onNode, openCronDuplicate: onNode, openStandingOrderEdit: onNode };
  const actions = createJobsActions({ state, requestMutation, refresh: reload, confirm, notify, download: onNode });
  return Object.freeze({ ...actions, openCronEdit: onNode, openCronDuplicate: onNode, openStandingOrderEdit: onNode, openOnNode: onNode });
}

// usePeerJobs reads each peer's listing (the same filter, kind and page as
// this node's) while active, every PEER_JOBS_POLL_MS. It returns
// { byNode: id -> { rows } | { why } | { pending }, reload(id) }.
export function usePeerJobs({ peers, params, active, fetchFor = nodeFetch, timers = globalThis }) {
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
        const response = await fetchFor(node.id)(`/api/jobs?${params}`, { headers: { Accept: 'application/json' } });
        if (!response.ok) entry = { why: peerJobsRefusal(node, response.status) };
        else {
          const body = await response.json();
          // A peer's exports download on that node only.
          entry = { rows: (body?.rows || []).filter((row) => row.kind === 'cron' || row.kind === 'standing-order') };
        }
      } catch (_) {
        entry = { why: `${node.name} is unreachable` };
      }
      if (live.current) setByNode((current) => ({ ...current, [node.id]: entry }));
    };
    loaders.current = Object.fromEntries(peers.map((p) => [p.id, () => load(p)]));
    for (const p of peers) void load(p);
    const timer = timers.setInterval(() => { for (const p of peers) void load(p); }, PEER_JOBS_POLL_MS);
    return () => { live.current = false; timers.clearInterval(timer); };
  }, [key, params, active]);
  return { byNode, reload: (id) => { void loaders.current[id]?.(); } };
}
