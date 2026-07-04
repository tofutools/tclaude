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
import { templateReadbackBadges, openTemplatesManageModal } from './modal-templates.js';

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
    title: () => wizWord('Profiles', 'Patterns'),
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
    title: () => wizWord('Templates', 'Summoning circles'),
    empty: () => wizWord('no templates yet', 'no circles yet'),
    items: (snap) => (snap && snap.templates) || [],
    name: (t) => t.name,
    chips: (t) => templateReadbackBadges(t),
    drag: true,
    onManageItem: () => openTemplatesManageModal(),
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

// applyDockOpen reflects the open state onto the body class (CSS reflows the
// page to reclaim the space when collapsed) and keeps every show/hide control
// in sync: the edge tab, the top-bar toggle (req 2) and the in-dock collapse
// button all mirror one state, so whichever the operator finds first reads
// correctly. The dock top-inset is re-synced too, since the reserved space
// changes with the open state.
function applyDockOpen(open) {
  document.body.classList.toggle('dock-open', open);
  const edge = $('#dock-toggle');
  if (edge) {
    edge.setAttribute('aria-expanded', open ? 'true' : 'false');
    edge.title = open
      ? wizWord('Collapse the palette', 'Furl the grimoire')
      : wizWord('Expand the palette', 'Unfurl the grimoire');
  }
  const top = $('#dock-toggle-top');
  if (top) top.setAttribute('aria-expanded', open ? 'true' : 'false');
  syncDockTop();
  // Toggling the dock swaps the horizontal scroll container (page ↔ body) and
  // changes the reserved width, but mutates no <main> child — so hscroll's
  // MutationObserver won't fire. Nudge it directly so the full-bleed bars
  // re-fit to the (now correct) scroll container in the same frame.
  syncFullBleedBars();
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
  applyDockOpen(isDockOpen());
  // Enable the slide transition only AFTER the initial state is painted, so a
  // default-open dock doesn't flash-slide in on load (the CSS resting state is
  // collapsed). A rAF lands after the first paint of the applied state.
  requestAnimationFrame(() => document.body.classList.add('dock-anim'));

  // One toggler drives the three show/hide controls (req 2) — the edge tab,
  // the top-bar button and the in-dock collapse — all flipping the same
  // dashPrefs-backed state.
  const toggleDock = () => {
    const next = !isDockOpen();
    dashPrefs.setItem(DOCK_OPEN_KEY, next ? '1' : '0');
    applyDockOpen(next);
  };
  $('#dock-toggle')?.addEventListener('click', toggleDock);
  $('#dock-toggle-top')?.addEventListener('click', toggleDock);
  $('#dock-collapse')?.addEventListener('click', toggleDock);

  // Keep the content-area top-inset (req 1) fresh as the page scrolls the
  // chrome away and as the chrome's own height changes (slop marquee, wrapping
  // controls, window resize). syncDockTop is rAF-coalesced, so these can fire
  // freely. Passive scroll listener — we never preventDefault.
  window.addEventListener('scroll', syncDockTop, { passive: true });
  window.addEventListener('resize', syncDockTop);
  const chrome = document.querySelector('header');
  if (chrome && 'ResizeObserver' in window) {
    new ResizeObserver(syncDockTop).observe(chrome);
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
