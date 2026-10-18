// peer-notifications.js — a peer's unread human notifications, which its
// snapshot never carries: read from its Messages mailbox (the human folder)
// while a view shows its agents (the fused Groups view, a peer's terminal
// tabs). Each list is that peer's own, its files at its own routes.
import { useEffect, useRef, useState } from 'preact/hooks';
import { onNodeChanged } from './node-api.js';
import { featureState as peerFeatureState } from './peer-view-limits.js';

// PEER_NOTIFICATIONS_POLL_MS paces each peer's unread-notification read;
// PEER_NOTIFICATIONS_PAGE is how many of its newest unread ones it reads.
export const PEER_NOTIFICATIONS_POLL_MS = 15000;
const PEER_NOTIFICATIONS_PAGE = 100;

// peerSharesNotifications reports whether a peer lets this operator read its
// human notifications: under node.messages.read, which unrestricted trust
// holds. Its snapshot never carries them.
export function peerSharesNotifications(snapshot) {
  const view = snapshot?.peer_view;
  return !!view && (view.level === 'unrestricted' || peerFeatureState('node.messages', view) === 'shared');
}

// usePeerNotifications reads each listed peer's unread human notifications
// from its Messages mailbox (the human folder) while active, and again at
// once when a control here changes that peer. It returns id -> messages.
export function usePeerNotifications({ active, peers, fetchFor, timers = globalThis }) {
  const [lists, setLists] = useState({});
  const peersRef = useRef(peers);
  peersRef.current = peers;
  const fetchRef = useRef(fetchFor);
  fetchRef.current = fetchFor;
  // read is the live poll's reader; requested names the peers it has read,
  // so one newly listed (its first snapshot just arrived) is read at once
  // without the others being read again.
  const reader = useRef({ read: null, requested: new Set() });
  const any = peers.length > 0;
  useEffect(() => {
    if (!active || !any) return undefined;
    let live = true;
    const requested = new Set();
    // refused names the peers that do not share their messages from here
    // (a 403, or a reply whose view withholds them): not read again until
    // the poll restarts (the view shown anew).
    const refused = new Set();
    const read = async (id) => {
      requested.add(id);
      try {
        const response = await fetchRef.current(id)(`/api/mailbox?id=human&unread=1&page=1&page_size=${PEER_NOTIFICATIONS_PAGE}`, { headers: { Accept: 'application/json' } });
        if (response.status === 403) { refused.add(id); return; }
        if (!response.ok) return;
        const body = await response.json();
        if (body?.peer_view && !peerSharesNotifications(body)) { refused.add(id); return; }
        // An older peer ignores unread=1 and sends its newest page.
        const messages = (Array.isArray(body?.messages) ? body.messages : []).filter((m) => m && !m.read);
        if (live) setLists((current) => ({ ...current, [id]: messages }));
      } catch (_) { /* keeps the last list */ }
    };
    reader.current = { read, requested };
    let timer = 0;
    const all = () => {
      // Nothing is read while the page is in the background.
      if (!globalThis.document?.hidden) for (const id of peersRef.current) if (!refused.has(id)) void read(id);
      timer = timers.setTimeout(all, PEER_NOTIFICATIONS_POLL_MS);
    };
    all();
    const stopChanged = onNodeChanged((node) => { if (peersRef.current.includes(node)) void read(node); });
    return () => { live = false; reader.current = { read: null, requested: new Set() }; timers.clearTimeout(timer); stopChanged(); };
  }, [active, any]);
  const key = peers.join(',');
  useEffect(() => {
    const { read, requested } = reader.current;
    if (read) for (const id of peers) if (!requested.has(id)) void read(id);
  }, [key]);
  // A peer that stopped sharing (or left the view) shows none of its last.
  return Object.fromEntries(peers.filter((id) => lists[id]).map((id) => [id, lists[id]]));
}

// A restricted peer's page: its snapshot withholds human notifications (a
// peer snapshot never carries them; unrestricted trust reads its own full
// one), so a member's ! and a terminal tab's have nothing to show. Under
// node.messages.read the snapshot poll (refresh.js) attaches the peer's unread
// ones, read from its mailbox through the page's fetch at the notification
// cadence, never per tick.
const pageRead = { at: -Infinity, inflight: false, list: null, total: 0, gen: 0 };

export function resetPageNotificationsForTest() {
  Object.assign(pageRead, { at: -Infinity, inflight: false, list: null, total: 0, gen: 0 });
}

// refreshPageNotifications records a read or unread written from here (row:
// the notification as shown) and has the next snapshot tick read them anew.
// The row stays listed with its new state, so a reader holding it sees the
// change, and a read already in flight, answered from before the write, is
// dropped.
export function refreshPageNotifications(id = null, read = false, row = null) {
  pageRead.at = -Infinity;
  pageRead.gen += 1;
  if (id == null || !pageRead.list) return;
  const current = pageRead.list.find((m) => m.id === id);
  if (current) {
    if (current.read === read) return;
    pageRead.list = pageRead.list.map((m) => (m.id === id ? { ...m, read } : m));
  } else if (!read && row) {
    pageRead.list = [...pageRead.list, { ...row, read }].sort((a, b) => b.id - a.id);
  } else return;
  pageRead.total = Math.max(0, pageRead.total + (read ? -1 : 1));
}

export function attachPageNotifications(data, {
  remote = globalThis.__tclaudeRemoteNode?.id || '', now = Date.now(), hidden = !!globalThis.document?.hidden,
  fetchImpl = (...a) => globalThis.fetch(...a),
} = {}) {
  if (!remote || !data || Array.isArray(data.messages) || !peerSharesNotifications(data)) return data;
  if (!pageRead.inflight && !hidden && now - pageRead.at >= PEER_NOTIFICATIONS_POLL_MS) {
    pageRead.at = now;
    pageRead.inflight = true;
    const gen = pageRead.gen;
    void fetchImpl(`/api/mailbox?id=human&unread=1&page=1&page_size=${PEER_NOTIFICATIONS_PAGE}`, { credentials: 'same-origin', cache: 'no-store' })
      .then((response) => (response.ok ? response.json() : null))
      .then((body) => {
        if (!Array.isArray(body?.messages)) return;
        // A write from here since this read began: its answer is older.
        if (gen !== pageRead.gen) { pageRead.at = -Infinity; return; }
        pageRead.list = body.messages.filter((m) => m && !m.read);
        pageRead.total = Number.isFinite(body.total) ? body.total : pageRead.list.length;
      })
      .catch(() => {})
      .finally(() => { pageRead.inflight = false; });
  }
  if (pageRead.list) {
    // Copies: a consumer flipping a row's read state must not write the cache.
    data.messages = pageRead.list.map((m) => ({ ...m }));
    data.messages_unread = pageRead.total;
  }
  return data;
}
