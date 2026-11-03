// page-place.js -- where a live page shows (tcl-r4sg0c): a tab of the Collab
// strip, a tab of an overlay window, or a browser window of its own. A page is
// in one place at a time in this dashboard. New pages open here, from the
// snapshot: unfocused, marked for attention, with a toast that opens them.
// Held apart from the island so the terminal shell, the overlay windows and the
// tests reach it without Preact components.

import { computed, signal } from '@preact/signals';
import {
  PAGE_WINDOW_KIND, findLivePage, isPageId, livePagesOf, pageLabel, pagePaneKey, pagePaneSeed, pageSender, pageShowEvents, pageWindowId,
} from './page-pane-model.js';
import { closeOverlayTab, labelOverlayWindow, openOverlayWindow, overlayTabs } from './overlay-instances.js';
import { openTerminalPane } from './terminals-tab.js';

export { PAGE_WINDOW_KIND, pagePaneKey, pageWindowId };

// The overlay tabs that hold pages.
export const pageOverlays = computed(() => overlayTabs.value.filter((tab) => tab.kind === PAGE_WINDOW_KIND));

const overlayOf = (id) => pageOverlays.peek().find((tab) => tab.page === id) || null;

// Pages shown or shown again that the operator has not looked at yet: a strip
// tab marks them with a dot until it is opened.
export const unseenPages = signal(new Set());

export function markPageSeen(id) {
  if (!unseenPages.peek().has(id)) return;
  const next = new Set(unseenPages.peek());
  next.delete(id);
  unseenPages.value = next;
}

function markPageUnseen(id) {
  if (unseenPages.peek().has(id)) return;
  unseenPages.value = new Set([...unseenPages.peek(), id]);
}

// Pages this dashboard sent to a separate window, by id: the window, while it
// is open. A page there is not opened again here when it is shown again.
const separateWindows = new Map();

export function notePageWindow(id, win) {
  if (win) separateWindows.set(id, win);
}

function inSeparateWindow(id) {
  const win = separateWindows.get(id);
  if (win && !win.closed) return true;
  separateWindows.delete(id);
  return false;
}

// The strip, while one is mounted: { has(id), open(seed, options), activate(id),
// remove(ids) }.
let strip = null;

export function registerPageStrip(next) {
  strip = next;
  return () => {
    if (strip === next) strip = null;
  };
}

// revealPageWindow brings a floating page forward, and reports whether it was.
export function revealPageWindow(id) {
  const floating = overlayOf(id);
  if (floating) openOverlayWindow(floating);
  return !!floating;
}

// openPagePane opens a page as a strip tab, or brings forward the place that
// holds it. activate false opens it behind the tab showing, unfocused.
export function openPagePane(id, label, { activate = true } = {}) {
  const floating = overlayOf(id);
  if (floating) {
    if (activate) openOverlayWindow(floating);
    return Promise.resolve(null);
  }
  return openTerminalPane(pagePaneSeed(id, label), activate ? undefined : { reveal: false, activate: false });
}

// openPageOverlay floats a page in an overlay window of its own.
export function openPageOverlay(id, label, at = null) {
  openOverlayWindow({ id: pageWindowId(id), kind: PAGE_WINDOW_KIND, page: id, label: label || id, at });
}

// returnPageToTab moves a floating page back to a strip tab and shows it.
export function returnPageToTab(id) {
  const floating = overlayOf(id);
  if (!floating) return Promise.resolve(null);
  closeOverlayTab(floating.id);
  return openPagePane(id, floating.label);
}

export function setPageOverlayLabel(id, label) {
  const floating = overlayOf(id);
  if (floating && label && floating.label !== label) labelOverlayWindow(floating.id, label);
}

// closeLivePage closes a page for good: the daemon ends its origin and tells
// the agent that showed it. The views go when the snapshot no longer lists it.
// While a close is on its way, and until the snapshot drops the page, it is
// closing: the watcher does not give it a place again. A close that fails
// leaves it to the watcher, which opens it again quietly.
const closing = new Set();

