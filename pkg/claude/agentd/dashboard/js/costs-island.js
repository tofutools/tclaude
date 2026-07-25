import { Fragment, h, render } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { AsyncLoadState } from './async-load-state.js';
import { CostsChart } from './costs-chart.js';
import {
  COST_COLUMNS, COST_SPANS, fmtLastActivity, fmtUSD, harnessLabel,
  harnessSegmentClass, monthProjectionLabel,
} from './costs-model.js';
import { idTooltip, isModifiedClick, shortAgentId } from './helpers.js';

const html = htm.bind(h);

function Summary({ current }) {
  if (!current.narrowed) return null;
  const to = current.span === 'month' ? (() => {
    const now = new Date();
    const last = new Date(now.getFullYear(), now.getMonth() + 1, 0);
    const pad = (value) => String(value).padStart(2, '0');
    return `${last.getFullYear()}-${pad(last.getMonth() + 1)}-${pad(last.getDate())}`;
  })() : current.narrowed.to;
  const projection = current.projection;
  const unit = projection?.weekendsIncluded ? 'day' : 'weekday';
  const tip = projection
    ? `Spend so far divided by elapsed ${unit}s (${projection.daysElapsed}), extrapolated over the month's remaining ${unit}s — ${projection.weekendsIncluded ? 'weekends included in the estimate.' : 'weekends excluded from the estimate.'}${projection.fillEmpty ? ` The empty ${unit}s before the first run this month are also filled at the per-${unit} average, so this reflects a representative full month.` : ''}${projection.includesWhatIf ? ' Projection includes hypothetical WHAT-IF cost.' : ''}`
    : '';
  return html`<span id="costs-summary">
    <span class="cost-total">Total: <strong>${fmtUSD(current.narrowed.total_usd)}</strong></span>
    ${current.hasWhatIf && html`<span class="muted">
      (${fmtUSD(current.narrowed.real_total_usd || 0)} real + ≈${fmtUSD(current.narrowed.what_if_total_usd || 0)} WHAT-IF)
    </span>`}
    <span class="cost-sep">·</span><span class="muted">${current.narrowed.from} → ${to}</span>
    ${projection && html`<${Fragment}><span class="cost-sep">·</span>
      <span class="cost-proj" title=${tip}>
        ${monthProjectionLabel(projection, current.hasReal)}:${' '}
        <strong>~${fmtUSD(projection.total)}</strong> <span class="muted">(${fmtUSD(projection.perDay)}/${unit})</span>
      </span>
    </${Fragment}>`}
  </span>`;
}

function FactorEditor({ state, actions }) {
  const current = state.view.value.factor;
  const timer = useRef(null);
  useEffect(() => () => clearTimeout(timer.current), []);
  const save = () => {
    clearTimeout(timer.current);
    void actions.saveFactor(state.factor.value.raw);
  };
  const edit = (raw) => {
    state.editFactor(raw);
    clearTimeout(timer.current);
    timer.current = setTimeout(() => void actions.saveFactor(state.factor.value.raw), 600);
  };
  return html`<label class="filter-toggle" id="costs-factor-label"
    title="Display multiplier applied to every cost figure here, on the per-agent badges, and in the top bar. Display-only: recorded data is never changed; 1 = no adjustment.">
    <span>×</span><input id="costs-factor" type="number" min="0" max="10" step="0.01" placeholder="1.0"
      aria-label="Cost display multiplier" style="width:5em" value=${current.raw}
      onInput=${(event) => edit(event.currentTarget.value)} onChange=${save}
      onKeyDown=${(event) => { if (event.key === 'Enter') save(); }} />
    <span id="costs-factor-status" class=${`muted${current.error ? ' error' : ''}`} role=${current.error ? 'alert' : 'status'}>${current.status}</span>
  </label>`;
}

