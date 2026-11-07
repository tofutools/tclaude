// collab-rail-gear.js -- a Collab panel row's ⚙ edit menu (tcl-usrm7h): what
// the Groups tab offers the same thing, reused rather than copied. An agent
// gets its Groups row ⚙ (AgentGearMenu, with the group the panel lists it
// under), plus Move to group, which goes through the Groups tab's drop
// operations as a drag onto that group would (collab-rail-actions.js). A
// drawing board or page gets its Groups row ⋯ items: Rename…, Move to group ›,
// Delete… (groups-resources.js' resourceMenu). The row's ⋯ keeps what it
// had: open, download, kill.

import { h } from 'preact';
import htm from 'htm';
import { AgentGearMenu } from './groups-member-table.js';
import { RowMenu, resourceMenu } from './groups-resources.js';
import { MenuItem, MenuSep } from './ui-components.js';
import { railAgent, railItemToGroup, railResource } from './collab-rail-actions.js';
import { processContext, processRunApi } from './process-context.js';
import { shellConfirm, shellToast } from './shell-state.js';

const html = htm.bind(h);

// railGearKind is which ⚙ item gets: 'agent', 'resource' or none ('').
export function railGearKind(item) {
  if (railAgent(item)) return 'agent';
  const resource = railResource(item);
  // A page the panel lists with no group row (open here only) has no owner
  // to say where it may go; a drawing board needs none.
  if (resource && (resource.kind === 'drawing_board' || item.resource)) return 'resource';
  return '';
}

// agentMoveTargets are the groups an agent listed under from may move to,
// and '' (Not in a group) when it is in one.
export function agentMoveTargets(snapshotValue, from = '') {
  const names = (snapshotValue?.groups || []).filter((g) => g?.id > 0 && g.name && g.name !== from).map((g) => g.name);
  return from ? [...names, ''] : names;
}

function AgentGear({ item, snapshotValue, notify, ctx }) {
  const agent = railAgent(item);
  const group = item.group ? (snapshotValue?.groups || []).find((g) => g.name === item.group) || null : null;
  // The group's member row carries what the menu reads (owner, notify, …).
  const member = (group?.members || []).find((m) => m.conv_id === agent.conv_id || (agent.agent_id && m.agent_id === agent.agent_id)) || agent;
  const targets = agentMoveTargets(snapshotValue, item.group || '');
  const move = (to) => (event) => {
    event.preventDefault();
    void railItemToGroup(item, { to, agent }, { notify, ...ctx });
  };
  // First: moving is what the panel's ⚙ is for; the Groups row items follow.
  const extra = targets.length ? html`${targets.map((to) => html`<${MenuItem} key=${`move:${to}`} data-rail-move-to=${to}
    title=${to ? `Move ${item.title} to ${to}` : `Take ${item.title} out of ${item.group}`}
    onClick=${move(to)}>${to ? `move to ${to}` : 'move to Not in a group'}<//>`)}<${MenuSep} />` : null;
  // Clicks must reach the document: the menu's data-act items are handled
  // there (row-actions.js), as on a Groups row.
  return html`<span class="cr-gear row-actions" draggable="false">
    <${AgentGearMenu} member=${member} group=${group} snapshot=${snapshotValue} menuKey=${`rail:${item.key}`} extra=${extra} />
  </span>`;
}

function ResourceGear({ item, snapshotValue, notify, onRename, ctx }) {
  const resource = railResource(item);
  const groupId = (snapshotValue?.groups || []).find((g) => g.name === item.group)?.id || 0;
  const items = resourceMenu(resource, {
    groupId, snapshotValue, api: ctx.api || processRunApi, source: ctx.source || processContext, notify,
    confirm: ctx.confirm || shellConfirm, startRename: onRename,
  });
  return html`<span class="cr-gear" draggable="false">
    <${RowMenu} label=${item.title} items=${items} glyph="⚙" title=${`Edit ${item.title}`} buttonClass="cr-tool cr-gear-btn" />
  </span>`;
}

// RailGear is item's ⚙, or nothing. onRename shows the row's name field.
//   ctx: { api, source, confirm, groupOps } (tests pass their own).
export function RailGear({ item, snapshot, notify = shellToast, onRename, ctx = {} }) {
  const kind = railGearKind(item);
  if (!kind) return null;
  const snapshotValue = snapshot?.value;
  return kind === 'agent'
    ? html`<${AgentGear} item=${item} snapshotValue=${snapshotValue} notify=${notify} ctx=${ctx} />`
    : html`<${ResourceGear} item=${item} snapshotValue=${snapshotValue} notify=${notify} onRename=${onRename} ctx=${ctx} />`;
}
