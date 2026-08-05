// menu-filter.js — type-to-filter for the dashboard's ⚙ cog menus.
//
// The Groups tab grew three cog menus that are now too long to scan: the
// toolbar cog (import / cleanup / profiles / templates / roles / …), the
// per-group header cog (~20 items) and the per-agent row cog (~25). This
// module turns each of them into a small command palette WITHOUT touching a
// single menu item: the items, their handlers, their order and their
// click-to-run behaviour are exactly what they were. A filter box above the
// list narrows which of them are displayed, and ↑/↓/Enter runs one from the
// keyboard.
//
// Two deliberate departures from the Ctrl/Cmd-K palette (palette.js):
//
//   - Order is PRESERVED WITHIN TWO MATCH GROUPS. A hit in the action's name
//     comes before one found only in its tooltip / data-act slug, so typing
//     "terminal" prefers "open web terminal" over an otherwise-earlier action
//     whose help text merely mentions a terminal. Within each group the menu's
//     familiar spatial order stays intact; scoreCommand's finer score ladder
//     does not continuously reshuffle the rows.
//   - The searchable text is READ FROM THE RENDERED DOM, not declared by each
//     item. That is not a shortcut, it is the only complete source: several
//     items (NotifyMenuItem, RemoteMenuItem, RestartMenuItem,
//     SandboxRestartMenuItem) compute their own label inside the component from
//     live member state, so nothing at the call site knows what they say.
//     Reading the rendered node means the filter matches what the human can
//     actually see, by construction, and can never drift from the labels.
//
// Everything else — the synonym expansion ("hide" finds "veil", "spawn" finds
// "summon") and the prefix-beats-mid-word score ladder — is scoreCommand() from
// palette-score.js, reused verbatim so both surfaces answer a query the same
// way.
//
// Ownership: the four attributes below are written ONLY here, and are absent
// from every menu item's vnode props. Preact therefore never diffs them, so
// applying a filter to a Preact-rendered menu cannot conflict with a snapshot
// re-render — the owner just re-applies after each one (ActionMenu does this in
// a layout effect). Presentation lives in dashboard.css; this module sets
// state, never style.
//
// Because those re-applies are frequent (one per ~2s snapshot publish), the
// cursor is preserved across them and only cleared when a caller explicitly
// asks via applyMenuFilter's resetActive — see the note there.
//
// These menus keep role="menu" / role="menuitem" rather than converting to
// combobox+listbox: the click path is a menu, ~55 items across three files
// would have to change, and Go guards assert the menu roles. The filter box is
// a combobox over that menu, linked with aria-controls so its
// aria-activedescendant resolves (linkFilterInput).

import { scoreCommand } from './palette-score.js';

export const MENU_ITEM_SELECTOR = '[role="menuitem"]';
export const MENU_SEPARATOR_SELECTOR = '[role="separator"]';

// Both cog owners show the same prompt, re-lettered in 🧙 wizard mode like the
// Ctrl/Cmd-K palette's placeholder. It lives here, with the shared behaviour,
// so the imperative toolbar cog does not have to import a Preact module for it.
export const MENU_FILTER_PLACEHOLDER = 'filter actions…';
export const MENU_FILTER_WIZARD_PLACEHOLDER = 'name your intent…';

// Set on an item the live query excludes (CSS hides it).
export const FILTERED_OUT_ATTR = 'data-menu-filtered-out';
// Set on the one item ↑/↓/Enter act on (CSS highlights it).
export const ACTIVE_ATTR = 'data-menu-active';
// Set on the menu when a query matches nothing (CSS renders the empty note).
export const EMPTY_ATTR = 'data-menu-empty';
// Set while a query is live so CSS and keyboard navigation agree on the two
// stable result groups: name hits first, descriptive-text hits second.
export const MATCH_PRIORITY_ATTR = 'data-menu-match-priority';
const NAME_MATCH_PRIORITY = 1;
const DETAIL_MATCH_PRIORITY = 2;

function toggleAttr(el, name, on) {
  if (on) el.setAttribute(name, '1');
  else el.removeAttribute(name);
}

// A disabled item is still listed — its title explains why it cannot run — but
// it is skipped by ↑/↓ and never becomes the Enter target. Preact assigns
// `disabled` as a DOM property, so check the property first and fall back to
// the attribute for statically-authored markup.
function isNavigable(item) {
  return !(item.disabled === true || item.hasAttribute('disabled'));
}

export function menuItems(menu) {
  return [...menu.querySelectorAll(MENU_ITEM_SELECTOR)];
}

function visibleItems(menu) {
  return menuItems(menu)
    .filter((item) => !item.hasAttribute(FILTERED_OUT_ATTR))
    .sort((a, b) => matchPriority(a) - matchPriority(b));
}

function matchPriority(item) {
  return Number(item.getAttribute(MATCH_PRIORITY_ATTR)) || 0;
}