function Controls({ state, actions, current }) {
  const loadAfter = (change) => { change(); void actions.load(); };
  const monthView = current.span === 'month' || current.span === 'calmonth';
  // Two rows, always: the controls on top, the totals/projection line under
  // them. Sharing one line only worked while the summary was short — with the
  // real/WHAT-IF split, the date range and the month projection it now runs
  // past the window edge, so it gets a row of its own regardless of length.
  return html`<div class="costs-header">
    <div class="filter-bar" id="costs-spans">
      ${COST_SPANS.map((span) => html`<button class=${`tool${current.span === span.key ? ' active' : ''}`}
        data-span=${span.key} title=${span.key === 'month' ? 'Calendar month to date — the only span with a projection' : `Trailing ${span.days} days`}
        onClick=${() => loadAfter(() => state.setSpan(span.key))}>${span.label}</button>`)}
      <span id="costs-month-nav" title="Browse a month — click the month, then ‹ / › to step">
        <button class="tool costs-month-step" id="costs-month-prev" title="Older month" aria-label="Older month"
          disabled=${current.monthOffset >= current.oldestMonthOffset}
          onClick=${() => loadAfter(() => state.activateMonth(monthView ? current.monthOffset + 1 : current.monthOffset))}>‹</button>
        <button class=${`tool${monthView ? ' active' : ''}`} id="costs-month-cur" data-span="calmonth" title="View this month"
          onClick=${() => loadAfter(() => state.activateMonth(current.monthOffset))}>${current.monthLabel}</button>
        <button class="tool costs-month-step" id="costs-month-next" title="Newer month" aria-label="Newer month"
          disabled=${current.monthOffset <= 0}
          onClick=${() => loadAfter(() => state.activateMonth(monthView ? current.monthOffset - 1 : current.monthOffset))}>›</button>
      </span>
      <label class=${`filter-toggle${current.span !== 'month' ? ' disabled' : ''}`} id="costs-fill-weekdays-label"
        title="Fill the empty weekdays before your first run this month with the per-weekday average.">
        <input id="costs-fill-weekdays" type="checkbox" checked=${current.fillEmpty} disabled=${current.span !== 'month'}
          onChange=${(event) => state.setFillEmpty(event.currentTarget.checked)} /><span>fill empty weekdays</span>
      </label>
      <label class=${`filter-toggle${current.span !== 'month' ? ' disabled' : ''}`} id="costs-include-weekends-label"
        title="Count weekends in the month projection instead of projecting them at zero.">
        <input id="costs-include-weekends" type="checkbox" checked=${current.includeWeekends} disabled=${current.span !== 'month'}
          onChange=${(event) => state.setIncludeWeekends(event.currentTarget.checked)} /><span>include weekends</span>
      </label>
      <${FactorEditor} state=${state} actions=${actions} />
    </div>
    <${Summary} current=${current} />
  </div>`;
}

function HarnessFilter({ state, current }) {
  if (current.harnesses.length <= 1) return null;
  return html`<span id="filter-costs-harnesses" class="costs-harness-filter">
    ${current.harnesses.map((harness) => html`<label class="filter-toggle costs-harness-choice" title=${`Show ${harness} cost rows`}>
      <input type="checkbox" data-harness=${harness} checked=${current.selectedHarnesses.has(harness)}
        onChange=${() => state.toggleHarness(harness)} />
      <span class=${`cost-legend-sw ${harnessSegmentClass(harness, current.harnesses)}`}></span><span>${harness}</span>
    </label>`)}
  </span>`;
}

function SortHeader({ state, current }) {
  const activate = (event, key) => {
    if (event.type === 'keydown' && event.key !== 'Enter' && event.key !== ' ' && event.key !== 'Spacebar') return;
    event.preventDefault();
    state.cycleSort(key);
  };
  return html`<thead><tr>${COST_COLUMNS.map((column) => {
    const active = current.sort.key === column.sort;
    return html`<th class=${`cost-sort${active ? ' active' : ''}`} tabIndex="0"
      aria-sort=${active ? (current.sort.dir === 'asc' ? 'ascending' : 'descending') : 'none'}
      title=${`Sort by ${column.label}`} onClick=${(event) => activate(event, column.sort)}
      onKeyDown=${(event) => activate(event, column.sort)}>${column.label}${active ? (current.sort.dir === 'asc' ? ' ▲' : ' ▼') : ''}</th>`;
  })}</tr></thead>`;
}

// Send the reader to the tab's WHAT-IF banner, for a click on a row's WHAT-IF
// marker. The marker is a real anchor to the banner, so a modified or
// non-primary click is left to the browser to open however the reader asked
// (the same bail-out every other in-page dashboard anchor makes). A plain
// click is handled here because the native fragment jump would push a history
// entry whose Back press then appears to do nothing.
function gotoWhatIfBanner(event) {
  if (isModifiedClick(event)) return;
  event.preventDefault();
  const banner = document.getElementById('costs-whatif-banner');
  if (!banner) return;
  const reduceMotion = window.matchMedia
    && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  banner.scrollIntoView?.({ block: 'center', behavior: reduceMotion ? 'auto' : 'smooth' });
  // The suppressed jump would also have made the banner the sequential-focus
  // start point and announced it on arrival. Do both by hand, or a keyboard or
  // screen-reader visitor is sent to text they are never told about and lands
  // back in the table on the next Tab. preventScroll leaves the scroll above
  // in charge of how they get there; the focus ring is also the whole landing
  // cue under reduced motion, where the flash is dropped.
  banner.tabIndex = -1;
  banner.focus?.({ preventScroll: true });
  banner.classList.remove('cost-whatif-flash');
  // Forced reflow between the remove and the add, so a repeat click restarts
  // the animation instead of being swallowed as "already has that class".
  void banner.offsetWidth;
  banner.classList.add('cost-whatif-flash');
}

