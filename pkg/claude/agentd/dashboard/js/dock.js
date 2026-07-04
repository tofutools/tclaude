// dock.js — the retractable right-side PALETTE DOCK (JOH-374).
//
// A vertical dock pinned to the right edge of the dashboard listing what
// you can drop onto a group: your spawn PROFILES, your group TEMPLATES
// (summoning circles) and your ROLES (classes). The panel SHELL + card
// rendering live here; the DRAG behaviour (dragstart/dragover/drop) lives in
// dock-dnd.js. All three kinds are drag sources now: profile + role cards
// drop onto a group to open the spawn dialog prefilled (JOH-375 2/4);
// template cards drop onto a group to open the unified summon dialog with a
// drop-mode chooser (reinforce the group / new group in its image), or onto
// empty space for a plain "new party from circle" open (JOH-377 4/4).
//
// NB the name: js/palette.js is already taken (the Ctrl/Cmd-K command
// palette), so this module + its CSS/ids live under the `dock` namespace
// (#agent-dock, .dock-*), NOT .palette-*.
//
// Design intent (operator, 2026-07-04): this dock is a FOUNDATION, not a
// one-off — future editors and agent-work-graph attach points are meant to
// grow from it. So the three sections are instances of ONE data-driven
// idiom (a section = title + item list + card renderer; a card = key, icon,
// name, chips, actions) rather than three hand-rolled blocks — a fourth
// item kind slots in by adding one SECTIONS entry.
//
// Data rides the 2s poll: the daemon carries the profile / template / role
// registries on the snapshot (dashboard.go), so renderDock() reads
// lastSnapshot and paints through the keyed morphInto reconciler — a
// manager edit shows up on the next tick, and selections/scroll survive the
// re-render. The shell (#agent-dock) + the edge toggle are STATIC markup in
// dashboard.html, so they are never morphed and survive every poll; only
// #dock-body's inner sections are reconciled.
//
// Collapse/expand is persisted server-side via dashPrefs (NOT localStorage,
// which the random per-start port would reset) under DOCK_OPEN_KEY. The
// open state is a body class so CSS can reflow <main> to reclaim the space
// when collapsed rather than overlaying dead area.

import { $, esc } from './helpers.js';
import { morphInto } from './morph.js';
import { wizWord } from './slop.js';
import { dashPrefs } from './prefs.js';
import { lastSnapshot } from './dashboard.js';
import { syncFullBleedBars } from './hscroll.js';
// The compact one-line summaries live in the DATA modules (profiles.js /
// roles.js); the editor/manager openers live in the MODAL modules. Importing
// each from the module that actually exports it — a bad named import would
// abort the whole ES-module graph at link time (node --check can't catch that,
// it's single-file only).
import { profileSummary } from './profiles.js';
import { openProfileEditor, openProfilesManageModal } from './modal-profiles.js';
import { roleSummary } from './roles.js';
import { openRoleEditor, openRolesManageModal } from './modal-roles.js';
import { templateReadbackBadges, openTemplatesManageModal, openTemplateEditor } from './modal-templates.js';

// The persisted open/collapsed flag. dash-namespaced like every other
// server-backed dashboard pref. Default OPEN (see isDockOpen): the dock is a
// new surface and discoverability beats density here.
const DOCK_OPEN_KEY = 'tclaude.dash.dock.open';

// summaryChips turns a profileSummary/roleSummary "·"-joined string into a
// few compact chip spans — the profile/role twin of the template's
// roster-shape badges. Capped so a rich profile doesn't blow out the narrow
// dock; the tooltip on the card name carries the full picture.
function summaryChips(summary, max = 4) {
  const parts = String(summary || '')
    .split('·')
    .map(s => s.trim())
    .filter(Boolean);
  if (!parts.length) return '';
  const shown = parts.slice(0, max);
  const extra = parts.length - shown.length;
  const chips = shown.map(p => `<span class="dock-chip">${esc(p)}</span>`);
  if (extra > 0) chips.push(`<span class="dock-chip dock-chip-more">+${extra}</span>`);
  return chips.join(' ');
}