// textPieces collects each text node separately instead of taking textContent,
// because every themed item renders BOTH its plain and its 🧙 wizard label as
// sibling spans (CSS shows one). textContent would fuse them into
// "delete groupdisband party" and invent a phrase that matches neither
// vocabulary; joining the pieces with a space keeps both searchable and leaves
// each label's own words contiguous.
function textPieces(node, out) {
  for (const child of node.childNodes || []) {
    if (child.nodeType === 3) {
      const text = (child.nodeValue || '').trim();
      if (text) out.push(text);
    } else if (child.nodeType === 1) {
      textPieces(child, out);
    }
  }
  return out;
}

// menuItemLabel is what the item SAYS — both theme vocabularies, so a query in
// either one scores against the label tiers rather than falling back to the
// weaker hint tiers.
export function menuItemLabel(item) {
  return textPieces(item, []).join(' ');
}

// menuItemHaystack is the label plus the item's own descriptive text: its
// `title` (these menus carry long, genuinely explanatory tooltips — the same
// role `hint` plays for a palette command) and its data-act slug with hyphens
// opened into words, so "sandbox-restart" is reachable by typing "restart
// sandbox" and "cleanup-worktrees-group" by "worktrees".
export function menuItemHaystack(item) {
  const act = (item.getAttribute('data-act') || '').replace(/[-_]+/g, ' ');
  return [menuItemLabel(item), item.getAttribute('title') || '', act]
    .filter(Boolean).join(' ');
}

// menuItemMatches takes an ALREADY normalised query (trimmed + lowercased) so a
// filter pass over 25 items normalises once rather than per item.
export function menuItemMatches(item, query) {
  if (!query) return true;
  return scoreCommand(query, menuItemLabel(item).toLowerCase(),
    menuItemHaystack(item).toLowerCase()) > 0;
}

// Return a coarse priority rather than the palette's full score. Cog menus
// retain their authored order among equally direct matches, while still making
// an action whose NAME answers the query easier to reach than an action found
// only through its explanatory metadata. Both plain and wizard labels count as
// names so either vocabulary behaves the same in either theme.
export function menuItemMatchPriority(item, query) {
  if (!query) return 0;
  const label = menuItemLabel(item).toLowerCase();
  if (scoreCommand(query, label, label) > 0) return NAME_MATCH_PRIORITY;
  return scoreCommand(query, label, menuItemHaystack(item).toLowerCase()) > 0
    ? DETAIL_MATCH_PRIORITY
    : 0;
}

export function menuActiveItem(menu) {
  const active = menu.querySelector(`${MENU_ITEM_SELECTOR}[${ACTIVE_ATTR}]`);
  if (!active) return null;
  // A re-render or a narrowed query can leave the marker on a node that is no
  // longer a candidate; treat that as "nothing selected".
  return !active.hasAttribute(FILTERED_OUT_ATTR) && isNavigable(active) ? active : null;
}

// The keyboard cursor has to be announceable, so the focused filter box points
// at it with aria-activedescendant. Menu items are authored without ids, so one
// is minted lazily on first selection; the counter is module-global and never
// reused, which keeps ids unique across every cog menu on the page.
//
// aria-activedescendant only resolves against a DESCENDANT of the element that
// carries it, or against something it aria-owns / aria-controls. The box is a
// SIBLING of the items, so linkFilterInput below supplies the aria-controls
// edge; without it the attribute would be written and read by nothing. This is
// the same relationship the Ctrl/Cmd-K palette declares between its input and
// #palette-list — the roles differ (these stay menu / menuitem so the click
// path keeps its menu semantics) but the reference has to resolve either way.
let idSeq = 0;
function mintId(node, prefix) {
  if (!node.id) node.id = `${prefix}-${++idSeq}`;
  return node.id;
}

function itemId(item) {
  return mintId(item, 'menu-item');
}

// linkFilterInput is idempotent — applyMenuFilter calls it on every pass.
export function linkFilterInput(menu, input) {
  if (!input) return;
  input.setAttribute('aria-controls', mintId(menu, 'action-menu'));
}

export function setMenuActive(menu, item, { input } = {}) {
  for (const other of menuItems(menu)) {
    if (other !== item) other.removeAttribute(ACTIVE_ATTR);
  }
  if (item) toggleAttr(item, ACTIVE_ATTR, true);
  if (!input) return;
  if (item) input.setAttribute('aria-activedescendant', itemId(item));
  else input.removeAttribute('aria-activedescendant');
}

// moveMenuActive walks the visible, enabled items. It WRAPS, matching the
// palette's ↑/↓ so every item stays reachable by repeated presses. With nothing
// selected yet, ↓ starts at the top and ↑ at the bottom.
export function moveMenuActive(menu, delta, { input } = {}) {
  const navigable = visibleItems(menu).filter(isNavigable);
  if (!navigable.length) return null;
  const current = menuActiveItem(menu);
  const index = current ? navigable.indexOf(current) : -1;
  const next = index < 0
    ? (delta > 0 ? 0 : navigable.length - 1)
    : (index + delta + navigable.length) % navigable.length;
  const item = navigable[next];
  setMenuActive(menu, item, { input });
  // Long menus scroll; keep the keyboard cursor on screen.
  if (typeof item.scrollIntoView === 'function') item.scrollIntoView({ block: 'nearest' });
  return item;
}

