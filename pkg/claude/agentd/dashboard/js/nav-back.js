// nav-back.js -- a phone page's ‹ leads to its parent, never just "back".
//
// The ‹ at the top of a routed phone page (Mail's reader) goes to the page it
// belongs under (its folder's list), however the page was reached. When the
// entry under the current one is exactly that parent, ‹ is a step back, so
// the parent's own entry comes back and no history piles up. Otherwise (the
// page was reached from Now, a notification, a link, a fresh load) the caller
// shows the parent in place of the page, correcting the current entry, so a
// later Back does not land on the page again. The browser's own Back keeps
// its meaning either way.
//
// The router (nav-history.js) registers the step; this bridge keeps the
// pages free of the router's imports.

let step = null;

// registerStepBack installs the router's step: given the parent location, it
// steps back onto it and returns true, or returns false when the entry under
// the current one is not that parent. Returns the uninstall function.
export function registerStepBack(next) {
  step = next;
  return () => { if (step === next) step = null; };
}

// toParent leads ‹ to `parent`: a step back onto its entry when that is the
// one under this, else `inPlace()`, which shows the parent and corrects the
// current entry.
export function toParent(parent, inPlace) {
  if (parent && step?.(parent)) return;
  inPlace();
}