// SECTIONS — the whole dock is three instances of this one shape. To add a
// fourth item kind (a future editor / work-graph node), add an entry here;
// the renderer + the delegated click handler are already generic.
//
//   key         stable section id (data-key + the snapshot field name)
//   icon        per-card leading glyph
//   title()     the section heading (both vocab modes via wizWord)
//   empty()     the quiet "(none yet)" line when the list is empty
//   items(snap) the item array off the live snapshot
//   name(item)  the card's display name
//   chips(item) the card's chip HTML (already escaped)
//   drag        true → cards are drag SOURCES (draggable, wired by dock-dnd.js)
//   onManageItem(item)  jump to that item's editor / manager overlay
//   onManageAll()       jump to the whole-kind manager overlay
//
// `drag` gates the draggable attribute (dock-dnd.js's dragstart still keys off
// data-dock-kind): all three kinds drop onto a group — profiles + roles open
// the spawn dialog prefilled (JOH-375 2/4), templates open the unified summon
// dialog with a drop-mode chooser (JOH-377 4/4).
const SECTIONS = [
  {
    key: 'profiles',
    icon: '⚙',
    // Full section name (JOH-390 item 5): the operator wants the profiles
    // heading spelled out — "Agent profiles" / "Familiar patterns" — rather
    // than the bare "Profiles" / "Patterns". Roles keeps its short heading.
    title: () => wizWord('Agent profiles', 'Familiar patterns'),
    empty: () => wizWord('no profiles yet', 'no patterns yet'),
    items: (snap) => (snap && snap.profiles) || [],
    name: (p) => p.name,
    chips: (p) => summaryChips(profileSummary(p)),
    drag: true,
    onManageItem: (p) => openProfileEditor(p),
    onManageAll: () => openProfilesManageModal(),
  },
  {
    key: 'templates',
    icon: '🧩',
    // Spelled out like the profiles heading (operator follow-up to JOH-390
    // item 5): these are the GROUP templates, and "Summoning circles" is
    // already the full arcane name.
    title: () => wizWord('Group templates', 'Summoning circles'),
    empty: () => wizWord('no templates yet', 'no circles yet'),
    items: (snap) => (snap && snap.templates) || [],
    name: (t) => t.name,
    chips: (t) => templateReadbackBadges(t),
    drag: true,
    // The per-card ⚙ deep-links into THIS template's editor (JOH-390 item 6),
    // matching the profiles/roles cards — it used to fall back to the whole-kind
    // manager, which ignored the item.
    onManageItem: (t) => openTemplateEditor(t),
    onManageAll: () => openTemplatesManageModal(),
  },
  {
    key: 'roles',
    icon: '🎭',
    title: () => wizWord('Roles', 'Classes'),
    empty: () => wizWord('no roles yet', 'no classes yet'),
    items: (snap) => (snap && snap.roles) || [],
    name: (rl) => rl.name,
    chips: (rl) => summaryChips(roleSummary(rl)),
    drag: true,
    onManageItem: (rl) => openRoleEditor(rl),
    onManageAll: () => openRolesManageModal(),
  },
];

// sectionByKey resolves a section config from its key (the delegated click
// handler reads data-dock-kind off the card / button).
function sectionByKey(key) {
  return SECTIONS.find(s => s.key === key) || null;
}

