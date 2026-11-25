// phone-now.js -- the phone shell's Now view, in the shape of the Now mock.
// A row of chips picks what it shows: All (below), Needs you (the cards
// only), Idle (online idle agents by group), Recent (every online agent, the
// latest heard from first) and Problems (one go-look list: errors, CI failed,
// at work but quiet). A row's context meter sits by its state pill
// (context-meter.js): a full context is no problem. All is a one-line usage
// strip that opens Usage, then what needs the operator as cards (access
// requests, decisions, process gates, agents asking), the online agents in
// one card of rows (every one when there are few, else the ones at work or in
// error, with Show all), and the operator's open PRs behind a collapsed
// header, then the shells running (phone-now-shells.js), folded too. The
// shell (phone-shell-island.js) shows it as its home view.

import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { companionAgents } from './companion-model.js';
import { authoredOpenPRsView } from './shell-model.js';
import { featureState } from './peer-view-limits.js';
import {
  FEW_ONLINE, NOW_DEFAULTS, PROBLEMS, agentRows, askerName, hookAt, idleGroups, contextHigh, nowSettings, nowView, problemRows, recentRows,
  saveNowSettings,
} from './phone-shell-model.js';
import { usageStrip } from './phone-now-model.js';
import { AskCard, PhoneApprovalCard, PhoneDecisionCard } from './phone-approval.js';
import { ContextMeter } from './context-meter.js';
import { Button, Switch } from './ui-components.js';
import { Card, ChipRow, Empty, RowMore, Section, SheetHead, SheetTitle, StatePill, ViewIcon } from './phone-ui.js';
import { openPhoneSheet } from './phone-sheet.js';
import { usePhoneBar } from './phone-page.js';
import { dashPrefs } from './prefs.js';
import { useHold } from './phone-hold.js';
import { relTime } from './helpers.js';
import { processContext } from './process-context.js';
import { GateCard, gateItems, useProcessRuns } from './phone-process-runs.js';
import { NowShells } from './phone-now-shells.js';
import { checksIncomplete, incompleteChecksText } from './pr-checks-hover.js';

// spawnable says whether the node has a group an agent can be spawned into
// (one that is not archived), for Now's ＋ Agent.
export function spawnable(snapshot) {
  return (snapshot?.groups || []).some((g) => g?.name && !g.archived);
}

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

// AgentRows is one card of plain rows, one per agent, with its state pill;
// an error row says what went wrong, and a row may carry a time or reason
// line (when) in a tone (bad, or quiet for a quiet busy agent). A row opens the agent's
// terminal. heads put a heading over a row (Idle's groups, Problems' kinds),
// and the agent's context meter sits by its state pill.
// NowRow is one agent's row: a tap opens its terminal; a hold, or its ⋯,
// opens its menu (phone-agent-menu.js) when onMenu is wired.
function NowRow({ agent, state, when, tone, onOpen, onMenu }) {
  const { holding, handlers } = useHold(onMenu ? () => onMenu(agent) : null);
  return html`<div class=${`phone-now-agent${holding ? ' phone-holding' : ''}`}>
    <button data-ui-exempt type="button" class="phone-row phone-holdable" ...${handlers} onClick=${() => onOpen(agent)}
      aria-label=${`${agent.name}, ${state}${agent.owner && agent.group ? `, owner of ${agent.group}` : ''}${contextHigh(agent) ? `, context ${agent.ctx}% full` : ''}${when ? `, ${when}` : ''}: open its terminal`}>
      <span class="phone-row-text"><span class="phone-who-name">${agent.name}</span>${agent.group && html`<span class="phone-who-sub">${agent.group}</span>`}${agent.owner && agent.group && html`<span class="ui-tag ui-tag-own owner-badge" title=${`Owner of ${agent.group}`}>owner</span>`}
        ${state === 'error' && agent.detail && tone === undefined && html`<span class="phone-card-line">${agent.detail}</span>`}
        ${when && html`<span class=${`phone-now-when${tone ? ` ${tone}` : ''}`}>${when}</span>`}</span>
      ${agent.ctxState && html`<${ContextMeter} state=${agent.ctxState} />`}<${StatePill} state=${state} />
    </button>
    ${onMenu && html`<${RowMore} label=${`${agent.name}: more`} class="phone-now-more" onClick=${() => onMenu(agent)} />`}
  </div>`;
}

