// layout.js — the one place modules ask "is this the phone layout?".
//
// js/layout-boot.js decides the layout once per page load, before the first
// paint, and marks <html data-layout="phone"> in phone mode. This module reads
// that mark. The layout never changes during a page load: switching it
// (setLayoutOverride) reloads the page, so islands mount for one layout only
// and nothing has to re-render between the two.

const PHONE = 'phone';
const DESKTOP = 'desktop';
const OVERRIDES = ['phone', 'desktop', 'auto'];

// readLayout returns 'phone' or 'desktop' for a document.
export function readLayout(doc = globalThis.document) {
  return doc?.documentElement?.dataset?.layout === PHONE ? PHONE : DESKTOP;
}

export const layout = readLayout();
export const isPhone = layout === PHONE;

// layoutDecision is what layout-boot.js saw: the layout in force, the layout
// the device was detected as, and the override ('phone', 'desktop' or null).
// A "Desktop site" / "Phone view" switch reads `detected` to decide whether to
// offer itself at all.
export function layoutDecision(win = globalThis) {
  const seen = win?.__tclaudeLayout;
  return {
    layout: seen?.layout === PHONE ? PHONE : DESKTOP,
    detected: seen?.detected === PHONE ? PHONE : DESKTOP,
    override: seen?.override === PHONE || seen?.override === DESKTOP ? seen.override : null,
  };
}

// layoutOverrideURL is the current page's URL with ?layout=<mode>, which
// layout-boot.js applies, remembers in this browser and strips again.
export function layoutOverrideURL(mode, href = globalThis.location?.href) {
  if (!OVERRIDES.includes(mode)) throw new RangeError(`unknown layout override: ${mode}`);
  const url = new URL(href);
  url.searchParams.set('layout', mode);
  return url.pathname + url.search + url.hash;
}

// setLayoutOverride switches the layout ('phone', 'desktop', or 'auto' to go
// back to detection) by reloading the page through layout-boot.js. It
// replaces the history entry: Back must not return to a page cached in the
// other layout, nor act as a layout toggle.
export function setLayoutOverride(mode, loc = globalThis.location) {
  loc.replace(layoutOverrideURL(mode, loc.href));
}
