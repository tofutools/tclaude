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
// Every group as the desktop's tree nests them (phone-agents-model.js
// agentsLayout): subgroups indented on a guide line, a worker under the
// agent that spawned it, a group with nothing online dimmed and folded until
// unfolded. A tap on a group's header folds it (remembered, TREE_FOLD), as
// a tap on an agent does its one thing; a touch and hold, or its ⋯, opens
// its page (GroupPage): its group actions, its subgroups and its members. Under a header, folded too, a line counts its
// agents by state. The sliders in the top bar open the View sheet
// (ViewSheet): Tree or List, Show offline / empty / archived, the order of
// the groups, and Reset; the sliders carry a dot when any is off its
// default. No drag and drop, no columns, no side panel: those stay on the
// desktop.
//
// Every action goes through the desktop's own entry points (the row action
// handler, the message dialog, the spawn dialog, the member editor), so the
// confirmations and transactions are the desktop's; the short spawn sheet
// posts the same spawn route `tclaude agent spawn` does.

import { h, render } from 'preact';
import { useEffect, useReducer, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  ALL, LAYOUTS, ORDERS, VIEW_DEFAULTS, TREE_FOLD, agentsFilter, agentsLayout, atDefaults, findAgent, groupPage, groupSummaryLine,
  metaParts, saveViewSettings, shownTree, spawnBody, spawnChoices, toggleFilter, treeTabs, viewSettings,
} from './phone-agents-model.js';
import { openPhoneSheet } from './phone-sheet.js';
import { DiscardAsk, useDraftGuard } from './phone-draft-guard.js';
import { AgentMenu } from './phone-agent-menu.js';
import { useHold } from './phone-hold.js';
import { usePhoneBar, usePhonePage } from './phone-page.js';
import { Avatar, ButtonRow, Card, ChipRow, Empty, RowMore, Section, SheetHead, SheetTitle, StatePill, ViewIcon } from './phone-ui.js';
import { ContextMeter } from './context-meter.js';
import { dashPrefs } from './prefs.js';
import { openBoardGroup } from './phone-board.js';
import { Check, Switch } from './ui-components.js';
import { shellToast } from './shell-state.js';
import { agentFocus, doneAgentFocus } from './agent-route.js';
import { groupShellOffer, openGroupShell } from './phone-open-shell.js';

// FOUND_MS is how long a found row glows (phone-agents.css
// .phone-agent-found): long enough to spot it after the scroll.
export const FOUND_MS = 3000;

const html = htm.bind(h);

function Pill({ row }) {
  return html`<${StatePill} tone=${row.pill.tone} class="phone-agent-state">${row.pill.text}<//>`;
}

const CI_MARK = Object.freeze({ passing: '✓', failing: '✗', running: '…', incomplete: '?' });

function PRLabel({ pr }) {
  return html`<span class=${`phone-agent-pr${pr.ci ? ` ci-${pr.ci}` : ''}`} title=${pr.ciNote || undefined}>PR #${pr.number}${pr.ci ? ` ${CI_MARK[pr.ci]}` : ''}</span>`;
}

