// phone-agents-island.js -- the phone's Agents view (the Groups tab in the
// phone layout): a tab per group, one row per agent with its state, role,
// harness, context and branch or PR, and a bottom sheet per agent with what a
// phone can do to it. No drag and drop, no columns, no side panel: those stay
// on the desktop.
//
// Every action goes through the desktop's own entry points (the row action
// handler, the message dialog, the spawn dialog, the member editor), so the
// confirmations and transactions are the desktop's.

import { h, render } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { ALL, VARIANT_LABELS, agentGroups, findRow, groupTabs, shownGroups } from './phone-agents-model.js';
import { openPhoneSheet } from './phone-sheet.js';
import { Button } from './ui-components.js';
import { Tabs } from './ui-tabs.js';

const html = htm.bind(h);

// The tag a state wears: what needs a look stands out, the rest is quiet.
const TAG_CLASS = Object.freeze({
  error: 'ui-tag ui-tag-warn', crashed: 'ui-tag ui-tag-warn', asking: 'ui-tag ui-tag-accent',
  working: 'ui-tag', idle: 'ui-tag', offline: 'ui-tag',
});

function StateTag({ row }) {
  const label = row.waking ? 'waking' : VARIANT_LABELS[row.variant];
  return html`<span class=${`${TAG_CLASS[row.variant]} phone-agent-state v-${row.variant}`}>${label}</span>`;
}

const CI_MARK = Object.freeze({ passing: '✓', failing: '✗', running: '…' });

function PRLabel({ pr }) {
  return html`<span class=${`phone-agent-pr${pr.ci ? ` ci-${pr.ci}` : ''}`}>PR #${pr.number}${pr.ci ? ` ${CI_MARK[pr.ci]}` : ''}</span>`;
}

// MetaLine is a row's second line: role · harness · context · PR or branch.
function MetaLine({ row }) {
  const parts = [row.role, row.harness, row.ctx !== null ? `ctx ${row.ctx}%` : ''].filter(Boolean);
  return html`<span class="phone-agent-meta">
    ${parts.join(' · ')}${row.pr ? html`${parts.length ? ' · ' : ''}<${PRLabel} pr=${row.pr} />` : row.branch ? `${parts.length ? ' · ' : ''}⎇ ${row.branch}` : ''}
  </span>`;
}

function AgentRow({ row, onOpen }) {
  return html`<li>
    <button data-ui-exempt type="button" class=${`phone-agent-row v-${row.variant}`} onClick=${() => onOpen(row)}>
      <span class="phone-agent-line"><span class="phone-agent-title">${row.name}</span><${StateTag} row=${row} /></span>
      <${MetaLine} row=${row} />
      ${row.detail && html`<span class="phone-agent-why">${row.detail}</span>`}
    </button>
  </li>`;
}

// AgentSheet is one agent's sheet. It reads the agent live from the snapshot,
// so a state change while it is open shows (and a retired agent says so).
export function AgentSheet({ state, order, actions, agentKey, groupKey, close }) {
  const found = findRow(agentGroups(state.snapshot.value, order), agentKey, groupKey);
  if (!found) return html`<p class="ui-empty phone-empty">This agent is no longer listed.</p>`;
  const { row, group } = found;
  const run = (act) => () => { close(); act(row, group); };
  const item = (label, onClick, extra = '') => html`<li><button data-ui-exempt type="button" class=${`phone-menu-item${extra}`} onClick=${onClick}>${label}</button></li>`;
  return html`<div class="phone-agent-sheet">
    <div class="phone-agent-line"><h2 class="phone-agent-sheet-name">${row.name}</h2><${StateTag} row=${row} /></div>
    <p class="phone-agent-sheet-meta">${[group.group ? group.name : 'No group', row.role, row.harness, row.ctx !== null ? `context ${row.ctx}%` : ''].filter(Boolean).join(' · ')}</p>
    ${row.detail && html`<p class="phone-agent-why">${row.detail}</p>`}
    ${row.pr && html`<p class="phone-agent-sheet-meta"><a href=${row.pr.url} target="_blank" rel="noopener">PR #${row.pr.number}${row.pr.title ? ` · ${row.pr.title}` : ''}</a>${row.pr.ci ? ` · checks ${row.pr.ci}` : ''}</p>`}
    ${!row.pr && row.branch && html`<p class="phone-agent-sheet-meta">⎇ ${row.branch}</p>`}
    ${row.cwd && html`<p class="phone-agent-sheet-meta phone-agent-cwd">${row.cwd}</p>`}
    <ul class="phone-menu">
      ${row.online && item('Open terminal', run(actions.openTerminal))}
      ${item('Message…', run(actions.message))}
      ${row.online
        ? item('Shut down…', run((r) => actions.rowAction('dot-toggle', r)))
        : item(row.waking ? 'Waking…' : 'Power on', run((r) => actions.rowAction('dot-toggle', r)))}
      ${row.online && item('Restart…', run((r) => actions.rowAction('restart', r)))}
      ${group.group && item('Rename…', run(actions.rename))}
      ${item('Retire…', run((r) => actions.rowAction('retire-agent', r)), ' phone-menu-danger')}
    </ul>
  </div>`;
}

export function PhoneAgents({ state, order, actions }) {
  const [tab, setTab] = useState(ALL);
  const snapshot = state.snapshot.value;
  if (!snapshot) return html`<p class="ui-empty phone-empty">Loading…</p>`;
  const groups = agentGroups(snapshot, order);
  if (!groups.length) return html`<p class="ui-empty phone-empty">No agents yet.</p>`;
  const tabs = groupTabs(groups);
  const current = tabs.some((t) => t.key === tab) ? tab : ALL;
  const open = (g) => (row) => openPhoneSheet({ label: row.name, Body: AgentSheet, props: { state, order, actions, agentKey: row.key, groupKey: g.key } });
  return html`<div class="phone-agents">
    <${Tabs} class="phone-chips" label="Group" value=${current} onSelect=${setTab}
      items=${tabs.map((t) => ({ key: t.key, label: html`${t.label} <span class="phone-chip-count">${t.online}/${t.total}</span>` }))} />
    ${shownGroups(groups, current).map((g) => html`<div key=${g.key} class="phone-section" role="group" aria-label=${g.name}>
      <h2 class="phone-section-head ui-eyebrow">
        <span>${g.name} · ${g.live}/${g.total}</span>
        ${g.group && html`<${Button} size="sm" class="phone-agents-spawn" onClick=${() => actions.spawn(g.name)} aria-label=${`New agent in ${g.name}`}>＋ New<//>`}
      </h2>
      <ul class="phone-list">${g.shown.map((row) => html`<${AgentRow} key=${row.key} row=${row} onOpen=${open(g)} />`)}</ul>
      ${g.offline.length > 0 && html`<details class="phone-agents-offline">
        <summary>Offline · ${g.offline.length}</summary>
        <ul class="phone-list">${g.offline.map((row) => html`<${AgentRow} key=${row.key} row=${row} onOpen=${open(g)} />`)}</ul>
      </details>`}
    </div>`)}
  </div>`;
}

export function mountPhoneAgentsIsland({ host, state, order, actions, registerCleanup }) {
  render(html`<${PhoneAgents} state=${state} order=${order} actions=${actions} />`, host);
  registerCleanup(() => render(null, host));
}
