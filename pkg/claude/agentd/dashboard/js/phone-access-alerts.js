// phone-access-alerts.js -- the app badge counting access requests that
// wait on the operator. (Alerts themselves are Web Push: push-settings.js.)

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
