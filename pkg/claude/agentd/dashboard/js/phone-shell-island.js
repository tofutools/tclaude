// phone-shell-island.js -- the dashboard's phone shell (data-layout="phone").
//
// A top bar and a five-slot bottom nav (Now · Agents · Mail · Board · More)
// replace the desktop header, tab bar, footer, dock and overlay windows, which
// phone.css hides. The content area between the bars is either one of the
// shell's own views (Now, Board) or the dashboard tab that is active: Agents is
// the Groups tab, Mail is Messages, and the More sheet opens every other tab.
// Tabs keep their desktop islands until each gets its phone view.
//
// The shell marks <html data-phone-view="now|board"> while an own view is up, so
// phone.css can hide <main> under it. Any tab activation (a slot, a More entry,
// a deep link, Back) clears the own view.

import { h, render } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import { signal } from '@preact/signals';
import htm from 'htm';
import { companionAgents } from './companion-model.js';
import { ApprovalCard, DecisionCard } from './companion-approval.js';
import { messagesBadgeView, footerMetaView, authoredOpenPRsView } from './shell-model.js';
import { Usage } from './shell-island.js';
import {
  MORE_TABS, OWN_VIEWS, PHONE_SLOTS, askerName, boardGroups, boardLanes, nowView, slotFor, taskPath, titleFor,
} from './phone-shell-model.js';
import { Button } from './ui-components.js';
import { Tabs } from './ui-tabs.js';
import { phonePage } from './phone-page.js';
import { PhoneSheetHost, closePhoneSheet, openPhoneSheet } from './phone-sheet.js';

const html = htm.bind(h);

// createPhoneShellState holds the shell's own view ('now', 'board' or null
// for a tab). A fresh load of the bare dashboard URL lands on Now; a deep
// link lands on its tab.
export function createPhoneShellState({ startOnNow = true } = {}) {
  return {
    view: signal(startOnNow ? 'now' : null),
  };
}

// landsOnNow reports whether a page load at this URL starts on Now: the bare
// dashboard path with no deep-link query.
export function landsOnNow(loc = globalThis.location) {
  const path = loc?.pathname || '/';
  return (path === '/' || path === '/dashboard') && !loc?.search;
}

function AgentRow({ agent, actions, tag = null, detail = '' }) {
  return html`<li class="phone-agent">
    <div class="phone-agent-text">
      <span class="phone-agent-name">${agent.name}</span>
      ${agent.group && html`<span class="phone-agent-group">${agent.group}</span>`}
      ${detail && html`<span class="phone-agent-detail">${detail}</span>`}
    </div>
    ${tag}
    <div class="phone-agent-acts">
      <${Button} size="sm" onClick=${() => actions.message(agent)} aria-label=${`Message ${agent.name}`}>✉<//>
      <${Button} size="sm" onClick=${() => actions.openTerminal(agent)}>Open<//>
    </div>
  </li>`;
}

function Section({ title, count = null, children, more = null }) {
  // A div, not a <section>: dashboard.css hides every section but the active tab.
  return html`<div class="phone-section" role="group" aria-label=${title}>
    <h2 class="phone-section-head ui-eyebrow">
      <span>${title}${count !== null && html` · ${count}`}</span>${more}
    </h2>
    ${children}
  </div>`;
}

function OpenPRs({ snapshot }) {
  const view = authoredOpenPRsView(snapshot);
  if (!view.available || !view.total) return null;
  const shown = view.items.slice(0, 5);
  return html`<${Section} title="Open PRs" count=${view.total}>
    <ul class="phone-list">
      ${shown.map((pr) => html`<li key=${pr.url} class="phone-pr">
        <a href=${pr.url} target="_blank" rel="noopener" class="phone-pr-title">${pr.title || pr.url}</a>
        ${pr.draft && html`<span class="ui-tag">draft</span>`}
        ${pr.checks?.state && html`<span class=${`ui-tag phone-pr-checks phone-pr-${pr.checks.state}`}>${pr.checks.state}</span>`}
      </li>`)}
    </ul>
  <//>`;
}