function AgentRows({ rows, onOpen, onMenu = null }) {
  return html`<${Card} class="phone-rows-card">
    <ul class="phone-rows">
      ${rows.map(({ agent, state, when, tone, head }) => html`<li key=${agent.agentId}>
        ${head && html`<p class="ui-eyebrow phone-now-group">${head}</p>`}
        <${NowRow} agent=${agent} state=${state} when=${when} tone=${tone} onOpen=${onOpen} onMenu=${onMenu} />
      </li>`)}
    </ul>
  <//>`;
}

// since is how long ago an agent last called a hook ("4m"), or '' if never.
const since = (agent) => (hookAt(agent) === null ? '' : relTime(agent.lastHook).replace(/ ago$/, ''));

// CHIPS are Now's views, one picked at a time; the pick is remembered per
// viewer (this browser) under CHIP_KEY.
export const CHIPS = Object.freeze(['all', 'needs', 'idle', 'recent', 'problems']);
const CHIP_KEY = 'tclaude.phone.now.chip';
export function storedChip(storage = globalThis.localStorage) {
  try {
    const chip = storage?.getItem(CHIP_KEY);
    if (chip === 'stuck') return 'problems'; // Problems took Stuck?'s place
    return CHIPS.includes(chip) ? chip : 'all';
  } catch { return 'all'; }
}
function storeChip(chip, storage = globalThis.localStorage) {
  try { storage?.setItem(CHIP_KEY, chip); } catch { /* private mode: the pick lasts this visit */ }
}

// LEGACY_LOOKED_KEY is where Now's former Since you looked section kept
// what the viewer last saw; Now drops it, so it does not linger.
const LEGACY_LOOKED_KEY = 'tclaude.phone.now.looked';
function dropLegacyLooked(storage = globalThis.localStorage) {
  try { storage?.removeItem(LEGACY_LOOKED_KEY); } catch { /* no storage: nothing kept */ }
}

// The tone of a problem's line.
const PROBLEM_TONE = Object.freeze({ error: 'bad', ci: 'bad', quiet: 'quiet' });

// problemLine says why a row is in Problems.
function problemLine({ kind, agent }) {
  if (kind === 'error') return agent.detail || 'in error';
  if (kind === 'ci') return `PR #${agent.pr.number} checks failing`;
  return since(agent) ? `last update ${since(agent)} ago` : 'no update time';
}

