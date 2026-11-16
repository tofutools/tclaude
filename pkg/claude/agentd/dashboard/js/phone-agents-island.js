// phone-agents-island.js -- the phone's Agents view (the Groups tab in the
// phone layout): group chips that filter (any number of groups, All for
// every one), then each group's agents with an avatar, a
// state pill and a line of role · harness · context · PR or branch. A tap
// does a row's one thing (an online agent's terminal, an offline one's
// resume); a touch and hold, or ⋯, opens its menu with the rest
// (phone-agent-menu.js). ＋ in the top bar spawns an agent
// from a short sheet (group, profile, prompt), and a group's "Board ›" opens
// its board. A link to an agent (/groups?agent=<id>, agent-route.js) opens
// its group up, scrolls to its row and makes it glow a moment (FOUND_MS).
// A tap on a group's header folds it (remembered, phone-agents-
// model.js FOLD_PREF); under the header, folded too, a line counts its agents
// by state. A worker sits indented under the agent that spawned it.
//
// Above them, Active | All groups: Active is the above, All groups every
// group as the desktop's tree nests them (phone-agents-model.js groupsTree),
// subgroups indented on a guide line, a group with nothing online dimmed and
// folded until unfolded, archived ones last behind Show archived. There a tap
// on a group's caret folds it and one on its name opens its page
// (GroupPage): its group actions, its subgroups and its members. No drag
// and drop, no columns, no side panel: those stay on the desktop.
//
// Every action goes through the desktop's own entry points (the row action
// handler, the message dialog, the spawn dialog, the member editor), so the
// confirmations and transactions are the desktop's; the short spawn sheet
// posts the same spawn route `tclaude agent spawn` does.

import { h, render } from 'preact';
import { useEffect, useReducer, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  ALL, ALL_GROUPS, ARCHIVED_PREF, OFFLINE_PREF, VIEW_PREF, showOffline, agentsFilter, agentGroups, agentsView, findAgent, foldPref, groupPage, groupSummaryLine,
  groupTabs, groupsTree, metaParts, shownGroups, shownTree, spawnBody, spawnChoices, toggleFilter, treeFoldPref,
  treeFolded, treeTabs,
} from './phone-agents-model.js';
import { openPhoneSheet } from './phone-sheet.js';
import { AgentMenu } from './phone-agent-menu.js';
import { useHold } from './phone-hold.js';
import { usePhoneBar, usePhonePage } from './phone-page.js';
import { Avatar, ButtonRow, Card, IconButton, Section, StatePill } from './phone-ui.js';
import { ContextMeter } from './context-meter.js';
import { dashPrefs } from './prefs.js';
import { openBoardGroup } from './phone-board.js';
import { Button } from './ui-components.js';
import { shellToast } from './shell-state.js';
import { agentFocus, doneAgentFocus } from './agent-route.js';

// FOUND_MS is how long a found row glows (phone-agents.css
// .phone-agent-found): long enough to spot it after the scroll.
export const FOUND_MS = 3000;

const html = htm.bind(h);

function Pill({ row }) {
  return html`<${StatePill} tone=${row.pill.tone} class="phone-agent-state">${row.pill.text}<//>`;
}

const CI_MARK = Object.freeze({ passing: '✓', failing: '✗', running: '…' });

function PRLabel({ pr }) {
  return html`<span class=${`phone-agent-pr${pr.ci ? ` ci-${pr.ci}` : ''}`}>PR #${pr.number}${pr.ci ? ` ${CI_MARK[pr.ci]}` : ''}</span>`;
}

// MetaLine is a row's second line (phone-agents-model.js metaParts). A
// lead's says how many workers it has right after its role, where a narrow
// phone's ellipsis does not reach.
function MetaLine({ row, kids = 0 }) {
  const parts = metaParts(row);
  if (kids > 0) parts.splice(row.role ? 1 : 0, 0, { kids });
  const part = (p) => (typeof p === 'string' ? p
    : p.kids ? html`<span class="phone-agent-kids">${p.kids} ${p.kids === 1 ? 'worker' : 'workers'}</span>`
      : html`<${PRLabel} pr=${p.pr} />`);
  return html`<span class="phone-agent-meta">${parts.map((p, i) => html`${i ? ' · ' : ''}${part(p)}`)}</span>`;
}

