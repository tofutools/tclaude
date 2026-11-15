// phone-agents-island.js -- the phone's Agents view (the Groups tab in the
// phone layout): group chips, then each group's agents with an avatar, a
// state pill and a line of role · harness · context · PR or branch. A tap
// does a row's one thing (an online agent's terminal, an offline one's
// resume); ⋯ opens its sheet with the rest. ＋ in the top bar spawns an agent
// from a short sheet (group, profile, prompt), and a group's "Board ›" opens
// its board. No drag and drop, no columns, no side panel: those stay on the
// desktop.
//
// Every action goes through the desktop's own entry points (the row action
// handler, the message dialog, the spawn dialog, the member editor), so the
// confirmations and transactions are the desktop's; the short spawn sheet
// posts the same spawn route `tclaude agent spawn` does.

import { h, render } from 'preact';
import { useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ALL, agentsTab, agentGroups, findRow, groupTabs, metaParts, shownGroups, spawnBody, spawnChoices } from './phone-agents-model.js';
import { openPhoneSheet } from './phone-sheet.js';
import { usePhoneBar } from './phone-page.js';
import { Avatar, ChipRow, IconButton, StatePill } from './phone-ui.js';
import { openBoardGroup } from './phone-board.js';
import { Button } from './ui-components.js';
import { shellToast } from './shell-state.js';

const html = htm.bind(h);

function Pill({ row }) {
  return html`<${StatePill} tone=${row.pill.tone} class="phone-agent-state">${row.pill.text}<//>`;
}

const CI_MARK = Object.freeze({ passing: '✓', failing: '✗', running: '…' });

function PRLabel({ pr }) {
  return html`<span class=${`phone-agent-pr${pr.ci ? ` ci-${pr.ci}` : ''}`}>PR #${pr.number}${pr.ci ? ` ${CI_MARK[pr.ci]}` : ''}</span>`;
}

// MetaLine is a row's second line (phone-agents-model.js metaParts).
function MetaLine({ row }) {
  const parts = metaParts(row);
  return html`<span class="phone-agent-meta">${parts.map((part, i) => html`${i ? ' · ' : ''}${typeof part === 'string' ? part : html`<${PRLabel} pr=${part.pr} />`}`)}</span>`;
}

// AgentRow is one agent: the row itself does its one thing, ⋯ the rest.
function AgentRow({ row, steward, onTap, onMore }) {
  return html`<li class=${`phone-agent v-${row.variant}${row.online ? '' : ' phone-agent-off'}`}>
    <button data-ui-exempt type="button" class="phone-agent-row" onClick=${() => onTap(row)}
      aria-label=${`${row.name}, ${row.pill.text}${row.detail ? ` (${row.detail})` : ''}: ${row.online ? 'open its terminal' : row.waking ? 'waking' : 'resume it'}`}>
      <${Avatar} bot glyph=${steward ? '✦' : '🤖'} badge=${row.pill.badge} dim=${!row.online} />
      <span class="phone-agent-text">
        <span class="phone-agent-line"><span class="phone-agent-title">${row.name}</span><${Pill} row=${row} /></span>
        <${MetaLine} row=${row} />
        ${row.detail && html`<span class="phone-agent-why">${row.detail}</span>`}
      </span>
    </button>
    <${IconButton} icon="⋯" label=${`${row.name}: more`} aria-haspopup="dialog" class="phone-agent-more" onClick=${() => onMore(row)} />
  </li>`;
}

