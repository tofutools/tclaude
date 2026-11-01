// companion-cog.js -- the ⚙ of a companion tab and of a floated chat's window.
// The title bar keeps voice, ✉ and whatever needs the operator right now; every
// other tool is an item here, on the same type-to-filter as the Groups cogs
// (menu-filter.js). Items stay in the page while the menu is closed, as theirs
// do, so the filter reads what is rendered and each keeps its own condition.

import { h } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ClearContext, StewardShutdown } from './companion-approval.js';
import { useDismissablePopover } from './hover-popover.js';
import { MENU_FILTER_PLACEHOLDER, MENU_FILTER_WIZARD_PLACEHOLDER, applyMenuFilter, bindMenuHover, handleMenuFilterKeyDown, setMenuActive } from './menu-filter.js';
import { StewardRoster } from './steward-roster.js';
import { isWizardActive } from './slop.js';

const html = htm.bind(h);

// Room left under the menu inside the window, which clips what it holds.
const MENU_MARGIN_PX = 8;

const DOUBLE_CLICK_MS = 500;

// The menu covers the top of the window, where approval cards sit, and goes on
// the first click. The second click of a double-click must not land on one.
export function dropDoubleClick(doc) {
  const drop = (event) => {
    if (event.detail < 2) return;
    event.preventDefault();
    event.stopPropagation();
  };
  doc?.addEventListener?.('click', drop, true);
  setTimeout(() => doc?.removeEventListener?.('click', drop, true), DOUBLE_CLICK_MS);
}

// rosterHint says how many Stewards are up, for the Stewards item.
function rosterHint(roster) {
  const running = roster.filter((entry) => entry.online).length;
  return running ? `${running} running` : '';
}

// agentCogSections lists what the cog offers for an agent. A handler left out
// leaves its item out: a floated chat has no Float, for one.
export function agentCogSections({
  agent, deciding = false, parked = false, roster = [], canShutDown = !!agent?.steward,
  onCollab = null, onFloat = null, onDetach = null, onMorph = null,
  onClearContext, openPermissions = null, openSudo = null, openRoster = null, onShutDown,
}) {
  if (!agent || deciding) return agent && onCollab ? [{ title: 'This agent', items: [collabItem(onCollab)] }] : [];
  const offline = `${agent.name} is not running. Resume it to change its access.`;
  const running = agent.online;
  const thisAgent = [
    onCollab && collabItem(onCollab),
    running && onFloat && { key: 'float', glyph: '▣', label: 'Open in overlay', className: 'companion-float', ariaLabel: `Open ${agent.name} in an overlay window`, run: onFloat },
    running && onDetach && !parked && { key: 'detach', glyph: '⧉', label: 'Open in separate window', className: 'companion-detach', ariaLabel: `Open ${agent.name} in a separate window`, run: onDetach },
    running && onMorph && { key: 'morph', glyph: '⇄', label: 'Morph', hint: 'harness, model, profile', className: 'companion-morph', ariaLabel: `Morph ${agent.name}`, run: onMorph },
    running && agent.harness !== 'shell' && { key: 'clear', glyph: '⟲', label: 'Clear context', className: 'companion-clear-open', ariaLabel: `Clear ${agent.name}'s context`, run: onClearContext },
  ];
  const access = agent.steward ? [
    openPermissions && { key: 'permissions', glyph: '🛡', label: 'Permissions', className: 'companion-steward-perms', ariaLabel: `Edit ${agent.name}'s permissions`, disabled: !running, title: running ? '' : offline, run: () => openPermissions(agent.convId, agent.name) },
    openSudo && { key: 'sudo', glyph: '🔓', label: 'Sudo', className: 'companion-steward-sudo', ariaLabel: `Grant ${agent.name} sudo`, disabled: !running, title: running ? '' : offline, run: () => openSudo(agent.agentId, agent.name) },
  ] : [];
  const stewards = [
    openRoster && { key: 'roster', glyph: '✦', label: 'Stewards', hint: rosterHint(roster), className: 'companion-steward-roster', ariaLabel: 'Steward roster', run: openRoster },
    canShutDown && { key: 'shutdown', glyph: '⏻', label: 'Shut down…', className: 'steward-shutdown-open', ariaLabel: `Shut down ${agent.name}`, danger: true, run: onShutDown },
  ];
  return [
    { title: 'This agent', items: thisAgent.filter(Boolean) },
    { title: 'Access', items: access.filter(Boolean) },
    { title: 'Stewards', items: stewards.filter(Boolean) },
  ].filter((section) => section.items.length);
}

function collabItem(onCollab) {
  return { key: 'collab', glyph: '⤢', label: 'Open in the Collab tab', ariaLabel: 'Open in the Collab tab', run: onCollab };
}

