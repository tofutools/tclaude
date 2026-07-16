// list-paging.js — client pagination for the windowed server-paged lists:
// the Groups tab's Retired / Conversations / Replaced virtual groups.
//
// These three lists used to ship IN FULL inside every 2s /api/snapshot poll,
// so a few hundred retired agents meant an ever-growing payload on every tick.
// They now come from their own paginated endpoints — /api/retired,
// /api/conversations, /api/replaced — which refresh.js fetches in parallel with
// the snapshot while the corresponding virtual group is visible, then stitches
// the page back onto the snapshot object (so downstream renderers keep reading
// data.retired / data.conversations / data.replaced unchanged). This module
// owns the per-list offset/limit those fetches send and renders the pager footer
// the virtual groups show.
//
// Modals + cross-tab filters that need the COMPLETE list (bulk delete-retired,
// the cleanup tool, the add-member promote picker, the Messages-tab prev-gen
// folder filter) call the same endpoints WITHOUT offset/limit — the server's
// "limit == 0" path returns every row. See fetchListFull.

import { dashPrefs } from './prefs.js';

// Page sizes the per-list selector offers; mirrors the Mail / Audit tabs.
// Every value stays at or below the server's 500 cap.
export const PAGE_SIZES = [25, 50, 100, 200];
const DEFAULT_PAGE_SIZE = 50;

// limit is persisted per list (sticky across reloads); offset is session state
// — a fresh load starts on the newest page.
const offsets = { retired: 0, conversations: 0, replaced: 0 };

function sizeKey(kind) { return `tclaude.dash.list.${kind}.pagesize`; }

export function listLimit(kind) {
  const n = parseInt(dashPrefs.getItem(sizeKey(kind)) || '', 10);
  return PAGE_SIZES.includes(n) ? n : DEFAULT_PAGE_SIZE;
}

export function listOffset(kind) { return offsets[kind] || 0; }

// listParams builds the offset/limit/q query fragment the snapshot-tick fetch
// sends for one list. Always windowed — the modals call the same endpoints
// with no params to get the full set (fetchListFull). q (the Groups-tab filter
// box) is applied server-side so the filter searches the WHOLE list, not just
// the loaded page.
export function listParams(kind, q) {
  let s = `offset=${listOffset(kind)}&limit=${listLimit(kind)}`;
  if (q) s += `&q=${encodeURIComponent(q)}`;
  return s;
}

// fetchVisibleGroupListPages starts the windowed roster requests owned by the
// Groups tab. Keeping the visibility gate beside the request construction makes
// the no-poll contract directly testable: a hidden virtual group must not even
// invoke get(). The returned tuple mirrors refresh.js's retired / conversations
// / replaced Promise.all slots; disabled slots resolve to undefined so the
// stitcher preserves the previous page without special casing.
export function fetchVisibleGroupListPages(groups, onGroups, q, get) {
  if (typeof get !== 'function') throw new TypeError('group list polling requires get');
  const visible = groups?.visibility.value || {};
  const request = (kind) => get('/api/' + kind + '?' + listParams(kind, q));
  return [
    (onGroups && visible.retired) ? request('retired') : Promise.resolve(undefined),
    (onGroups && visible.conversations) ? request('conversations') : Promise.resolve(undefined),
    (onGroups && visible.replaced) ? request('replaced') : Promise.resolve(undefined),
  ];
}

// resetListOffsets zeroes every list's offset — used when the Groups-tab filter
// query changes, so a search starts on the first page of its (server-filtered)
// results rather than a stale deep page.
export function resetListOffsets() {
  offsets.retired = 0;
  offsets.conversations = 0;
  offsets.replaced = 0;
}

// syncServedOffset reconciles our stored offset with the offset the server
// actually served. A stale offset past the last page comes back clamped to the
// last page, so the next tick asks for the window that actually exists.
export function syncServedOffset(kind, served) {
  if (typeof served === 'number' && served >= 0) offsets[kind] = served;
}

// listPagerNav applies a pager action to a list's offset against the current
// server total. Returns true when the offset actually moved (so the caller can
// re-fetch).
export function listPagerNav(kind, action, total) {
  const limit = listLimit(kind);
  const cur = listOffset(kind);
  const lastOffset = total > 0 ? Math.floor((total - 1) / limit) * limit : 0;
  let next = cur;
  switch (action) {
    case 'first': next = 0; break;
    case 'prev': next = Math.max(0, cur - limit); break;
    case 'next': next = Math.min(lastOffset, cur + limit); break;
    case 'last': next = lastOffset; break;
    default: return false;
  }
  if (next === cur) return false;
  offsets[kind] = Math.max(0, next);
  return true;
}

// setListPageSize changes a list's page size (persisted) and resets it to the
// first page — a page-2 view of a differently-sized set is meaningless.
export function setListPageSize(kind, size) {
  if (!PAGE_SIZES.includes(size)) size = DEFAULT_PAGE_SIZE;
  dashPrefs.setItem(sizeKey(kind), String(size));
  offsets[kind] = 0;
}

// listPagerHTML renders the pager footer for a virtual group, or '' when the
// list fits on one page. paging is the {offset,limit,total,total_unfiltered}
// envelope refresh.js stitched in for this list.
export function listPagerHTML(kind, paging) {
  if (!paging) return '';
  const limit = listLimit(kind);
  const total = paging.total || 0;
  const off = paging.offset || 0;
  if (total <= limit && off === 0) return ''; // single page — no pager chrome
  const from = total === 0 ? 0 : off + 1;
  const to = Math.min(off + limit, total);
  const atFirst = off <= 0;
  const atLast = off + limit >= total;
  const sizeOpts = PAGE_SIZES
    .map(s => `<option value="${s}"${s === limit ? ' selected' : ''}>${s}/page</option>`)
    .join('');
  // Pager controls use data-pager (NOT data-act) so the global row-action
  // click handler ignores them; refresh.js binds a dedicated delegated
  // listener on #groups-list. They stay <button>/<select> so the drag/menu
  // suppression checks (closest('button, …')) treat them as interactive.
  const btn = (act, glyph, title, disabled) =>
    `<button type="button" class="list-pager-btn" data-pager="${act}" data-list="${kind}"`
    + `${disabled ? ' disabled' : ''} title="${title}" aria-label="${title}">${glyph}</button>`;
  return `<div class="list-pager" data-list="${kind}">`
    + btn('first', '«', 'First page', atFirst)
    + btn('prev', '‹', 'Previous page', atFirst)
    + `<span class="list-pager-count">${from}–${to} of ${total}</span>`
    + btn('next', '›', 'Next page', atLast)
    + btn('last', '»', 'Last page', atLast)
    + `<select class="list-pager-size" data-pager="size" data-list="${kind}" title="Rows per page">${sizeOpts}</select>`
    + `</div>`;
}

// fetchListFull pulls the COMPLETE list from one of the three endpoints (no
// offset/limit → the server returns every row). Used by the modals + filters
// that need completeness, not the windowed page the snapshot tick shows.
// Resolves to the rows array; rejects on a non-OK response.
export async function fetchListFull(kind) {
  const r = await fetch('/api/' + kind, { credentials: 'same-origin' });
  if (!r.ok) throw new Error('HTTP ' + r.status);
  const data = await r.json();
  return data.rows || [];
}
