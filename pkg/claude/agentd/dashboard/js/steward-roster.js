// The Steward roster: who holds the seat, who stands by, and the one-click
// switch between them, in the Steward window's header. Making any other agent
// the Steward goes through its confirmation (steward-handover-dialog.js); the
// daemon takes these calls from the operator's browser and refuses agents.
import { h } from 'preact';
import { useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { useDismissablePopover } from './hover-popover.js';

const html = htm.bind(h);

async function stewardCall(fetchImpl, path, init = {}) {
  const response = await fetchImpl(path, { credentials: 'same-origin', ...init });
  const result = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(result.message || result.error || `${response.status} ${response.statusText}`);
  return result;
}

function stewardPost(fetchImpl, path, body) {
  return stewardCall(fetchImpl, path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
}

export function loadStewardHandover(fetchImpl, agentId) {
  return stewardCall(fetchImpl, `/api/steward/handover?agent=${encodeURIComponent(agentId)}`);
}

export function handOverSteward(fetchImpl, agentId, then) {
  return stewardPost(fetchImpl, '/api/steward/handover', { agent: agentId, then });
}

export function stepDownSteward(fetchImpl, agentId, then) {
  return stewardPost(fetchImpl, '/api/steward/step-down', { agent: agentId, then });
}

export function removeFromStewardRoster(fetchImpl, agentId) {
  return stewardPost(fetchImpl, '/api/steward/roster/remove', { agent: agentId });
}

// holdsMoreThanSteward reports access the operator has to see before a switch:
// what an agent holds above a Steward's defaults becomes the Steward's.
export function holdsMoreThanSteward(preview) {
  const { standing = [], inherited = [] } = preview?.extra || {};
  return standing.length + inherited.length + (preview?.extra_sudo?.length || 0) > 0;
}

// handoverSummary says in a line what a finished handover did.
export function handoverSummary(outcome) {
  const name = outcome?.name || 'That agent';
  const then = { roster: 'is on the roster', keep: 'stays a normal agent', retire: 'is retired' }[outcome?.then];
  const head = outcome?.previous && then ? `${name} is the Steward. ${outcome.previous} ${then}.` : `${name} is the Steward.`;
  return outcome?.warning ? `${head} ${outcome.warning}` : head;
}

const SEAT_LABEL = { active: 'Steward', alongside: 'also running', standby: 'on the roster' };

// rosterCandidates are the running agents that could take the role.
export function rosterCandidates(agents, roster, filter = '') {
  const listed = new Set(roster.map((entry) => entry.agent_id));
  const needle = filter.trim().toLowerCase();
  return [...agents.values()]
    .filter((agent) => agent.online && agent.harness !== 'shell' && !listed.has(agent.agentId))
    .filter((agent) => !needle || agent.name.toLowerCase().includes(needle))
    .slice(0, 8);
}

export function StewardRoster({ roster, agents, fetchImpl, notify, openHandover, showAgent, openRef = null, trigger = true }) {
  const [open, setOpen] = useState(false);
  if (openRef) openRef.current = () => setOpen(true);
  const [busy, setBusy] = useState('');
  const [filter, setFilter] = useState('');
  const rootRef = useRef(null);
  useDismissablePopover(rootRef, open, () => setOpen(false), { hold: () => !!busy });
  const candidates = useMemo(() => rosterCandidates(agents, roster, filter), [agents, roster, filter]);

  async function run(key, work) {
    setBusy(key);
    try {
      await work();
    } catch (cause) {
      notify(cause?.message || String(cause), true);
    } finally {
      setBusy('');
    }
  }
  const handOverTo = (entry) => { setOpen(false); openHandover(entry.agent_id, entry.name); };
  // Switching back is reversible and shows nothing new, so one click does it. An
  // agent that picked up more access since gets the confirmation that says so.
  const switchTo = (entry) => run(entry.agent_id, async () => {
    const preview = await loadStewardHandover(fetchImpl, entry.agent_id);
    if (preview.refused) throw new Error(preview.refused);
    if (holdsMoreThanSteward(preview)) { handOverTo(entry); return; }
    const outcome = await handOverSteward(fetchImpl, entry.agent_id, 'roster');
    notify(handoverSummary(outcome));
    setOpen(false);
    showAgent?.(entry.agent_id);
  });
  const stepDown = (entry) => run(entry.agent_id, async () => {
    await stepDownSteward(fetchImpl, entry.agent_id, 'roster');
    notify(`${entry.name} stood down and is on the roster.`);
  });
  const remove = (entry) => run(entry.agent_id, async () => {
    await removeFromStewardRoster(fetchImpl, entry.agent_id);
    notify(`${entry.name} is off the roster.`);
  });

  return html`<span ref=${rootRef} class="steward-roster">
    ${trigger && html`<button type="button" class="companion-steward-roster" aria-label="Steward roster" aria-expanded=${open ? 'true' : 'false'}
      title="Steward roster: switch or make another agent the Steward" onClick=${() => setOpen((value) => !value)}>✦ ${roster.length || ''}</button>`}
    ${open && html`<div class="steward-roster-menu" role="dialog" aria-label="Steward roster">
      ${roster.length === 0 && html`<p class="steward-roster-note">No Steward yet.</p>`}
      ${roster.map((entry) => html`<div key=${entry.agent_id} class=${`steward-roster-row ${entry.seat}`}>
        <span class="steward-roster-who"><b>${entry.name}</b><small>${SEAT_LABEL[entry.seat] || entry.seat}${entry.online ? '' : ', offline'}</small></span>
        ${entry.seat === 'active'
          ? html`<button type="button" disabled=${!!busy} title="Remove its Steward access and put it on the roster. It keeps running." onClick=${() => stepDown(entry)}>Stand down</button>`
          : html`<button type="button" class="primary" disabled=${!!busy} title="Make this the active Steward. The current one goes on the roster." onClick=${() => switchTo(entry)}>${busy === entry.agent_id ? 'Switching…' : 'Switch'}</button>`}
        ${entry.seat === 'standby' && html`<button type="button" class="steward-roster-remove" disabled=${!!busy} aria-label=${`Take ${entry.name} off the roster`} title="Take off the roster. It stays a normal agent." onClick=${() => remove(entry)}>×</button>`}
      </div>`)}
      <input type="search" class="steward-roster-filter" placeholder="Make another agent the Steward…" aria-label="Find an agent to make the Steward"
        value=${filter} onInput=${(event) => setFilter(event.currentTarget.value)} />
      ${candidates.map((agent) => html`<button key=${agent.agentId} type="button" class="steward-roster-candidate"
        title=${`Make ${agent.name} the Steward`} onClick=${() => handOverTo({ agent_id: agent.agentId, name: agent.name })}>${agent.name}</button>`)}
    </div>`}
  </span>`;
}
