// phone-now.js -- the phone shell's Now view: what needs the operator first
// (access requests, decisions, process gates, agents asking), then errors,
// the agents at work and the operator's open PRs. The shell
// (phone-shell-island.js) shows it as its home view.

import { h } from 'preact';
import htm from 'htm';
import { companionAgents } from './companion-model.js';
import { ApprovalCard, DecisionCard } from './companion-approval.js';
import { authoredOpenPRsView } from './shell-model.js';
import { Usage } from './shell-island.js';
import { askerName, nowView } from './phone-shell-model.js';
import { Button } from './ui-components.js';
import { Section } from './phone-ui.js';
import { processContext } from './process-context.js';
import { GateCard, gateItems, useProcessRuns } from './phone-process-runs.js';

const html = htm.bind(h);

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

export function NowView({ state, actions, fetchImpl = globalThis.fetch, processSource = processContext }) {
  // Process gates waiting on the operator come from the process read model,
  // not the snapshot: Now keeps it fresh while it shows.
  const gates = gateItems(useProcessRuns(processSource).context);
  const snapshot = state.snapshot.value;
  if (!snapshot) return html`<div class="ui-empty phone-empty">Loading…</div>`;
  const now = nowView(snapshot);
  const view = { ...now, needs: now.needs + gates.length };
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
      ${gates.map((item) => html`<${GateCard} key=${item.run.runId} item=${item} source=${processSource} />`)}
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
