// phone-grant.js -- Access › + Grant on a phone (data-layout="phone"): sudo
// granted in two steps, each a page of its own.
//
//   1. the agent: a search over the agents, grouped by group, online ones
//      first (offline ones behind "Show offline"); a tap picks one.
//   2. what, for how long, how often: the temporary-grant warning, the picked
//      permissions as chips, a search with Risky / Owner / Picked chips over a
//      checkbox list, the time limit and the use limit as segments ("…" takes
//      a custom value), a reason, and Grant pinned at the bottom with the
//      limits in its label. Grant stays off until a permission is picked.
//
// The desktop does this in its agent picker and permission editor, whose four
// filter rows, scope drawers and keyboard hints do not fit a phone. This runs
// on the same rows (permissionRows), policy (GET /api/sudo/policy) and grant
// call (POST /api/sudo) as the desktop's; scopes and a Steward's carry stay
// on the desktop.

import { h } from 'preact';
import { useEffect, useMemo, useState } from 'preact/hooks';
import htm from 'htm';
import {
  DEFAULT_SUDO_POLICY, SUDO_DURATION_PRESETS, permissionRows, sudoDurationSeconds, sudoDurationText,
} from './message-access-dialog-model.js';
import { shortAgentId } from './helpers.js';
import { goDuration } from './phone-now-model.js';
import { usePhonePage } from './phone-page.js';
import { ChipRow, Section } from './phone-ui.js';
import { Button, Input, Seg } from './ui-components.js';

const html = htm.bind(h);

// USES_PRESETS are the use limits offered as segments; '' is no limit.
export const USES_PRESETS = Object.freeze(['', '1', '3', '10']);
// ROWS_SHOWN caps the permission list until a search, a filter or Show all:
// the registry is long.
export const ROWS_SHOWN = 40;
const MAX_USES = 1000;

// grantAgents are the agents a grant can go to (those with an agent id), by
// group, each group's online ones first, then by name. An agent in no group
// is under "No group". offline: include the agents that are not online.
export function grantAgents(snapshot, { offline = false, query = '' } = {}) {
  const needle = String(query || '').trim().toLowerCase();
  const agents = (snapshot?.agents || []).filter((agent) => agent.agent_id && (offline || agent.online)
    && (!needle || [agent.title, agent.agent_id, ...(agent.groups || [])].some((value) => String(value || '').toLowerCase().includes(needle))));
  const byGroup = new Map();
  for (const agent of agents) {
    const group = (agent.groups || [])[0] || 'No group';
    if (!byGroup.has(group)) byGroup.set(group, []);
    byGroup.get(group).push(agent);
  }
  const order = (a, b) => (!!b.online - !!a.online) || String(a.title || a.agent_id).localeCompare(String(b.title || b.agent_id));
  return [...byGroup.entries()]
    .sort(([a], [b]) => (a === 'No group') - (b === 'No group') || a.localeCompare(b))
    .map(([group, list]) => ({ group, agents: list.sort(order) }));
}

// usesValid: blank (no limit) or a whole number from 1 to MAX_USES.
export function usesValid(text) {
  const value = String(text ?? '').trim();
  return !value || (/^[1-9]\d*$/.test(value) && Number(value) <= MAX_USES);
}

// noLimitWhy is why a permission takes no use limit, or '' when it can have
// one: as the desktop's editor says it.
export function noLimitWhy(row) {
  if (!row.countable) return row.not_countable_why || 'It has no single action to count.';
  if (row.granted) return 'It already holds this, and what it already holds does not use a count.';
  return '';
}