// MetaLine is a row's second line (phone-agents-model.js metaParts). A
// lead's says how many workers it has right after its role, where a narrow
// phone's ellipsis does not reach.
// The owner tag is the desktop member table's (ui-tag-own owner-badge), and
// sits right after the role, as it does there.
function MetaLine({ row, kids = 0 }) {
  const parts = metaParts(row);
  // The worker count goes after the role and its owner tag.
  const lead = (row.role && !(row.owner && row.role === 'owner') ? 1 : 0) + (row.owner ? 1 : 0);
  if (kids > 0) parts.splice(lead, 0, { kids });
  const part = (p) => (typeof p === 'string' ? p
    : p.owner ? html`<span class="ui-tag ui-tag-own owner-badge" title=${`Owner of ${row.group}`}>owner</span>`
      : p.kids ? html`<span class="phone-agent-kids">${p.kids} ${p.kids === 1 ? 'worker' : 'workers'}</span>`
        : html`<${PRLabel} pr=${p.pr} />`);
  return html`<span class="phone-agent-meta">${parts.map((p, i) => html`${i && !p.owner ? ' · ' : ''}${part(p)}`)}</span>`;
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
      aria-label=${`${row.name}, ${row.pill.text}${row.detail ? ` (${row.detail})` : ''}${row.owner ? `, owner of ${row.group}` : ''}${under ? `, spawned by ${under.name}` : ''}${kids > 0 ? `, ${kids} ${kids === 1 ? 'worker' : 'workers'}` : ''}: ${row.online ? 'open its terminal' : row.waking ? 'waking' : 'resume it'}`}>
      <${Avatar} bot glyph=${steward ? '✦' : '🤖'} badge=${row.pill.badge} dim=${!row.online} />
      <span class="phone-agent-text">
        <span class="phone-agent-line"><span class="phone-agent-title">${row.name}</span><${ContextMeter} state=${row.member?.state || {}} /><${Pill} row=${row} /></span>
        <${MetaLine} row=${row} kids=${kids} />
        ${row.detail && html`<span class="phone-agent-why">${row.detail}</span>`}
      </span>
    </button>
    <${RowMore} label=${`${row.name}: more`} class="phone-agent-more" onClick=${() => onMore(row)} />
  </li>`;
}

// openSpawnSheet opens the short spawn sheet on group ('' for the group
// list's first), or, on a peer's page (remote-node.js), which spawns through
// its own route and shape, the desktop form (remoteSpawn). The Agents top
// bar's ＋, a group header's ＋ Agent, a group page's ＋ Spawn and Now's
// ＋ Agent all open it.
export function openSpawnSheet({ state, group = '', fetchImpl = globalThis.fetch, openFull, remoteSpawn }) {
  // A peer's form needs a group to spawn into: with none given, the first
  // the sheet would have started on.
  if (globalThis.__tclaudeRemoteNode?.id) return remoteSpawn(group || spawnChoices(state.snapshot.peek?.() ?? state.snapshot.value).groups[0]?.name || '');
  return openPhoneSheet({ label: 'New agent', Body: SpawnSheet, props: { state, group, fetchImpl, openFull } });
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
  // A prompt written and not spawned is asked about before Back drops it;
  // All options… takes it along, so it closes without asking.
  // With no group left the form is gone (the empty state below): nothing to
  // ask about, so the sheet closes.
  const form = useRef(null);
  const guard = useDraftGuard(groups.length > 0 && !!prompt.trim(), busy,
    () => form.current?.querySelector('textarea'));
  if (!groups.length) return html`<${Empty}>Make a group on the desktop first.<//>`;
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
  return html`<form ref=${form} class="phone-agents-spawn" onSubmit=${submit}>
    <${SheetTitle}>New agent<//>
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
    ${guard.asking
      ? html`<${DiscardAsk} what="this prompt" guard=${guard} close=${close} />`
      : html`<${ButtonRow} buttons=${[
          { key: 'all', label: 'All options…', grow: false, onClick: () => { close(); openFull(group, { initialMessage: prompt.trim(), name: name.trim(), profileName: profile }); } },
          { key: 'spawn', label: busy ? 'Spawning…' : 'Spawn', tone: 'pri', type: 'submit', disabled: busy || !prompt.trim() },
        ]} />`}
  </form>`;
}

