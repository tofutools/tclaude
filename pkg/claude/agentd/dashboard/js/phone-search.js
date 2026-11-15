// phone-search.js -- the phone shell's 🔍 search: one sheet that finds agents,
// groups, the operator's mail and board tasks by name as the operator types,
// and opens what they tap (the agent's terminal, the group in Agents, the
// message in Mail, the task).
//
// searchHits is the model: plain data in, ranked hits out, scored like the
// desktop command palette (palette-score.js scoreMatch). SearchSheet renders it
// inside a phone sheet (phone-sheet.js).

import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { companionAgents } from './companion-model.js';
import { scoreMatch } from './palette-score.js';
import { agentsTab, groupKey } from './phone-agents-model.js';
import { taskPath } from './phone-shell-model.js';
import { openHumanNotifications } from './mail-bridge.js';
import { Input } from './ui-components.js';
import { Avatar, Section, StatePill } from './phone-ui.js';

const html = htm.bind(h);

// SEARCH_LIMIT is how many hits each kind shows.
export const SEARCH_LIMIT = 5;

const fold = (text) => String(text || '').toLowerCase();

// rank scores items against q and keeps the matches, best first (ties keep
// their input order), with how many matched in all.
function rank(items, q, tokens, label, hay) {
  const scored = [];
  items.forEach((item, index) => {
    const name = fold(label(item));
    const score = scoreMatch(q, tokens, name, `${name} ${fold(hay(item))}`);
    if (score > 0) scored.push({ item, score, index });
  });
  scored.sort((a, b) => b.score - a.score || a.index - b.index);
  return { total: scored.length, items: scored.slice(0, SEARCH_LIMIT).map((s) => s.item) };
}

// boardTasks lists every task on the boards the taskboard context holds, with
// its group.
function boardTasks(board) {
  const out = [];
  for (const entry of board?.groups || []) {
    for (const task of entry?.tasks || []) out.push({ ...task, group: entry.group });
  }
  return out;
}

// searchHits finds query in the snapshot's agents, groups and mail and in the
// board's tasks. An empty query finds nothing.
export function searchHits(query, { snapshot, board } = {}) {
  const q = fold(query).trim().replace(/\s+/g, ' ');
  const empty = { total: 0, items: [] };
  if (!q) return { query: '', agents: empty, groups: empty, mail: empty, tasks: empty };
  const tokens = q.split(' ');
  const agents = [...companionAgents(snapshot).values()];
  const groups = (snapshot?.groups || []).filter((g) => g?.name).map((g) => ({
    name: g.name,
    members: (g.members || []).length,
    online: (g.members || []).filter((m) => m?.online === true).length,
  }));
  return {
    query: q,
    // Not by group: a group has its own hits, and would list all its agents.
    agents: rank(agents, q, tokens, (a) => a.name, (a) => a.harness),
    groups: rank(groups, q, tokens, (g) => g.name, () => ''),
    // A message opens in Mail through its sender's folder: one without a
    // sender could not be opened, so it is not offered.
    mail: rank((snapshot?.messages || []).filter((m) => m?.from_agent || m?.from_conv), q, tokens, (m) => m.subject || m.body, (m) => `${m.from_title} ${m.group} ${m.body}`),
    tasks: rank(boardTasks(board), q, tokens, (t) => t.title, (t) => `${t.id} ${t.group}`),
  };
}

// agentState is the word an agent's pill shows: what it waits on, else its
// status, else idle; offline when it is not running.
const WAITING = Object.freeze({ awaiting_permission: 'approval', awaiting_input: 'asking' });
export function agentState(agent) {
  if (!agent.online) return 'offline';
  return WAITING[agent.status] || agent.status || 'idle';
}

const sender = (message) => message.from_title || message.from_agent || message.from_conv || 'agent';

function Hit({ onClick, lead, title, sub = '', tail = null }) {
  return html`<li><button data-ui-exempt type="button" class="phone-search-hit" onClick=${onClick}>
    ${lead}
    <span class="phone-search-text">
      <span class="phone-search-title">${title}</span>
      ${sub && html`<span class="phone-search-sub">${sub}</span>`}
    </span>
    ${tail}
  </button></li>`;
}

function Kind({ title, found, children }) {
  if (!found.total) return null;
  return html`<${Section} title=${title} count=${found.total}>
    <ul class="phone-list phone-search-list">${children}</ul>
  <//>`;
}

// SearchSheet is the search sheet's body. actions are the shell's view
// actions (each leaves the shell's own view first); boardSource is the
// taskboard context, attached while the sheet is open so tasks are found too.
export function SearchSheet({ state, actions, boardSource = null, close }) {
  const [query, setQuery] = useState('');
  useEffect(() => boardSource?.attach?.(), [boardSource]);
  const hits = searchHits(query, { snapshot: state.snapshot.value, board: boardSource?.context?.value });
  const go = (run) => () => { close(); run(); };
  const none = hits.query && !hits.agents.total && !hits.groups.total && !hits.mail.total && !hits.tasks.total;
  return html`<div class="phone-search">
    <${Input} type="search" class="phone-search-input" placeholder="Search agents, groups, mail, tasks" aria-label="Search"
      value=${query} onInput=${(event) => setQuery(event.currentTarget.value)} autocomplete="off" enterkeyhint="search" />
    ${!hits.query && html`<p class="ui-empty phone-empty">Find an agent, a group, a message or a task by name.</p>`}
    ${none && html`<p class="ui-empty phone-empty">Nothing matches “${query.trim()}”.</p>`}
    <${Kind} title="Agents" found=${hits.agents}>
      ${hits.agents.items.map((agent) => html`<${Hit} key=${agent.agentId} onClick=${go(() => actions.openTerminal(agent))}
        lead=${html`<${Avatar} bot glyph=${agent.steward ? '✦' : '🤖'} dim=${!agent.online} />`}
        title=${agent.name} sub=${[agent.group, agent.harness].filter(Boolean).join(' · ')}
        tail=${html`<${StatePill} state=${agentState(agent)} />`} />`)}
    <//>
    <${Kind} title="Groups" found=${hits.groups}>
      ${hits.groups.items.map((group) => html`<${Hit} key=${group.name}
        onClick=${go(() => { agentsTab.value = groupKey(group.name); actions.showTab('groups'); })}
        lead=${html`<${Avatar} name=${group.name} />`} title=${group.name} sub=${`${group.online}/${group.members} online`} />`)}
    <//>
    <${Kind} title="Mail" found=${hits.mail}>
      ${hits.mail.items.map((message) => html`<${Hit} key=${message.id}
        onClick=${go(() => { actions.showTab('messages'); openHumanNotifications(message.from_agent || message.from_conv, message.id); })}
        lead=${html`<${Avatar} name=${sender(message)} />`} title=${message.subject || message.body}
        sub=${[sender(message), message.group].filter(Boolean).join(' · ')} />`)}
    <//>
    <${Kind} title="Tasks" found=${hits.tasks}>
      ${hits.tasks.items.map((task) => html`<${Hit} key=${`${task.group}:${task.id}`} onClick=${go(() => actions.navigate(taskPath(task, task.group)))}
        lead=${html`<${Avatar} bot glyph="▤" />`} title=${task.title} sub=${task.group} />`)}
    <//>
  </div>`;
}
