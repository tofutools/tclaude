import { Fragment, h, render } from 'preact';
import { useEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { relTime } from './helpers.js';
import { fieldsText, fmtAbsTime, fmtBytes, fmtInt, levelKey, LOG_PAGE_SIZES } from './logs-model.js';

const html = htm.bind(h);
const STREAM_MS = 2000;

function Status({ current, state, actions }) {
  const data = current.response;
  if (!data) return null;
  const bits = [];
  if (data.sources?.length) {
    const primary = data.sources[0];
    const extra = data.sources.length - 1;
    const totalLines = data.sources.reduce((total, source) => total + (source.lines || 0), 0);
    const detail = `Reading ${data.sources.length} file${data.sources.length === 1 ? '' : 's'}:\n`
      + data.sources.map((source) => `  ${source.name} — ${fmtInt(source.lines)} line${source.lines === 1 ? '' : 's'}${source.bytes ? ` · ${fmtBytes(source.bytes)}` : ''}`).join('\n');
    bits.push(html`<span class="logs-sources" title=${detail}>${primary.path}${extra > 0 && html` <span class="muted">+ ${extra} rotated file${extra === 1 ? '' : 's'}</span>`} <span class="muted">· ${fmtInt(totalLines)} line${totalLines === 1 ? '' : 's'}</span></span>`);
  } else if (data.path) bits.push(data.path);
  if (data.rotated_available && !data.include_rotated) {
    bits.push(html`<a href="#" class="logs-rotated-hint" onClick=${(event) => { event.preventDefault(); state.setFilter('includeRotated', true); actions.load(); }}>+ ${data.rotated_available} rotated file${data.rotated_available === 1 ? '' : 's'} available</a>`);
  }
  if (data.truncated) bits.push(html`<span class="logs-warn" title="Only the newest slice of the log was read; older lines were skipped.">newest slice only</span>`);
  return html`<${Fragment}>${bits.map((bit, index) => html`<${Fragment} key=${index}>${index > 0 && ' · '}${bit}</${Fragment}>`)}</${Fragment}>`;
}

function Pager({ current, state, actions }) {
  if (!current.totalUnfiltered) return null;
  const atStart = current.page <= 1; const atEnd = current.page >= current.pages;
  const go = (page) => { state.setPage(page); actions.load(); };
  return html`<div id="logs-pager" class="audit-pager">
    ${current.pages > 1 && html`<${Fragment}>
      <button title="First page (newest)" disabled=${atStart} onClick=${() => go(1)}>«</button>
      <button title="Previous page" disabled=${atStart} onClick=${() => go(current.page - 1)}>‹</button>
      <span class="audit-pager-pos">Page ${current.page} / ${current.pages}</span>
      <button title="Next page (older)" disabled=${atEnd} onClick=${() => go(current.page + 1)}>›</button>
      <button title="Last page (oldest)" disabled=${atEnd} onClick=${() => go(current.pages)}>»</button>
    </${Fragment}>`}
    <span class="grow"></span><label class="audit-pager-size" title="Rows per page"><select id="logs-page-size" value=${current.pageSize} onChange=${(event) => { state.setPageSize(event.currentTarget.value); actions.load(); }}>
      ${LOG_PAGE_SIZES.map((size) => html`<option key=${size} value=${size}>${size}</option>`)}
    </select> / page</label>
  </div>`;
}

function LogRows({ current }) {
  if (current.request.phase === 'loading') return html`<div class="empty">Loading logs…</div>`;
  if (current.request.phase === 'error') return html`<div role="alert" class="island-error">Failed to load logs: ${current.request.error}</div>`;
  if (!current.rows.length) return html`<div class="empty">${current.totalUnfiltered ? 'No log lines match the filter.' : html`No log lines yet. tclaude writes its daemon + CLI log to <code>~/.tclaude/output.log</code>.`}</div>`;
  return html`<table class="logs-table"><thead><tr><th>When</th><th>Level</th><th>Message</th></tr></thead><tbody>
    ${current.rows.map(({ row, key }) => { const level = levelKey(row.level); const fields = fieldsText(row.fields); return html`<tr key=${key} data-key=${key} class=${`log-row log-row-${level}`}>
      <td class="logs-nowrap"><span class="last-hook" title=${row.time || ''}>${fmtAbsTime(row.time)}</span>${relTime(row.time) && html` <span class="muted">(${relTime(row.time)})</span>`}</td>
      <td class="logs-nowrap"><span class=${`log-level log-${level}`} title=${level === 'raw' ? 'not a structured log line' : undefined}>${level}</span></td>
      <td class="logs-msg-cell"><span class="logs-msg">${row.msg || ''}</span>${fields && html` <span class="logs-fields muted" title=${fields}>${fields}</span>`}</td>
    </tr>`; })}
  </tbody></table>`;
}

export function LogsApp({ state, actions }) {
  const current = state.view.value;
  const searchTimer = useRef(null);
  useEffect(() => {
    if (!current.active) {
      clearTimeout(searchTimer.current);
      return undefined;
    }
    actions.load();
    if (!current.stream) return undefined;
    const timer = setInterval(actions.load, STREAM_MS);
    return () => clearInterval(timer);
  }, [current.active, current.stream]);
  useEffect(() => () => clearTimeout(searchTimer.current), []);
  const filter = (name, value, debounce = false) => {
    state.setFilter(name, value); clearTimeout(searchTimer.current);
    if (debounce) searchTimer.current = setTimeout(actions.load, 300); else actions.load();
  };
  return html`<${Fragment}>
    <div class="filter-bar">
      <input id="filter-logs" aria-label="Search logs" type="text" placeholder="Search (message / level / fields) — server-side" autocomplete="off" spellcheck=${false} value=${current.query} onInput=${(event) => filter('query', event.currentTarget.value, true)} />
      <span class="filter-count" id="filter-logs-count">${current.totalUnfiltered === 0 ? '' : current.total === current.totalUnfiltered ? `${current.total} line${current.total === 1 ? '' : 's'}` : `${current.total} / ${current.totalUnfiltered}`}</span>
      <button class="clear-filter" id="filter-logs-clear" title="Clear search" onClick=${() => filter('query', '')}>×</button>
      <label class="filter-toggle"><span>level</span><select id="logs-level" value=${current.level} onChange=${(event) => filter('level', event.currentTarget.value)}><option value="">all</option><option value="debug">debug+</option><option value="info">info+</option><option value="warn">warn+</option><option value="error">error</option></select></label>
      <label class="filter-toggle"><span>since</span><select id="logs-range" value=${current.rangeMs} onChange=${(event) => filter('rangeMs', Number(event.currentTarget.value))}><option value="0">all</option><option value="900000">15m</option><option value="3600000">1h</option><option value="21600000">6h</option><option value="86400000">24h</option><option value="604800000">7d</option></select></label>
      <label class="filter-toggle"><input type="checkbox" id="logs-rotated" checked=${current.includeRotated} onChange=${(event) => filter('includeRotated', event.currentTarget.checked)} /> <span>rotated</span></label>
      <label class="filter-toggle"><input type="checkbox" id="logs-hide-raw" checked=${current.hideRaw} onChange=${(event) => filter('hideRaw', event.currentTarget.checked)} /> <span>hide raw</span></label>
      <button class="tool" id="logs-refresh" disabled=${current.request.phase === 'loading' || current.request.phase === 'refreshing'} onClick=${actions.load}>⟳ refresh</button>
      <label class="filter-toggle"><input type="checkbox" id="logs-stream" checked=${current.stream} onChange=${(event) => state.setStream(event.currentTarget.checked)} /> <span>stream</span></label>
      <span class="spacer"></span><span id="logs-status" class="muted"><${Status} current=${current} state=${state} actions=${actions} /></span>
    </div>
    <div id="logs-list" aria-busy=${current.request.phase === 'loading' || current.request.phase === 'refreshing'}><${LogRows} current=${current} /></div>
    <${Pager} current=${current} state=${state} actions=${actions} />
  </${Fragment}>`;
}

export function mountLogsIsland({ host, state, actions, registerCleanup }) {
  render(html`<${LogsApp} state=${state} actions=${actions} />`, host);
  registerCleanup(() => render(null, host));
}
