// phone-sessions.js -- the Collab tab on a phone: a plain list of what is open,
// in place of the desktop's strip, rail and empty-strip home (collab-home.js).
//
// It lists the sessions the Collab panel lists (collab-rail-actions.js
// collabItems), by group: the agents, sessions and shells running. A tap opens
// one the way the panel does (openRailItem), and a phone docks its terminal
// full screen (phone-terminal-island.js). Drawing boards, processes and pages
// are left out: the panel opens them on the desktop's tab strip, which a phone
// does not have.

import { h } from 'preact';
import htm from 'htm';
import { bucketOf, needsYou } from './collab-rail-model.js';
import { collabItems, openRailItem, railAgent } from './collab-rail-actions.js';
import { agentMenuOpener, useHold } from './phone-hold.js';
import { Avatar, Card, IconButton, Section, StatePill } from './phone-ui.js';

const html = htm.bind(h);

// openSessions is collabItems' sections with only the open sessions: each
// kept section with its terminals in the panel's order, and no empty section.
export function openSessions(sections) {
  return (sections || [])
    .map((section) => ({ ...section, items: section.items.filter((item) => item.kind === 'terminal' && (item.bucket || bucketOf(item.view)) !== 'closed') }))
    .filter((section) => section.items.length > 0);
}

// sessionKind is what a row says it is, and its avatar glyph ('' draws the
// name's initial).
export function sessionKind(item) {
  const kind = item.row?.kind;
  if (kind === 'shell' || kind === 'group_shell') return { glyph: '›', word: 'shell' };
  if (kind === 'session') return { glyph: '', word: 'session' };
  return { glyph: '', word: 'agent' };
}

// SessionRow is one open session; a local agent's row opens its menu
// (phone-agent-menu.js) on a hold, or its ⋯.
function SessionRow({ item, onOpen, onMenu }) {
  const kind = sessionKind(item);
  const agent = onMenu ? railAgent(item) : null;
  const menu = agent?.agent_id ? () => onMenu({ agentId: agent.agent_id, name: item.title }) : null;
  const { holding, handlers } = useHold(menu);
  const waiting = needsYou(item);
  const state = item.status?.label || '';
  const sub = [kind.word, item.row?.node?.name].filter(Boolean).join(' · ');
  return html`<li class=${`phone-session-item${holding ? ' phone-holding' : ''}`}><button data-ui-exempt type="button"
    class=${`phone-session${menu ? ' phone-holdable' : ''}`} ...${handlers} onClick=${() => onOpen(item)}
    aria-label=${[item.title, waiting ? 'needs you' : state, kind.word].filter(Boolean).join(', ')}>
    <${Avatar} name=${item.title} glyph=${kind.glyph} bot=${!!kind.glyph} />
    <span class="phone-session-text">
      <span class="phone-session-title">${item.title}</span>
      <span class="phone-session-sub">${sub}</span>
    </span>
    ${waiting ? html`<${StatePill} state="ask">needs you<//>` : state && html`<${StatePill} state=${state} />`}
    ${!menu && html`<span class="phone-session-chevron" aria-hidden="true">›</span>`}
  </button>${menu && html`<${IconButton} icon="⋯" label=${`${item.title}: more`} aria-haspopup="dialog" class="phone-session-more" onClick=${menu} />`}</li>`;
}

// PhoneSessions is the list. ctx (actions, snapshot, notify) is what
// openRailItem takes; listed and open are injectable for tests.
const panelSections = (panes, snapshot) => collabItems(panes, snapshot).sections;
export function PhoneSessions({ panes = [], actions, snapshot, notify = () => {}, listed = panelSections, open = openRailItem, onMenu = agentMenuOpener() }) {
  const sections = openSessions(listed(panes, snapshot));
  const onOpen = (item) => open(item, { actions, snapshot, notify });
  if (!sections.length) {
    return html`<div class="phone-sessions">
      <${Card} class="phone-sessions-empty"><p>Nothing is open.</p><p>An agent's session shows here while it runs.</p><//>
    </div>`;
  }
  return html`<div class="phone-sessions">
    ${sections.map((section) => html`<${Section} key=${section.id} title=${section.name || 'Other'} count=${section.items.length}>
      <ul class="phone-list">${section.items.map((item) => html`<${SessionRow} key=${item.key} item=${item} onOpen=${onOpen} onMenu=${onMenu} />`)}</ul>
    <//>`)}
  </div>`;
}