// AgentRow is one agent: the row itself does its one thing; a hold or ⋯ the
// rest (its menu). A
// worker (under) sits indented under the lead that spawned it, last marking
// the end of that lead's workers; a lead says how many it has (kids).
function AgentRow({ row, steward, onTap, onMore, under = null, last = false, kids = 0, found = false }) {
  const { holding, handlers } = useHold(() => onMore(row));
  const cls = ['phone-agent', `v-${row.variant}`, !row.online && 'phone-agent-off', under && 'phone-agent-worker', last && 'phone-agent-last', holding && 'phone-holding', found && 'phone-agent-found'].filter(Boolean).join(' ');
  return html`<li class=${cls}>
    <button data-ui-exempt type="button" class="phone-agent-row phone-holdable" ...${handlers} onClick=${() => onTap(row)}
      aria-label=${`${row.name}, ${row.pill.text}${row.detail ? ` (${row.detail})` : ''}${under ? `, spawned by ${under.name}` : ''}${kids > 0 ? `, ${kids} ${kids === 1 ? 'worker' : 'workers'}` : ''}: ${row.online ? 'open its terminal' : row.waking ? 'waking' : 'resume it'}`}>
      <${Avatar} bot glyph=${steward ? '✦' : '🤖'} badge=${row.pill.badge} dim=${!row.online} />
      <span class="phone-agent-text">
        <span class="phone-agent-line"><span class="phone-agent-title">${row.name}</span><${ContextMeter} state=${row.member?.state || {}} /><${Pill} row=${row} /></span>
        <${MetaLine} row=${row} kids=${kids} />
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
      <select name="ui-select" class="ui-select phone-agents-field" value=${group} onChange=${(event) => setGroup(event.currentTarget.value)}>
        ${groups.map((g) => html`<option key=${g.name} value=${g.name} selected=${g.name === group}>${g.name}</option>`)}
      </select>
    </label>
    <label class="phone-agents-label">Profile
      <select name="ui-select" class="ui-select phone-agents-field" value=${profile} onChange=${(event) => setProfile(event.currentTarget.value)}>
        <option value="" selected=${!profile}>${fallback ? `Group default (${fallback})` : 'No profile'}</option>
        ${profiles.filter((p) => p !== fallback).map((p) => html`<option key=${p} value=${p} selected=${p === profile}>${p}</option>`)}
      </select>
    </label>
    <label class="phone-agents-label">Prompt
      <textarea name="what-should-it-do" class="ui-input phone-agents-field" rows="4" placeholder="What should it do?"
        value=${prompt} onInput=${(event) => setPrompt(event.currentTarget.value)}></textarea>
    </label>
    <label class="phone-agents-label">Name
      <input name="optional" class="ui-input phone-agents-field" placeholder="Optional" autocapitalize="off" autocomplete="off"
        value=${name} onInput=${(event) => setName(event.currentTarget.value)} />
    </label>
    <div class="phone-btns">
      <${Button} class="phone-btn" onClick=${() => { close(); openFull(group); }}>All options…<//>
      <${Button} type="submit" class="phone-btn phone-btn-pri phone-btn-grow" disabled=${busy || !prompt.trim()}>${busy ? 'Spawning…' : 'Spawn'}<//>
    </div>
  </form>`;
}

// GroupChips is the filter row: All, then each group, any number of them
// picked. It has the phone's chip look (phone.css .phone-chips), but its
// chips are toggle buttons, not tabs: tabs pick one.
function GroupChips({ tabs, picked, onToggle }) {
  const chip = (t, on) => html`<button data-ui-exempt key=${t.key} type="button" class=${`ui-tab${on ? ' active' : ''}`}
    aria-pressed=${on ? 'true' : 'false'} title=${`${t.online} of ${t.total} online`} onClick=${() => onToggle(t.key)}>
    ${t.label} <span class="phone-chip-count">${t.total}</span></button>`;
  return html`<div class="phone-chips phone-agents-chips" role="group" aria-label="Show groups">
    ${tabs.map((t) => chip(t, t.key === ALL ? !picked.length : picked.includes(t.key)))}
  </div>`;
}

// summaryText is a group's state summary in words: "1 error, 3 working".
const summaryText = (g) => g.summary.map((s) => `${s.count} ${s.text}`).join(', ');

// Summary is a group's state summary as dots: a dot in each pill's colour and
// its count, loudest first.
function Summary({ g }) {
  if (!g.summary.length) return null;
  return html`<p class="phone-agents-summary" aria-hidden="true">${g.summary.map((s) => html`<span key=${s.text} class=${`phone-agents-sum tone-${s.tone}`}>
    <i class="phone-agents-dot"></i>${s.count} ${s.text}</span>`)}</p>`;
}