// cardHTML renders one card: a grip handle, the leading icon, the name, a
// compact chip row, and a ⚙ manage affordance that jumps to the item's editor.
// The card carries data-dock-kind / data-dock-name — dock-dnd.js reads them off
// dragstart. A section flagged `drag` makes its cards drag SOURCES
// (draggable="true"); a future non-drag section (an editor / work-graph node)
// would leave `drag` unset and fall back to the "(coming soon)" grip hint. All
// three current kinds — profiles, templates, roles — are drag sources.
function cardHTML(section, item) {
  const name = section.name(item);
  const chips = section.chips(item) || '';
  const draggable = section.drag ? 'true' : 'false';
  const gripTitle = section.drag
    ? wizWord('drag onto a group to spawn', 'drag onto a party to summon')
    : wizWord('drag onto a group (coming soon)', 'drag onto a party (coming soon)');
  return `<div class="dock-card" draggable="${draggable}" data-key="${esc(name)}" data-dock-kind="${esc(section.key)}" data-dock-name="${esc(name)}" title="${esc(name)}">
    <span class="dock-grip" aria-hidden="true" title="${gripTitle}">⠿</span>
    <span class="dock-card-icon" aria-hidden="true">${section.icon}</span>
    <span class="dock-card-body">
      <span class="dock-card-name">${esc(name)}</span>
      ${chips ? `<span class="dock-chips">${chips}</span>` : ''}
    </span>
    <button type="button" class="dock-card-manage" data-dock-act="manage-item" data-dock-kind="${esc(section.key)}" data-dock-name="${esc(name)}" title="${wizWord('Edit this item', 'Edit this item')}" aria-label="${wizWord('Edit', 'Edit')} ${esc(name)}">⚙</button>
  </div>`;
}

// The per-section collapse flag lives under this dashPrefs prefix (req 5),
// server-backed like the open/collapsed dock flag itself — NOT localStorage,
// which the random per-start port would reset. Default EXPANDED (see
// isSectionOpen): a collapsed '0' is the only stored value, mirroring the
// groups' per-group fold key idiom.
const DOCK_SECTION_KEY = 'tclaude.dash.dock.section.';

// isSectionOpen reads a section's persisted collapse flag, defaulting to OPEN
// (only an explicit '0' collapses) — the three kinds stay discoverable by
// default, and a deliberate collapse survives restarts.
function isSectionOpen(key) {
  return dashPrefs.getItem(DOCK_SECTION_KEY + key) !== '0';
}

// sectionHTML renders one whole section: a heading with a ⧉ manage… jump,
// then the keyed card list (or a quiet empty line — sections never hide, so
// the three kinds are always discoverable).
//
// A <details>, NOT a plain <div> (req 5): each category collapses/expands on
// its own, and native <details> gives us keyboard + a11y for free. The `open`
// attribute is seeded from the persisted flag; the morph reconciler treats
// <details open> as LIVE-owned (js/morph.js) so a fold survives every 2s tick,
// and bindDock's toggle listener writes the flag back — live and fresh always
// agree. (It's a <details>, not a <section>, so the dashboard's global
// `section { display:none }` tab-pane rule can't hide it.)
function sectionHTML(section, snap) {
  const items = section.items(snap);
  const body = items.length
    ? items.map(it => cardHTML(section, it)).join('')
    : `<div class="dock-empty">(${esc(section.empty())})</div>`;
  const open = isSectionOpen(section.key) ? ' open' : '';
  return `<details class="dock-section" data-key="${esc(section.key)}"${open}>
    <summary class="dock-section-head">
      <span class="dock-section-title"><span class="dock-section-chevron" aria-hidden="true">▸</span><span class="dock-section-icon" aria-hidden="true">${section.icon}</span> ${esc(section.title())} <span class="dock-section-count">${items.length}</span></span>
      <button type="button" class="dock-section-manage" data-dock-act="manage-all" data-dock-kind="${esc(section.key)}" title="${wizWord('Open the manager for this kind', 'Open the manager for this kind')}">⧉</button>
    </summary>
    <div class="dock-section-items">${body}</div>
  </details>`;
}

// renderDock repaints #dock-body from the live snapshot through morphInto —
// called every 2s poll from refresh.js. Keys are stable (section key + item
// name) so selections/scroll survive the reconcile and no duplicate sibling
// keys corrupt the match (names are unique within a kind). A no-op when the
// dock shell isn't present.
export function renderDock() {
  const body = $('#dock-body');
  if (!body) return;
  const snap = lastSnapshot;
  morphInto(body, SECTIONS.map(s => sectionHTML(s, snap)).join(''));
}

// isDockOpen reads the persisted flag, defaulting to OPEN when unset (the
// dock is a new, discovery-worthy surface). Only an explicit '0' collapses.
function isDockOpen() {
  return dashPrefs.getItem(DOCK_OPEN_KEY) !== '0';
}

