// phone-access-alerts.js -- the phone's one-tap "Alert me on access requests"
// (More), and the app badge counting requests that wait on the operator.
//
// An access request raises a banner only when three settings agree: the
// notifications master switch, the access-request knob (off by default), and
// a delivery that reaches a browser, since the default delivery is the
// desktop of the machine agentd runs on, which a phone never sees. Spread
// over Settings › Notifications, that is three switches to find while an
// agent waits. The row reads and sets them together.
//
// What it cannot do is wake a locked phone: browser delivery is painted by an
// open dashboard (browser-notify.js polls), so the app must be running.

// accessAlertsOn reports whether a notification settings block (GET
// /api/notifications) raises a browser banner for an access request.
export function accessAlertsOn(settings) {
  return !!settings?.enabled && !!settings?.access_requests &&
    (settings?.delivery === 'browser' || settings?.delivery === 'both');
}

// accessAlertsChange is the POST /api/notifications body that flips the row:
// on switches the master, the knob and a browser-reaching delivery on (a
// desktop delivery becomes both, so the desktop keeps its banners); off turns
// only the access-request knob off, leaving every other notification as it
// was.
export function accessAlertsChange(settings, on) {
  if (!on) return { access_requests: false };
  const delivery = settings?.delivery === 'browser' || settings?.delivery === 'both' ? settings.delivery : 'both';
  return { enabled: true, access_requests: true, delivery };
}

// pendingAccessCount is how many access requests wait on the operator in a
// snapshot, for the app badge.
export function pendingAccessCount(snapshot) {
  const n = Number(snapshot?.access_requests_pending);
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : 0;
}

// syncAppBadge puts count on the installed app's icon (the Badging API), or
// clears it at zero. A browser without the API, or one that refuses (not
// installed, no permission), is left alone: the badge is a nicety.
export function syncAppBadge(count, nav = globalThis.navigator) {
  try {
    if (count > 0 && typeof nav?.setAppBadge === 'function') return Promise.resolve(nav.setAppBadge(count)).catch(() => {});
    if (count === 0 && typeof nav?.clearAppBadge === 'function') return Promise.resolve(nav.clearAppBadge()).catch(() => {});
  } catch { /* not supported here */ }
  return Promise.resolve();
}
