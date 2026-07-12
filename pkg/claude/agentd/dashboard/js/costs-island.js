import { Fragment, h, render } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { AsyncLoadState } from './async-load-state.js';
import { CostsChart } from './costs-chart.js';
import {
  COST_COLUMNS, COST_SPANS, fmtLastActivity, fmtUSD, harnessLabel,
  harnessSegmentClass,
} from './costs-model.js';
import { idTooltip, shortAgentId } from './helpers.js';

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
    ? `Spend so far divided by elapsed ${unit}s (${projection.daysElapsed}), extrapolated over the month's remaining ${unit}s — ${projection.weekendsIncluded ? 'weekends included in the estimate.' : 'weekends excluded from the estimate.'}${projection.fillEmpty ? ` The empty ${unit}s before the first run this month are also filled at the per-${unit} average, so this reflects a representative full month.` : ''}`
    : '';
  return html`<span id="costs-summary">
    <span class="cost-total">Total: <strong>${fmtUSD(current.narrowed.total_usd)}</strong></span>
    <span class="cost-sep">·</span><span class="muted">${current.narrowed.from} → ${to}</span>
    ${projection && html`<${Fragment}><span class="cost-sep">·</span>
      <span class="cost-proj" title=${tip}>
        ${projection.fillEmpty ? 'Projected avg month total' : 'Projected month total'}:
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
  return html`<div class="filter-bar" id="costs-spans">
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
    <span class="spacer"></span><${Summary} current=${current} />
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
            return html`<tr key=${`${agent.conv_id}:${agent.day}`} data-key=${`cost-${agent.conv_id}-${agent.day}`}
              data-conv=${chain ? agent.conv_id : undefined} class=${classes || undefined}>
              <td title=${agent.title || '(unknown)'}>${marker && html`<span class=${agent.continued ? 'cost-cont' : 'cost-head'}
                title=${agent.continued ? 'Continued conversation — hover to highlight all its days' : `Latest day of an agent active across ${slices[agent.conv_id]} days`}>${marker}</span>`} 
                <span class="rowname">${agent.title || '(unknown)'}</span> <span class="id" title=${idTooltip(agent.agent_id, agent.conv_id)}>${shortAgentId(agent.agent_id, agent.conv_id)}</span></td>
              <td><span class="cost-amt" title=${`$${(agent.cost_usd || 0).toFixed(4)}`}>${fmtUSD(agent.cost_usd)}</span></td>
              <td><span class="muted">${harnessLabel(agent.harness)}</span></td>
              <td><span class="muted">${agent.model || ''}</span></td>
              <td><span class="muted">${fmtLastActivity(agent)}</span></td>
            </tr>`;
          })}
          <tr class="cost-total-row"><td><span class="muted">${current.filtered ? 'matched' : 'total'} (${current.shownConversations} agent${current.shownConversations === 1 ? '' : 's'})</span></td>
            <td><span class="cost-amt">${fmtUSD(current.tableTotal)}</span></td><td></td><td></td><td></td></tr>
        </tbody></table>`}
    </div>
  </${Fragment}>`;
}

export function CostsApp({ state, actions }) {
  const current = state.view.value;
  useEffect(() => {
    if (!current.snapshotLoaded) return;
    document.body.classList.toggle('hide-costs', !current.visible);
    document.body.classList.toggle('cost-whatif', current.whatif);
    if (!current.visible && current.activeTab === 'costs') document.querySelector('nav [data-tab="groups"]')?.click();
  }, [current.snapshotLoaded, current.visible, current.whatif, current.activeTab]);
  useEffect(() => {
    if (!current.active) return undefined;
    void actions.load();
    void actions.loadFactor();
    const timer = setInterval(() => void actions.load(), 60_000);
    return () => clearInterval(timer);
  }, [current.active, current.whatif]);
  useEffect(() => {
    const onClick = (event) => {
      if (event.target.closest?.('[data-goto-tab="costs"]')) document.querySelector('nav [data-tab="costs"]')?.click();
    };
    document.addEventListener('click', onClick);
    return () => document.removeEventListener('click', onClick);
  }, []);
  return html`<div class="costs-island">
    <${Controls} state=${state} actions=${actions} current=${current} />
    <${AsyncLoadState} label="Costs" request=${current.request} retry=${actions.load} errorClass="costs-error" />
    <div id="costs-whatif-banner" class="cost-whatif-banner" hidden=${!current.whatif}>
      <strong>⚠ WHAT-IF</strong> — you're on a subscription, so these figures are an <em>estimate</em> of what this would cost on pay-per-token billing. They are <strong>not a real charge</strong>.
    </div>
    ${current.request.hasLoaded && html`<${Fragment}><${CostsChart} chart=${current.chart} /><${CostsTable} state=${state} current=${current} /></${Fragment}>`}
  </div>`;
}

export function mountCostsIsland({ host, state, actions, registerCleanup }) {
  state.initialize();
  render(html`<${CostsApp} state=${state} actions=${actions} />`, host);
  registerCleanup(() => render(null, host));
}
