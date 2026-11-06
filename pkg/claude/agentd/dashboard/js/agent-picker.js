// agent-picker.js — the one agent picker (tcl-fmd0ne). The message, access
// and cron dialogs each had their own copy of the pick-an-agent dialog and of
// the solo / group target picker; they share these now, on FilterCombobox, so
// a node with hundreds of agents gets the same ranked, capped, filter-as-you-
// type list everywhere instead of one long flat list or <select>.
//
//   agentPickerItems   agents as FilterCombobox items
//   AgentPickerDialog  the 🔍 dialog: pick one agent, or close for none
//   AgentCombobox      an in-form single choice over a given agent list
//   TargetPicker       solo agent or group (multicast) target of a message
//                      or job

import { Fragment, h } from 'preact';
import { useEffect, useMemo, useState } from 'preact/hooks';
import htm from 'htm';
import { Button, Select } from './ui-components.js';
import { Dialog } from './ui-dialog.js';
import { FilterCombobox } from './filter-combobox.js';
import { agentCandidates, groupMembers, groupsForPicker } from './message-access-dialog-model.js';
import { idTooltip, shortAgentId } from './helpers.js';
import { wizWord } from './slop.js';

const html = htm.bind(h);

// PICKER_CAP is how many agents a picker lists at once; past it, nothing
// typed shows the groups to narrow by, and a query shows its best matches.
export const PICKER_CAP = 30;

function Words({ plain, wizard }) {
  return html`<span class="theme-copy-regular">${plain}</span><span class="theme-copy-wizard">${wizard}</span>`;
}

function useLiveTheme() {
  const [, setRevision] = useState(0);
  useEffect(() => {
    const refresh = () => setRevision((value) => value + 1);
    document.addEventListener('tclaude:wizard', refresh);
    return () => document.removeEventListener('tclaude:wizard', refresh);
  }, []);
}

const agentKey = (agent) => agent.agent_id || agent.key || agent.conv_id;

// agentPickerItem is one agent (a snapshot agent, or a group member) as a
// FilterCombobox item. It sits under its first group, and is found by its
// title, ids, groups, roles and descriptions. identity 'conv' keys it by its
// conversation id, else by its agent id; sudo marks active sudo grants.
export function agentPickerItem(agent, { identity = 'agent', sudo = null, group = '' } = {}) {
  const memberships = agent.memberships || [];
  const groups = [...new Set([...memberships.map((item) => item.group), ...(agent.groups || []), group].filter(Boolean))];
  const key = identity === 'conv' ? agent.conv_id : agentKey(agent);
  const grants = sudo?.get(agentKey(agent))?.length || 0;
  return {
    key,
    label: agent.title || '(unnamed)',
    hint: shortAgentId(agent.agent_id || agent.key, agent.conv_id),
    group: groups[0] || '',
    keywords: [agent.agent_id, agent.conv_id, ...groups, ...memberships.flatMap((item) => [item.role, item.descr])].filter(Boolean).join(' '),
    // A list that does not say who is online (an {id, label} choice) shows no dot.
    lead: agent.online === undefined ? null : html`<span class=${agent.online ? 'online' : 'offline'} title=${agent.online ? 'online' : 'offline'}>${agent.online ? '●' : '○'}</span>`,
    tail: html`${grants ? html`<span class="sudo-badge" title=${`${grants} active sudo grant(s)`}>🔓</span>` : null}${groups.length
      ? html`<span class="groups-tag" title=${idTooltip(agent.agent_id || agent.key, agent.conv_id)}>in: ${groups.join(', ')}</span>` : null}`,
  };
}

// choiceAgents turns { id, label } (or { id, title }) choices, the shape
// several dialogs list agents in, into the agents AgentCombobox takes.
export function choiceAgents(choices = []) {
  return choices.map((choice) => ({ agent_id: choice.id, title: choice.label ?? choice.title ?? choice.id, online: choice.online }));
}

// agentPickerItems are the snapshot's agents a picker offers: the online
// ones, or every one with includeOffline.
export function agentPickerItems(snapshot, { includeOffline = false, identity = 'agent', sudo = null } = {}) {
  return agentCandidates(snapshot, { includeOffline }).map((agent) => agentPickerItem(agent, { identity, sudo }));
}

