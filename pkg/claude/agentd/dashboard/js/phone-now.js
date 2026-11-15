// phone-now.js -- the phone shell's Now view, in the shape of the Now mock.
// A row of chips picks what it shows: All (below), Needs you (the cards
// only), Idle (online idle agents by group), Recent (every online agent, the
// latest heard from first) and Problems (one go-look list: errors, CI failed,
// context nearly full, at work but quiet). All also says what changed since
// this viewer last looked, and a nearly full context is tagged by the name. All is a one-line usage strip that opens Usage, then what needs the operator as
// cards (access requests, decisions, process gates, agents asking), the
// online agents in one card of rows (every one when there are few, else the
// ones at work or in error, with Show all), and the operator's open PRs
// behind a collapsed header. The shell (phone-shell-island.js) shows it as
// its home view.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { companionAgents } from './companion-model.js';
import { authoredOpenPRsView } from './shell-model.js';
import { featureState } from './peer-view-limits.js';
import { FEW_ONLINE, PROBLEMS, agentRows, askerName, hookAt, idleGroups, lookedState, nowView, problemRows, recentRows, rowState, sinceLooked } from './phone-shell-model.js';
import { usageStrip } from './phone-now-model.js';
import { AskCard, ContextTag, PhoneApprovalCard, PhoneDecisionCard } from './phone-approval.js';
import { Button } from './ui-components.js';
import { Card, ChipRow, Section, StatePill } from './phone-ui.js';
import { relTime } from './helpers.js';
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