// AgentList is a group's agents, each worker under its lead; found is the
// key of the row a link found (it glows), if it is in this group.
function AgentList({ g, isSteward, onTap, onMore, found = null }) {
  return html`<ul class="phone-list phone-agents-list">${g.tree.map(({ row, kids }) => html`
    <${AgentRow} key=${row.key} row=${row} kids=${kids.length} steward=${isSteward(row)} onTap=${onTap} onMore=${onMore} found=${row.key === found} />
    ${kids.map((kid, i) => html`<${AgentRow} key=${kid.key} row=${kid} under=${row} last=${i === kids.length - 1}
      steward=${isSteward(kid)} onTap=${onTap} onMore=${onMore} found=${kid.key === found} />`)}`)}</ul>`;
}

// OfflineToggle is Show offline, beside Active | All groups: a pressed
// button, on (pressed) showing offline agents and nobody-online groups.
function OfflineToggle({ on, onFlip }) {
  return html`<button data-ui-exempt type="button" class=${`phone-agents-offline${on ? ' on' : ''}`} aria-pressed=${on ? 'true' : 'false'}
    aria-label="Show offline" title=${on ? 'Offline agents shown: tap to hide them' : 'Offline agents hidden: tap to show them'} onClick=${onFlip}>
    <span class="phone-agents-offline-box" aria-hidden="true">${on ? '✓' : ''}</span>Offline</button>`;
}

// ViewToggle is Active | All groups: two pressed buttons in a group, each
// with its count (online agents; groups).
function ViewToggle({ view, active, all, onPick }) {
  const seg = (key, label, count) => html`<button data-ui-exempt type="button" class=${`phone-agents-seg-btn${view === key ? ' on' : ''}`}
    aria-pressed=${view === key ? 'true' : 'false'} onClick=${() => onPick(key)}>${label} <span class="phone-chip-count">· ${count}</span></button>`;
  return html`<div class="phone-agents-seg" role="group" aria-label="Show">
    ${seg('active', 'Active', active)}${seg(ALL_GROUPS, 'All groups', all)}
  </div>`;
}

const Tag = ({ children }) => html`<span class="phone-agents-tag">${children}</span>`;

// Chevron is a fold's caret: it points right folded and turns down open
// (phone-agents.css).
const Chevron = () => html`<svg class="phone-agents-chev-icon" viewBox="0 0 16 16" width="16" height="16" aria-hidden="true">
  <path d="M6 3.5 10.5 8 6 12.5" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" /></svg>`;

// GroupHead is a group's header, in Active and All groups alike: the caret,
// a 44px zone left of the name, folds it; the name opens its page (the
// agents in no group have no page: their name folds them too). A group with
// nothing to fold has no caret. Under it, folded too, its state summary.
function GroupHead({ node, folded, onFold, onOpen }) {
  const canFold = node.total > 0 || node.children.length > 0;
  const words = `${node.name}, ${node.live} of ${node.total} online${node.summary.length ? `: ${summaryText(node)}` : ''}${node.empty ? ', empty' : ''}${node.archived ? ', archived' : ''}`;
  const name = html`<span class="phone-agents-name">${node.name}</span><span class="phone-agents-count">${node.live}/${node.total}</span>
    ${node.empty && html`<${Tag}>empty<//>`}${node.archived && html`<${Tag}>archived<//>`}`;
  return html`<h2 class="phone-section-head ui-eyebrow phone-agents-head">
    ${canFold
      ? html`<button data-ui-exempt type="button" class="phone-agents-caret-btn" aria-expanded=${folded ? 'false' : 'true'}
          aria-label=${`${folded ? 'Unfold' : 'Fold'} ${node.name}`} onClick=${onFold}><${Chevron} /></button>`
      : html`<span class="phone-agents-caret-btn" aria-hidden="true"></span>`}
    ${node.group
      ? html`<button data-ui-exempt type="button" class="phone-agents-fold" aria-label=${`${words}. Open its page`} onClick=${onOpen}>${name}</button>`
      : html`<button data-ui-exempt type="button" class="phone-agents-fold" aria-expanded=${folded ? 'false' : 'true'}
          aria-label=${`${words}. ${folded ? 'Unfold' : 'Fold'}`} onClick=${onFold}>${name}</button>`}
    ${node.group && !node.archived && html`<button data-ui-exempt type="button" class="phone-link phone-agents-board"
      aria-label=${`${node.name}'s board`} onClick=${() => openBoardGroup(node.name)}>Board ›</button>`}
  </h2>
  <${Summary} g=${node} />`;
}