// U+26A0 with U+FE0E (text presentation selector) so a system with an emoji
// font in the fallback chain can't promote the marker to a full-colour emoji
// triangle, which would ignore .cost-whatif-mark's dim amber and shout. The
// label is the marker's accessible NAME, kept short because a subscription
// install marks every row; the caveat itself rides in the title, which becomes
// the description, so a screen reader doesn't read one long sentence twice.
const WHAT_IF_MARK = '⚠︎';
const WHAT_IF_LABEL = 'About this WHAT-IF estimate';

// Tooltip for the amount itself. fmtUSD rounds to cents and collapses anything
// under one into "<1¢", so the exact figure is worth a hover — and a mixed row
// names its two parts rather than calling the whole total an estimate.
function amountTip(agent) {
  const exact = (value) => `$${(value || 0).toFixed(4)}`;
  switch (agent.cost_kind) {
    case 'mixed':
      return `${exact(agent.cost_usd)} total — ${exact(agent.real_cost_usd)} real spend`
        + ` + ${exact(agent.what_if_cost_usd)} estimated (WHAT-IF)`;
    case 'what_if':
      return `${exact(agent.cost_usd)} estimated (WHAT-IF)`;
    default:
      return `${exact(agent.cost_usd)} real spend`;
  }
}

// Tooltip for a row's WHAT-IF marker. Mixed rows spell out the split the cell
// no longer shows inline; pure WHAT-IF rows just qualify the amount.
function whatIfRowTip(agent) {
  if (agent.cost_kind === 'mixed') {
    return `${fmtUSD(agent.cost_usd)} total = ${fmtUSD(agent.real_cost_usd)} real`
      + ` + ${fmtUSD(agent.what_if_cost_usd)} WHAT-IF estimate — the estimate is hypothetical,`
      + ' not a real charge. Click for details.';
  }
  return `${fmtUSD(agent.cost_usd)} WHAT-IF estimate — hypothetical pay-per-token equivalent,`
    + ' not a real charge. Click for details.';
}

function CostsTable({ state, current }) {
  const [hovered, setHovered] = useState(null);
  const inputRef = useRef(null);
  const agents = current.payload?.agents || [];
  if (!agents.length) return html`<div id="costs-table"></div>`;
  const slices = {};
  for (const agent of agents) slices[agent.conv_id] = (slices[agent.conv_id] || 0) + 1;
  return html`<${Fragment}>
    <div class="filter-bar" id="costs-table-filter">
      <${HarnessFilter} state=${state} current=${current} />
      <input ref=${inputRef} id="filter-costs" type="text" aria-label="Filter cost agents"
        placeholder="Filter agents (name / id / harness / model)" autocomplete="off" spellcheck=${false}
        value=${current.query} onInput=${(event) => state.setQuery(event.currentTarget.value)}
        onKeyDown=${(event) => { if (event.key === 'Escape') state.setQuery(''); }} />
      <span class="filter-count" id="filter-costs-count">${current.filtered ? `${current.shownConversations} / ${current.totalConversations}` : ''}</span>
      <button class="clear-filter" id="filter-costs-clear" title="Clear filter" aria-label="Clear cost filter"
        onClick=${() => { state.setQuery(''); inputRef.current?.focus(); }}>×</button>
    </div>
    <div id="costs-table" onMouseLeave=${() => setHovered(null)}
      onMouseOver=${(event) => setHovered(event.target.closest('tr[data-conv]')?.dataset.conv || null)}>
      ${current.rows.length === 0
        ? html`<div class="empty">No agents match the filter.</div>`
        : html`<table><${SortHeader} state=${state} current=${current} /><tbody>
          ${current.rows.map((agent) => {
            const chain = slices[agent.conv_id] > 1;
            const classes = [agent.continued ? 'cost-continued' : '', chain ? 'cost-chain' : '', hovered === agent.conv_id ? 'cost-chain-hl' : ''].filter(Boolean).join(' ');
            const marker = agent.continued ? '↩' : chain ? '↳' : '';
            // Every row shows one plain total in the same money-green,
            // WHAT-IF or not, so the column scans as a column of amounts. The
            // caveat is a single dim marker pointing back at the banner that
            // already states it in full — see .cost-whatif-mark in the CSS.
            // Tested against the two hypothetical kinds rather than "not real",
            // so a zero-cost slice (kind "", which costKind returns when both
            // subtotals are 0) is not marked as an estimate it isn't.
            const whatIf = agent.cost_kind === 'what_if' || agent.cost_kind === 'mixed';
            return html`<tr key=${`${agent.conv_id}:${agent.day}`} data-key=${`cost-${agent.conv_id}-${agent.day}`}
              data-conv=${chain ? agent.conv_id : undefined} class=${classes || undefined}>
              <td title=${agent.title || '(unknown)'}>${marker && html`<span class=${agent.continued ? 'cost-cont' : 'cost-head'}
                title=${agent.continued ? 'Continued conversation — hover to highlight all its days' : `Latest day of an agent active across ${slices[agent.conv_id]} days`}>${marker}</span>`}${marker ? ' ' : ''}
                <span class="rowname">${agent.title || '(unknown)'}</span> <span class="id" title=${idTooltip(agent.agent_id, agent.conv_id)}>${shortAgentId(agent.agent_id, agent.conv_id)}</span></td>
              <td><span class="cost-amt" title=${amountTip(agent)}>
                ${fmtUSD(agent.cost_usd)}${whatIf && html`<a class="cost-whatif-mark" href="#costs-whatif-banner"
                  title=${whatIfRowTip(agent)} aria-label=${WHAT_IF_LABEL}
                  onClick=${gotoWhatIfBanner}>${WHAT_IF_MARK}</a>`}</span></td>
              <td><span class="muted">${harnessLabel(agent.harness)}</span></td>
              <td><span class="muted">${agent.model || ''}</span></td>
              <td><span class="muted">${fmtLastActivity(agent)}</span></td>
            </tr>`;
          })}
          <tr class="cost-total-row"><td><span class="muted">${current.filtered ? 'matched' : 'total'} (${current.shownConversations} agent${current.shownConversations === 1 ? '' : 's'})</span></td>
            <td><span class="cost-amt">${current.tableWhatIfTotal > 0
              ? `${fmtUSD(current.tableTotal)} (${fmtUSD(current.tableTotal - current.tableWhatIfTotal)} real + ≈${fmtUSD(current.tableWhatIfTotal)} WHAT-IF)`
              : fmtUSD(current.tableTotal)}</span></td><td></td><td></td><td></td></tr>
        </tbody></table>`}
    </div>
  </${Fragment}>`;
}