// AgentRows is one card of plain rows, one per agent, with its state pill;
// an error row says what went wrong, and a row may carry a time or reason
// line (when) in a tone (ok, bad, info, warn). A row opens the agent's
// terminal. heads put a heading over a row (Idle's groups, Problems' kinds);
// changed (agent ids) puts a blue dot on what changed since the viewer
// looked, and a nearly full context is tagged by the name.
function AgentRows({ rows, onOpen, changed = null }) {
  return html`<${Card} class="phone-rows-card">
    <ul class="phone-rows">
      ${rows.map(({ agent, state, when, tone, head }) => html`<li key=${agent.agentId}>
        ${head && html`<p class="ui-eyebrow phone-now-group">${head}</p>`}
        <button data-ui-exempt type="button" class="phone-row" onClick=${() => onOpen(agent)}
          aria-label=${`${agent.name}${changed?.has(agent.agentId) ? ' (changed since you looked)' : ''}, ${state}${when ? `, ${when}` : ''}: open its terminal`}>
          ${changed?.has(agent.agentId) && html`<span class="phone-now-new" aria-hidden="true"></span>`}
          <span class="phone-row-text"><span class="phone-who-name">${agent.name}</span>${agent.group && html`<span class="phone-who-sub">${agent.group}</span>`}<${ContextTag} ctx=${agent.ctx ?? null} />
            ${state === 'error' && agent.detail && tone === undefined && html`<span class="phone-card-line">${agent.detail}</span>`}
            ${when && html`<span class=${`phone-now-when${tone ? ` ${tone}` : ''}`}>${when}</span>`}</span>
          <${StatePill} state=${state} />
        </button>
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

// LOOKED_KEY holds, per viewer, what Now showed when the viewer last looked
// (phone-shell-model lookedState). It moves on Mark seen, or on leaving Now
// (or hiding the page) after it has been up LOOK_MS, so a glance does not
// clear what changed.
const LOOKED_KEY = 'tclaude.phone.now.looked';
export const LOOK_MS = 5000;
export function storedLooked(storage = globalThis.localStorage) {
  try {
    const value = JSON.parse(storage?.getItem(LOOKED_KEY) || 'null');
    return value && typeof value === 'object' && !Array.isArray(value) ? value : null;
  } catch { return null; }
}
function storeLooked(value, storage = globalThis.localStorage) {
  try { storage?.setItem(LOOKED_KEY, JSON.stringify(value)); } catch { /* private mode: marks last this visit */ }
}

// The tones and marks of a change's or a problem's line.
const CHANGE_TONE = Object.freeze({ errored: 'bad', ci: 'bad', merged: 'info', finished: 'ok' });
const CHANGE_MARK = Object.freeze({ errored: '✗', ci: '✗', merged: '⇡', finished: '✓' });
const PROBLEM_TONE = Object.freeze({ error: 'bad', ci: 'bad', context: 'quiet', quiet: 'quiet' });

// changeRows folds Since you looked's changes into one row per agent, the
// worst change's tone leading.
function changeRows(changes) {
  const rows = new Map();
  for (const change of changes) {
    const row = rows.get(change.agent.agentId);
    const text = `${CHANGE_MARK[change.kind]} ${change.text}`;
    if (row) row.when += ` · ${text}`;
    else rows.set(change.agent.agentId, { agent: change.agent, state: rowState(change.agent), when: text, tone: CHANGE_TONE[change.kind] });
  }
  return [...rows.values()];
}

// problemLine says why a row is in Problems.
function problemLine({ kind, agent }) {
  if (kind === 'error') return agent.detail || 'in error';
  if (kind === 'ci') return `PR #${agent.pr.number} checks failing`;
  if (kind === 'context') return `context ${agent.ctx}% full`;
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
  const [showAll, setShowAll] = useState(false);
  const [chip, setChip] = useState(storedChip);
  const pick = (key) => { setChip(key); storeChip(key); };
  const [looked, setLooked] = useState(storedLooked);
  // latest is what Now shows now, for the look to keep when it moves.
  const latest = useRef(null);
  useEffect(() => {
    const since = Date.now();
    const leave = () => { if (latest.current && Date.now() - since >= LOOK_MS) storeLooked(latest.current); };
    const onHide = () => { if (globalThis.document?.visibilityState === 'hidden') leave(); };
    globalThis.document?.addEventListener('visibilitychange', onHide);
    return () => { globalThis.document?.removeEventListener('visibilitychange', onHide); leave(); };
  }, []);
  const ready = !!state.snapshot.value;
  // A first visit has nothing to compare: what it sees is the first look.
  useEffect(() => { if (ready && !looked && latest.current) { storeLooked(latest.current); setLooked(latest.current); } }, [ready]);
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
  const list = agentRows(view, { showAll });
  const rows = list.rows;
  latest.current = lookedState(view);
  const problems = problemRows(view);
  const changes = changeRows(sinceLooked(view, looked));
  const changed = new Set(changes.map((row) => row.agent.agentId));
  const markSeen = () => { storeLooked(latest.current); setLooked(latest.current); };
  const online = new Map(view.online.map((agent) => [agent.agentId, agent]));
  const chips = [
    { key: 'all', label: 'All', count: list.total },
    { key: 'needs', label: 'Needs you', badge: view.needs, badgeTone: 'danger' },
    { key: 'idle', label: 'Idle', count: view.resting.length },
    { key: 'recent', label: 'Recent' },
    { key: 'problems', label: 'Problems', badge: problems.length, badgeTone: 'danger' },
  ];
  const needsYou = html`<${Section} title="Needs you" count=${view.needs}>
      ${quiet && html`<p class="ui-empty phone-empty">Nothing needs you right now.</p>`}
      ${view.approvals.map((request) => {
        const agent = agents.get(request.agent_id);
        return html`<${PhoneApprovalCard} key=${request.id} request=${request} fetchImpl=${fetchImpl} notify=${actions.notify}
          from=${agent?.name || request.conv_title || ''} group=${agent?.group || ''} ctx=${online.get(request.agent_id)?.ctx ?? null}
          onMore=${() => actions.openAccessRequest(request.id)} />`;
      })}
      ${view.elsewhere > 0 && html`<${Button} class="phone-btn phone-wide" onClick=${() => actions.openAccessRequest()}>
        ${view.elsewhere} more access request${view.elsewhere === 1 ? '' : 's'} in Mail ›<//>`}
      ${view.decisions.map((decision) => html`<${PhoneDecisionCard} key=${decision.id} decision=${decision} fetchImpl=${fetchImpl}
        notify=${actions.notify} from=${asker(decision)} steward=${!!agents.get(decision.from_agent)?.steward} />`)}
      ${gates.map((item) => html`<${GateCard} key=${item.run.runId} item=${item} source=${processSource} />`)}
      ${view.asking.map((agent) => html`<${AskCard} key=${agent.agentId} agent=${agent} onOpen=${actions.openTerminal} />`)}
    <//>`;
  const rowsOr = (list, empty) => (list.length
    ? html`<${AgentRows} rows=${list} onOpen=${actions.openTerminal} changed=${changed} />`
    : html`<p class="ui-empty phone-empty">${empty}</p>`);
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
    body = html`<${Section} title="Problems" count=${rows.length}>${rowsOr(rows, 'Nothing to look at: no errors, failing checks, full contexts or quiet agents.')}<//>`;
  } else {
    body = html`
    <${UsageStrip} snapshot=${snapshot} onOpen=${() => actions.navigate('/usage')} />
    ${needsYou}
    ${changes.length > 0 && html`<${Section} title="Since you looked" count=${changes.length}
      more=${html`<button data-ui-exempt type="button" class="phone-link" onClick=${markSeen}>Mark seen</button>`}>
      <${AgentRows} rows=${changes} onOpen=${actions.openTerminal} />
    <//>`}
    <${Section} title=${list.all ? 'Online' : 'Active'} count=${rows.length}
      more=${html`<button data-ui-exempt type="button" class="phone-link" onClick=${() => actions.navigate('/')}>All agents ›</button>`}>
      ${rows.length === 0
        ? html`<p class="ui-empty phone-empty">${list.all ? 'No agent is online.' : 'No agent is active right now.'}</p>`
        : html`<${AgentRows} rows=${rows} onOpen=${actions.openTerminal} changed=${changed} />`}
      ${list.total > FEW_ONLINE && html`<button data-ui-exempt type="button" class="phone-now-all" aria-expanded=${showAll ? 'true' : 'false'}
        onClick=${() => setShowAll(!showAll)}>${showAll ? 'Only the recent ones' : `Show all ${list.total} online`}
        <span aria-hidden="true">${showAll ? '⌄' : '›'}</span></button>`}
    <//>
    <${OpenPRs} snapshot=${snapshot} />`;
  }
  return html`<div class="phone-now">
    <${ChipRow} label="Show on Now" class="phone-now-chips" items=${chips} value=${chip} onSelect=${pick} />
    ${body}
  </div>`;
}
