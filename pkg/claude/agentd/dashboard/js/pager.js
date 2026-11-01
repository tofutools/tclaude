// pager.js — the dashboard's one pager footer, for every paged list: the
// Messages folders (one node's and the fused view's), Audit, Logs, Jobs and
// the Groups tab's virtual lists. Each list owns its paging state and reads;
// this renders the controls the same way everywhere.
//
// Two layouts, by what the list knows:
//   pages: "Page n / N" — a list paged by page number (Messages, Audit, Logs).
//          A fused list merged from several nodes (fleet-pager.js) is paged
//          this way too: its N is exact (the nodes' summed totals), but a
//          deeper page is reached by stepping forward, so `stepping` names the
//          page it is on the way to.
//   range: "a–b of N" — a list paged by offset (Jobs, Groups virtual lists).
//
// prefix keeps each list's CSS class family (mail-pager, audit-pager,
// list-pager). A pages pager calls onPage(n); a range pager calls
// onAction(first|prev|next|last), its list owning the offset arithmetic. A
// list whose controls are wired by event delegation (the Groups virtual
// lists) passes delegated and data (attributes on the pager and its
// controls): each button then carries data-pager=<action>, the select
// data-pager="size".
import { Fragment, h } from 'preact';
import htm from 'htm';

const html = htm.bind(h);

const NAV = [
  ['first', '«', 'First page'],
  ['prev', '‹', 'Previous page'],
  ['next', '›', 'Next page'],
  ['last', '»', 'Last page'],
];

// pagerTarget is the page a pager action moves to.
export function pagerTarget(action, page, pages) {
  switch (action) {
    case 'first': return 1;
    case 'prev': return Math.max(1, page - 1);
    case 'next': return Math.min(pages, page + 1);
    case 'last': return pages;
    default: return page;
  }
}

export function Pager({
  id, prefix = 'list-pager', layout = 'pages', hidden = false,
  page = 1, pages = 1, onPage, stepping = 0, note = '',
  total = 0, offset = 0, limit = 0, onAction,
  sizes = [], pageSize, onPageSize, sizeId, sizeClass, sizeTitle = 'Rows per page', sizeOption = String,
  titles = {}, disabled = false, delegated = false, data = {},
}) {
  if (hidden) return html`<div id=${id} class=${prefix} hidden></div>`;
  const range = layout === 'range';
  // A range list that fits on its first page shows no pager.
  if (range && total <= limit && offset === 0) return null;
  if (!range) page = Math.min(page, pages);
  const atStart = range ? offset <= 0 : page <= 1;
  const atEnd = range ? offset + limit >= total : page >= pages;
  const act = (action) => {
    if (range) onAction?.(action);
    else onPage?.(pagerTarget(action, page, pages));
  };
  const button = ([action, glyph, fallback], atBoundary) => {
    const title = titles[action] || fallback;
    return html`<button key=${action} type="button" class=${range ? `${prefix}-btn` : undefined}
      data-pager=${delegated ? action : undefined} ...${data} disabled=${disabled || atBoundary}
      title=${title} aria-label=${range ? title : undefined} onClick=${delegated ? undefined : () => act(action)}>${glyph}</button>`;
  };
  const position = range
    ? html`<span class=${`${prefix}-count`}>${total ? offset + 1 : 0}–${Math.min(offset + limit, total)} of ${total}</span>`
    : html`<span class=${`${prefix}-pos`}>Page ${page} / ${pages}${stepping > page && page < pages ? html` <span class="muted">→ ${stepping}…</span>` : ''}</span>`;
  const nav = (range || pages > 1) && html`<${Fragment}>${button(NAV[0], atStart)}${button(NAV[1], atStart)}${position}${button(NAV[2], atEnd)}${button(NAV[3], atEnd)}</${Fragment}>`;
  const select = html`<select id=${sizeId} class=${range ? `${prefix}-size` : sizeClass} data-pager=${delegated ? 'size' : undefined} ...${data}
    title=${range ? sizeTitle : undefined} aria-label=${range ? sizeTitle : undefined} disabled=${disabled} value=${String(range ? limit : pageSize)}
    onChange=${delegated ? undefined : (event) => onPageSize?.(Number(event.currentTarget.value))}>
    ${sizes.map((size) => html`<option key=${size} value=${size}>${sizeOption(size)}</option>`)}</select>`;
  // A capture's text says how much of the list this page holds.
  if (range) return html`<div class=${prefix} data-capture-note=${`showing ${Math.max(0, Math.min(offset + limit, total) - offset)} of ${total} rows`} ...${data}>${nav}${select}</div>`;
  return html`<div id=${id} class=${prefix}>${nav}${note && html`<span class=${`${prefix}-note muted`}>${note}</span>`}
    <span class="grow"></span><label class=${`${prefix}-size`} title=${sizeTitle}>${select} / page</label></div>`;
}
