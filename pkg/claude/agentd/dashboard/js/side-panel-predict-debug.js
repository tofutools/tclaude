// side-panel-predict-debug.js -- the predictive peek's debug overlay
// (tcl-o4nims): the edge zone's start as a line, the projected landing point
// as a dot (green when it peeks) and the prediction's reason. It is off unless
// ?side_panel_predict_debug=1 is in the URL or localStorage
// tclaude.dash.sidePanelPredictDebug is "1". side-panel-reveal.js publishes
// each prediction to predictDebug; this island draws it into the empty
// #side-panel-predict-debug host in dashboard.html. It never takes the
// pointer.

import { h, render } from 'preact';
import htm from 'htm';
import { signal } from '@preact/signals';

const html = htm.bind(h);

// predictDebug is the last prediction and the edge it was judged against, or
// null before the first.
export const predictDebug = signal(null);

// predictDebugOn is the overlay's switch: the URL flag or the localStorage key.
export function predictDebugOn(win = globalThis) {
  try {
    if (new URLSearchParams(win.location?.search || '').get('side_panel_predict_debug') === '1') return true;
  } catch { /* no location */ }
  try {
    return win.localStorage?.getItem('tclaude.dash.sidePanelPredictDebug') === '1';
  } catch {
    return false;
  }
}

export function PredictDebugOverlay() {
  const shown = predictDebug.value;
  if (!shown) return null;
  const { prediction, edgeX, fullLeft } = shown;
  const known = Number.isFinite(prediction.landingX);
  return html`<div class=${`spp-overlay${prediction.peek ? ' peek' : ''}`} aria-hidden="true">
    <div class="spp-edge" style=${{ transform: `translateX(${edgeX}px)` }}></div>
    ${Number.isFinite(fullLeft) && fullLeft !== edgeX ? html`<div class="spp-edge spp-full" style=${{ transform: `translateX(${fullLeft}px)` }}></div>` : null}
    ${known ? html`<div class="spp-dot" style=${{ transform: `translate(${prediction.landingX}px, ${prediction.landingY}px)` }}></div>` : null}
    <div class="spp-label">${prediction.reason} vx=${prediction.vx.toFixed(2)} vy=${prediction.vy.toFixed(2)}${Number.isFinite(prediction.minVx)
      ? ` need=${prediction.minVx.toFixed(2)}` : ''}${known
      ? ` land=${Math.round(prediction.landingX)}` : ''}</div>
  </div>`;
}

// mountPredictDebug renders the overlay into its host, once; without a host
// (a test shell) it draws nothing.
let mounted = false;
export function mountPredictDebug(doc = globalThis.document) {
  const host = doc.getElementById('side-panel-predict-debug');
  if (mounted || !host) return;
  mounted = true;
  render(html`<${PredictDebugOverlay} />`, host);
}
