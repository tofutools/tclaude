// phone-shell-island.js -- the dashboard's phone shell (data-layout="phone").
//
// A top bar and a five-slot bottom nav (Now · Agents · Mail · Board · More)
// replace the desktop header, tab bar, footer, dock and overlay windows, which
// phone.css hides. The content area between the bars is either one of the
// shell's own views (Now, Board) or the dashboard tab that is active: Agents is
// the Groups tab, Mail is Messages, and the More sheet opens every other tab.
// Tabs keep their desktop islands until each gets its phone view.
//
// The shell marks <html data-phone-view="now|board|desktop-only"> while an own
// view or a desktop-only page (js/phone-desktop-only.js) is up, so
// phone.css can hide <main> under it. Any tab activation (a slot, a More entry,
// a deep link, Back) clears the own view.


import { h, render } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import { signal } from '@preact/signals';
import htm from 'htm';
import { messagesBadgeView } from './shell-model.js';
import { OWN_VIEWS, PHONE_SLOTS, nowView, rootBarFor, slotFor, titleFor } from './phone-shell-model.js';
import { barFor, phoneBar, phonePage } from './phone-page.js';
import { processContext } from './process-context.js';
import { gateItems, openRunLink } from './phone-process-runs.js';
import { PROCESSES_SUBTAB, openAutomationsEntry } from './automations-subnav.js';
import { PhoneSheetHost, closePhoneSheet, openPhoneSheet, phoneSheet } from './phone-sheet.js';
import { DesktopOnlyPage, desktopOnlyView } from './phone-desktop-only.js';
import { featureState } from './feature-state-registry.js';
import { activitySubtab, setActivitySubtab } from './activity-state.js';
import { installPhoneHistory, keptView } from './phone-history.js';
import { installKeyboardInset } from './phone-keyboard.js';
import { installInputModality } from './phone-input.js';
import { installHeaderPull, pullHint } from './standalone-app.js';
import { fleetState } from './fleet-state.js';
import { IconButton } from './phone-ui.js';
import { NodeChip } from './phone-node-picker.js';
import { SearchSheet } from './phone-search.js';
import { NowView } from './phone-now.js';
import { BoardView, boardRequest, openBoardTask } from './phone-board.js';
import { MoreMenu } from './phone-more.js';
import { navBadge, navShown, terminalDock } from './phone-nav-toggle.js';
import { Chevron } from './chevron-icon.js';
import { STEWARD_FAB_SLOTS, StewardFab } from './phone-steward.js';

// The own views live in their own files; re-exported for this module's
// callers.
export { NowView, BoardView };

const html = htm.bind(h);
// Relative to this module, so it shares the page's versioned asset prefix.
const LOGO_SRC = new URL('../tclaude-icon.svg', import.meta.url).href;

// createPhoneShellState holds the shell's own view ('now', 'board' or null
// for a tab). A fresh load of the bare dashboard URL lands on Now; a deep
// link lands on its tab.
export function createPhoneShellState({ startOnNow = true } = {}) {
  return {
    view: signal(startOnNow ? 'now' : null),
  };
}

// LOOK_PARAMS only change how the dashboard looks (the wizard or slop skin,
// a layout override), so a link carrying them is still the bare dashboard.
// layout-boot.js already drops ?layout before the shell starts; it is listed
// so the answer holds whenever this is asked.
const LOOK_PARAMS = new Set(['wizard', 'slop', 'layout']);

// landsOnNow reports whether a page load at this URL starts on Now: the bare
// dashboard path with no deep-link query beyond the look params.
export function landsOnNow(loc = globalThis.location) {
  const path = loc?.pathname || '/';
  if (path !== '/' && path !== '/dashboard') return false;
  return [...new URLSearchParams(loc?.search || '').keys()].every((key) => LOOK_PARAMS.has(key));
}

// NAV_ICONS are the bottom nav's drawn icons, by a slot's icon name: one
// colour (currentColor), like the text glyphs beside them, where an emoji
// (🤖) would be drawn in its own colours and look like the tab on screen.
const NAV_ICONS = Object.freeze({
  robot: html`<svg class="phone-nav-svg" viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor"
    stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
    <path d="M12 3v3" /><circle cx="12" cy="2.6" r="0.9" fill="currentColor" stroke="none" />
    <rect x="4.5" y="6.5" width="15" height="12" rx="3.5" />
    <circle cx="9.2" cy="12" r="1.4" fill="currentColor" stroke="none" /><circle cx="14.8" cy="12" r="1.4" fill="currentColor" stroke="none" />
    <path d="M9.5 15.6h5" /><path d="M2.5 11.5v3M21.5 11.5v3" />
  </svg>`,
});