export function CostsApp({ state, actions }) {
  const current = state.view.value;
  useEffect(() => {
    if (!current.snapshotLoaded) return;
    document.body.classList.toggle('hide-costs', !current.visible);
    document.body.classList.toggle('cost-whatif', current.whatIfEnabled);
    if (!current.visible && current.activeTab === 'costs') document.querySelector('nav [data-tab="groups"]')?.click();
  }, [current.snapshotLoaded, current.visible, current.whatIfEnabled, current.activeTab]);
  useEffect(() => {
    if (!current.active) return undefined;
    void actions.load();
    void actions.loadFactor();
    const timer = setInterval(() => void actions.load(), 60_000);
    return () => clearInterval(timer);
  }, [current.active]);
  useEffect(() => {
    const onClick = (event) => {
      if (event.target.closest?.('[data-goto-tab="costs"]')) document.querySelector('nav [data-tab="costs"]')?.click();
    };
    document.addEventListener('click', onClick);
    return () => document.removeEventListener('click', onClick);
  }, []);
  // The WHAT-IF caveat leads the tab: it qualifies every figure below it,
  // including the totals in the header, so it has to be read before them.
  // (htm drops the whitespace around a newline, hence the explicit ${' '}
  // between the interpolated first sentence and the second one.)
  return html`<div class="costs-island">
    <div id="costs-whatif-banner" class="cost-whatif-banner" hidden=${!current.hasWhatIf}>
      <strong>⚠ WHAT-IF</strong> — ${current.hasReal ? 'this view mixes real billed spend with hypothetical subscription estimates.' : 'these figures are hypothetical subscription estimates.'}${' '}
      WHAT-IF values estimate equivalent pay-per-token pricing and are <strong>not real charges</strong>.
    </div>
    <${Controls} state=${state} actions=${actions} current=${current} />
    <${AsyncLoadState} label="Costs" request=${current.request} retry=${actions.load} errorClass="costs-error" />
    ${current.request.hasLoaded && html`<${Fragment}><${CostsChart} chart=${current.chart} enabled=${current.active && current.visible} /><${CostsTable} state=${state} current=${current} /></${Fragment}>`}
  </div>`;
}

export function mountCostsIsland({ host, state, actions, registerCleanup }) {
  state.initialize();
  render(html`<${CostsApp} state=${state} actions=${actions} />`, host);
  registerCleanup(() => render(null, host));
}
