// assets-version.js — "did agentd's frontend change under this page?" watcher.
//
// An agentd upgrade usually ships new dashboard code. The daemon fingerprints
// its embedded asset tree (dashboard_assets_version.go) and (1) stamps the
// fingerprint into the served HTML as <meta name="tclaude-assets-version">,
// and (2) carries it on every /api/snapshot as assets_version. When the two
// stop matching, the modules running on this page no longer correspond to
// what the daemon serves — the only safe move is a full page reload, which
// refetches everything (static assets are served Cache-Control: no-store).
//
// The meta tag is the baseline of record: it names the build that produced
// THIS page's HTML, so even an upgrade that lands between page load and the
// first poll is caught. When the tag is absent (older daemon's HTML, tests),
// the first snapshot's fingerprint seeds the baseline instead.

// baseline: undefined = not yet seeded; then the fingerprint reloads compare
// against. reloading latches once a reload has been requested so the 2s poll
// cannot ask for it again while the navigation is in flight.
let baseline;
let reloading = false;

// The un-latch delay after requesting navigation. If the reload proceeds the
// JS context is destroyed and the timer never matters; if the page is still
// alive when it fires, the navigation was cancelled (see below).
const RELOAD_CANCELLED_MS = 5000;

// checkAssetsVersion notes one poll's assets_version and returns true when the
// page is (now) reloading and the caller must stop rendering this tick —
// stale modules must not paint a snapshot from a newer daemon. An empty /
// absent fingerprint (older daemon) never triggers a reload.
//
// The requested navigation can be REFUSED: with a terminal pane open the
// terminals island arms a beforeunload confirm, and the operator may click
// "stay" (reasonable mid-command). The latch must not brick the page then, so
// it self-releases: still being alive RELOAD_CANCELLED_MS after asking to
// navigate means the reload was cancelled, and the new fingerprint is adopted
// as the baseline — rendering resumes (stale-but-chosen beats frozen), the
// poll does not re-prompt every tick for the SAME version, and the NEXT
// upgrade prompts again. The native confirm dialog suspends timers while it
// is up, so a slow human decision cannot fire the un-latch early.
export function checkAssetsVersion(version, reloadImpl, scheduleImpl) {
  if (reloading) return true;
  if (!version) return false;
  if (baseline === undefined) {
    baseline = document.querySelector('meta[name="tclaude-assets-version"]')?.content
      || version;
  }
  if (baseline === version) return false;
  reloading = true;
  (scheduleImpl || ((fn) => setTimeout(fn, RELOAD_CANCELLED_MS)))(() => {
    reloading = false;
    baseline = version;
  });
  (reloadImpl || (() => location.reload()))();
  return true;
}