export function NowView({ state, actions, fetchImpl = globalThis.fetch }) {
  const snapshot = state.snapshot.value;
  if (!snapshot) return html`<div class="ui-empty phone-empty">Loading…</div>`;
  const view = nowView(snapshot);
  const agents = companionAgents(snapshot);
  const nameOf = (id) => agents.get(id)?.name || '';
  const quiet = view.needs === 0;
  return html`<div class="phone-now">
    <div class="phone-usage ui-card"><${Usage} state=${state} /></div>
    <${Section} title="Needs you" count=${view.needs}>
      ${quiet && html`<p class="ui-empty phone-empty">Nothing needs you right now.</p>`}
      ${view.approvals.map((request) => html`<div key=${request.id} class="phone-card">
        <${ApprovalCard} request=${request} fetchImpl=${fetchImpl} notify=${actions.notify} from=${nameOf(request.agent_id) || request.conv_title || ''}
          onMore=${() => actions.openAccessRequest(request.id)} />
      </div>`)}
      ${view.elsewhere > 0 && html`<${Button} class="phone-wide" onClick=${() => actions.openAccessRequest()}>
        ${view.elsewhere} more access request${view.elsewhere === 1 ? '' : 's'} in Mail ›<//>`}
      ${view.decisions.map((decision) => html`<div key=${decision.id} class="phone-card">
        <${DecisionCard} decision=${decision} fetchImpl=${fetchImpl} notify=${actions.notify} from=${askerName(snapshot, agents)(decision)} />
      </div>`)}
      ${view.asking.length > 0 && html`<ul class="phone-list">
        ${view.asking.map((agent) => html`<${AgentRow} key=${agent.agentId} agent=${agent} actions=${actions}
          detail=${agent.detail || 'is asking you'} tag=${html`<span class="ui-tag ui-tag-accent">asking</span>`} />`)}
      </ul>`}
    <//>
    ${view.errors.length > 0 && html`<${Section} title="Errors" count=${view.errors.length}>
      <ul class="phone-list">
        ${view.errors.map((agent) => html`<${AgentRow} key=${agent.agentId} agent=${agent} actions=${actions}
          detail=${agent.detail} tag=${html`<span class="ui-tag ui-tag-warn">error</span>`} />`)}
      </ul>
    <//>`}
    <${Section} title="Working" count=${view.working.length}
      more=${html`<button data-ui-exempt type="button" class="phone-link" onClick=${() => actions.navigate('/')}>All agents ›</button>`}>
      ${view.working.length === 0
        ? html`<p class="ui-empty phone-empty">No agent is working.</p>`
        : html`<ul class="phone-list">
          ${view.working.map((agent) => html`<${AgentRow} key=${agent.agentId} agent=${agent} actions=${actions} />`)}
        </ul>`}
    <//>
    <${OpenPRs} snapshot=${snapshot} />
  </div>`;
}

export function BoardView({ source, state, actions }) {
  const [picked, setPicked] = useState('');
  useEffect(() => source.attach(), [source]);
  const status = source.status.value;
  if (status === 'unavailable') return html`<p class="ui-empty phone-empty">Task boards are turned off.</p>`;
  const context = source.context.value;
  if (!context) return html`<p class="ui-empty phone-empty">${status === 'error' ? `Could not read the boards: ${source.error.value}` : 'Loading…'}</p>`;
  const groups = boardGroups(context);
  if (groups.length === 0) return html`<p class="ui-empty phone-empty">No open tasks on any board.</p>`;
  const group = groups.find((g) => g.group === picked)?.group || groups[0].group;
  const entry = context.groups.find((g) => g.group === group);
  const agents = companionAgents(state.snapshot.value);
  return html`<div class="phone-board">
    <${Tabs} class="phone-chips" label="Group" value=${group} onSelect=${setPicked}
      items=${groups.map((g) => ({
        key: g.group,
        label: html`${g.group} <span class="phone-chip-count">${g.live}</span>${g.waiting > 0 && html` <span class="ui-badge ui-badge-accent">${g.waiting}</span>`}`,
      }))} />
    ${boardLanes(entry).map((lane) => html`<${Section} key=${lane.id} title=${lane.label} count=${lane.tasks.length}>
      <ul class="phone-list">
        ${lane.tasks.map((task) => html`<li key=${task.id}>
          <button data-ui-exempt type="button" class="phone-task" onClick=${() => actions.navigate(taskPath(task, group))}>
            <span class="phone-task-title">${task.title}</span>
            ${task.assignee?.agent_id && !task.assignee.operator && html`<span class="phone-task-who">@${agents.get(task.assignee.agent_id)?.name || task.assignee.agent_id.slice(0, 12)}</span>`}
            ${task.assignee?.operator && html`<span class="phone-task-who">you</span>`}
          </button>
        </li>`)}
      </ul>
    <//>`)}
  </div>`;
}

// MoreMenu is the More sheet: every other tab, the power actions and the
// layout switch.
function MoreMenu({ state, actions, close }) {
  const go = (path) => { close(); actions.navigate(path); };
  const run = (act) => { close(); act(); };
  const meta = footerMetaView(state.snapshot.value);
  return html`<ul class="phone-menu">
      ${MORE_TABS.filter((entry) => actions.tabAvailable(entry.tab)).map((entry) => html`<li key=${entry.tab}>
        <button data-ui-exempt type="button" class="phone-menu-item" onClick=${() => go(entry.path)}>
          <span class="phone-menu-icon" aria-hidden="true">${entry.icon}</span>${entry.label}
        </button>
      </li>`)}
    </ul>
    <h2 class="ui-eyebrow phone-menu-head">Actions</h2>
    <ul class="phone-menu">
      <li><button data-ui-exempt type="button" class="phone-menu-item" onClick=${() => run(actions.powerOnAll)}>
        <span class="phone-menu-icon" aria-hidden="true">🟢</span>Power on all</button></li>
      <li><button data-ui-exempt type="button" class="phone-menu-item phone-menu-danger" onClick=${() => run(actions.shutdownAll)}>
        <span class="phone-menu-icon" aria-hidden="true">🛑</span>Shut down all…</button></li>
      <li><button data-ui-exempt type="button" class="phone-menu-item" onClick=${() => run(actions.desktopSite)}>
        <span class="phone-menu-icon" aria-hidden="true">🖥</span>Desktop site</button></li>
    </ul>
    ${meta && html`<p class="phone-sheet-meta">tclaude ${meta.version}</p>`}`;
}

export function PhoneShell({ shell, state, actions, boardSource, fetchImpl = globalThis.fetch }) {
  const activeTab = state.activeTab.value;
  const view = shell.view.value;
  const slot = slotFor(view, activeTab);
  const snapshot = state.snapshot.value;
  const needs = snapshot ? nowView(snapshot).needs : 0;
  const mail = messagesBadgeView(snapshot);
  const connection = state.connection.value.status;
  // A tab view's pushed page (js/phone-page.js): back, title and actions in
  // the top bar, and maybe no bottom nav. An own view has no pages.
  const page = OWN_VIEWS.includes(view) ? null : phonePage.value;
  const hideNav = !!page?.hideNav;

  // Any tab activation after the first render clears the own view, so a deep
  // link, Back, or a link inside a view shows its tab.
  const seenTab = useRef(activeTab);
  useEffect(() => {
    if (seenTab.current === activeTab) return;
    seenTab.current = activeTab;
    shell.view.value = null;
  }, [activeTab]);
  useEffect(() => {
    const root = document.documentElement;
    if (OWN_VIEWS.includes(view)) root.dataset.phoneView = view;
    else delete root.dataset.phoneView;
  }, [view]);
  useEffect(() => {
    const root = document.documentElement;
    if (hideNav) root.dataset.phonePage = 'full';
    else delete root.dataset.phonePage;
  }, [hideNav]);

  // What the views and the More sheet do. Anything that goes to a tab leaves
  // the own view first: the tab may already be the active one (Groups under a
  // fresh Now), and then no tab change would clear it.
  const viewActions = useMemo(() => {
    const leave = (run) => (...args) => {
      shell.view.value = null;
      closePhoneSheet();
      return run(...args);
    };
    return {
      ...actions,
      navigate: leave(actions.navigate),
      openTerminal: leave(actions.openTerminal),
      openAccessRequest: leave(actions.openAccessRequest),
    };
  }, [actions, shell]);

  // A tab's pushed page goes when its tab does (the view takes it down when
  // it goes inactive; clearing it here as well covers a view that does not).
  // Picking the tab that is already showing keeps it: nothing would put it
  // back, and the page would lose its back button.
  function pick(target) {
    closePhoneSheet();
    if (target.id === 'more') {
      openPhoneSheet({ label: 'More', Body: MoreMenu, props: { state, actions: viewActions } });
      return;
    }
    if (OWN_VIEWS.includes(target.id)) {
      shell.view.value = target.id;
      return;
    }
    shell.view.value = null;
    if (target.tab !== activeTab) phonePage.value = null;
    actions.showTab(target.tab);
  }

  const badge = (id) => {
    if (id === 'now' && needs > 0) return html`<span class="ui-badge ui-badge-danger phone-nav-badge">${needs}</span>`;
    if (id === 'mail' && !mail.hidden) return html`<span class="ui-badge ui-badge-accent phone-nav-badge">${mail.text}</span>`;
    return null;
  };

  return html`<div class="phone-shell">
    <header class=${`phone-bar${page ? ' phone-bar-page' : ''}`}>
      ${page ? html`<button data-ui-exempt type="button" class="phone-bar-back" aria-label="Back" onClick=${() => page.onBack?.()}>‹</button>
        <h1 class="phone-bar-title">${page.title || ''}</h1>
        ${(page.actions || []).map((act) => html`<button data-ui-exempt key=${act.key || act.label} type="button"
          class=${`phone-bar-act${act.danger ? ' danger' : ''}`} aria-label=${act.label} title=${act.label}
          disabled=${!!act.disabled} onClick=${act.onClick}>${act.icon || act.label}</button>`)}`
      : html`<img class="phone-bar-logo" src="/static/tclaude-icon.svg" alt="" />
        <h1 class="phone-bar-title">${titleFor(view, activeTab)}</h1>
        <span class=${`phone-bar-live ${connection}`} role="status" title=${connection === 'connected' ? 'Connected' : connection}
          aria-label=${connection === 'connected' ? 'Connected' : `Connection: ${connection}`}></span>`}
    </header>
    ${view === 'now' && html`<div class="phone-view" role="region" aria-label="Now"><${NowView} state=${state} actions=${viewActions} fetchImpl=${fetchImpl} /></div>`}
    ${view === 'board' && html`<div class="phone-view" role="region" aria-label="Board"><${BoardView} source=${boardSource} state=${state} actions=${viewActions} /></div>`}
    ${!hideNav && html`<nav class="phone-nav" aria-label="Views">
      ${PHONE_SLOTS.map((target) => html`<button data-ui-exempt key=${target.id} type="button"
        class=${`phone-nav-item${slot === target.id ? ' on' : ''}`} aria-current=${slot === target.id ? 'page' : undefined}
        aria-haspopup=${target.id === 'more' ? 'dialog' : undefined} onClick=${() => pick(target)}>
        <span class="phone-nav-icon" aria-hidden="true">${target.icon}</span>${target.label}${badge(target.id)}
      </button>`)}
    </nav>`}
    <${PhoneSheetHost} />
  </div>`;
}

export function mountPhoneShellIsland({ host, shell, state, actions, boardSource, registerCleanup }) {
  render(html`<${PhoneShell} shell=${shell} state=${state} actions=${actions} boardSource=${boardSource} />`, host);
  registerCleanup(() => {
    render(null, host);
    phonePage.value = null;
    closePhoneSheet();
    delete document.documentElement.dataset.phoneView;
    delete document.documentElement.dataset.phonePage;
  });
}
