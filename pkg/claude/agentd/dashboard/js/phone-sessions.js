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
import { openShellMenu } from './phone-now-shells.js';
import { Avatar, Card, RowMore, Section, StatePill } from './phone-ui.js';

const html = htm.bind(h);

// openSessions is collabItems' sections with only the open sessions: each
// kept section with its terminals and running shells (a group's or an
// agent's shell is a resource of its group, not a terminal item) in the
// panel's order, and no empty section.
const isSession = (item) => item.kind === 'terminal' || item.kind === 'shell';
export function openSessions(sections) {
  return (sections || [])
    .map((section) => ({ ...section, items: section.items.filter((item) => isSession(item) && (item.bucket || bucketOf(item.view)) !== 'closed') }))
    .filter((section) => section.items.length > 0);
}

// sessionKind is what a row says it is, and its avatar glyph ('' draws the
// name's initial).
export function sessionKind(item) {
  if (item.kind === 'shell') return { glyph: '›', word: 'shell' };
  const kind = item.row?.kind;
  if (kind === 'shell' || kind === 'group_shell') return { glyph: '›', word: 'shell' };
  if (kind === 'session') return { glyph: '', word: 'session' };
  return { glyph: '', word: 'agent' };
}

// SessionRow is one open session; a local agent's row opens its menu
// (phone-agent-menu.js) on a hold, or its ⋯, and a shell's its own
// (phone-now-shells.js ShellMenu), as Now's Shells do.
function SessionRow({ item, onOpen, onMenu, onShellMenu }) {
  const kind = sessionKind(item);
  const agent = onMenu ? railAgent(item) : null;
  const menu = kind.word === 'shell' ? (onShellMenu ? () => onShellMenu(item) : null)
    : agent?.agent_id ? () => onMenu({ agentId: agent.agent_id, name: item.title }) : null;
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
  </button>${menu && html`<${RowMore} label=${`${item.title}: more`} class="phone-session-more" onClick=${menu} />`}</li>`;
}

// PhoneSessions is the list. ctx (actions, snapshot, notify) is what
// openRailItem takes; listed and open are injectable for tests.
const panelSections = (panes, snapshot) => collabItems(panes, snapshot).sections;
export function PhoneSessions({ panes = [], actions, snapshot, notify = () => {}, listed = panelSections, open = openRailItem, onMenu = agentMenuOpener(),
  shellMenu = openShellMenu }) {
  const sections = openSessions(listed(panes, snapshot));
  const onOpen = (item) => open(item, { actions, snapshot, notify });
  const onShellMenu = (item) => shellMenu(item, { open: onOpen, notify });
  if (!sections.length) {
    return html`<div class="phone-sessions">
      <${Card} class="phone-sessions-empty"><p>Nothing is open.</p><p>An agent's session shows here while it runs.</p><//>
    </div>`;
  }
  return html`<div class="phone-sessions">
    ${sections.map((section) => html`<${Section} key=${section.id} title=${section.name || 'Other'} count=${section.items.length}>
      <ul class="phone-list">${section.items.map((item) => html`<${SessionRow} key=${item.key} item=${item} onOpen=${onOpen} onMenu=${onMenu} onShellMenu=${onShellMenu} />`)}</ul>
    <//>`)}
  </div>`;
}
