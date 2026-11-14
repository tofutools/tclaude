// phone-diagnostics.js -- Diagnostics › Audit and Logs on a phone
// (data-layout="phone").
//
// The desktop's tables are six and three columns wide; a phone shows each
// event or line as two lines instead (who did what, or the message, over its
// detail), with the outcome or level on the right, and a tap opens the rest.
// One search and one filter (outcome, or level) replace the filter bar. They
// run on the desktop islands' state and actions, and on their feeds
// (useAuditFeed, useLogsFeed), so loading, polling and paging are the
// desktop's. Doctor and Performance keep their islands: phone.css lays them
// out for a phone.

import { h, render } from 'preact';
import htm from 'htm';
import { actorTitle, fmtAuditTime, statusView, targetTitle } from './audit-model.js';
import { useAuditFeed } from './audit-island.js';
import { fieldsText, fmtAbsTime, levelKey } from './logs-model.js';
import { useLogsFeed } from './logs-island.js';
import { Button, Input, Select } from './ui-components.js';

const html = htm.bind(h);

// clock is a row's time: the time of day today, else the date.
export function clock(iso, now = new Date()) {
  const date = new Date(iso);
  if (!iso || Number.isNaN(date.getTime())) return '—';
  const pad = (value) => String(value).padStart(2, '0');
  if (date.toDateString() === now.toDateString()) return `${pad(date.getHours())}:${pad(date.getMinutes())}`;
  return date.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
}