// isDockTab reports whether the Groups tab is the active one. The dock is
// offered ONLY there: its cards drag onto GROUP rows (dock-dnd.js), so the
// palette is meaningless on Jobs / Access / Config / Costs / … . We read the
// pane's `.active` class — the same source of truth every tab-switch site
// writes (bindTabs, the costs/plugins auto-hide redirects, showAccessTab, the
// command palette, keyboard cycling) — so a single observer over it (bindDock)
// catches every path without hooking each one. When Groups isn't active,
// applyDockOpen forces the effective open state off (no reserved page space)
// and CSS (body:not(.dock-tab)) hides the whole shell, edge toggle included.
function isDockTab() {
  return !!document.getElementById('tab-groups')?.classList.contains('active');
}

// applyDockOpen reflects the open state onto the body class (CSS reflows the
// page to reclaim the space when collapsed) and keeps the two show/hide controls
// in sync: the edge tab and the in-dock collapse button both mirror one state
// (JOH-390 item 7 removed the third, top-bar, toggle). It also re-homes the
// groups-toolbar globals (item 4) and re-syncs the dock top-inset, since the
// reserved space changes with the open state.
//
// The dock is Groups-tab-only: the tab availability rides a `dock-tab` body
// class (CSS hides the whole shell — panel + edge toggle — off the Groups tab)
// and folds into the EFFECTIVE open state, so no page space is reserved while
// the dock is hidden. `open` here is still the persisted PREF (isDockOpen),
// left untouched, so returning to Groups restores whatever open/collapsed state
// the human last chose. Called on boot, on the toggle, and on every tab switch
// (bindDock's Groups-pane observer) so the gate re-evaluates each time.
function applyDockOpen(open) {
  const onTab = isDockTab();
  document.body.classList.toggle('dock-tab', onTab);
  const eff = open && onTab;
  document.body.classList.toggle('dock-open', eff);
  const edge = $('#dock-toggle');
  if (edge) {
    edge.setAttribute('aria-expanded', eff ? 'true' : 'false');
    edge.title = eff
      ? wizWord('Collapse the palette', 'Furl the grimoire')
      : wizWord('Expand the palette', 'Unfurl the grimoire');
  }
  // Re-home the groups-toolbar globals into the open dock's head (JOH-390 item 4)
  // / return them to the toolbar when collapsed OR off the Groups tab. Done here
  // so it tracks EVERY effective-open change (boot, toggle, tab switch) in
  // lockstep with the body class. Off-tab both homes are hidden (the toolbar
  // lives in the inactive #tab-groups pane), so the move is invisible churn.
  syncDockActions(eff);
  syncDockTop();
  // Toggling the dock changes the reserved width and whether the horizontal
  // clearance spacer should be parked (req 3), but mutates no <main> child — so
  // hscroll's MutationObserver won't fire. Nudge it directly so the spacer +
  // full-bleed bars re-fit in the same frame.
  syncFullBleedBars();
}