// AgentPickerDialog is the 🔍 dialog. onChoose gets the picked agent's id (its
// conversation id for descriptor.identity 'conv'); onClose means none.
//   descriptor: { title, identity?, showSudo?, includeOfflineHint? }
//   sudo: the active sudo grants by agent, shown when descriptor.showSudo
export function AgentPickerDialog({ descriptor, snapshot, onChoose, onClose, confirmDiscard, sudo = null }) {
  const [includeOffline, setIncludeOffline] = useState(false);
  const items = useMemo(() => agentPickerItems(snapshot, {
    includeOffline, identity: descriptor.identity, sudo: descriptor.showSudo ? sudo : null,
  }), [snapshot, includeOffline, descriptor.identity, descriptor.showSudo, sudo]);
  return html`<${Dialog} id="cron-pick-target-modal" dialogClass="add-member-modal" onClose=${onClose}
    dirty=${false} blocked=${false} confirmDiscard=${confirmDiscard} title=${html`${descriptor.title} <span class="muted"><${Words} plain="— pick agent" wizard="— pick hero"/></span>`}>
    <div class="agent-picker">
      <${FilterCombobox} id="cron-pick-target-list" inputId="cron-pick-target-search" items=${items} value="" multi=${false}
        alwaysOpen autoFocus cap=${PICKER_CAP} ariaLabel="Filter agents"
        placeholder="Filter by title / role / descr / conv-id / group…"
        emptyHint=${`No matching conversations. ${includeOffline ? '(Try a different filter.)' : '(Try ticking “Include offline / archived” for a wider pool.)'}`}
        onChange=${(key) => { if (key) onChoose(key); }} />
    </div>
    <div class="add-member-foot">
      <label title=${descriptor.includeOfflineHint || 'Include offline / archived agents'}>
        <input id="cron-pick-target-all" type="checkbox" checked=${includeOffline}
          onChange=${(event) => setIncludeOffline(event.currentTarget.checked)} />Include offline / archived
      </label>
      <span class="spacer"></span><span><kbd>↑↓</kbd> nav · <kbd>Enter</kbd> pick · <kbd>Esc</kbd> close</span>
    </div>
  </${Dialog}>`;
}

// AgentCombobox is an in-form choice of one agent from agents (snapshot
// agents or group members): the chosen one shows above the filter box, with
// × to clear it. A value no agent has stays, marked missing. group names the
// group agents are members of, when they do not say (a group's members).
//   ariaLabel, disabled: the box's; attrs: more attributes on the wrapper
//   (a data-* hook the surrounding view finds it by)
export function AgentCombobox({
  id, value, onChange, agents, group = '', placeholder = 'Type to find an agent…', emptyHint = 'No agents.',
  ariaLabel = 'Find an agent', disabled = false, attrs = {},
}) {
  const items = useMemo(() => agents.map((agent) => agentPickerItem(agent, { group })), [agents, group]);
  const chosen = value ? items.find((item) => item.key === value) : null;
  // A click on the chosen agent must not reach a <label> around the form
  // row either: it would hand it to the row's first radio (a standing
  // order's "Everyone"), changing the target.
  return html`<div ...${attrs} class="agent-combobox" id=${id} data-value=${value || ''} onClick=${(event) => event.preventDefault()}>
    ${value && html`<div class="agent-combobox-chosen">
      ${chosen ? html`${chosen.lead}<span class="rowname">${chosen.label}</span><span class="id">${chosen.hint}</span>`
        : html`<span class="rowname">${value}</span><span class="muted">(missing)</span>`}
      ${!disabled && html`<${Button} size="sm" variant="ghost" class="agent-combobox-clear" title="Clear" aria-label="Clear the agent" onClick=${() => onChange('')}>×<//>`}
    </div>`}
    <${FilterCombobox} id=${id ? `${id}-list` : undefined} inputId=${id ? `${id}-input` : undefined} items=${items} value=${value || ''} multi=${false}
      cap=${PICKER_CAP} ariaLabel=${ariaLabel} disabled=${disabled} placeholder=${value ? 'Type to change…' : placeholder} emptyHint=${emptyHint}
      onChange=${(key) => onChange(key)} />
  </div>`;
}

