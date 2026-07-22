// browser-notify.js — raise tclaude notifications as Web Notifications from
// the dashboard instead of (or alongside) the daemon's desktop notifier.
//
// Why this exists at all: the OS path (D-Bus / toast / terminal-notifier)
// only reaches a human sitting at the machine agentd runs on, and only when
// the notifying process can talk to that desktop — neither holds when you
// are on the dashboard from another device, nor when a sandboxed agent
// process is fenced off from the session bus. The browser already has the
// human's attention in both cases.
//
// Delivery is decided server-side (config notifications.delivery). This
// module is a dumb consumer: it polls the queue and paints what it gets.
//
// Its own timer, deliberately NOT the /api/snapshot poll: that one throttles
// to 10s on a hidden tab, and a hidden tab is exactly when a notification is
// worth the most.

export const BROWSER_NOTIFY_POLL_MS = 3000;

// Cadence used while the server reports browser delivery is switched off.
// Permission is granted per BROWSER, delivery is configured per DAEMON, so
// a human who granted permission once and then left delivery at the default
// `os` would otherwise poll every 3s forever for a channel that can never
// produce anything. The slow heartbeat still notices the operator flipping
// the knob, within half a minute.
export const BROWSER_NOTIFY_IDLE_POLL_MS = 30000;

const ENDPOINT = '/api/browser-notifications';

// notificationsUsable reports whether this browsing context can raise a
// notification at all. The API is absent outside a secure context (an http://
// dashboard reached by IP over a LAN), so feature-detect rather than assume.
function notificationsUsable(win) {
  return typeof win?.Notification === 'function';
}

// requestBrowserNotifyPermission asks the browser for permission. Must be
// called from a user gesture — browsers reject (and Chrome permanently
// blocks) an unprompted request on page load, which is why nothing here
// calls it automatically. Returns the resulting permission string.
export async function requestBrowserNotifyPermission(win = globalThis) {
  if (!notificationsUsable(win)) return 'unsupported';
  try {
    return await win.Notification.requestPermission();
  } catch {
    return win.Notification.permission || 'default';
  }
}

// browserNotifyPermission is the current permission, for UI that reports it.
export function browserNotifyPermission(win = globalThis) {
  if (!notificationsUsable(win)) return 'unsupported';
  return win.Notification.permission;
}

// startBrowserNotifyPoll installs the delivery loop and returns a cleanup.
//
// The cursor is per-tab and starts at the queue HEAD (a `since`-less first
// call), so opening the dashboard never replays a backlog of banners for
// states the agents left long ago. Every open tab keeps its own cursor and
// therefore sees every notification — the same way each would have seen one
// OS banner.
//
// Injection points exist so the loop is testable without wall-clock waits.
export function startBrowserNotifyPoll({
  fetchImpl = globalThis.fetch,
  setTimeoutImpl = globalThis.setTimeout,
  clearTimeoutImpl = globalThis.clearTimeout,
  win = globalThis,
  pollMs = BROWSER_NOTIFY_POLL_MS,
  idlePollMs = BROWSER_NOTIFY_IDLE_POLL_MS,
  onJump = defaultJump,
} = {}) {
  let cursor = null;
  let stopped = false;
  let timer = null;
  let inFlight = false;
  // Assume delivery is on until the server says otherwise, so the very
  // first banner after a fresh grant is never delayed by a slow tick.
  let deliveryEnabled = true;

  const get = async (path) => {
    const res = await fetchImpl(path, { credentials: 'same-origin', cache: 'no-store' });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    return res.json();
  };

  const tick = async () => {
    // Re-checked every tick, not once at start: the human may grant
    // permission (or flip delivery to browser) long after page load, and
    // that must take effect without a reload.
    if (stopped || inFlight) return;
    if (browserNotifyPermission(win) !== 'granted') return;
    inFlight = true;
    try {
      if (cursor === null) {
        // Adopt the head without painting anything — "from now on".
        const data = await get(ENDPOINT);
        deliveryEnabled = data?.enabled !== false;
        // A body without a usable cursor leaves us UNADOPTED (cursor stays
        // null) and we retry next tick. Defaulting to 0 here would make the
        // next poll ask `since=0` and raise every un-expired queued banner
        // at once — precisely the backlog flood adoption exists to prevent.
        const head = Number(data?.cursor);
        if (Number.isFinite(head) && head >= 0) cursor = head;
        return;
      }
      const data = await get(`${ENDPOINT}?since=${encodeURIComponent(cursor)}`);
      deliveryEnabled = data?.enabled !== false;
      // Advance the cursor BEFORE painting: a throw out of the Notification
      // constructor (a browser that refuses one for its own reasons) must
      // not pin the cursor and re-deliver the whole batch every 3s.
      const next = Number(data?.cursor);
      if (Number.isFinite(next) && next > cursor) cursor = next;
      for (const item of data?.notifications || []) {
        raise(item, win, onJump);
      }
    } catch {
      // A blip, agentd restarting, a lost VPN — keep the cursor and retry on
      // the next tick. Never surface this: it is a background nicety, and the
      // snapshot poll already owns the connection banner.
    } finally {
      inFlight = false;
    }
  };

  const schedule = () => {
    if (!stopped) timer = setTimeoutImpl(run, deliveryEnabled ? pollMs : idlePollMs);
  };
  const run = () => {
    void tick().finally(schedule);
  };

  run();

  return () => {
    stopped = true;
    if (timer !== null) clearTimeoutImpl(timer);
  };
}

// raise paints one queued item as a Web Notification.
//
// `tag` is the queue id, so a notification re-delivered by a second tab (or
// after a reload that reset a cursor) collapses onto the same banner instead
// of stacking duplicates.
function raise(item, win, onJump) {
  if (!item?.title) return;
  try {
    const n = new win.Notification(item.title, {
      body: item.body || '',
      tag: `tclaude-bn-${item.id}`,
    });
    n.onclick = () => {
      // Bring the dashboard forward. Deliberately NOT a tmux window focus:
      // the whole point of browser delivery is that the human may be nowhere
      // near the machine agentd runs on, where raising a terminal there does
      // nothing for them. session_id rides along in the payload for a future
      // "and also jump the pane when I am local" action.
      try { win.focus(); } catch { /* a browser may refuse; the banner still cleared */ }
      n.close();
      onJump?.(item);
    };
  } catch {
    // Some browsers throw when the page is not in a secure context or the
    // notification budget is exhausted. Dropping it is correct — the
    // dashboard's own UI still shows the state.
  }
}

// defaultJump is a seam, not behaviour: browser delivery currently does no
// server-side jump on click (see raise). Tests and future callers override it.
function defaultJump() {}
