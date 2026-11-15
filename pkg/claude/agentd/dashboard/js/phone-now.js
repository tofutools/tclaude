// phone-now.js -- the phone shell's Now view, in the shape of the Now mock:
// a one-line usage strip that opens Usage, then what needs the operator as
// cards (access requests, decisions, process gates, agents asking), the
// agents at work and in error in one card of rows, and the operator's open
// PRs behind a collapsed header. The shell (phone-shell-island.js) shows it as
// its home view.

import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { companionAgents } from './companion-model.js';
import { authoredOpenPRsView } from './shell-model.js';
import { featureState } from './peer-view-limits.js';
import { askerName, nowView } from './phone-shell-model.js';
import { usageStrip } from './phone-now-model.js';
import { AskCard, PhoneApprovalCard, PhoneDecisionCard } from './phone-approval.js';
import { Button } from './ui-components.js';
import { Card, Section, StatePill } from './phone-ui.js';
import { processContext } from './process-context.js';
import { GateCard, gateItems, useProcessRuns } from './phone-process-runs.js';

const html = htm.bind(h);

// usageShared reports whether the page's node shares its usage: a peer's
// page may not (peer-view-limits.js).
function usageShared(snapshot) {
  return !globalThis.__tclaudeRemoteNode?.id || featureState('costs', snapshot?.peer_view) === 'shared';
}

// UsageStrip is the usage line: each subscription's fullest window as a small
// bar and a percentage. It opens Usage.
function UsageStrip({ snapshot, onOpen }) {
  const strip = usageShared(snapshot) ? usageStrip(snapshot?.usage) : [];
  if (!strip.length) return null;
  const said = strip.map((s) => `${s.name} ${s.pct}% of ${s.window}`).join(', ');
  return html`<button data-ui-exempt type="button" class="phone-usage-strip" aria-label=${`Usage: ${said}. Open Usage`} onClick=${onOpen}>
    ${strip.map((s) => html`<span key=${s.key} class="phone-usage-item">
      <span class="phone-usage-name">${s.name}</span>
      <span class="phone-usage-bar" aria-hidden="true"><b style=${`width:${Math.min(100, s.pct)}%;background:${s.color}`}></b></span>
      <span class="phone-usage-pct">${s.pct}%</span><span class="phone-usage-win">${s.window}</span>
    </span>`)}
    <span class="phone-usage-go" aria-hidden="true">›</span>
  </button>`;
}

// AgentRows is one card of plain rows, one per agent at work or in error,
// with its state pill; an error row says what went wrong. A row opens the
// agent's terminal.
function AgentRows({ rows, onOpen }) {
  return html`<${Card} class="phone-rows-card">
    <ul class="phone-rows">
      ${rows.map(({ agent, state }) => html`<li key=${agent.agentId}>
        <button data-ui-exempt type="button" class="phone-row" onClick=${() => onOpen(agent)} aria-label=${`${agent.name}, ${state}: open its terminal`}>
          <span class="phone-row-text"><span class="phone-who-name">${agent.name}</span>${agent.group && html`<span class="phone-who-sub">${agent.group}</span>`}
            ${state === 'error' && agent.detail && html`<span class="phone-card-line">${agent.detail}</span>`}</span>
          <${StatePill} state=${state} />
        </button>
      </li>`)}
    </ul>
  <//>`;
}

// OpenPRs is the operator's open PRs, collapsed under its header: the count
// says how many, and the header opens them.
function OpenPRs({ snapshot }) {
  const [open, setOpen] = useState(false);
  const view = authoredOpenPRsView(snapshot);
  if (!view.available || !view.total) return null;
  const shown = view.items.slice(0, 5);
  return html`<${Section} title="Open PRs" count=${view.total}
    more=${html`<button data-ui-exempt type="button" class="phone-link" aria-expanded=${open ? 'true' : 'false'}
      aria-label=${open ? 'Hide open PRs' : 'Show open PRs'} onClick=${() => setOpen(!open)}>${open ? '⌄' : '›'}</button>`}>
    ${open && html`<${Card} class="phone-rows-card">
      <ul class="phone-rows">
        ${shown.map((pr) => html`<li key=${pr.url}>
          <a href=${pr.url} target="_blank" rel="noopener" class="phone-row">
            <span class="phone-row-text"><span class="phone-pr-title">${pr.title || pr.url}</span></span>
            ${pr.draft && html`<${StatePill} tone="idle">draft<//>`}
            ${pr.checks?.state && html`<${StatePill} tone=${CHECK_TONES[pr.checks.state] || 'idle'}>${pr.checks.state}<//>`}
          </a>
        </li>`)}
      </ul>
    <//>`}
  <//>`;
}

// CHECK_TONES colour a PR's checks state.
const CHECK_TONES = Object.freeze({ success: 'work', passing: 'work', failure: 'err', failing: 'err', pending: 'key', running: 'key' });

export function NowView({ state, actions, fetchImpl = globalThis.fetch, processSource = processContext }) {
  // Process gates waiting on the operator come from the process read model,
  // not the snapshot: Now keeps it fresh while it shows.
  const gates = gateItems(useProcessRuns(processSource).context);
  const snapshot = state.snapshot.value;
  if (!snapshot) return html`<div class="ui-empty phone-empty">Loading…</div>`;
  const now = nowView(snapshot);
  const view = { ...now, needs: now.needs + gates.length };
  const agents = companionAgents(snapshot);
  const asker = askerName(snapshot, agents);
  const quiet = view.needs === 0;
  const rows = [...view.errors.map((agent) => ({ agent, state: 'error' })), ...view.working.map((agent) => ({ agent, state: 'working' }))];
  return html`<div class="phone-now">
    <${UsageStrip} snapshot=${snapshot} onOpen=${() => actions.navigate('/usage')} />
    <${Section} title="Needs you" count=${view.needs}>
      ${quiet && html`<p class="ui-empty phone-empty">Nothing needs you right now.</p>`}
      ${view.approvals.map((request) => {
        const agent = agents.get(request.agent_id);
        return html`<${PhoneApprovalCard} key=${request.id} request=${request} fetchImpl=${fetchImpl} notify=${actions.notify}
          from=${agent?.name || request.conv_title || ''} group=${agent?.group || ''} onMore=${() => actions.openAccessRequest(request.id)} />`;
      })}
      ${view.elsewhere > 0 && html`<${Button} class="phone-btn phone-wide" onClick=${() => actions.openAccessRequest()}>
        ${view.elsewhere} more access request${view.elsewhere === 1 ? '' : 's'} in Mail ›<//>`}
      ${view.decisions.map((decision) => html`<${PhoneDecisionCard} key=${decision.id} decision=${decision} fetchImpl=${fetchImpl}
        notify=${actions.notify} from=${asker(decision)} steward=${!!agents.get(decision.from_agent)?.steward} />`)}
      ${gates.map((item) => html`<${GateCard} key=${item.run.runId} item=${item} source=${processSource} />`)}
      ${view.asking.map((agent) => html`<${AskCard} key=${agent.agentId} agent=${agent} onOpen=${actions.openTerminal} />`)}
    <//>
    <${Section} title="Working" count=${rows.length}
      more=${html`<button data-ui-exempt type="button" class="phone-link" onClick=${() => actions.navigate('/')}>All agents ›</button>`}>
      ${rows.length === 0
        ? html`<p class="ui-empty phone-empty">No agent is working.</p>`
        : html`<${AgentRows} rows=${rows} onOpen=${actions.openTerminal} />`}
    <//>
    <${OpenPRs} snapshot=${snapshot} />
  </div>`;
}