// TargetPicker is a message's or a job's target: a solo agent (typed, or
// picked with 🔍) or a group to multicast to. A scoped one (value.scopeGroup)
// picks among that group's members, and cannot leave the group.
//   prefix      the ids' prefix (`${prefix}-target`, `${prefix}-group`, …)
//   onPick      the 🔍 button: opens AgentPickerDialog
//   groupLocked why group mode is unavailable (shown on its 🔒), or ''
//   groupOnly   only a group target fits (a worker spawn's)
//   LockReason  renders groupLocked's reason
export function TargetPicker({ prefix, value, onChange, snapshot, onPick, groupLocked = '', groupOnly = false, LockReason = null }) {
  useLiveTheme();
  const scope = value.scopeGroup || '';
  const groups = groupsForPicker(snapshot, scope);
  const members = useMemo(() => (scope ? groupMembers(snapshot, scope) : []), [snapshot, scope]);
  const groupOptions = value.groupName && !groups.includes(value.groupName)
    ? [value.groupName, ...groups] : groups;
  const modeName = `${prefix}-target-mode`;
  const setMode = (mode) => onChange({ ...value, mode });
  return html`<div class="cron-create-target" id=${`${prefix}-target-picker`}>
    <div class="cron-target-modes">
      ${groupOnly ? html`<span class="muted">Group target required for worker spawns</span>` : html`<${Fragment}>
      <label><input type="radio" name=${modeName} value="solo" checked=${value.mode === 'solo'}
        onChange=${() => setMode('solo')} /> <${Words} plain="Solo agent" wizard="Solo hero"/></label>
      <label class=${groupLocked ? 'locked-field' : ''}><input type="radio" name=${modeName} value="group" checked=${value.mode === 'group'}
        disabled=${!!groupLocked} onChange=${() => setMode('group')} /> <${Words} plain="Group (multicast)" wizard="Party (multicast)"/></label>${LockReason && html`<${LockReason} why=${groupLocked}/>`}
      </${Fragment}>`}
    </div>
    ${value.mode === 'solo' && !scope && html`<div class="cron-target-input-row" id=${`${prefix}-target-solo`}>
      <input id=${`${prefix}-target`} type="text" value=${value.target}
        placeholder="agt_ id / title / conv-id / 8+-char prefix" autocomplete="off" spellcheck=${false}
        onInput=${(event) => onChange({ ...value, target: event.currentTarget.value })} />
      <${Button} id=${`${prefix}-target-pick`} title="Pick from the agent / hero list" onClick=${onPick}>🔍<//>
    </div>`}
    ${value.mode === 'solo' && scope && html`<div class="cron-target-input-row" id=${`${prefix}-target-scoped`}>
      <${AgentCombobox} id=${`${prefix}-scoped-member`} value=${value.target} agents=${members} group=${scope}
        placeholder=${wizWord('Type to find a member…', 'Type to find a hero…')}
        emptyHint=${wizWord('No members in this group.', 'No heroes in this party.')}
        onChange=${(target) => onChange({ ...value, target })} />
    </div>`}
    ${value.mode === 'group' && html`<div class="cron-target-input-row" id=${`${prefix}-target-group`}>
      <${Select} id=${`${prefix}-group`} value=${value.groupName} disabled=${!!scope}
        onChange=${(event) => onChange({ ...value, groupName: event.currentTarget.value })}>
        ${groupOptions.length
          ? html`<${Fragment}><option value="">${wizWord('(pick a group)', '(pick a party)')}</option>${groupOptions.map((name) => html`<option key=${name} value=${name}>${name}${groups.includes(name) ? '' : ' (missing)'}</option>`)}</${Fragment}>`
          : html`<option value="">${wizWord('(no groups — create one first)', '(no parties — form one first)')}</option>`}
      <//>
    </div>`}
  </div>`;
}
