// get-help-menu.js -- the "Get help ▾" toolbar menu (tcl-z32qq1) a page drops in
// beside its own controls: New helper, Ask an existing agent…, and the page's
// own actions, which reach the helper the page last brought in. The agent
// picker is a standard Dialog. What a helper does lives in get-help.js.

import { h } from 'preact';
import { useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Button, Input, Menu, MenuItem, MenuSep } from './ui-components.js';
import { Dialog } from './ui-dialog.js';
import { useDismissablePopover } from './hover-popover.js';
import { dashboardState } from './snapshot-store.js';
import { shellToast } from './shell-state.js';
import { askExistingAgent, liveHelpers, missingSlugs, summonHelper } from './get-help.js';

const html = htm.bind(h);

// AgentPicker lists the running agents to ask, with a filter, and says which
// of them would be granted what.
export function AgentPicker({ helpers, slugs = [], onPick, onClose }) {
  const [query, setQuery] = useState('');
  const wanted = query.trim().toLowerCase();
  const shown = helpers.filter((helper) => !wanted || `${helper.label} ${helper.id} ${helper.groups.join(' ')}`.toLowerCase().includes(wanted));
  return html`<${Dialog} id="get-help-pick-modal" title="Ask an existing agent" onClose=${onClose} cancel="Cancel" dialogClass="get-help-pick">
    <p class="muted">The agent gets this page's brief as a message and opens in its chat window. One that lacks the permissions the help needs is granted them after you confirm.</p>
    <${Input} size="sm" placeholder="Filter agents" aria-label="Filter agents" value=${query} data-get-help-filter
      onInput=${(event) => setQuery(event.currentTarget.value)} />
    ${helpers.length === 0 && html`<p class="muted">No agent is running. Start a new helper instead.</p>`}
    ${helpers.length > 0 && shown.length === 0 && html`<p class="muted">No running agent matches.</p>`}
    <ul class="get-help-agents" role="list">
      ${shown.map((helper) => {
        const missing = missingSlugs(helper, slugs);
        return html`<li key=${helper.id} data-get-help-agent=${helper.id}>
          <${Button} size="sm" class="get-help-agent" title=${missing.length ? `Asks first: grants ${missing.join(', ')}` : 'Holds every permission this help needs'} onClick=${() => onPick(helper)}>
            <span class="get-help-agent-name">${helper.label}</span>
            ${helper.groups.length > 0 && html`<span class="ui-tag">${helper.groups[0]}${helper.groups.length > 1 ? ` +${helper.groups.length - 1}` : ''}</span>`}
            ${missing.length > 0 && html`<span class="ui-tag ui-tag-warn">needs ${missing.length} grant${missing.length === 1 ? '' : 's'}</span>`}
          <//>
        </li>`;
      })}
    </ul>
  <//>`;
}

// GetHelpMenu is the button and its menu. help says what a helper is for:
// { name, slugs, scope, exclusive, taskRef, subject, brief(), confirmation,
// askConfirmation(missing, helper) }; brief may be async. before() runs first
// and may refuse, as a page's save-before-handoff guard does. onHelper hears
// the agent a pick brought in, so the page's actions reach it. actions are the
// page's own: [{ key, label, title, disabled, run }].
export function GetHelpMenu({
  help, actions = [], before = null, onHelper = null, label = 'Get help', title = 'Bring an agent in to help with this page',
  snapshot = dashboardState.snapshot, notify = shellToast, fetchImpl = globalThis.fetch, confirm = undefined, class: extra = '',
}) {
  const [open, setOpen] = useState(false);
  const [picking, setPicking] = useState(false);
  const [busy, setBusy] = useState(false);
  const rootRef = useRef(null);
  useDismissablePopover(rootRef, open, () => setOpen(false));
  const run = async (work) => {
    setOpen(false);
    if (busy) return;
    setBusy(true);
    try {
      if (before && !(await before())) return;
      const helper = await work();
      if (helper) onHelper?.(helper);
    } catch (error) {
      notify(`Could not get help: ${error?.message || error}`, true);
    } finally {
      setBusy(false);
    }
  };
  const brief = async () => (typeof help.brief === 'function' ? help.brief() : help.brief);
  const newHelper = () => run(async () => summonHelper({
    name: help.name, slugs: help.slugs, scope: help.scope, exclusive: help.exclusive, taskRef: help.taskRef,
    confirmation: typeof help.confirmation === 'function' ? help.confirmation() : help.confirmation,
    brief: await brief(), fetchImpl, ...(confirm ? { confirm } : {}),
  }));
  const ask = (helper) => {
    setPicking(false);
    return run(async () => askExistingAgent({
      helper, slugs: help.slugs, subject: help.subject || 'Help wanted', brief: await brief(),
      confirmation: (missing) => help.askConfirmation?.(missing, helper), fetchImpl, ...(confirm ? { confirm } : {}),
    }));
  };
  return html`<div ref=${rootRef} class=${`get-help${extra ? ` ${extra}` : ''}`}>
    <${Button} class="get-help-toggle" aria-haspopup="menu" aria-expanded=${open ? 'true' : 'false'} title=${title} disabled=${busy}
      onClick=${() => setOpen((value) => !value)}>${busy ? 'Starting…' : `${label} ▾`}<//>
    ${open && html`<${Menu} class="get-help-menu" aria-label=${label}>
      <${MenuItem} data-get-help="new" title="Start an agent scoped to this page's work, and open its chat window" onClick=${newHelper}>New helper<//>
      <${MenuItem} data-get-help="ask" title="Brief an agent that is already running, and open its chat window" onClick=${() => { setOpen(false); setPicking(true); }}>Ask an existing agent…<//>
      ${actions.length > 0 && html`<${MenuSep} />`}
      ${actions.map((action) => html`<${MenuItem} key=${action.key} data-get-help=${action.key} title=${action.title || ''} disabled=${action.disabled}
        onClick=${() => { setOpen(false); void action.run(); }}>${action.label}<//>`)}
    <//>`}
    ${picking && html`<${AgentPicker} helpers=${liveHelpers(snapshot.value)} slugs=${help.slugs} onPick=${ask} onClose=${() => setPicking(false)} />`}
  </div>`;
}
