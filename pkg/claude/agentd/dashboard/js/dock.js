// dock.js — the retractable right-side PALETTE DOCK (JOH-374).
//
// A vertical dock pinned to the right edge of the dashboard listing what
// you can drop onto a group: your spawn PROFILES, your group TEMPLATES
// (summoning circles) and your ROLES (classes). This ticket is the panel
// SHELL only — the cards render as future drag SOURCES (a grip glyph + a
// grab cursor + data-dock-kind/name hooks) but drag itself lands in the
// follow-up tickets (2/4 profile-drag, 4/4 template-drag).
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
import {
  profileSummary, openProfileEditor, openProfilesManageModal,
} from './modal-profiles.js';
import { roleSummary, openRoleEditor, openRolesManageModal } from './modal-roles.js';
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
//   onManageItem(item)  jump to that item's editor / manager overlay
//   onManageAll()       jump to the whole-kind manager overlay
const SECTIONS = [
  {
    key: 'profiles',
    icon: '⚙',
    title: () => wizWord('Profiles', 'Patterns'),
    empty: () => wizWord('no profiles yet', 'no patterns yet'),
    items: (snap) => (snap && snap.profiles) || [],
    name: (p) => p.name,
    chips: (p) => summaryChips(profileSummary(p)),
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
    onManageItem: (rl) => openRoleEditor(rl),
    onManageAll: () => openRolesManageModal(),
  },
];

// sectionByKey resolves a section config from its key (the delegated click
// handler reads data-dock-kind off the card / button).
function sectionByKey(key) {
  return SECTIONS.find(s => s.key === key) || null;
}

// cardHTML renders one draggable-source card: a grip handle, the leading
// icon, the name, a compact chip row, and a ⚙ manage affordance that jumps
// to the item's editor. The card carries data-dock-kind / data-dock-name so
// the follow-up DnD tickets can wire dragstart off these without re-touching
// this module. draggable is deliberately NOT set yet — this is the panel
// shell; drag lands in 2/4 and 4/4.
function cardHTML(section, item) {
  const name = section.name(item);
  const chips = section.chips(item) || '';
  return `<div class="dock-card" data-key="${esc(name)}" data-dock-kind="${esc(section.key)}" data-dock-name="${esc(name)}" title="${esc(name)}">
    <span class="dock-grip" aria-hidden="true" title="${wizWord('drag onto a group (coming soon)', 'drag onto a party (coming soon)')}">⠿</span>
    <span class="dock-card-icon" aria-hidden="true">${section.icon}</span>
    <span class="dock-card-body">
      <span class="dock-card-name">${esc(name)}</span>
      ${chips ? `<span class="dock-chips">${chips}</span>` : ''}
    </span>
    <button type="button" class="dock-card-manage" data-dock-act="manage-item" data-dock-kind="${esc(section.key)}" data-dock-name="${esc(name)}" title="${wizWord('Edit this item', 'Edit this item')}" aria-label="${wizWord('Edit', 'Edit')} ${esc(name)}">⚙</button>
  </div>`;
}

// sectionHTML renders one whole section: a heading with a ⧉ manage… jump,
// then the keyed card list (or a quiet empty line — sections never hide, so
// the three kinds are always discoverable).
function sectionHTML(section, snap) {
  const items = section.items(snap);
  const body = items.length
    ? items.map(it => cardHTML(section, it)).join('')
    : `<div class="dock-empty">(${esc(section.empty())})</div>`;
  return `<section class="dock-section" data-key="${esc(section.key)}">
    <div class="dock-section-head">
      <span class="dock-section-title"><span class="dock-section-icon" aria-hidden="true">${section.icon}</span> ${esc(section.title())} <span class="dock-section-count">${items.length}</span></span>
      <button type="button" class="dock-section-manage" data-dock-act="manage-all" data-dock-kind="${esc(section.key)}" title="${wizWord('Open the manager for this kind', 'Open the manager for this kind')}">⧉</button>
    </div>
    <div class="dock-section-items">${body}</div>
  </section>`;
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

// applyDockOpen reflects the open state onto the body class (CSS reflows
// <main> to reclaim the space when collapsed) and the toggle's ARIA/label.
function applyDockOpen(open) {
  document.body.classList.toggle('dock-open', open);
  const toggle = $('#dock-toggle');
  if (toggle) {
    toggle.setAttribute('aria-expanded', open ? 'true' : 'false');
    toggle.title = open
      ? wizWord('Collapse the palette', 'Furl the grimoire')
      : wizWord('Expand the palette', 'Unfurl the grimoire');
  }
}

// bindDock wires the edge toggle + seeds the open state from dashPrefs. Must
// run after initDashPrefs so the persisted flag is already loaded. The
// toggle button + shell are static HTML, so this binds once and survives
// every poll (renderDock only touches #dock-body's inner sections).
export function bindDock() {
  if (!$('#agent-dock')) return;
  applyDockOpen(isDockOpen());

  $('#dock-toggle')?.addEventListener('click', () => {
    const next = !isDockOpen();
    dashPrefs.setItem(DOCK_OPEN_KEY, next ? '1' : '0');
    applyDockOpen(next);
  });

  // One delegated handler for every card / section manage affordance.
  $('#dock-body')?.addEventListener('click', (e) => {
    const btn = e.target.closest('[data-dock-act]');
    if (!btn) return;
    const section = sectionByKey(btn.getAttribute('data-dock-kind'));
    if (!section) return;
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