export function PhoneShell({ shell, state, actions, boardSource, fetchImpl = globalThis.fetch, processSource = processContext, fleet = fleetState }) {
  const activeTab = state.activeTab.value;
  const view = shell.view.value;
  const slot = slotFor(view, activeTab);
  // The nav lights the slot a sheet over it belongs to (More) while it is up.
  const lit = phoneSheet.value?.slot || slot;
  const snapshot = state.snapshot.value;
  const needs = (snapshot ? nowView(snapshot).needs : 0) + gateItems(processSource.context.value).length;
  const mail = messagesBadgeView(snapshot);
  // What the nav's badges add up to, for the ▾/▴ toggle of a view that has
  // hidden the nav (js/phone-nav-toggle.js).
  const waiting = needs + (mail.hidden ? 0 : Number(snapshot?.messages_unread || 0) + Number(snapshot?.access_requests_pending || 0));
  useEffect(() => { navBadge.value = waiting; }, [waiting]);
  const connection = state.connection.value.status;
  // A pushed page (js/phone-page.js): back, title and actions in the top
  // bar, and maybe no bottom nav. A page shows over the view it was pushed
  // on: a tab's page under no own view, a task's page over Board.
  const pushed = phonePage.value;
  const page = pushed && (pushed.view || '') === (OWN_VIEWS.includes(view) ? view : '') ? pushed : null;
  const hideNav = !!page?.hideNav;
  // A terminal docked over the dashboard shows the nav under it unless the
  // operator hid it there, or the phone's keyboard is up (the nav would sit
  // on the keys).
  const dock = terminalDock.value;
  const navOff = dock ? dock.keyboard || !navShown('terminal') : hideNav;
  useEffect(() => {
    const root = document.documentElement;
    if (dock && !navOff) root.dataset.phoneTerminalNav = '';
    else delete root.dataset.phoneTerminalNav;
  }, [!!dock, navOff]);
  // A view a phone does not offer (js/phone-desktop-only.js) gets a page
  // that says so in place of its desktop content.
  const routeMap = featureState('groups')?.subview?.value === 'routes';
  const subview = activeTab === 'groups' ? (routeMap ? 'routes' : '') : activeTab === 'diagnostics' ? activitySubtab.value : '';
  // Performance is desktop only on a phone: once the operator leaves it,
  // Diagnostics opens on the Doctor again, not on that page (nor with its
  // islands polling behind it).
  useEffect(() => {
    if (activeTab !== 'diagnostics' && activitySubtab.peek() === 'performance') setActivitySubtab('doctor');
  }, [activeTab]);
  const desktopOnly = OWN_VIEWS.includes(view) ? '' : desktopOnlyView(activeTab, subview);
  const covering = OWN_VIEWS.includes(view) ? view : desktopOnly ? 'desktop-only' : '';
  // A root view's top bar: the shell's own furniture for its slot, and what
  // the view on screen added (phoneBar, js/phone-page.js). A desktop-only
  // page has neither.
  const onScreen = OWN_VIEWS.includes(view) ? view : activeTab;
  const own = desktopOnly ? {} : rootBarFor(slot);
  const added = desktopOnly ? null : barFor(phoneBar.value, onScreen);

  // Any tab activation after the first render clears the own view, so a deep
  // link, Back, or a link inside a view shows its tab; unless Back restored
  // the view over that tab (Back onto Now, phone-history.js).
  const seenTab = useRef(activeTab);
  useEffect(() => installPhoneHistory({ view: shell.view, tab: () => state.activeTab.peek() }), [shell]);
  // A Groups run link opens the run in Automations (phone-process-runs.js),
  // once the router has finished landing on it.
  useEffect(() => {
    const onRestore = (event) => {
      const loc = event.detail?.location;
      if (loc?.tab !== 'groups' || loc.subtab !== 'runs') return;
      setTimeout(() => {
        // The hidden desktop Groups island took the link as its single-run
        // focus; left set, the next Agents tap would record the run link.
        featureState('groups')?.setFocusedRun?.(null);
        openRunLink(loc, { open: () => openAutomationsEntry(PROCESSES_SUBTAB) });
      }, 0);
    };
    document.addEventListener('tclaude:restore-location', onRestore);
    return () => document.removeEventListener('tclaude:restore-location', onRestore);
  }, []);
  useEffect(() => installKeyboardInset(), []);
  useEffect(() => installInputModality(), []);
  // Installed as an app, a pull down on the header reloads (Chrome's own
  // pull-to-refresh is off in a standalone window).
  const [pull, setPull] = useState(null);
  useEffect(() => installHeaderPull({ onPull: setPull }), []);
  useEffect(() => {
    if (seenTab.current === activeTab) return;
    seenTab.current = activeTab;
    if (shell.view.peek() && keptView(activeTab) === shell.view.peek()) return;
    shell.view.value = null;
  }, [activeTab]);
  useEffect(() => {
    const root = document.documentElement;
    if (covering) root.dataset.phoneView = covering;
    else delete root.dataset.phoneView;
  }, [covering]);
  useEffect(() => {
    const root = document.documentElement;
    if (hideNav) root.dataset.phonePage = 'full';
    else delete root.dataset.phonePage;
  }, [hideNav]);

  // What the views and the More sheet do. Anything that goes to a tab leaves
  // the own view first: the tab may already be the active one (Groups under a
  // fresh Now), and then no tab change would clear it.
  // A terminal is not a tab: it docks over the view (phone-terminal-island.js),
  // and closing it comes back to where it was opened.
  const viewActions = useMemo(() => {
    const leave = (run) => (...args) => {
      shell.view.value = null;
      closePhoneSheet();
      return run(...args);
    };
    return {
      ...actions,
      navigate: leave(actions.navigate),
      // More's anchor-opened entries (Fleet) may name the tab already under
      // Now or Board: leaving the own view is what shows it.
      showTab: leave(actions.showTab),
      openAccessRequest: leave(actions.openAccessRequest),
      // An agent's menu (a hold or ⋯ on Now). What in it goes to a tab
      // (Grant sudo…) leaves the own view first, like everything else here.
      agentMenu: actions.agentMenu ? (agent) => actions.agentMenu(agent, { navigate: leave(actions.navigate) }) : null,
      // A task opens on its phone page over the Board, never in the
      // desktop's task form.
      openTask: (id, group) => {
        closePhoneSheet();
        openBoardTask(id, group);
        shell.view.value = 'board';
      },
    };
  }, [actions, shell]);

  // A view outside the shell asked for the Board (Agents' "Board ›",
  // phone-board.js openBoardGroup): it comes up over the tab.
  const boardAsked = useRef(boardRequest.peek());
  const boardAsk = boardRequest.value;
  useEffect(() => {
    if (boardAsk === boardAsked.current) return;
    boardAsked.current = boardAsk;
    closePhoneSheet();
    shell.view.value = 'board';
  }, [boardAsk, shell]);

  // A tab's pushed page goes when its tab does (the view takes it down when
  // it goes inactive; clearing it here as well covers a view that does not).
  // Picking the tab that is already showing keeps it: nothing would put it
  // back, and the page would lose its back button.
  function pick(target) {
    // More's sheet sits above the nav: More again closes it.
    const wasMore = phoneSheet.peek()?.slot === 'more';
    // A view picked under a terminal takes the terminal down first.
    terminalDock.peek()?.close();
    closePhoneSheet();
    if (target.id === 'more') {
      if (!wasMore) openPhoneSheet({ label: 'More', Body: MoreMenu, props: { state, actions: viewActions, fleet }, overNav: true, slot: 'more' });
      return;
    }
    if (OWN_VIEWS.includes(target.id)) {
      shell.view.value = target.id;
      return;
    }
    shell.view.value = null;
    if (target.tab !== activeTab) phonePage.value = null;
    // Agents shows the agent list, never the desktop-only route map a deep
    // link left Groups on: the bare Groups location takes it back to Members.
    if (target.tab === 'groups' && routeMap) {
      actions.navigate('/');
      return;
    }
    actions.showTab(target.tab);
  }

  const openSheet = (label, Body, props = {}) => {
    closePhoneSheet();
    openPhoneSheet({ label, Body, props: { state, actions: viewActions, ...props } });
  };
  const title = desktopOnly || titleFor(view, activeTab);
  // A desktop-only page over a tab with a phone view of its own offers it,
  // both as a button on the page and as the bar's ‹ (as in the mock).
  const desktopOther = !desktopOnly ? null
    : activeTab === 'groups' ? { label: 'Show agents', onClick: () => viewActions.navigate('/') }
      : activeTab === 'map' ? { label: 'Show fleet', onClick: () => actions.showTab('fleet-admin') }
        : activeTab === 'diagnostics' ? { label: 'Open the Doctor', onClick: () => viewActions.navigate('/diagnostics/doctor') }
          : null;

  const badge = (id) => {
    if (id === 'now' && needs > 0) return html`<span class="ui-badge ui-badge-danger phone-nav-badge">${needs}</span>`;
    if (id === 'mail' && !mail.hidden) return html`<span class="ui-badge ui-badge-accent phone-nav-badge">${mail.text}</span>`;
    return null;
  };

  const hint = pull === null ? null : pullHint(pull);
  return html`<div class="phone-shell">
    ${hint && html`<div class=${`phone-pull${hint.ready ? ' ready' : ''}`} role="status" aria-live="polite"
      style=${`--phone-pull:${hint.offset}px`}>${hint.text}</div>`}
    <header class=${`phone-bar${page ? ' phone-bar-page' : ''}`}>
      ${page ? html`<button data-ui-exempt type="button" class="phone-bar-back" aria-label="Back" onClick=${() => page.onBack?.()}><${Chevron} dir="left" size=${22} /></button>
        <h1 class="phone-bar-title">${page.title || ''}</h1>
        ${(page.actions || []).map((act) => html`<button data-ui-exempt key=${act.key || act.label} type="button"
          class=${`phone-bar-act${act.danger ? ' danger' : ''}`} aria-label=${act.label} title=${act.label}
          disabled=${!!act.disabled} aria-disabled=${act.muted ? 'true' : undefined} onClick=${act.onClick}>${act.icon || act.label}</button>`)}`
      : html`${desktopOther
          ? html`<button data-ui-exempt type="button" class="phone-bar-back" aria-label=${`Back: ${desktopOther.label}`} onClick=${desktopOther.onClick}><${Chevron} dir="left" size=${22} /></button>`
          : html`<img class="phone-bar-logo" src=${LOGO_SRC} alt="" />`}
        <h1 class="phone-bar-title">${added?.menu
          ? html`<button data-ui-exempt type="button" class="phone-bar-title-btn" aria-haspopup="dialog"
              aria-label=${`${title}${added.menu.sub ? ` ${added.menu.sub}` : ''}, ${added.menu.label || 'pick'}`} onClick=${added.menu.onClick}>
              ${title}<small>${added.menu.sub ? ` ${added.menu.sub}` : ''} ▾</small></button>`
          : title}</h1>
        ${(added?.actions || []).map((act) => (act.text
          ? html`<button data-ui-exempt key=${act.key || act.label} type="button" class=${`phone-bar-act${act.danger ? ' danger' : ''}`}
              aria-label=${act.label} title=${act.label} disabled=${!!act.disabled} onClick=${act.onClick}>${act.text}</button>`
          : html`<${IconButton} key=${act.key || act.label} icon=${act.icon || act.label} label=${act.label}
              danger=${!!act.danger} disabled=${!!act.disabled} badge=${act.badge} dot=${!!act.dot} onClick=${act.onClick} />`))}
        ${own.node && html`<${NodeChip} fleet=${fleet} />`}
        <span class=${`phone-bar-live ${connection}`} role="status" title=${connection === 'connected' ? 'Connected' : connection}
          aria-label=${connection === 'connected' ? 'Connected' : `Connection: ${connection}`}></span>
        ${own.search && html`<${IconButton} icon="🔍" label="Search" aria-haspopup="dialog"
          onClick=${() => openSheet('Search', SearchSheet, { boardSource })} />`}`}
    </header>
    ${view === 'now' && html`<div class="phone-view" role="region" aria-label="Now"><${NowView} state=${state} actions=${viewActions} fetchImpl=${fetchImpl} processSource=${processSource} /></div>`}
    ${view === 'board' && html`<div class="phone-view" role="region" aria-label="Board"><${BoardView} source=${boardSource} state=${state} actions=${viewActions} fetchImpl=${fetchImpl} /></div>`}
    ${desktopOnly && html`<div class="phone-view" role="region" aria-label=${desktopOnly}><${DesktopOnlyPage} what=${desktopOnly} open=${actions.desktopSite}
      other=${desktopOther} /></div>`}
    ${!navOff && html`<nav class="phone-nav" aria-label="Views">
      ${PHONE_SLOTS.map((target) => html`<button data-ui-exempt key=${target.id} type="button"
        class=${`phone-nav-item${lit === target.id ? ' on' : ''}`} aria-current=${lit === target.id ? 'page' : undefined}
        aria-haspopup=${target.id === 'more' ? 'dialog' : undefined}
        aria-expanded=${target.id === 'more' ? String(lit === 'more' && !!phoneSheet.value) : undefined} onClick=${() => pick(target)}>
        <span class="phone-nav-icon" aria-hidden="true">${NAV_ICONS[target.icon] || target.icon}</span>${target.label}${badge(target.id)}
      </button>`)}
    </nav>`}
    ${STEWARD_FAB_SLOTS.includes(slot) && !page && !dock && !desktopOnly
      && html`<${StewardFab} snapshot=${snapshot} actions=${viewActions} fetchImpl=${fetchImpl} />`}
    <${PhoneSheetHost} />
  </div>`;
}

export function mountPhoneShellIsland({ host, shell, state, actions, boardSource, registerCleanup }) {
  render(html`<${PhoneShell} shell=${shell} state=${state} actions=${actions} boardSource=${boardSource} />`, host);
  registerCleanup(() => {
    render(null, host);
    phonePage.value = null;
    phoneBar.value = null;
    closePhoneSheet();
    delete document.documentElement.dataset.phoneView;
    delete document.documentElement.dataset.phonePage;
    delete document.documentElement.dataset.phoneTerminalNav;
  });
}
