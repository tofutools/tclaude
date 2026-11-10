// side-panel.js -- the dashboard's right side panel, shared by the tabs that
// offer one (tcl-8nfzbb): the Groups tab's palette (dock.js) and the Collab
// view's list (collab-rail.js). One static shell (#agent-dock in
// dashboard.html) with its edge handle and in-panel Hide button; each tab
// brings its own body. A panel is offered while its tab is the active one
// (and, for a panel that needs its view mounted, while it is offered), and
// each panel remembers its own open or folded state per browser.
//
// The body classes are the shell's whole state, for CSS: dock-tab while a
// panel is offered, dock-open while it is open, dock-revealed while a folded
// panel floats in because the pointer reached the edge (side-panel-reveal.js),
// dock-peeking while only its leading edge shows because the pointer is
// heading there fast (the predictive peek, same module), and data-dock-panel
// naming which one.

import { signal } from '@preact/signals';
import { wizWord } from './slop.js';
import { dashPrefs } from './prefs.js';

const PANELS = new Map();

// sidePanelShown is what the shell last painted: the active panel's name ('' for
// none) and whether it is open, for a view that offers to open it.
export const sidePanelShown = signal({ name: '', open: false });

// registerSidePanel declares a tab's panel. pane is the id of the tab's
// <section>; pref the dashPrefs key its open state persists under; open what
// it defaults to; body the selector of its body inside the shell; titles the
// handle's [hide, show] titles as [regular, wizard] pairs. needsOffer panels
// show only while offerSidePanel has them on. autoReveal opts a panel in to
// floating in, while folded, when the pointer reaches the right edge; a panel
// that does not set it never does.
export function registerSidePanel(name, { pane, pref, open = true, body, titles, needsOffer = false, autoReveal = false }) {
  PANELS.set(name, { name, pane, pref, open, body, titles, needsOffer, autoReveal, offered: false });
}

// revealed is whether the active panel, folded, is floating in over the
// content. It never reflows the page and is never remembered: unfolding the
// panel (its handle) is what pins it.
let revealed = false;
let revealedName = '';

// peeked is whether the folded panel shows only its leading edge, ahead of a
// reveal the pointer looks to be heading for. It is its own state, not a kind
// of reveal: a peek holds nothing and is never "over", so none of the reveal's
// hover-out rules run for it. A reveal or a conceal ends it.
let peeked = false;
let peekedName = '';

// isSidePanelPeeking reports whether the folded panel is peeking.
export function isSidePanelPeeking() {
  return peeked;
}

// peekSidePanel shows the folded active panel's leading edge; unpeekSidePanel
// takes it back. Neither touches the remembered open state.
export function peekSidePanel(doc = globalThis.document) {
  if (peeked || revealed || !canRevealSidePanel(doc)) return;
  peeked = true;
  peekedName = activeSidePanel(doc).name;
  applySidePanel(doc);
}

export function unpeekSidePanel(doc = globalThis.document) {
  if (!peeked) return;
  peeked = false;
  applySidePanel(doc);
}

// isSidePanelRevealed reports whether the folded panel is floating in.
export function isSidePanelRevealed() {
  return revealed;
}

// canRevealSidePanel is whether the active panel may float in: it opted in
// and is folded.
export function canRevealSidePanel(doc = globalThis.document) {
  const panel = activeSidePanel(doc);
  return !!panel && panel.autoReveal && !isSidePanelOpen(panel.name);
}

// revealSidePanel floats the folded active panel in; concealSidePanel takes it
// away again. Neither touches the remembered open state.
export function revealSidePanel(doc = globalThis.document) {
  if (revealed || !canRevealSidePanel(doc)) return;
  revealed = true;
  revealedName = activeSidePanel(doc).name;
  peeked = false;
  applySidePanel(doc);
}

export function concealSidePanel(doc = globalThis.document) {
  if (!revealed && !peeked) return;
  revealed = false;
  peeked = false;
  applySidePanel(doc);
}

