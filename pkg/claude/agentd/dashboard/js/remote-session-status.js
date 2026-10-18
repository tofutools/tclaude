// remote-session-status.js — the status light for a peer's terminal pane.
//
// A remote pane's agent is not in this node's roster, so its tab would only
// say "status unavailable". The peers push their shared sessions' state to
// this node (GET /api/federation/sessions, the catalog remote-terminal.js
// opens from), and that is read here while any remote pane is open.

import { signal } from '@preact/signals';

export const REMOTE_SESSIONS_PATH = '/api/federation/sessions';
const POLL_MS = 5000;

// remoteSessionAgents maps "<instance>/<agent>" to a roster-shaped agent that
// terminal-tab-status.js reads like a local one.
export const remoteSessionAgents = signal(new Map());

export function remoteAgentKey(peer, agent) {
  return `${peer}/${agent}`;
}

// remoteSessionRoster keeps the catalog's live rows: a stale row may describe
// a pane long gone, so its agent stays unknown rather than shown idle.
export function remoteSessionRoster(rows) {
  const out = new Map();
  for (const row of Array.isArray(rows) ? rows : []) {
    if (!row?.instance || !row.agent || row.stale) continue;
    out.set(remoteAgentKey(row.instance, row.agent), {
      agent_id: row.agent, title: row.name || '', online: true, state: { status: row.state || '' },
    });
  }
  return out;
}

// startRemoteSessionStatusPoll reads the catalog now and every few seconds
// while the page is visible and hasRemote() says a remote pane is open.
// Returns the stop function.
export function startRemoteSessionStatusPoll({
  hasRemote, fetchImpl = globalThis.__tclaudeRemoteNode?.localFetch || ((...args) => fetch(...args)),
  intervalMs = POLL_MS, doc = globalThis.document,
} = {}) {
  let stopped = false;
  let inflight = false;
  let last = '';
  const read = async () => {
    if (stopped || inflight || doc?.hidden || !hasRemote()) return;
    inflight = true;
    try {
      const response = await fetchImpl(REMOTE_SESSIONS_PATH, { credentials: 'same-origin', cache: 'no-store' });
      if (!response.ok) return;
      const roster = remoteSessionRoster(await response.json());
      const key = JSON.stringify([...roster]);
      // Publish only on change so an unchanged poll re-renders nothing.
      if (!stopped && key !== last) {
        last = key;
        remoteSessionAgents.value = roster;
      }
    } catch {
      // Keep the last good catalog; the next tick retries.
    } finally {
      inflight = false;
    }
  };
  const timer = setInterval(read, intervalMs);
  const onVisible = () => { if (!doc?.hidden) void read(); };
  doc?.addEventListener?.('visibilitychange', onVisible);
  void read();
  return () => {
    stopped = true;
    clearInterval(timer);
    doc?.removeEventListener?.('visibilitychange', onVisible);
  };
}