// grantPlan is what Grant would send, and whether it may: { ready, why,
// label, body }. why says what is missing while it is not ready.
export function grantPlan({ agentID, picked, duration, uses, reason, policy = DEFAULT_SUDO_POLICY }) {
  const seconds = sudoDurationSeconds(duration);
  const usesText = String(uses ?? '').trim();
  const why = !picked.length ? 'Pick at least one permission.'
    : !seconds ? 'Set a time limit, like 20m or 1h30m.'
      : seconds > policy.max_seconds ? `The longest this agent may have is ${goDuration(sudoDurationText(policy.max_seconds))}.`
        : !usesValid(usesText) ? `A use limit is a whole number from 1 to ${MAX_USES}.` : '';
  const counted = usesText ? picked.filter((row) => !noLimitWhy(row)) : [];
  const label = why ? 'Grant' : [`Grant ${picked.length}`, goDuration(sudoDurationText(seconds)),
    ...(usesText ? [`${usesText} use${usesText === '1' ? '' : 's'}`] : [])].join(' · ');
  return {
    ready: !why, why, label,
    body: {
      agentID, slugs: picked.map((row) => row.slug), duration: String(duration).trim(), reason: String(reason || '').trim(),
      ...(counted.length ? { uses: Object.fromEntries(counted.map((row) => [row.slug, Number(usesText)])) } : {}),
    },
  };
}