// CompanionCog is the button and its menu. Choosing an item closes the menu first,
// so what the item opens takes the focus.
export function CompanionCog({ sections, children = null }) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState('');
  const rootRef = useRef(null);
  const buttonRef = useRef(null);
  const menuRef = useRef(null);
  const filterRef = useRef(null);
  // Escape clears a query first, as on the Groups cogs.
  const close = (why) => {
    if (why === 'escape' && query) {
      setQuery('');
      setMenuActive(menuRef.current, null, { input: filterRef.current });
      return;
    }
    setOpen(false);
    if (why === 'escape') buttonRef.current?.focus?.();
  };
  useDismissablePopover(rootRef, open, close);
  useEffect(() => bindMenuHover(menuRef.current, { resolveInput: () => filterRef.current }), []);
  useLayoutEffect(() => {
    applyMenuFilter(menuRef.current, query, { input: filterRef.current });
  });
  useLayoutEffect(() => {
    const menu = menuRef.current;
    if (!open) {
      setQuery('');
      applyMenuFilter(menu, '', { input: filterRef.current, resetActive: true });
      return;
    }
    const edge = menu.closest('.companion-window')?.getBoundingClientRect?.();
    const below = buttonRef.current?.getBoundingClientRect?.();
    if (edge && below) menu.style.maxHeight = `${Math.max(0, edge.bottom - below.bottom - MENU_MARGIN_PX)}px`;
    filterRef.current?.focus?.();
  }, [open]);
  const choose = (item) => {
    dropDoubleClick(menuRef.current?.ownerDocument);
    setOpen(false);
    item.run();
  };
  return html`<span ref=${rootRef} class="companion-cog">
    <button ref=${buttonRef} type="button" class="companion-cog-button" aria-haspopup="menu" aria-expanded=${open ? 'true' : 'false'}
      aria-label="More actions" title="More actions" onClick=${() => setOpen((value) => !value)}>⚙</button>
    <div ref=${menuRef} class=${`action-menu companion-cog-menu${open ? ' open' : ''}`} role="menu"
      onPointerDown=${(event) => event.stopPropagation()}>
      <input ref=${filterRef} class="action-menu-filter" type="text" autocomplete="off" spellcheck=${false} role="combobox"
        aria-expanded="true" aria-autocomplete="list" aria-label="Filter actions"
        placeholder=${isWizardActive() ? MENU_FILTER_WIZARD_PLACEHOLDER : MENU_FILTER_PLACEHOLDER} value=${query}
        onInput=${(event) => setQuery(event.currentTarget.value)}
        onKeyDown=${(event) => handleMenuFilterKeyDown(menuRef.current, event, { hasQuery: !!query, clearQuery: () => setQuery('') })} />
      ${sections.map((section) => html`<div key=${section.title} class="companion-cog-head" role="separator">${section.title}</div>
        ${section.items.map((item) => html`<button key=${item.key} type="button" role="menuitem" class=${`companion-cog-item ${item.className || ''}${item.danger ? ' danger' : ''}`}
          disabled=${!!item.disabled} aria-label=${item.ariaLabel} title=${item.title || null} onClick=${() => choose(item)}>
          <span class="companion-cog-glyph" aria-hidden="true">${item.glyph}</span><span class="companion-cog-label">${item.label}</span>${item.hint && html`<span class="companion-cog-hint">${item.hint}</span>`}
        </button>`)}`)}
    </div>
    ${children}
  </span>`;
}

// AgentCog is the cog with what its items open: the questions that ask before
// acting and the roster, which keep their own popovers and have no button here.
export function AgentCog({
  agent, deciding, parked, roster, showRoster = false, canShutDown, onCollab, onFloat, onDetach, onMorph, openPermissions, openSudo,
  openHandover, agents, showAgent, fetchImpl, notify, onDown, askShutDownRef = null, rosterRef = null,
}) {
  const ownShutDown = useRef(null);
  const askShutDown = askShutDownRef || ownShutDown;
  const askClear = useRef(null);
  const ownRoster = useRef(null);
  const openRoster = rosterRef || ownRoster;
  if (!agent) return null;
  const sections = agentCogSections({
    agent, deciding, parked, roster, canShutDown, onCollab, onFloat, onDetach, onMorph, openPermissions, openSudo,
    openRoster: showRoster ? () => openRoster.current?.() : null,
    onClearContext: () => askClear.current?.('here'), onShutDown: () => askShutDown.current?.('here'),
  });
  if (!sections.length) return null;
  const asksShutDown = !deciding && (canShutDown ?? agent.steward);
  return html`<${CompanionCog} sections=${sections}>
    ${!deciding && agent.online && agent.harness !== 'shell' && html`<${ClearContext} agent=${agent} fetchImpl=${fetchImpl} notify=${notify} askRef=${askClear} trigger=${false} />`}
    ${asksShutDown && html`<${StewardShutdown} agent=${agent} fetchImpl=${fetchImpl} onDown=${onDown} askRef=${askShutDown} trigger=${false} />`}
    ${showRoster && !deciding && html`<${StewardRoster} roster=${roster} agents=${agents} fetchImpl=${fetchImpl} notify=${notify} openHandover=${openHandover} showAgent=${showAgent} openRef=${openRoster} trigger=${false} />`}
  <//>`;
}
