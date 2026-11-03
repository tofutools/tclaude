// tclaude live-page probe (skeleton).
//
// The stage listener (stage.go) inserts this script into every HTML document
// it serves to a live-page frame. It runs inside the page's sandboxed,
// opaque-origin frame and is exactly as untrusted as the page itself: it holds
// no secret and reaches nothing of the dashboard's. The command protocol
// (element lookup, scroll reporting, CSS/DOM patches, eval) arrives with the
// marks and live-patch tickets; until then the probe only marks its presence.
//
// This file must never contain a closing script tag: it is inlined verbatim.
(function () {
  'use strict';
  if (window.__tclaudeProbe) return;
  Object.defineProperty(window, '__tclaudeProbe', {
    value: Object.freeze({ version: 0 }),
    configurable: false,
    enumerable: false,
    writable: false,
  });
})();