// AgentStep is step 1: which agent.
function AgentStep({ snapshot, onPick, onBack }) {
  const [query, setQuery] = useState('');
  const [offline, setOffline] = useState(false);
  usePhonePage({ title: 'Grant sudo', onBack, hideNav: true, actions: [] }, true);
  useEffect(() => { globalThis.scrollTo?.(0, 0); }, []);
  const groups = grantAgents(snapshot, { offline, query });
  const hidden = (snapshot?.agents || []).filter((agent) => agent.agent_id && !agent.online).length;
  return html`<div class="phone-grant phone-grant-agents">
    <${Input} class="phone-access-search" type="search" aria-label="Search agents" placeholder="Search agents"
      autocomplete="off" spellcheck=${false} value=${query} onInput=${(event) => setQuery(event.currentTarget.value)} />
    ${!groups.length && html`<p class="ui-empty phone-empty">${query ? 'No agent matches.' : 'No agent is online.'}</p>`}
    ${groups.map(({ group, agents }) => html`<${Section} key=${group} title=${group}>
      <ul class="phone-list">${agents.map((agent) => html`<li key=${agent.agent_id}>
        <button data-ui-exempt type="button" class="phone-grant-agent" data-agent=${agent.agent_id} onClick=${() => onPick(agent)}>
          <span class=${`phone-grant-dot${agent.online ? ' on' : ''}`} role="img" aria-label=${agent.online ? 'online' : 'offline'}></span>
          <span class="phone-access-text">
            <span class="phone-access-name">${agent.title || agent.agent_id}</span>
            <span class="phone-access-sub">${shortAgentId(agent.agent_id, agent.conv_id)}</span>
          </span>
          <span class="phone-grant-more" aria-hidden="true">›</span>
        </button>
      </li>`)}</ul>
    <//>`)}
    ${hidden > 0 && html`<${Button} class="phone-wide" aria-pressed=${offline ? 'true' : 'false'} onClick=${() => setOffline(!offline)}>
      ${offline ? 'Hide' : 'Show'} ${hidden} offline agent${hidden === 1 ? '' : 's'}<//>`}
  </div>`;
}

const FILTERS = [
  { key: 'all', label: 'All' },
  { key: 'risky', label: '⚠ Risky' },
  { key: 'owner', label: '👑 Owner' },
  { key: 'picked', label: 'Picked' },
];

// Limit is a row of segments with "…" for a value of one's own, which opens
// a text field under the row.
function Limit({ label, presets, value, onChange, show, customLabel, placeholder, inputMode = 'text', bad = false }) {
  const [custom, setCustom] = useState(() => !presets.includes(value));
  return html`<div class="phone-grant-limit">
    <h2 class="ui-eyebrow phone-grant-label">${label}</h2>
    <${Seg} class="phone-grant-seg" aria-label=${label}>
      ${presets.map((preset) => html`<${Button} key=${preset || 'none'} pressed=${!custom && value === preset}
        onClick=${() => { setCustom(false); onChange(preset); }}>${show(preset)}<//>`)}
      <${Button} pressed=${custom} aria-label=${customLabel} onClick=${() => setCustom(true)}>…<//>
    <//>
    ${custom && html`<${Input} class="phone-grant-custom" aria-label=${customLabel} placeholder=${placeholder} inputmode=${inputMode}
      autocomplete="off" spellcheck=${false} invalid=${bad} value=${value} onInput=${(event) => onChange(event.currentTarget.value)} />`}
  </div>`;
}

// PermsStep is step 2: what, for how long, how often, why.
function PermsStep({ snapshot, agent, grantActions, onBack, onGranted }) {
  const [policy, setPolicy] = useState(DEFAULT_SUDO_POLICY);
  const [duration, setDuration] = useState(() => sudoDurationText(DEFAULT_SUDO_POLICY.default_seconds));
  const [touched, setTouched] = useState(false);
  const [uses, setUses] = useState('');
  const [reason, setReason] = useState('');
  const [picked, setPicked] = useState(() => new Set());
  const [query, setQuery] = useState('');
  const [filter, setFilter] = useState('all');
  const [all, setAll] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const name = agent.title || agent.agent_id;
  usePhonePage({ title: `Grant to ${name}`, onBack, hideNav: true, actions: [] }, true);
  useEffect(() => { globalThis.scrollTo?.(0, 0); }, []);
  useEffect(() => {
    let live = true;
    grantActions.sudoPolicy?.({ agentID: agent.agent_id }).then((value) => {
      if (!live || !value?.max_seconds) return;
      setPolicy({ ...DEFAULT_SUDO_POLICY, ...value });
    }).catch(() => {});
    return () => { live = false; };
  }, [agent.agent_id]);
  // The policy's default time, unless the operator already chose one.
  useEffect(() => {
    if (!touched && policy.default_seconds) setDuration(sudoDurationText(policy.default_seconds));
  }, [policy]);
  const rows = useMemo(() => permissionRows(snapshot, { mode: 'sudo', agentID: agent.agent_id, conv: agent.conv_id }, {}),
    [snapshot, agent.agent_id, agent.conv_id]);
  const refused = (row) => policy.refused?.[row.slug] || '';
  const pickedRows = rows.filter((row) => picked.has(row.slug) && !refused(row));
  const toggle = (slug) => setPicked((current) => {
    const next = new Set(current);
    if (next.has(slug)) next.delete(slug); else next.add(slug);
    return next;
  });
  const needle = query.trim().toLowerCase();
  const matching = rows.filter((row) => (!needle || [row.slug, row.description, row.descr].some((value) => String(value || '').toLowerCase().includes(needle)))
    && (filter === 'all' || (filter === 'risky' && row.risky) || (filter === 'owner' && row.owner_implied) || (filter === 'picked' && picked.has(row.slug))));
  const shown = all || needle || filter !== 'all' ? matching : matching.slice(0, ROWS_SHOWN);
  const presets = SUDO_DURATION_PRESETS.filter((preset) => sudoDurationSeconds(preset) <= policy.max_seconds);
  const plan = grantPlan({ agentID: agent.agent_id, picked: pickedRows, duration, uses, reason, policy });
  const uncounted = uses.trim() ? pickedRows.filter((row) => noLimitWhy(row)).length : 0;
  const grant = async () => {
    if (!plan.ready || busy) return;
    setBusy(true); setError('');
    try {
      await grantActions.grantSudo(plan.body);
      onGranted();
    } catch (cause) {
      setError(String(cause?.message || cause));
      setBusy(false);
    }
  };
  return html`<div class="phone-grant phone-grant-perms">
    <p class="phone-grant-warn" role="note"><b>Temporary:</b> what you pick applies at once and ends by itself when a limit runs out. Sudo wins over an explicit Deny.</p>
    <h2 class="ui-eyebrow phone-grant-label">Permissions · ${pickedRows.length} picked</h2>
    ${pickedRows.length > 0 && html`<div class="phone-grant-picked">${pickedRows.map((row) => html`<button data-ui-exempt type="button" key=${row.slug}
      class="phone-grant-chip" aria-label=${`Remove ${row.slug}`} onClick=${() => toggle(row.slug)}>${row.slug} <span aria-hidden="true">✕</span></button>`)}</div>`}
    <${Input} class="phone-access-search" type="search" aria-label="Filter permissions" placeholder=${`Filter ${rows.length} permissions`}
      autocomplete="off" spellcheck=${false} value=${query} onInput=${(event) => setQuery(event.currentTarget.value)} />
    <${ChipRow} class="phone-grant-filters" label="Show" value=${filter} onSelect=${setFilter}
      items=${FILTERS.map((item) => ({ ...item, count: item.key === 'picked' ? pickedRows.length : null }))} />
    ${!shown.length && html`<p class="ui-empty phone-empty">${filter === 'picked' ? 'Nothing picked yet.' : 'No permission matches.'}</p>`}
    <ul class="phone-list phone-grant-rows">${shown.map((row) => {
      const why = refused(row);
      const on = picked.has(row.slug) && !why;
      return html`<li key=${row.slug}>
        <label class=${`phone-grant-row${why ? ' refused' : ''}`} data-slug=${row.slug}>
          <input type="checkbox" class="phone-grant-box" checked=${on} disabled=${!!why || busy} onClick=${() => toggle(row.slug)} />
          <span class="phone-access-text">
            <span class="phone-access-name slug">${row.slug}${row.risky ? html` <span class="ui-tag ui-tag-warn">risky</span>` : ''}${row.owner_implied ? html` <span class="ui-tag ui-tag-own">👑 owner</span>` : ''}${row.granted ? html` <span class="ui-tag">has it</span>` : ''}</span>
            <span class="phone-access-desc">${why || row.description || row.descr || ''}</span>
          </span>
        </label>
      </li>`;
    })}</ul>
    ${shown.length < matching.length && html`<${Button} class="phone-wide" onClick=${() => setAll(true)}>Show all ${matching.length} permissions<//>`}
    <${Limit} label="Time limit" presets=${presets} value=${duration} customLabel="Custom time limit" placeholder="like 20m or 1h30m"
      show=${(preset) => goDuration(sudoDurationText(sudoDurationSeconds(preset)))}
      bad=${!!duration.trim() && (!sudoDurationSeconds(duration) || sudoDurationSeconds(duration) > policy.max_seconds)}
      onChange=${(value) => { setTouched(true); setDuration(value); }} />
    <${Limit} label="Max uses" presets=${USES_PRESETS} value=${uses} customLabel="Custom use limit" placeholder=${`1 to ${MAX_USES}`} inputMode="numeric"
      show=${(preset) => preset || 'No limit'} bad=${!usesValid(uses)} onChange=${setUses} />
    ${uncounted > 0 && html`<p class="phone-grant-note">${uncounted} picked ${uncounted === 1 ? 'takes' : 'take'} no use limit, only the time limit.</p>`}
    <h2 class="ui-eyebrow phone-grant-label">Reason</h2>
    <${Input} class="phone-grant-reason" aria-label="Reason" placeholder="optional, kept on the audit row" autocomplete="off"
      value=${reason} onInput=${(event) => setReason(event.currentTarget.value)} />
    ${error && html`<div role="alert" class="ui-error-box">${error}</div>`}
    <div class="phone-grant-bar">
      ${!plan.ready && html`<p class="phone-grant-why">${plan.why}</p>`}
      <${Button} variant="primary" class="phone-wide phone-grant-submit" disabled=${!plan.ready || busy} onClick=${grant}>
        ${busy ? 'Granting…' : plan.label}<//>
    </div>
  </div>`;
}

// PhoneGrant is the whole flow over Access: step 1, then step 2 for the
// agent picked. Back steps back; close() leaves it; granted() runs after a
// grant goes through.
export function PhoneGrant({ snapshot, grantActions, close, granted }) {
  const [agentID, setAgentID] = useState('');
  const agent = agentID ? (snapshot?.agents || []).find((item) => item.agent_id === agentID) : null;
  if (agent) {
    return html`<${PermsStep} key=${agent.agent_id} snapshot=${snapshot} agent=${agent} grantActions=${grantActions}
      onBack=${() => setAgentID('')} onGranted=${granted} />`;
  }
  return html`<${AgentStep} snapshot=${snapshot} onPick=${(picked) => setAgentID(picked.agent_id)} onBack=${close} />`;
}