// The groups-toolbar globals re-homed into the open dock's head (JOH-390 item 4):
// the "+ new group" primary, the ⚙ more-actions cog (+ its .action-menu) and the
// 🧠 dashboard-default-profile chip. While the dock is OPEN they live in the dock
// head (row 1 = new-group + cog; row 2 = the profile chip); collapsed, they go
// back to their exact toolbar slots so the filter bar renders as before.
//
// We MOVE the live DOM nodes (not clones), so every listener rides along:
// id-bound ones (#group-create-open's click) stay attached to the element across
// the move; the cog + chip run off document-level delegated handlers (data-act /
// the .action-menu cog bus) that don't care where the node lives. The toolbar
// filter bar + the dock head are both STATIC markup (the poll only morphs
// #dock-body / #groups-list), so nothing re-creates or clobbers the moved nodes.
//
// The cog's .action-menu still anchors to .filter-bar-cog (position:relative
// rides along) and opens downward INTO the dock body; at the header's top it
// stays within #agent-dock's box, so .dock-inner's overflow:hidden never clips it.
const DOCK_ACTION_SPECS = [
  { sel: '#group-create-open', dock: '#dock-actions-primary' },
  { sel: '.filter-bar-cog', dock: '#dock-actions-primary' },
  { sel: '#dashboard-default-profile', dock: '#dock-actions-profile' },
];
let dockActionHomes = null;
let lastDockActionsOpen = null;
function syncDockActions(open) {
  // Only act when the dock-open bit actually FLIPPED — the class observer below
  // fires on every body-class mutation (hscroll flags, dock-anim, wizard, …), so
  // this guard keeps those to a cheap no-op and prevents a redundant re-append
  // that could reorder the primary row.
  if (open === lastDockActionsOpen) return;
  lastDockActionsOpen = open;
  // Capture each control's ORIGINAL toolbar slot once, before the first move.
  // The nextSibling anchors are the inter-control whitespace text nodes, which
  // never move — so insertBefore restores the exact slot regardless of the order
  // we process the controls in.
  if (!dockActionHomes) {
    dockActionHomes = DOCK_ACTION_SPECS.map((s) => {
      const el = $(s.sel);
      if (!el) return null;
      return { el, dock: $(s.dock), home: el.parentNode, next: el.nextSibling };
    });
  }
  for (const h of dockActionHomes) {
    if (!h || !h.el) continue;
    if (open) {
      // Append in spec order → primary row becomes [+ new group][⚙ cog]. Guarded
      // so a repeat call (idempotent re-apply) doesn't re-append and reorder.
      if (h.dock && h.el.parentNode !== h.dock) h.dock.appendChild(h.el);
    } else if (h.home && h.el.parentNode !== h.home) {
      h.home.insertBefore(h.el, h.next);
    }
  }
}

// syncDockTop keeps the fixed dock rail spanning ONLY the content area —
// below the top bar (header + marquee + nav) and above the fixed footer
// (req 1) — rather than covering the header's right-side controls as the
// old top:0 rail did. The chrome scrolls away with the page (it isn't
// sticky — making it sticky would spin up a stacking context that re-scopes
// the header popovers, a documented no-go), so we can't pin the top to a
// constant: instead --dock-top tracks nav's live viewport-bottom, clamped at
// 0. At rest it sits just under the nav; as the page scrolls down and the
// chrome leaves, it rises to fill the full height (where the content is now
// full-height too). Cheap: one getBoundingClientRect, rAF-coalesced. The
// bottom is pinned in CSS to the footer bar.
let dockTopScheduled = false;
function syncDockTop() {
  if (dockTopScheduled) return;
  dockTopScheduled = true;
  requestAnimationFrame(() => {
    dockTopScheduled = false;
    const nav = document.querySelector('nav');
    const navBottom = nav ? nav.getBoundingClientRect().bottom : 0;
    document.documentElement.style.setProperty('--dock-top', Math.max(0, navBottom) + 'px');
  });
}