const plural = (n, one, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;

// Placeholder is what a list shows instead of rows: loading, a failure, or
// nothing to show.
function Placeholder({ current, what, empty }) {
  if (current.request.phase === 'loading' && !current.rows.length) return html`<p class="ui-empty phone-empty">Loading ${what}…</p>`;
  if (current.request.phase === 'error') return html`<p class="ui-error-box" role="alert">Failed to load ${what}: ${current.request.error}</p>`;
  if (!current.rows.length) return html`<p class="ui-empty phone-empty">${empty}</p>`;
  return null;
}

// Pages steps through a long result, newest first.
function Pages({ current, state, actions }) {
  if (current.pages < 2) return null;
  const go = (page) => { state.setPage(page); actions.load(); };
  return html`<div class="phone-diag-pages">
    <${Button} size="sm" disabled=${current.page <= 1} onClick=${() => go(current.page - 1)}>‹ Newer<//>
    <span class="muted">${current.page} / ${current.pages}</span>
    <${Button} size="sm" disabled=${current.page >= current.pages} onClick=${() => go(current.page + 1)}>Older ›<//>
  </div>`;
}

function Search({ label, value, onInput }) {
  return html`<${Input} class="phone-diag-search" type="search" aria-label=${label} placeholder=${label} autocomplete="off"
    spellcheck=${false} value=${value} onInput=${(event) => onInput(event.currentTarget.value)} />`;
}

function actorName(entry) {
  if (entry.actor_kind === 'human') return 'operator';
  if (entry.actor_kind === 'system') return 'tclaude';
  return entry.actor_label || (entry.actor_kind === 'agent' ? '(agent)' : 'unknown');
}

function AuditRow({ entry }) {
  const status = statusView(entry.status);
  const target = targetTitle(entry);
  const sub = [target !== '—' ? target : '', entry.detail || ''].filter(Boolean).join(' · ');
  return html`<li class="phone-diag-item"><details class="phone-diag-row">
    <summary>
      <span class="phone-diag-time">${clock(entry.at)}</span>
      <span class="phone-diag-text">
        <span class="phone-diag-head">${actorName(entry)} → ${entry.verb}</span>
        ${sub && html`<span class="phone-diag-sub">${sub}</span>`}
      </span>
      <span class=${`phone-diag-outcome ${status.className}`} title=${status.title}>${status.label}</span>
    </summary>
    <dl class="phone-diag-detail">
      <dt>When</dt><dd>${fmtAuditTime(entry.at)}</dd>
      <dt>Who</dt><dd>${actorTitle(entry)}</dd>
      <dt>Action</dt><dd>${entry.verb}${entry.source ? ` (${entry.source})` : ''}</dd>
      ${target !== '—' && html`<dt>Target</dt><dd>${target}</dd>`}
      ${entry.detail && html`<dt>Detail</dt><dd>${entry.detail}</dd>`}
      <dt>Outcome</dt><dd>${status.title || status.label}</dd>
    </dl>
  </details></li>`;
}

// PhoneAudit is the audit log: one search, failures only on a tap.
export function PhoneAudit({ state, actions }) {
  const current = state.view.value;
  const filter = useAuditFeed(state, actions);
  const failed = current.outcome === 'failure';
  const count = current.totalUnfiltered === 0 ? ''
    : current.total === current.totalUnfiltered ? plural(current.total, 'event') : `${current.total} of ${current.totalUnfiltered} events`;
  return html`<div class="phone-diag">
    <div class="phone-diag-filters">
      <${Search} label="Search audit" value=${current.query} onInput=${(value) => filter('query', value, true)} />
      <${Button} size="sm" class=${`phone-diag-toggle${failed ? ' on' : ''}`} aria-pressed=${failed ? 'true' : 'false'}
        onClick=${() => filter('outcome', failed ? '' : 'failure')}>Failed only<//>
    </div>
    ${count && html`<p class="phone-diag-count muted" aria-live="polite">${count}</p>`}
    <${Placeholder} current=${current} what="the audit log"
      empty=${current.totalUnfiltered ? 'No events match.' : 'No audit events recorded yet.'} />
    ${current.rows.length > 0 && html`<ul class="phone-list phone-diag-list">${current.rows.map((entry) => {
      const key = entry.node ? `${entry.node.id}-${entry.id}` : entry.id;
      return html`<${AuditRow} key=${key} entry=${entry} />`;
    })}</ul>`}
    <${Pages} current=${current} state=${state} actions=${actions} />
  </div>`;
}

function LogRow({ row }) {
  const level = levelKey(row.level);
  const fields = fieldsText(row.fields);
  return html`<li class="phone-diag-item"><details class=${`phone-diag-row log-row-${level}`}>
    <summary>
      <span class="phone-diag-time">${clock(row.time)}</span>
      <span class="phone-diag-text">
        <span class="phone-diag-head">${row.msg || ''}</span>
        ${fields && html`<span class="phone-diag-sub phone-diag-mono">${fields}</span>`}
      </span>
      <span class=${`phone-diag-outcome log-level log-${level}`}>${level}</span>
    </summary>
    <dl class="phone-diag-detail">
      <dt>When</dt><dd>${fmtAbsTime(row.time)}</dd>
      <dt>Message</dt><dd>${row.msg || ''}</dd>
      ${fields && html`<dt>Fields</dt><dd class="phone-diag-mono">${fields}</dd>`}
    </dl>
  </details></li>`;
}

// PhoneLogs is tclaude's own log: one search and a level.
export function PhoneLogs({ state, actions }) {
  const current = state.view.value;
  const filter = useLogsFeed(state, actions);
  const count = current.totalUnfiltered === 0 ? ''
    : current.total === current.totalUnfiltered ? plural(current.total, 'line') : `${current.total} of ${current.totalUnfiltered} lines`;
  return html`<div class="phone-diag">
    <div class="phone-diag-filters">
      <${Search} label="Search logs" value=${current.query} onInput=${(value) => filter('query', value, true)} />
      <${Select} size="sm" class="phone-diag-level" aria-label="Level" value=${current.level}
        onChange=${(event) => filter('level', event.currentTarget.value)}>
        <option value="">all levels</option><option value="info">info+</option><option value="warn">warn+</option><option value="error">error</option>
      <//>
    </div>
    ${count && html`<p class="phone-diag-count muted" aria-live="polite">${count}</p>`}
    <${Placeholder} current=${current} what="logs" empty=${current.totalUnfiltered ? 'No log lines match.' : 'No log lines yet.'} />
    ${current.rows.length > 0 && html`<ul class="phone-list phone-diag-list">${current.rows.map(({ row, key }) => html`<${LogRow} key=${key} row=${row} />`)}</ul>`}
    <${Pages} current=${current} state=${state} actions=${actions} />
  </div>`;
}

export function mountPhoneAuditIsland({ host, state, actions, registerCleanup }) {
  render(html`<${PhoneAudit} state=${state} actions=${actions} />`, host);
  registerCleanup(() => render(null, host));
}

export function mountPhoneLogsIsland({ host, state, actions, registerCleanup }) {
  render(html`<${PhoneLogs} state=${state} actions=${actions} />`, host);
  registerCleanup(() => render(null, host));
}