// offerSidePanel turns a needsOffer panel on or off (its view mounted or
// gone) and re-applies the shell.
export function offerSidePanel(name, on) {
  const panel = PANELS.get(name);
  if (!panel || panel.offered === !!on) return;
  panel.offered = !!on;
  applySidePanel();
}

// activeSidePanel is the panel of the active tab, null when it has none.
export function activeSidePanel(doc = globalThis.document) {
  for (const panel of PANELS.values()) {
    if (!doc?.getElementById(panel.pane)?.classList.contains('active')) continue;
    if (panel.needsOffer && !panel.offered) continue;
    return panel;
  }
  return null;
}

// isSidePanelOpen is a panel's remembered state: only an explicit '0' or '1'
// overrides its default.
export function isSidePanelOpen(name) {
  const panel = PANELS.get(name);
  if (!panel) return false;
  const stored = dashPrefs.getItem(panel.pref);
  return stored === '1' ? true : stored === '0' ? false : panel.open;
}

// setSidePanelOpen remembers a panel's state and paints it. It defaults to
// the active tab's panel.
export function setSidePanelOpen(open, name = activeSidePanel()?.name) {
  const panel = PANELS.get(name);
  if (!panel) return;
  dashPrefs.setItem(panel.pref, open ? '1' : '0');
  applySidePanel();
}

const listeners = new Set();

// onSidePanelApply calls fn({ panel, open }) every time the shell is painted.
export function onSidePanelApply(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

// applySidePanel paints the shell for the active tab: the body classes, the
// handle's state and title, and which body shows.
export function applySidePanel(doc = globalThis.document) {
  if (!doc?.body) return;
  const panel = activeSidePanel(doc);
  const open = !!panel && isSidePanelOpen(panel.name);
  // A tab switch (even to another panel that opted in), an unfold or a panel
  // that never opted in ends a reveal.
  if (revealed && (!panel || panel.name !== revealedName || open || !panel.autoReveal)) revealed = false;
  if (peeked && (revealed || !panel || panel.name !== peekedName || open || !panel.autoReveal)) peeked = false;
  doc.body.classList.toggle('dock-tab', !!panel);
  doc.body.classList.toggle('dock-open', open);
  doc.body.classList.toggle('dock-revealed', revealed);
  doc.body.classList.toggle('dock-peeking', peeked);
  if (panel) doc.body.dataset.dockPanel = panel.name;
  else delete doc.body.dataset.dockPanel;
  const handle = doc.getElementById('dock-toggle');
  if (handle && panel?.titles) {
    const [hide, show] = panel.titles;
    handle.setAttribute('aria-expanded', open ? 'true' : 'false');
    handle.title = open ? wizWord(...hide) : wizWord(...show);
  }
  for (const other of PANELS.values()) {
    const body = other.body && doc.querySelector(other.body);
    if (body) body.hidden = other !== panel;
  }
  const shown = sidePanelShown.peek();
  if (shown.name !== (panel?.name || '') || shown.open !== open) sidePanelShown.value = { name: panel?.name || '', open };
  for (const fn of listeners) fn({ panel, open });
}

// bindSidePanel wires the handle and the Hide button to the active panel,
// and re-applies on every tab switch: each tab's <section> gets an observer
// on its class, which every switch path writes. The handle of a floating
// panel unfolds it, which pins it; its Hide button only sends it away.
export function bindSidePanel(doc = globalThis.document) {
  const toggle = () => {
    const panel = activeSidePanel(doc);
    if (panel) setSidePanelOpen(!isSidePanelOpen(panel.name), panel.name);
  };
  doc.getElementById('dock-toggle')?.addEventListener('click', toggle);
  doc.getElementById('dock-collapse')?.addEventListener('click', () => {
    if (revealed) concealSidePanel(doc);
    else toggle();
  });
  const Observer = globalThis.MutationObserver;
  if (Observer) {
    for (const panel of PANELS.values()) {
      const pane = doc.getElementById(panel.pane);
      if (pane) new Observer(() => applySidePanel(doc)).observe(pane, { attributes: true, attributeFilter: ['class'] });
    }
  }
  applySidePanel(doc);
}
