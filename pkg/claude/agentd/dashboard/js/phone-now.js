// phone-now.js -- the phone shell's Now view, in the shape of the Now mock.
// A row of chips picks what it shows: All (below), Needs you (the cards
// only), Idle (online idle agents by group), Recent (every online agent, the
// latest heard from first) and Problems (one go-look list: errors, CI failed,
// at work but quiet). All also says what changed since this viewer last
// looked. A row's context meter sits by its state pill (context-meter.js):
// a full context is no problem. All is a one-line usage strip that opens Usage, then what needs the operator as
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
import { FEW_ONLINE, PROBLEMS, agentRows, askerName, hookAt, idleGroups, contextHigh, lookedState, nowView, problemRows, recentRows, rowState, sinceLooked } from './phone-shell-model.js';
import { usageStrip } from './phone-now-model.js';
import { AskCard, PhoneApprovalCard, PhoneDecisionCard } from './phone-approval.js';
import { ContextMeter } from './context-meter.js';
import { Button } from './ui-components.js';
import { Card, ChipRow, IconButton, Section, StatePill } from './phone-ui.js';
import { useHold } from './phone-hold.js';
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
// looked, and the agent's context meter sits by its state pill.
// NowRow is one agent's row: a tap opens its terminal; a hold, or its ⋯,
// opens its menu (phone-agent-menu.js) when onMenu is wired.
function NowRow({ agent, state, when, tone, changed, onOpen, onMenu }) {
  const { holding, handlers } = useHold(onMenu ? () => onMenu(agent) : null);
  return html`<div class=${`phone-now-agent${holding ? ' phone-holding' : ''}`}>
    <button data-ui-exempt type="button" class="phone-row phone-holdable" ...${handlers} onClick=${() => onOpen(agent)}
      aria-label=${`${agent.name}${changed ? ' (changed since you looked)' : ''}, ${state}${contextHigh(agent) ? `, context ${agent.ctx}% full` : ''}${when ? `, ${when}` : ''}: open its terminal`}>
      ${changed && html`<span class="phone-now-new" aria-hidden="true"></span>`}
      <span class="phone-row-text"><span class="phone-who-name">${agent.name}</span>${agent.group && html`<span class="phone-who-sub">${agent.group}</span>`}
        ${state === 'error' && agent.detail && tone === undefined && html`<span class="phone-card-line">${agent.detail}</span>`}
        ${when && html`<span class=${`phone-now-when${tone ? ` ${tone}` : ''}`}>${when}</span>`}</span>
      ${agent.ctxState && html`<${ContextMeter} state=${agent.ctxState} />`}<${StatePill} state=${state} />
    </button>
    ${onMenu && html`<${IconButton} icon="⋯" label=${`${agent.name}: more`} aria-haspopup="dialog" class="phone-now-more" onClick=${() => onMenu(agent)} />`}
  </div>`;
}

function AgentRows({ rows, onOpen, onMenu = null, changed = null }) {
  return html`<${Card} class="phone-rows-card">
    <ul class="phone-rows">
      ${rows.map(({ agent, state, when, tone, head }) => html`<li key=${agent.agentId}>
        ${head && html`<p class="ui-eyebrow phone-now-group">${head}</p>`}
        <${NowRow} agent=${agent} state=${state} when=${when} tone=${tone} changed=${!!changed?.has(agent.agentId)} onOpen=${onOpen} onMenu=${onMenu} />
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
// (or hiding the page) once what changed has been on screen LOOK_MS, so a
// glance, or a chip that does not show the changes, does not clear them.
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
const PROBLEM_TONE = Object.freeze({ error: 'bad', ci: 'bad', quiet: 'quiet' });

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
  // latest is what Now shows now, for the look to keep when it moves;
  // marks says which changes are on screen (All, under Needs you), '' when
  // none; seenSince is when they came into view on a visible page.
  const latest = useRef(null);
  const marks = useRef('');
  const pending = useRef(0);
  const seenSince = useRef(null);
  const visible = () => globalThis.document?.visibilityState !== 'hidden';
  // After each render: the marks' time on screen starts when they show, and
  // again when a new change joins them or the page comes back.
  const shown = useRef('');
  useEffect(() => {
    if (!marks.current || !visible()) { seenSince.current = null; shown.current = ''; return; }
    if (marks.current !== shown.current) { seenSince.current = Date.now(); shown.current = marks.current; }
  });
  useEffect(() => {
    // The look moves past changes only once they have been on screen
    // LOOK_MS; with none waiting, keeping what shows loses nothing.
    const leave = () => {
      if (!latest.current) return false;
      const seen = seenSince.current !== null && Date.now() - seenSince.current >= LOOK_MS;
      if (pending.current > 0 && !seen) return false;
      storeLooked(latest.current);
      return true;
    };
    const onVisibility = () => {
      if (!visible()) { if (leave()) setLooked(latest.current); seenSince.current = null; shown.current = ''; return; }
      if (marks.current) { seenSince.current = Date.now(); shown.current = marks.current; }
    };
    globalThis.document?.addEventListener('visibilitychange', onVisibility);
    return () => { globalThis.document?.removeEventListener('visibilitychange', onVisibility); leave(); };
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
  pending.current = changes.length;
  marks.current = chip === 'all' ? changes.map((row) => `${row.agent.agentId}:${row.when}`).join('|') : '';
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
    ? html`<${AgentRows} rows=${list} onOpen=${actions.openTerminal} onMenu=${actions.agentMenu} changed=${changed} />`
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
    body = html`<${Section} title="Problems" count=${rows.length}>${rowsOr(rows, 'Nothing to look at: no errors, failing checks or quiet agents.')}<//>`;
  } else {
    body = html`
    <${UsageStrip} snapshot=${snapshot} onOpen=${() => actions.navigate('/usage')} />
    ${needsYou}
    ${changes.length > 0 && html`<${Section} title="Since you looked" count=${changes.length}
      more=${html`<button data-ui-exempt type="button" class="phone-link" onClick=${markSeen}>Mark seen</button>`}>
      <${AgentRows} rows=${changes} onOpen=${actions.openTerminal} onMenu=${actions.agentMenu} />
    <//>`}
    <${Section} title=${list.all ? 'Online' : 'Active'} count=${rows.length}
      more=${html`<button data-ui-exempt type="button" class="phone-link" onClick=${() => actions.navigate('/')}>All agents ›</button>`}>
      ${rows.length === 0
        ? html`<p class="ui-empty phone-empty">${list.all ? 'No agent is online.' : 'No agent is active right now.'}</p>`
        : html`<${AgentRows} rows=${rows} onOpen=${actions.openTerminal} onMenu=${actions.agentMenu} changed=${changed} />`}
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