// OpenPRs is the operator's open PRs, collapsed under its header: the count
// says how many, and the header opens them.
function OpenPRs({ snapshot }) {
  const [open, setOpen] = useState(false);
  const view = authoredOpenPRsView(snapshot);
  if (!view.available || !view.total) return null;
  const shown = view.items.slice(0, 5);
  return html`<${Section} title="Open PRs" count=${view.total} fold=${{ open, onToggle: () => setOpen(!open) }}>
    ${open && html`<${Card} class="phone-rows-card">
      <ul class="phone-rows">
        ${shown.map((pr) => html`<li key=${pr.url}>
          <a href=${pr.url} target="_blank" rel="noopener" class="phone-row">
            <span class="phone-row-text"><span class="phone-pr-title">${pr.title || pr.url}</span></span>
            ${pr.draft && html`<${StatePill} tone="idle">draft<//>`}
            ${pr.checks?.state && (checksIncomplete(pr.checks)
              ? html`<${StatePill} tone="idle" title=${incompleteChecksText(pr.checks)}>too many checks<//>`
              : html`<${StatePill} tone=${CHECK_TONES[pr.checks.state] || 'idle'}>${pr.checks.state}<//>`)}
          </a>
        </li>`)}
      </ul>
    <//>`}
  <//>`;
}

// CHECK_TONES colour a PR's checks state.
const CHECK_TONES = Object.freeze({ success: 'work', passing: 'work', failure: 'err', failing: 'err', pending: 'key', running: 'key' });

// NowViewSheet is Now's View settings (phone-shell-model.js nowSettings),
// in the Agents View sheet's look: Group by group first, and Reset (off at
// the defaults). A change applies at once and is kept per viewer.
export function NowViewSheet({ prefs, onChange }) {
  const [settings, setSettings] = useState(() => nowSettings(prefs));
  const set = (patch) => {
    const next = { ...settings, ...patch };
    saveNowSettings(prefs, next);
    setSettings(next);
    onChange?.(next);
  };
  return html`<div class="phone-agents-view" data-now-view>
    <div class="phone-agents-view-head"><${SheetTitle}>View<//>
      <button data-ui-exempt type="button" class="phone-link phone-agents-view-reset" disabled=${!settings.byGroup}
        onClick=${() => set({ ...NOW_DEFAULTS })}>Reset</button></div>
    <${SheetHead}>Order<//>
    <label class="phone-agents-view-switch">
      <span class="phone-agents-view-text">Group by group first<small>then the most recently active first in each group</small></span>
      <${Switch} name="now-view-bygroup" checked=${settings.byGroup} onChange=${(event) => set({ byGroup: event.currentTarget.checked })} />
    </label>
    <p class="ui-hint phone-agents-view-hint">Off, errors, work and idle agents each list the most recently active first.</p>
  </div>`;
}

export function NowView({ state, actions, fetchImpl = globalThis.fetch, processSource = processContext, prefs = dashPrefs }) {
  const [showAll, setShowAll] = useState(false);
  const [settings, setSettings] = useState(() => nowSettings(prefs));
  const changed = settings.byGroup !== NOW_DEFAULTS.byGroup;
  usePhoneBar({ tab: 'now', actions: [
    { key: 'view', icon: ViewIcon, label: changed ? 'View: changed from the defaults' : 'View', dot: changed,
      onClick: () => openPhoneSheet({ label: 'View', Body: NowViewSheet, props: { prefs, onChange: setSettings } }) },
  ] });
  const [chip, setChip] = useState(storedChip);
  const pick = (key) => { setChip(key); storeChip(key); };
  useEffect(() => { dropLegacyLooked(); }, []);
  // Process gates waiting on the operator come from the process read model,
  // not the snapshot: Now keeps it fresh while it shows.
  const gates = gateItems(useProcessRuns(processSource).context);
  const snapshot = state.snapshot.value;
  if (!snapshot) return html`<${Empty}>Loading…<//>`;
  const now = nowView(snapshot, settings);
  const view = { ...now, needs: now.needs + gates.length };
  const agents = companionAgents(snapshot);
  const asker = askerName(snapshot, agents);
  const quiet = view.needs === 0;
  const list = agentRows(view, { showAll });
  const rows = list.rows;
  const problems = problemRows(view);
  const online = new Map(view.online.map((agent) => [agent.agentId, agent]));
  const chips = [
    { key: 'all', label: 'All', count: list.total },
    { key: 'needs', label: 'Needs you', badge: view.needs, badgeTone: 'danger' },
    { key: 'idle', label: 'Idle', count: view.resting.length },
    { key: 'recent', label: 'Recent' },
    { key: 'problems', label: 'Problems', badge: problems.length, badgeTone: 'danger' },
  ];
  const needsYou = html`<${Section} title="Needs you" count=${view.needs}>
      ${quiet && html`<${Empty}>Nothing needs you right now.<//>`}
      ${view.approvals.map((request) => {
        const agent = agents.get(request.agent_id);
        return html`<${PhoneApprovalCard} key=${request.id} request=${request} fetchImpl=${fetchImpl} notify=${actions.notify}
          from=${agent?.name || request.conv_title || ''} group=${agent?.group || ''} ctxState=${online.get(request.agent_id)?.ctxState ?? null}
          onMore=${() => actions.openAccessRequest(request.id)} />`;
      })}
      ${view.elsewhere > 0 && html`<${Button} class="phone-btn phone-wide" onClick=${() => actions.openAccessRequest()}>
        ${view.elsewhere} more access request${view.elsewhere === 1 ? '' : 's'} in Mail ›<//>`}
      ${view.decisions.map((decision) => html`<${PhoneDecisionCard} key=${decision.id} decision=${decision} fetchImpl=${fetchImpl}
        notify=${actions.notify} from=${asker(decision)} steward=${!!agents.get(decision.from_agent)?.steward} />`)}
      ${gates.map((item) => html`<${GateCard} key=${item.run.runId} item=${item} source=${processSource} />`)}
      ${view.asking.map((agent) => html`<${AskCard} key=${agent.agentId} agent=${agent} onOpen=${actions.openTerminal} onMenu=${actions.agentMenu} />`)}
    <//>`;
  const rowsOr = (list, empty) => (list.length
    ? html`<${AgentRows} rows=${list} onOpen=${actions.openTerminal} onMenu=${actions.agentMenu} />`
    : html`<${Empty}>${empty}<//>`);
  let body;
  if (chip === 'needs') body = needsYou;
  else if (chip === 'idle') {
    const idle = idleGroups(view).flatMap(({ group, agents: list }) => list.map((agent, i) => ({
      agent, state: 'idle', when: since(agent) && `idle ${since(agent)}`, head: i === 0 ? `${group || 'No group'} · ${list.length}` : '',
    })));
    body = html`<${Section} title="Idle" count=${idle.length}>${rowsOr(idle, 'No agent is idle.')}<//>`;
  } else if (chip === 'recent') {
    const recent = recentRows(view).map((row) => ({ ...row, when: since(row.agent) && `updated ${since(row.agent)} ago` }));
    body = html`<${Section} title="Recent" count=${recent.length}>${rowsOr(recent, 'No agent is online.')}<//>`;
  } else if (chip === 'problems') {
    const rows = problems.map((row, i) => ({
      ...row, when: problemLine(row), tone: PROBLEM_TONE[row.kind],
      head: i === 0 || problems[i - 1].kind !== row.kind
        ? `${PROBLEMS.find((p) => p.kind === row.kind).title} · ${problems.filter((p) => p.kind === row.kind).length}` : '',
    }));
    body = html`<${Section} title="Problems" count=${rows.length}>${rowsOr(rows, 'Nothing to look at: no errors, failing checks or quiet agents.')}<//>`;
  } else {
    body = html`
    <${UsageStrip} snapshot=${snapshot} onOpen=${() => actions.navigate('/usage')} />
    ${needsYou}
    <${Section} title=${list.all ? 'Online' : 'Active'} count=${rows.length}
      more=${html`<span class="phone-section-links">
        ${actions.spawnAgent && spawnable(snapshot) && html`<button data-ui-exempt type="button" class="phone-link" data-agent-add
          aria-label="New agent" aria-haspopup="dialog" onClick=${() => actions.spawnAgent('')}>＋ Agent</button>`}
        <button data-ui-exempt type="button" class="phone-link" onClick=${() => actions.navigate('/')}>All groups ›</button></span>`}>
      ${rows.length === 0
        ? html`<${Empty}>${list.all ? 'No agent is online.' : 'No agent is active right now.'}<//>`
        : html`<${AgentRows} rows=${rows} onOpen=${actions.openTerminal} onMenu=${actions.agentMenu} />`}
      ${list.total > FEW_ONLINE && html`<button data-ui-exempt type="button" class="phone-now-all" aria-expanded=${showAll ? 'true' : 'false'}
        onClick=${() => setShowAll(!showAll)}>${showAll ? 'Only the recent ones' : `Show all ${list.total} online`}
        <span aria-hidden="true">${showAll ? '⌄' : '›'}</span></button>`}
    <//>
    <${OpenPRs} snapshot=${snapshot} />
    <${NowShells} snapshot=${state.snapshot} notify=${actions.notify} />`;
  }
  return html`<div class="phone-now">
    <${ChipRow} label="Show on Now" class="phone-now-chips" items=${chips} value=${chip} onSelect=${pick} />
    ${body}
  </div>`;
}