// SpawnSheet spawns an agent the short way: a group, a profile (the group's
// default unless another is picked), a prompt and maybe a name. Everything
// else is the desktop form's, one tap away.
export function SpawnSheet({ state, group: initial, fetchImpl = globalThis.fetch, openFull, close }) {
  const { groups, profiles } = spawnChoices(state.snapshot.value);
  const [group, setGroup] = useState(groups.some((g) => g.name === initial) ? initial : groups[0]?.name || '');
  const [profile, setProfile] = useState('');
  const [name, setName] = useState('');
  const [prompt, setPrompt] = useState('');
  const [busy, setBusy] = useState(false);
  // A second tap before the re-render must not spawn twice.
  const lock = useRef(false);
  if (!groups.length) return html`<p class="ui-empty phone-empty">Make a group on the desktop first.</p>`;
  const fallback = groups.find((g) => g.name === group)?.profile || '';
  const submit = async (event) => {
    event?.preventDefault?.();
    if (!prompt.trim() || !group || lock.current) return;
    lock.current = true;
    setBusy(true);
    const body = spawnBody({ prompt: prompt.trim(), name, profile });
    try {
      const response = await fetchImpl(`/api/groups/${encodeURIComponent(group)}/spawn`, {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!response.ok) throw new Error((await response.text().catch(() => '')).trim() || `HTTP ${response.status}`);
      const out = await response.json().catch(() => ({}));
      shellToast(out?.queued ? `Queued ${body.name || 'the agent'} in ${group}` : `Spawned ${body.name || 'an agent'} in ${group}`);
      close();
    } catch (cause) {
      shellToast(`Could not spawn: ${cause?.message || cause}`, true);
      lock.current = false;
      setBusy(false);
    }
  };
  return html`<form class="phone-agents-spawn" onSubmit=${submit}>
    <p class="phone-agents-sheet-title">New agent</p>
    <label class="phone-agents-label">Group
      <select class="ui-select phone-agents-field" value=${group} onChange=${(event) => setGroup(event.currentTarget.value)}>
        ${groups.map((g) => html`<option key=${g.name} value=${g.name} selected=${g.name === group}>${g.name}</option>`)}
      </select>
    </label>
    <label class="phone-agents-label">Profile
      <select class="ui-select phone-agents-field" value=${profile} onChange=${(event) => setProfile(event.currentTarget.value)}>
        <option value="" selected=${!profile}>${fallback ? `Group default (${fallback})` : 'No profile'}</option>
        ${profiles.filter((p) => p !== fallback).map((p) => html`<option key=${p} value=${p} selected=${p === profile}>${p}</option>`)}
      </select>
    </label>
    <label class="phone-agents-label">Prompt
      <textarea class="ui-input phone-agents-field" rows="4" placeholder="What should it do?"
        value=${prompt} onInput=${(event) => setPrompt(event.currentTarget.value)}></textarea>
    </label>
    <label class="phone-agents-label">Name
      <input class="ui-input phone-agents-field" placeholder="Optional" autocapitalize="off" autocomplete="off"
        value=${name} onInput=${(event) => setName(event.currentTarget.value)} />
    </label>
    <div class="phone-btns">
      <${Button} class="phone-btn" onClick=${() => { close(); openFull(group); }}>All options…<//>
      <${Button} type="submit" class="phone-btn phone-btn-pri phone-btn-grow" disabled=${busy || !prompt.trim()}>${busy ? 'Spawning…' : 'Spawn'}<//>
    </div>
  </form>`;
}

// AgentSheet is one agent's sheet. It reads the agent live from the snapshot,
// so a state change while it is open shows (and a retired agent says so).
// here: opened from the agent's own terminal, which it then does not offer.
export function AgentSheet({ state, order, actions, agentKey, groupKey, here = false, close }) {
  const found = findRow(agentGroups(state.snapshot.value, order), agentKey, groupKey);
  if (!found) return html`<p class="ui-empty phone-empty">This agent is no longer listed.</p>`;
  const { row, group } = found;
  const run = (act) => () => { close(); act(row, group); };
  const item = (label, onClick, extra = '') => html`<li><button data-ui-exempt type="button" class=${`phone-menu-item${extra}`} onClick=${onClick}>${label}</button></li>`;
  return html`<div class="phone-agent-sheet">
    <div class="phone-agent-line"><h2 class="phone-agent-sheet-name">${row.name}</h2><${Pill} row=${row} /></div>
    <p class="phone-agent-sheet-meta">${[group.group ? group.name : 'No group', row.role, row.harness, row.ctx !== null ? `context ${row.ctx}%` : ''].filter(Boolean).join(' · ')}</p>
    ${row.detail && html`<p class="phone-agent-why">${row.detail}</p>`}
    ${row.pr && html`<p class="phone-agent-sheet-meta"><a href=${row.pr.url} target="_blank" rel="noopener">PR #${row.pr.number}${row.pr.title ? ` · ${row.pr.title}` : ''}</a>${row.pr.ci ? ` · checks ${row.pr.ci}` : ''}</p>`}
    ${!row.pr && row.branch && html`<p class="phone-agent-sheet-meta">⎇ ${row.branch}</p>`}
    ${row.cwd && html`<p class="phone-agent-sheet-meta phone-agent-cwd">${row.cwd}</p>`}
    <ul class="phone-menu">
      ${row.online && !here && item('Open terminal', run(actions.openTerminal))}
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

export function PhoneAgents({ state, order, actions, fetchImpl = globalThis.fetch }) {
  const tab = agentsTab.value;
  const setTab = (key) => { agentsTab.value = key; };
  const snapshot = state.snapshot.value;
  const groups = snapshot ? agentGroups(snapshot, order) : [];
  const tabs = groupTabs(groups);
  const current = tabs.some((t) => t.key === tab) ? tab : ALL;
  const picked = groups.find((g) => g.key === current && g.group);
  // A peer's page (remote-node.js) spawns through its own route and shape,
  // which the desktop form knows; the short sheet is this node's.
  const spawn = (group) => (globalThis.__tclaudeRemoteNode?.id ? actions.spawn(group) : openPhoneSheet({
    label: 'New agent', Body: SpawnSheet,
    props: { state, group, fetchImpl, openFull: (g) => actions.spawn(g) },
  }));
  usePhoneBar({ tab: 'groups', actions: snapshot ? [{ key: 'spawn', icon: '＋', label: 'New agent', onClick: () => spawn(picked?.name || '') }] : [] });
  if (!snapshot) return html`<p class="ui-empty phone-empty">Loading…</p>`;
  if (!groups.length) return html`<p class="ui-empty phone-empty">No agents yet.</p>`;
  const steward = snapshot.steward_active || '';
  const more = (g) => (row) => openPhoneSheet({ label: row.name, Body: AgentSheet, props: { state, order, actions, agentKey: row.key, groupKey: g.key } });
  // A tap: an online agent's terminal; an offline one wakes (the desktop's
  // status light, no confirm: starting a session is harmless); one already
  // waking shows its sheet.
  const tap = (g) => (row) => {
    if (row.online) actions.openTerminal(row);
    else if (row.waking) more(g)(row);
    else actions.rowAction('dot-toggle', row);
  };
  return html`<div class="phone-agents">
    <${ChipRow} label="Group" value=${current} onSelect=${setTab}
      items=${tabs.map((t) => ({ key: t.key, label: t.label, count: t.total, title: `${t.online} of ${t.total} online` }))} />
    ${shownGroups(groups, current).map((g) => html`<div key=${g.key} class="phone-section" role="group" aria-label=${g.name}>
      <h2 class="phone-section-head ui-eyebrow">
        <span>${g.name}</span>
        ${g.group && html`<button data-ui-exempt type="button" class="phone-link phone-agents-board"
          aria-label=${`${g.name}'s board`} onClick=${() => openBoardGroup(g.name)}>Board ›</button>`}
      </h2>
      <ul class="phone-list phone-agents-list">${[...g.shown, ...g.offline].map((row) => html`<${AgentRow} key=${row.key} row=${row}
        steward=${!!steward && (row.agent === steward || row.conv === steward)} onTap=${tap(g)} onMore=${more(g)} />`)}</ul>
    </div>`)}
  </div>`;
}

export function mountPhoneAgentsIsland({ host, state, order, actions, registerCleanup }) {
  render(html`<${PhoneAgents} state=${state} order=${order} actions=${actions} />`, host);
  registerCleanup(() => render(null, host));
}