// applyMenuFilter is the single write path: it decides which items the query
// keeps, hides the separators while filtering (with an arbitrary subset of
// items showing, the original grouping they divide no longer means anything),
// flags an empty result, and leaves exactly one sensible Enter target — the
// topmost match whenever a query is live, so type-then-Enter works without a
// deliberate ↓ first.
// resetActive is for the open/close edges, where the menu must come up (and go
// away) with no cursor. Every other call is a RE-APPLY — the Preact cogs run one
// after each of their ~2s snapshot renders — and must leave a still-valid cursor
// exactly where the operator put it. Clearing on re-apply would silently undo
// ↑/↓ mid-use and leave Enter doing nothing.
export function applyMenuFilter(menu, query, { input, resetActive = false } = {}) {
  const normalized = String(query || '').trim().toLowerCase();
  linkFilterInput(menu, input);
  const items = menuItems(menu);
  let visible = [];
  for (const item of items) {
    const priority = menuItemMatchPriority(item, normalized);
    const matched = !normalized || priority > 0;
    toggleAttr(item, FILTERED_OUT_ATTR, !matched);
    if (priority) item.setAttribute(MATCH_PRIORITY_ATTR, String(priority));
    else item.removeAttribute(MATCH_PRIORITY_ATTR);
    if (matched) visible.push(item);
  }
  visible = visible.sort((a, b) => matchPriority(a) - matchPriority(b));
  for (const separator of menu.querySelectorAll(MENU_SEPARATOR_SELECTOR)) {
    toggleAttr(separator, FILTERED_OUT_ATTR, !!normalized);
  }
  toggleAttr(menu, EMPTY_ATTR, !!normalized && !visible.length);

  if (resetActive) setMenuActive(menu, null, { input });
  const navigable = visible.filter(isNavigable);
  // menuActiveItem returns null once the cursor's item is filtered out or
  // disabled, so a narrowing query naturally falls back to the new top match
  // while a cursor that survives the narrowing is kept.
  setMenuActive(menu, menuActiveItem(menu) || (normalized ? navigable[0] || null : null),
    { input });
  return { items, visible, navigable };
}

// handleMenuFilterKeyDown drives the filter input. Returns true when it
// consumed the key, so the caller can leave anything else to the browser (Tab,
// text editing) and to the menu's existing document-level handlers.
//
// Escape is two-stage on purpose: a mistyped query clears without throwing away
// the whole menu, and only a second Escape (or one on an empty box) falls
// through to the owner's document handler that closes the menu and restores
// focus to the cog.
export function handleMenuFilterKeyDown(menu, event, { hasQuery = false, clearQuery } = {}) {
  // The handler is bound to the filter box, so currentTarget is the element
  // that owns aria-activedescendant.
  const input = event.currentTarget || null;
  switch (event.key) {
    case 'ArrowDown':
      event.preventDefault();
      moveMenuActive(menu, 1, { input });
      return true;
    case 'ArrowUp':
      event.preventDefault();
      moveMenuActive(menu, -1, { input });
      return true;
    case 'Enter': {
      const item = menuActiveItem(menu);
      if (!item) return false;
      event.preventDefault();
      // Run it exactly the way a mouse would: the click travels through each
      // menu's own delegated routing and its close-on-item-click listener, so
      // the keyboard path adds no second way to invoke an action.
      item.click();
      return true;
    }
    case 'Escape':
      if (!hasQuery) return false;
      event.preventDefault();
      event.stopPropagation();
      // Clearing is a start-over: drop the cursor too, since the item under it
      // was picked by the query that is going away. Done here rather than in
      // each owner's clearQuery so both cogs agree on what Escape means.
      setMenuActive(menu, null, { input });
      clearQuery?.();
      return true;
    default:
      return false;
  }
}

// bindMenuHover keeps a single highlight on screen: the CSS :hover rule and the
// keyboard marker would otherwise light two rows at once. Moving the mouse
// takes the cursor over, exactly as it does in the Ctrl/Cmd-K palette.
// resolveInput is called per event rather than captured, because the filter box
// is rendered by the caller and may be replaced under it.
export function bindMenuHover(menu, { resolveInput = () => null } = {}) {
  const onOver = (event) => {
    const item = event.target?.closest?.(MENU_ITEM_SELECTOR);
    if (!item || !menu.contains(item)) return;
    if (item.hasAttribute(FILTERED_OUT_ATTR) || !isNavigable(item)) return;
    setMenuActive(menu, item, { input: resolveInput() });
  };
  menu.addEventListener('mouseover', onOver);
  return () => menu.removeEventListener('mouseover', onOver);
}