// GroupChips is the filter row: All, then each group, any number of them
// picked, so its chips are toggles (ChipRow toggle), not tabs: tabs pick one.
function GroupChips({ tabs, picked, onToggle }) {
  return html`<${ChipRow} toggle class="phone-agents-chips" label="Show groups" onSelect=${onToggle}
    items=${tabs.map((t) => ({ key: t.key, label: t.label, count: t.total, title: `${t.online} of ${t.total} online`,
      pressed: t.key === ALL ? !picked.length : picked.includes(t.key) }))} />`;
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

const LAYOUT_LABELS = Object.freeze({ tree: 'Tree', list: 'List' });
const ORDER_LABELS = Object.freeze({
  yours: ['Your group order', 'as on the desktop'],
  needs: ['Needs you first', 'errors, approvals, questions'],
  active: ['Most active', 'working first'],
  name: ['Name', 'A–Z'],
});

// ViewSheet is the Agents view's settings (phone-agents-model.js
// viewSettings): Layout, what to Show, how to Order groups, and Reset (off
// at the defaults). Each change applies at once and is kept per viewer.
export function ViewSheet({ prefs, onChange }) {
  const [settings, setSettings] = useState(() => viewSettings(prefs));
  const set = (patch) => {
    const next = { ...settings, ...patch };
    saveViewSettings(prefs, next);
    setSettings(next);
    onChange?.();
  };
  const show = (key, label, hint = '') => html`<label class="phone-agents-view-switch">
    <span class="phone-agents-view-text">${label}${hint && html`<small>${hint}</small>`}</span>
    <${Switch} name=${`agents-view-${key}`} checked=${settings[key]} onChange=${(event) => set({ [key]: event.currentTarget.checked })} />
  </label>`;
  return html`<div class="phone-agents-view" data-agents-view>
    <div class="phone-agents-view-head"><${SheetTitle}>View<//>
      <button data-ui-exempt type="button" class="phone-link phone-agents-view-reset" disabled=${atDefaults(settings)}
        onClick=${() => set({ ...VIEW_DEFAULTS })}>Reset</button></div>
    <${SheetHead}>Layout<//>
    <div class="phone-agents-seg" role="group" aria-label="Layout">${LAYOUTS.map((key) => html`<button data-ui-exempt key=${key} type="button"
      class=${`phone-agents-seg-btn${settings.layout === key ? ' on' : ''}`} aria-pressed=${settings.layout === key ? 'true' : 'false'}
      onClick=${() => set({ layout: key })}>${LAYOUT_LABELS[key]}</button>`)}</div>
    <p class="ui-hint phone-agents-view-hint">Tree nests subgroups and puts workers under their lead. List is one level.</p>
    <${SheetHead}>Show<//>
    ${show('offline', 'Offline agents', 'and groups with nobody online')}
    ${show('empty', 'Empty groups')}
    ${show('archived', 'Archived groups')}
    <${SheetHead}>Order groups by<//>
    <div role="radiogroup" aria-label="Order groups by">${ORDERS.map((key) => html`<${Check} key=${key} type="radio" name="agents-view-order"
      labelClass="phone-agents-view-order" value=${key} checked=${settings.order === key} onChange=${() => set({ order: key })}>
      ${ORDER_LABELS[key][0]} <small>${ORDER_LABELS[key][1]}</small><//>`)}</div>
  </div>`;
}

const Tag = ({ children }) => html`<span class="phone-agents-tag">${children}</span>`;

// Chevron is a fold's caret: it points right folded and turns down open
// (phone-agents.css).
const Chevron = () => html`<svg class="phone-agents-chev-icon" viewBox="0 0 16 16" width="16" height="16" aria-hidden="true">
  <path d="M6 3.5 10.5 8 6 12.5" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" /></svg>`;

// GroupHead is a group's header (tk_3aa23770). A tap anywhere on it (the
// caret, the name, the counts, the space after) folds or unfolds it, as a tap
// on an agent row does its one thing; a touch and hold, or its ⋯, opens the
// group's page (its menu), through the shared useHold as an agent row's menu.
// ＋ Agent and Board › stay their own taps. The agents in no group have no
// page; a group with nothing to fold opens its page on a tap. Under it,
// folded too, its state summary.
function GroupHead({ node, folded, onFold, onOpen, onSpawn }) {
  const canFold = node.total > 0 || (node.children || []).length > 0;
  const openPage = node.group ? () => onOpen() : null;
  const { holding, handlers } = useHold(openPage);
  const tap = canFold ? onFold : openPage;
  const words = `${node.name}, ${node.live} of ${node.total} online${node.summary.length ? `: ${summaryText(node)}` : ''}${node.empty ? ', empty' : ''}${node.archived ? ', archived' : ''}`;
  const name = html`<span class="phone-agents-name">${node.name}</span><span class="phone-agents-count">${node.live}/${node.total}</span>
    ${node.empty && html`<${Tag}>empty<//>`}${node.archived && html`<${Tag}>archived<//>`}`;
  return html`<h2 class=${`phone-section-head ui-eyebrow phone-agents-head${holding ? ' phone-holding' : ''}`}>
    ${canFold
      ? html`<button data-ui-exempt type="button" class="phone-agents-caret-btn phone-holdable" aria-expanded=${folded ? 'false' : 'true'}
          aria-label=${`${folded ? 'Unfold' : 'Fold'} ${node.name}`} aria-hidden="true" tabindex="-1" ...${handlers} onClick=${onFold}><${Chevron} /></button>`
      : html`<span class="phone-agents-caret-btn" aria-hidden="true"></span>`}
    <button data-ui-exempt type="button" class="phone-agents-fold phone-holdable" ...${handlers}
      aria-expanded=${canFold ? (folded ? 'false' : 'true') : undefined}
      aria-label=${`${words}. ${canFold ? (folded ? 'Unfold' : 'Fold') : 'Open its page'}`} onClick=${tap}>${name}</button>
    ${node.group && !node.archived && onSpawn && html`<button data-ui-exempt type="button" class="phone-link phone-agents-spawn-link" data-agent-add
      aria-label=${`New agent in ${node.name}`} aria-haspopup="dialog" onClick=${() => onSpawn(node.name)}>＋ Agent</button>`}
    ${node.group && !node.archived && html`<button data-ui-exempt type="button" class="phone-link phone-agents-board"
      aria-label=${`${node.name}'s board`} onClick=${() => openBoardGroup(node.name)}>Board ›</button>`}
    ${openPage && html`<${RowMore} label=${`Group ${node.name}: more`} popup=${null} class="phone-agents-more" onClick=${openPage} />`}
  </h2>
  <${Summary} g=${node} />`;
}

// AgentGroupsList is the one layout of groups and their agents: each group
// a section (GroupHead, then, unfolded, its agents as AgentList has them,
// then its subgroups indented on a guide line), one look and one spacing.
// nodes is what agentsLayout made of the View settings (the tree, or one
// level); fold is TREE_FOLD; list is the row actions and the found row
// (PhoneAgents).
export function AgentGroupsList({ nodes, fold, prefs, refold, list }) {
  return nodes.map((node) => html`<${GroupSection} key=${node.key} node=${node} fold=${fold} prefs=${prefs} refold=${refold} list=${list} />`);
}

function GroupSection({ node, fold, prefs, refold, list }) {
  const folded = fold.folded(node, prefs.getItem(fold.pref(node)));
  const flip = () => { fold.remember(prefs, node, !folded); refold(); };
  const children = node.children || [];
  return html`<div class=${`phone-section phone-agents-node${folded ? ' phone-agents-folded' : ''}${node.dim ? ' phone-agents-dim' : ''}${node.empty ? ' phone-agents-empty' : ''}`}
    role="group" aria-label=${node.name}>
    <${GroupHead} node=${node} folded=${folded} onFold=${flip} onOpen=${() => list.open(node.name)} onSpawn=${list.spawn} />
    ${!folded && node.total > 0 && html`<${AgentList} g=${node} isSteward=${list.isSteward} onTap=${list.tap(node)} onMore=${list.more(node)}
      found=${list.found?.group === node.key ? list.found.key : null} />`}
    ${!folded && children.length > 0 && html`<div class="phone-agents-sub">
      <${AgentGroupsList} nodes=${children} fold=${fold} prefs=${prefs} refold=${refold} list=${list} />
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
export function GroupPage({ page, actions, list, onOpen, spawn, snapshot = null, openShell = openGroupShell }) {
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
  // A shell of the operator's own in the group's default directory; greyed,
  // with why, where it has none.
  const shell = groupShellOffer(snapshot, { ...page.group, name, archived: page.archived });
  return html`<div class="phone-agents phone-group-page">
    ${page.crumbs.length > 1 && html`<div class="phone-agents-crumbs" role="navigation" aria-label="Where this group sits">${page.crumbs.map((crumb, i) => (i === page.crumbs.length - 1
      ? html`<b key=${crumb} aria-current="page">${crumb}</b>`
      : html`<span key=${crumb}><button data-ui-exempt type="button" class="phone-link" onClick=${() => onOpen(crumb)}>${crumb}</button> › </span>`))}</div>`}
    <${Card} class="phone-group-card">
      <p class="phone-agent-sheet-meta">${groupSummaryLine(page)}${page.archived ? ' · archived: unarchive it on the desktop to change it' : ''}</p>
      <${Summary} g=${page} />
      ${buttons.length > 0 && html`<${ButtonRow} buttons=${buttons} />`}
      <${ButtonRow} buttons=${links} />
      ${shell && html`<${ButtonRow} class="phone-group-shell" buttons=${[{
        key: 'shell', label: '›_ Shell', disabled: !shell.dir, title: shell.dir ? `A shell in ${shell.dir}` : shell.why,
        ariaLabel: shell.dir ? `Open a shell in ${shell.dir}` : 'Shell: this group has no default directory', onClick: () => openShell(name),
      }]} />
        <p class="phone-group-shell-where" data-group-shell=${shell.dir ? 'dir' : 'none'}>${shell.dir ? `In ${shell.dir}` : shell.why}</p>`}
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
  const settings = viewSettings(prefs);
  const nodes = snapshot ? agentsLayout(snapshot, order, settings) : [];
  const tabs = treeTabs(nodes);
  const picked = (agentsFilter.value || []).filter((key) => tabs.some((t) => t.key === key && key !== ALL));
  const onToggle = (key) => { agentsFilter.value = toggleFilter(picked, key); };
  const only = picked.length === 1 ? tabs.find((t) => t.key === picked[0] && t.key.startsWith('g:')) : null;
  const spawn = (group) => openSpawnSheet({ state, group, fetchImpl, openFull: (g, typed) => actions.spawn(g, typed), remoteSpawn: actions.spawn });
  const changed = !atDefaults(settings);
  const openView = () => openPhoneSheet({ label: 'View', Body: ViewSheet, props: { prefs, onChange: refold } });
  const page = snapshot && opened.length ? groupPage(snapshot, opened.at(-1), order) : null;
  const open = (name) => { globalThis.scrollTo?.(0, 0); setOpened((stack) => [...stack, name]); };
  usePhonePage(page ? { title: page.name, onBack: () => setOpened((stack) => stack.slice(0, -1)), actions: [] } : null, active);
  usePhoneBar({ tab: 'groups', actions: snapshot ? [
    { key: 'spawn', icon: '＋', label: 'New agent', onClick: () => spawn(only?.label || '') },
    { key: 'view', icon: ViewIcon, label: changed ? 'View: changed from the defaults' : 'View', dot: changed, onClick: openView },
  ] : [] }, !page);
  // A link to an agent (agent-route.js): open up what would hide it (a group
  // page over the list, the chips, its group's fold and every one above it,
  // Show offline, Show archived), then glow its row once it is drawn.
  const [found, setFound] = useState(null);
  const ask = agentFocus.value;
  useEffect(() => {
    if (!active || !ask || !snapshot) return;
    const spot = findAgent(snapshot, ask.id, order);
    doneAgentFocus();
    if (!spot) {
      shellToast(`No agent ${ask.id} here: it was retired or deleted`, true);
      return;
    }
    setOpened([]);
    // What would hide it: Show offline, Show archived, an empty group never.
    const shown = { ...settings, offline: settings.offline || !spot.row.online, archived: settings.archived || spot.archived };
    if (shown.offline !== settings.offline || shown.archived !== settings.archived) saveViewSettings(prefs, shown);
    if (picked.length && !picked.some((key) => spot.path.includes(key))) agentsFilter.value = [];
    for (const key of spot.path) TREE_FOLD.remember(prefs, { group: key.startsWith('g:'), name: key.slice(2) }, false);
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
  if (!snapshot) return html`<${Empty}>Loading…<//>`;
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
  const list = { isSteward, tap, more, open, found, spawn };
  if (page) return html`<${GroupPage} key=${page.name} page=${page} actions=${actions} list=${list} onOpen=${open} spawn=${spawn} snapshot=${snapshot} />`;
  const none = !(snapshot.groups || []).length && !(snapshot.ungrouped || []).length;
  const empty = none ? 'No groups yet. Make one on the desktop.' : 'Nothing to show here. View (the sliders above) shows more.';
  return html`<div class="phone-agents">
    ${tabs.length > 1 && html`<${GroupChips} tabs=${tabs} picked=${picked} onToggle=${onToggle} />`}
    ${!nodes.length && html`<${Empty}>${empty}<//>`}
    <${AgentGroupsList} nodes=${shownTree(nodes, picked)} fold=${TREE_FOLD} prefs=${prefs} refold=${refold} list=${list} />
  </div>`;
}

export function mountPhoneAgentsIsland({ host, state, order, actions, registerCleanup }) {
  render(html`<${PhoneAgents} state=${state} order=${order} actions=${actions} />`, host);
  registerCleanup(() => render(null, host));
}