// TreeNode is a group in All groups and, unfolded, its agents, then its
// subgroups indented on a guide line.
function TreeNode({ node, prefs, refold, list }) {
  const folded = treeFolded(node, prefs);
  const fold = () => { prefs.setItem(treeFoldPref(node), folded ? '0' : '1'); refold(); };
  return html`<div class=${`phone-section phone-agents-node${folded ? ' phone-agents-folded' : ''}${node.dim ? ' phone-agents-dim' : ''}${node.empty ? ' phone-agents-empty' : ''}`}
    role="group" aria-label=${node.name}>
    <${GroupHead} node=${node} folded=${folded} onFold=${fold} onOpen=${() => list.open(node.name)} />
    ${!folded && node.total > 0 && html`<${AgentList} g=${node} isSteward=${list.isSteward} onTap=${list.tap(node)} onMore=${list.more(node)}
      found=${list.found?.group === node.key ? list.found.key : null} />`}
    ${!folded && node.children.length > 0 && html`<div class="phone-agents-sub">
      ${node.children.map((child) => html`<${TreeNode} key=${child.key} node=${child} prefs=${prefs} refold=${refold} list=${list} />`)}
    </div>`}
  </div>`;
}

// SubgroupCard is a subgroup on a group's page: its name, count and summary,
// a tap opening its own page.
function SubgroupCard({ node, onOpen }) {
  return html`<li><button data-ui-exempt type="button" class=${`phone-agents-subcard${node.dim ? ' phone-agents-dim' : ''}`}
    aria-label=${`${node.name}, ${node.live} of ${node.total} online${node.summary.length ? `: ${summaryText(node)}` : ''}${node.archived ? ', archived' : ''}. Open its page`}
    onClick=${() => onOpen(node.name)}>
    <span class="phone-agents-head phone-agents-subcard-head"><span class="phone-agents-name">${node.name}</span>
      <span class="phone-agents-count">${node.live}/${node.total}</span>${node.empty && html`<${Tag}>empty<//>`}${node.archived && html`<${Tag}>archived<//>`}
      <span class="phone-agents-chev" aria-hidden="true">›</span></span>
    <${Summary} g=${node} />
  </button></li>`;
}

// GroupPage is one group's page: where it sits (the crumbs, each a tap to
// that group's page), a card with what is up and the group's own actions
// (through the desktop's handlers, so its confirmations), its subgroups, then
// its members as the lead/worker tree. An archived group refuses changes, so
// its card offers only its board and mail.
export function GroupPage({ page, actions, list, onOpen, spawn }) {
  const offline = page.total - page.live;
  const name = page.name;
  const buttons = page.archived ? [] : [
    offline > 0 && { key: 'on', label: '⏻ Power on all', tone: 'ok', onClick: () => actions.groupAction('power-on-group', name) },
    page.live > 0 && { key: 'off', label: '⏻ Shut down all', onClick: () => actions.groupAction('shutdown-group', name) },
    { key: 'spawn', label: '＋ Spawn', onClick: () => spawn(name) },
  ].filter(Boolean);
  const links = [
    !page.archived && { key: 'board', label: 'Board ›', onClick: () => openBoardGroup(name) },
    { key: 'mail', label: 'Mail ›', onClick: () => actions.groupAction('view-group-messages', name) },
  ].filter(Boolean);
  return html`<div class="phone-agents phone-group-page">
    ${page.crumbs.length > 1 && html`<div class="phone-agents-crumbs" role="navigation" aria-label="Where this group sits">${page.crumbs.map((crumb, i) => (i === page.crumbs.length - 1
      ? html`<b key=${crumb} aria-current="page">${crumb}</b>`
      : html`<span key=${crumb}><button data-ui-exempt type="button" class="phone-link" onClick=${() => onOpen(crumb)}>${crumb}</button> › </span>`))}</div>`}
    <${Card} class="phone-group-card">
      <p class="phone-agent-sheet-meta">${groupSummaryLine(page)}${page.archived ? ' · archived: unarchive it on the desktop to change it' : ''}</p>
      <${Summary} g=${page} />
      ${buttons.length > 0 && html`<${ButtonRow} buttons=${buttons} />`}
      <${ButtonRow} buttons=${links} />
    <//>
    <${Section} title="Subgroups" count=${page.children.length || null}>
      ${page.children.length
        ? html`<ul class="phone-list phone-agents-subcards">${page.children.map((child) => html`<${SubgroupCard} key=${child.key} node=${child} onOpen=${onOpen} />`)}</ul>`
        : html`<p class="phone-agents-note">None. Nest one from the desktop.</p>`}
    <//>
    <${Section} title="Members" count=${page.total}>
      ${page.total
        ? html`<${AgentList} g=${page} isSteward=${list.isSteward} onTap=${list.tap(page)} onMore=${list.more(page)} />
          ${offline > 0 && html`<p class="phone-agents-note">Tap an offline agent to resume it.</p>`}`
        : html`<p class="phone-agents-note">No agents yet.</p>`}
    <//>
  </div>`;
}

export function PhoneAgents({ state, order, actions, fetchImpl = globalThis.fetch, prefs = dashPrefs, activeTab = state.activeTab }) {
  const [, refold] = useReducer((n) => n + 1, 0);
  // The group pages open, the one on screen last: Back goes to the one before.
  const [opened, setOpened] = useState([]);
  const active = !activeTab || activeTab.value === 'groups';
  useEffect(() => { if (!active) setOpened([]); }, [active]);
  const snapshot = state.snapshot.value;
  const view = agentsView(prefs);
  const showArchived = prefs.getItem(ARCHIVED_PREF) === '1';
  const offline = showOffline(prefs);
  const groups = snapshot ? agentGroups(snapshot, order, { offline }) : [];
  const tree = snapshot ? groupsTree(snapshot, order, { showArchived, offline }) : { nodes: [], archived: 0, count: 0 };
  const allMode = view === ALL_GROUPS;
  const tabs = allMode ? treeTabs(tree.nodes) : groupTabs(groups);
  const picked = (agentsFilter.value || []).filter((key) => tabs.some((t) => t.key === key && key !== ALL));
  const onToggle = (key) => { agentsFilter.value = toggleFilter(picked, key); };
  const only = picked.length === 1 ? tabs.find((t) => t.key === picked[0] && t.key.startsWith('g:')) : null;
  // A peer's page (remote-node.js) spawns through its own route and shape,
  // which the desktop form knows; the short sheet is this node's.
  const spawn = (group) => (globalThis.__tclaudeRemoteNode?.id ? actions.spawn(group) : openPhoneSheet({
    label: 'New agent', Body: SpawnSheet,
    props: { state, group, fetchImpl, openFull: (g) => actions.spawn(g) },
  }));
  const page = snapshot && opened.length ? groupPage(snapshot, opened.at(-1), order) : null;
  const open = (name) => { globalThis.scrollTo?.(0, 0); setOpened((stack) => [...stack, name]); };
  usePhonePage(page ? { title: page.name, onBack: () => setOpened((stack) => stack.slice(0, -1)), actions: [] } : null, active);
  usePhoneBar({ tab: 'groups', actions: snapshot ? [{ key: 'spawn', icon: '＋', label: 'New agent', onClick: () => spawn(only?.label || '') }] : [] }, !page);
  // A link to an agent (agent-route.js): open up what would hide it (a group
  // page over the list, the chips, its group's fold and every one above it,
  // Show offline, Show archived), then glow its row once it is drawn.
  const [found, setFound] = useState(null);
  const ask = agentFocus.value;
  useEffect(() => {
    if (!active || !ask || !snapshot) return;
    const spot = findAgent(snapshot, ask.id, view, order);
    doneAgentFocus();
    if (!spot) {
      shellToast(`No agent ${ask.id} here: it was retired or deleted`, true);
      return;
    }
    setOpened([]);
    if (!spot.row.online) prefs.removeItem(OFFLINE_PREF);
    if (picked.length && !picked.some((key) => spot.path.includes(key))) agentsFilter.value = [];
    if (view === ALL_GROUPS) {
      for (const key of spot.path) prefs.setItem(treeFoldPref({ group: key.startsWith('g:'), name: key.slice(2) }), '0');
      if (spot.archived) prefs.setItem(ARCHIVED_PREF, '1');
    } else {
      prefs.removeItem(foldPref({ group: spot.groupKey.startsWith('g:'), name: spot.groupKey.slice(2) }));
    }
    setFound({ key: spot.row.key, group: spot.groupKey, n: ask.n });
    refold();
  }, [active, ask, snapshot]);
  useEffect(() => {
    if (!found) return undefined;
    const row = globalThis.document?.querySelector('.phone-agents .phone-agent-found');
    const still = globalThis.matchMedia?.('(prefers-reduced-motion: reduce)')?.matches;
    row?.scrollIntoView?.({ block: 'center', behavior: still ? 'auto' : 'smooth' });
    const timer = setTimeout(() => setFound(null), FOUND_MS);
    return () => clearTimeout(timer);
  }, [found]);
  if (!snapshot) return html`<p class="ui-empty phone-empty">Loading…</p>`;
  const steward = snapshot.steward_active || '';
  const isSteward = (row) => !!steward && (row.agent === steward || row.conv === steward);
  const more = (g) => (row) => openPhoneSheet({ label: row.name, Body: AgentMenu, props: { state, order, actions, agentKey: row.key, groupKey: g.key } });
  // A tap: an online agent's terminal; an offline one wakes (the desktop's
  // status light, no confirm: starting a session is harmless); one already
  // waking shows its sheet.
  const tap = (g) => (row) => {
    if (row.online) actions.openTerminal(row);
    else if (row.waking) more(g)(row);
    else actions.rowAction('dot-toggle', row);
  };
  const list = { isSteward, tap, more, open, found };
  if (page) return html`<${GroupPage} key=${page.name} page=${page} actions=${actions} list=${list} onOpen=${open} spawn=${spawn} />`;
  const pick = (key) => {
    if (key === ALL_GROUPS) prefs.setItem(VIEW_PREF, ALL_GROUPS); else prefs.removeItem(VIEW_PREF);
    refold();
  };
  const flipOffline = () => { if (offline) prefs.setItem(OFFLINE_PREF, '0'); else prefs.removeItem(OFFLINE_PREF); refold(); };
  const toggle = html`<div class="phone-agents-top">
    <${ViewToggle} view=${view} active=${groups.reduce((n, g) => n + g.live, 0)} all=${tree.count} onPick=${pick} />
    <${OfflineToggle} on=${offline} onFlip=${flipOffline} />
  </div>`;
  if (!allMode && !groups.length) return html`<div class="phone-agents">${toggle}<p class="ui-empty phone-empty">${offline ? 'No agents yet.' : 'No agents online.'}</p></div>`;
  if (allMode) {
    const flipArchived = () => { if (showArchived) prefs.removeItem(ARCHIVED_PREF); else prefs.setItem(ARCHIVED_PREF, '1'); refold(); };
    return html`<div class="phone-agents">
      ${toggle}
      ${tabs.length > 1 && html`<${GroupChips} tabs=${tabs} picked=${picked} onToggle=${onToggle} />`}
      ${!tree.nodes.length && html`<p class="ui-empty phone-empty">${offline ? 'No groups yet. Make one on the desktop.' : 'No group has anyone online.'}</p>`}
      ${shownTree(tree.nodes, picked).map((node) => html`<${TreeNode} key=${node.key} node=${node} prefs=${prefs} refold=${refold} list=${list} />`)}
      ${tree.archived > 0 && !picked.length && html`<button data-ui-exempt type="button" class="phone-link phone-agents-archived" aria-pressed=${showArchived ? 'true' : 'false'}
        onClick=${flipArchived}>${showArchived ? 'Hide archived' : `Show archived · ${tree.archived}`}</button>`}
    </div>`;
  }
  const isFolded = (g) => prefs.getItem(foldPref(g)) === '1';
  const fold = (g) => () => {
    if (isFolded(g)) prefs.removeItem(foldPref(g));
    else prefs.setItem(foldPref(g), '1');
    refold();
  };
  return html`<div class="phone-agents">
    ${toggle}
    <${GroupChips} tabs=${tabs} picked=${picked} onToggle=${onToggle} />
    ${shownGroups(groups, picked).map((g) => {
      const folded = isFolded(g);
      return html`<div key=${g.key} class=${`phone-section${folded ? ' phone-agents-folded' : ''}`} role="group" aria-label=${g.name}>
        <${GroupHead} node=${{ ...g, children: [], empty: false, archived: false }} folded=${folded} onFold=${fold(g)} onOpen=${() => open(g.name)} />
        ${!folded && html`<${AgentList} g=${g} isSteward=${isSteward} onTap=${tap(g)} onMore=${more(g)} found=${found?.group === g.key ? found.key : null} />`}
      </div>`;
    })}
  </div>`;
}

export function mountPhoneAgentsIsland({ host, state, order, actions, registerCleanup }) {
  render(html`<${PhoneAgents} state=${state} order=${order} actions=${actions} />`, host);
  registerCleanup(() => render(null, host));
}
