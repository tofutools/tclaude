import { Fragment, h, render } from 'preact';
import { useEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { fieldsText, fmtAbsTime, fmtBytes, fmtInt, levelKey, LOG_PAGE_SIZES, logsExportUrl } from './logs-model.js';
import { ActivityNodeSwitch, activityTarget } from './activity-node.js';
import { nodeDownloadPath } from './node-api.js';
import { Pager } from './pager.js';
import { Button, Input, Select } from './ui-components.js';
import { tableState } from './table-kit.js';
import { StampAgo } from './cell-parts.js';

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

function LogsPager({ current, state, actions }) {
  if (!current.totalUnfiltered) return null;
  return html`<${Pager} id="logs-pager" prefix="audit-pager" page=${current.page} pages=${current.pages}
    onPage=${(page) => { state.setPage(page); actions.load(); }} sizes=${LOG_PAGE_SIZES} pageSize=${current.pageSize} sizeId="logs-page-size"
    onPageSize=${(size) => { state.setPageSize(size); actions.load(); }}
    titles=${{ first: 'First page (newest)', next: 'Next page (older)', last: 'Last page (oldest)' }} />`;
}

function LogRows({ current }) {
  const placeholder = tableState({
    loading: current.request.phase === 'loading' && 'Loading logs…',
    error: current.request.phase === 'error' && html`Failed to load logs: ${current.request.error}`,
    empty: !current.rows.length && (current.totalUnfiltered ? 'No log lines match the filter.' : html`No log lines yet. tclaude writes its daemon + CLI log to <code>~/.tclaude/data/output.log</code>.`),
  });
  if (placeholder) return placeholder;
  return html`<table class="logs-table ui-compact"><thead><tr><th>When</th><th>Level</th><th>Message</th></tr></thead><tbody>
    ${current.rows.map(({ row, key }) => { const level = levelKey(row.level); const fields = fieldsText(row.fields); return html`<tr key=${key} data-key=${key} class=${`log-row log-row-${level}`}>
      <td class="logs-nowrap"><${StampAgo} iso=${row.time} title=${row.time || ''} text=${fmtAbsTime(row.time)} /></td>
      <td class="logs-nowrap"><span class=${`log-level log-${level}`} title=${level === 'raw' ? 'not a structured log line' : undefined}>${level}</span></td>
      <td class="logs-msg-cell"><span class="logs-msg">${row.msg || ''}</span>${fields && html` <span class="logs-fields muted" title=${fields}>${fields}</span>`}</td>
    </tr>`; })}
  </tbody></table>`;
}

// useLogsFeed loads the log while its view shows (and every STREAM_MS while
// streaming), re-reads it when the fused node switch moves, and returns
// `filter`, which sets one filter and reloads (debounced for typing). The
// phone's rows (phone-diagnostics.js) share it.
export function useLogsFeed(state, actions) {
  // The fused node switch (activity-node.js) re-reads the chosen daemon's log;
  // a peer unticked since falls back to this node.
  const node = activityTarget()?.id || '';
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
  }, [current.active, current.stream, node]);
  useEffect(() => () => clearTimeout(searchTimer.current), []);
  return (name, value, debounce = false) => {
    state.setFilter(name, value); clearTimeout(searchTimer.current);
    if (debounce) searchTimer.current = setTimeout(actions.load, 300); else actions.load();
  };
}

export function LogsApp({ state, actions }) {
  const node = activityTarget()?.id || '';
  const exportUrl = () => nodeDownloadPath(node, logsExportUrl(current));
  const current = state.view.value;
  const filterRef = useRef(null);
  const filter = useLogsFeed(state, actions);
  return html`<${Fragment}>
    <div class="filter-bar">
      <${Input} domRef=${filterRef} id="filter-logs" aria-label="Search logs" placeholder="Search (message / level / fields) — server-side" autocomplete="off" spellcheck=${false} value=${current.query} onInput=${(event) => filter('query', event.currentTarget.value, true)} />
      <span class="filter-count" id="filter-logs-count" aria-live="polite">${current.totalUnfiltered === 0 ? '' : current.total === current.totalUnfiltered ? `${current.total} line${current.total === 1 ? '' : 's'}` : `${current.total} / ${current.totalUnfiltered}`}</span>
      <button data-ui-exempt class="clear-filter" id="filter-logs-clear" title="Clear search" aria-label="Clear log search" onClick=${() => { filter('query', ''); filterRef.current?.focus(); }}>×</button>
      <${ActivityNodeSwitch} />
      <label class="filter-toggle"><span>level</span><${Select} size="sm" id="logs-level" value=${current.level} onChange=${(event) => filter('level', event.currentTarget.value)}><option value="">all</option><option value="debug">debug+</option><option value="info">info+</option><option value="warn">warn+</option><option value="error">error</option><//></label>
      <label class="filter-toggle"><span>since</span><${Select} size="sm" id="logs-range" value=${current.rangeMs} onChange=${(event) => filter('rangeMs', Number(event.currentTarget.value))}><option value="0">all</option><option value="900000">15m</option><option value="3600000">1h</option><option value="21600000">6h</option><option value="86400000">24h</option><option value="604800000">7d</option><//></label>
      <label class="filter-toggle"><input type="checkbox" id="logs-rotated" checked=${current.includeRotated} onChange=${(event) => filter('includeRotated', event.currentTarget.checked)} /> <span>rotated</span></label>
      <label class="filter-toggle"><input type="checkbox" id="logs-hide-raw" checked=${current.hideRaw} onChange=${(event) => filter('hideRaw', event.currentTarget.checked)} /> <span>hide raw</span></label>
      <${Button} id="logs-refresh" disabled=${current.request.phase === 'loading' || current.request.phase === 'refreshing'} onClick=${actions.load}>⟳ refresh<//>
      <a class="tool" id="logs-export" href=${exportUrl()} download title="Download every line matching the current filter (search, level, since, rotated, hide raw) as JSON lines, oldest first — not limited to this page" onClick=${(event) => { event.currentTarget.href = exportUrl(); }}>⤓ export</a>
      <label class="filter-toggle"><input type="checkbox" id="logs-stream" checked=${current.stream} onChange=${(event) => state.setStream(event.currentTarget.checked)} /> <span>stream</span></label>
      <span class="spacer"></span><span id="logs-status" class="muted"><${Status} current=${current} state=${state} actions=${actions} /></span>
    </div>
    <div id="logs-list" aria-busy=${current.request.phase === 'loading' || current.request.phase === 'refreshing'}><${LogRows} current=${current} /></div>
    <${LogsPager} current=${current} state=${state} actions=${actions} />
  </${Fragment}>`;
}

export function mountLogsIsland({ host, state, actions, registerCleanup }) {
  render(html`<${LogsApp} state=${state} actions=${actions} />`, host);
  registerCleanup(() => render(null, host));
}
