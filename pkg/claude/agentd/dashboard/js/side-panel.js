// side-panel.js -- the dashboard's right side panel, shared by the tabs that
// offer one (tcl-8nfzbb): the Groups tab's palette (dock.js) and the Collab
// view's list (collab-rail.js). One static shell (#agent-dock in
// dashboard.html) with its edge handle and in-panel Hide button; each tab
// brings its own body. A panel is offered while its tab is the active one
// (and, for a panel that needs its view mounted, while it is offered), and
// each panel remembers its own open or folded state per browser.
//
// The body classes are the shell's whole state, for CSS: dock-tab while a
// panel is offered, dock-open while it is open, and data-dock-panel naming
// which one.

import { wizWord } from './slop.js';
import { dashPrefs } from './prefs.js';

const PANELS = new Map();

// registerSidePanel declares a tab's panel. pane is the id of the tab's
// <section>; pref the dashPrefs key its open state persists under; open what
// it defaults to; body the selector of its body inside the shell; titles the
// handle's [hide, show] titles as [regular, wizard] pairs. needsOffer panels
// show only while offerSidePanel has them on.
export function registerSidePanel(name, { pane, pref, open = true, body, titles, needsOffer = false }) {
  PANELS.set(name, { name, pane, pref, open, body, titles, needsOffer, offered: false });
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
  doc.body.classList.toggle('dock-tab', !!panel);
  doc.body.classList.toggle('dock-open', open);
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
  for (const fn of listeners) fn({ panel, open });
}

// bindSidePanel wires the handle and the Hide button to the active panel,
// and re-applies on every tab switch: each tab's <section> gets an observer
// on its class, which every switch path writes.
export function bindSidePanel(doc = globalThis.document) {
  const toggle = () => {
    const panel = activeSidePanel(doc);
    if (panel) setSidePanelOpen(!isSidePanelOpen(panel.name), panel.name);
  };
  doc.getElementById('dock-toggle')?.addEventListener('click', toggle);
  doc.getElementById('dock-collapse')?.addEventListener('click', toggle);
  const Observer = globalThis.MutationObserver;
  if (Observer) {
    for (const panel of PANELS.values()) {
      const pane = doc.getElementById(panel.pane);
      if (pane) new Observer(() => applySidePanel(doc)).observe(pane, { attributes: true, attributeFilter: ['class'] });
    }
  }
  applySidePanel(doc);
}