export async function closeLivePage(id, { fetchImpl = (...args) => globalThis.fetch(...args), notify = null } = {}) {
  if (!isPageId(id)) return false;
  closing.add(id);
  try {
    const response = await fetchImpl(`/api/pages/${encodeURIComponent(id)}`, { method: 'DELETE', credentials: 'same-origin' });
    if (response?.ok || response?.status === 404) {
      forgetPage(id);
      return true;
    }
    notify?.(`Could not close the page: ${response?.status || 'no answer'}`, true);
  } catch (cause) {
    notify?.(`Could not close the page: ${cause?.message || cause}`, true);
  }
  closing.delete(id);
  return false;
}

// forgetPage takes a page away from every place in this dashboard.
function forgetPage(id) {
  const floating = overlayOf(id);
  if (floating) closeOverlayTab(floating.id);
  strip?.remove([id]);
  markPageSeen(id);
}

const placed = (id) => !!overlayOf(id) || inSeparateWindow(id) || !!strip?.has(id);

// watchLivePages follows the snapshot's pages. A page shown for the first time
// opens as a strip tab behind the one showing, marked unseen, with a toast that
// opens it. A page shown again is marked unseen where it is; its frame reloads
// on its own. Every open page always has a place in this dashboard: one that
// lost its place without being closed (a separate window shut, a close that failed, a dashboard load) opens quietly behind the tab
// showing. A page the snapshot no longer lists leaves every place, a restored
// overlay tab included. It answers its stop.
// expected holds pages this dashboard just created (groups-resources.js):
// each opens focused once the snapshot lists it, unannounced. Opening it
// before then would lose the race with an older snapshot, whose watcher
// pass forgets every page it does not list.
const expected = new Set();

export function expectPage(id) {
  if (id) expected.add(id);
}

export function watchLivePages({ snapshot, notify = null }) {
  let known = null;
  const stop = snapshot.subscribe((value) => {
    if (!value) return;
    const pages = livePagesOf(value);
    const { next, added, reshown } = pageShowEvents(known, pages);
    known = next;
    const live = new Set(pages.map((page) => page.id));
    const held = [...pageOverlays.peek().map((tab) => tab.page), ...(strip?.ids?.() || [])];
    for (const id of new Set(held)) if (!live.has(id)) forgetPage(id);
    for (const id of [...closing]) if (!live.has(id)) closing.delete(id);
    for (const page of pages) {
      const label = pageLabel(page);
      if (!placed(page.id) && !closing.has(page.id)) void openPagePane(page.id, label, { activate: expected.has(page.id) });
      else setPageOverlayLabel(page.id, label);
    }
    for (const page of [...added, ...reshown]) {
      if (!strip?.isActive?.(page.id) && !expected.has(page.id)) markPageUnseen(page.id);
    }
    for (const page of added) {
      // No one "showed" a page the operator made: no announcement for it.
      if (expected.has(page.id) || page.group_owned) continue;
      const label = pageLabel(page);
      notify?.(`${pageSender(value, page).name} showed ${label}`, false, { action: { label: 'Open', run: () => revealPage(page.id, label) } });
    }
    for (const id of [...expected]) if (live.has(id)) expected.delete(id);
  });
  return () => {
    stop();
  };
}

// revealPage shows a page wherever it is in this dashboard, opening a strip tab
// for it when it has no place.
export function revealPage(id, label) {
  markPageSeen(id);
  const floating = overlayOf(id);
  if (floating) {
    openOverlayWindow(floating);
    return;
  }
  if (strip?.has(id)) {
    strip.activate(id);
    return;
  }
  void openPagePane(id, label);
}

// pageInSnapshot reads a page from a snapshot signal.
export const pageInSnapshot = (snapshot, id) => findLivePage(snapshot?.value, id);