// bindDock wires the edge toggle + seeds the open state from dashPrefs. Must
// run after initDashPrefs so the persisted flag is already loaded. The
// toggle button + shell are static HTML, so this binds once and survives
// every poll (renderDock only touches #dock-body's inner sections).
export function bindDock() {
  if (!$('#agent-dock')) return;
  // Re-home the toolbar globals off ANY change to body.dock-open, not only the
  // applyDockOpen path (JOH-390 item 4). The production toggle always routes
  // through applyDockOpen, but this keeps the controls correctly placed if the
  // class is flipped directly (e.g. the dashsnap visual harness sets it), so the
  // move can never desync from the visible open state. The change-guard in
  // syncDockActions makes the extra body-class mutations a no-op.
  new MutationObserver(() => syncDockActions(document.body.classList.contains('dock-open')))
    .observe(document.body, { attributes: true, attributeFilter: ['class'] });
  // The dock is Groups-tab-only (req): re-evaluate the gate on every tab switch.
  // Every tab-switch site toggles the `active` class on the <section> panes, so
  // one observer over the Groups pane's class catches them all — present and
  // future — without hooking each site. applyDockOpen re-reads isDockTab, so it
  // hides/shows the shell and drops/reserves the page space to match. (This
  // observes the Groups PANE, not body, so applyDockOpen's own body-class writes
  // can't retrigger it — no feedback loop with the observer above.)
  const groupsPane = document.getElementById('tab-groups');
  if (groupsPane) {
    new MutationObserver(() => applyDockOpen(isDockOpen()))
      .observe(groupsPane, { attributes: true, attributeFilter: ['class'] });
  }
  applyDockOpen(isDockOpen());
  // Enable the slide transition only AFTER the initial state is painted, so a
  // default-open dock doesn't flash-slide in on load (the CSS resting state is
  // collapsed). A rAF lands after the first paint of the applied state.
  requestAnimationFrame(() => document.body.classList.add('dock-anim'));

  // One toggler drives both show/hide controls — the edge tab and the in-dock
  // collapse — flipping the same dashPrefs-backed state (JOH-390 item 7 removed
  // the third, top-bar, control).
  const toggleDock = () => {
    const next = !isDockOpen();
    dashPrefs.setItem(DOCK_OPEN_KEY, next ? '1' : '0');
    applyDockOpen(next);
  };
  // Two show/hide controls now (JOH-390 item 7 removed the top-bar toggle): the
  // edge chevron tab and the in-dock "Hide ›/Furl ›" collapse.
  $('#dock-toggle')?.addEventListener('click', toggleDock);
  $('#dock-collapse')?.addEventListener('click', toggleDock);

  // Keep the content-area top-inset (req 1) fresh as the page scrolls the
  // chrome away and as the chrome's own height changes (slop marquee showing/
  // hiding, wrapping controls or tab strip, window resize). syncDockTop is
  // rAF-coalesced, so these can fire freely. Passive scroll listener — we never
  // preventDefault. Observe EVERY chrome bar, not just the header: --dock-top
  // tracks nav's bottom, which also moves when the marquee toggles between
  // header and nav or the nav strip wraps — neither resizes the header.
  window.addEventListener('scroll', syncDockTop, { passive: true });
  window.addEventListener('resize', syncDockTop);
  if ('ResizeObserver' in window) {
    const ro = new ResizeObserver(syncDockTop);
    for (const sel of ['header', '#slop-marquee', 'nav']) {
      const el = document.querySelector(sel);
      if (el) ro.observe(el);
    }
  }

  // Persist each section's collapse/expand (req 5). <details> only fires
  // `toggle` on itself (no bubbling), so a document-level capturing listener
  // catches every section without re-binding per render — the same idiom the
  // group <details> use (bindDetailsPersistence in refresh.js). Default is
  // EXPANDED, so we store only the '0' collapse and clear it on re-open.
  document.addEventListener('toggle', (e) => {
    const d = e.target;
    if (!(d instanceof HTMLDetailsElement) || !d.classList.contains('dock-section')) return;
    const key = d.getAttribute('data-key');
    if (!key) return;
    if (d.open) dashPrefs.removeItem(DOCK_SECTION_KEY + key);
    else dashPrefs.setItem(DOCK_SECTION_KEY + key, '0');
  }, true);

  // One delegated handler for every card / section manage affordance. The
  // section manage (⧉) button lives inside the <summary>, so a plain click on
  // it would ALSO toggle the <details>; preventDefault here cancels that native
  // fold (the delegated listener runs in the bubble phase, before the default
  // action) so the manager opens without collapsing the section.
  $('#dock-body')?.addEventListener('click', (e) => {
    const btn = e.target.closest('[data-dock-act]');
    if (!btn) return;
    const section = sectionByKey(btn.getAttribute('data-dock-kind'));
    if (!section) return;
    e.preventDefault();
    const act = btn.getAttribute('data-dock-act');
    if (act === 'manage-all') {
      section.onManageAll();
      return;
    }
    if (act === 'manage-item') {
      const name = btn.getAttribute('data-dock-name');
      const item = section.items(lastSnapshot).find(it => section.name(it) === name);
      // Fall back to the whole-kind manager if the item vanished between
      // paint and click (a concurrent delete on another tab).
      if (item) section.onManageItem(item);
      else section.onManageAll();
    }
  });

  // First paint now so the dock isn't blank until the first poll lands.
  renderDock();
}
