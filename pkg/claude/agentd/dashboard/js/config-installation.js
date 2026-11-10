// config-installation.js — Config › Installation: how tclaude is installed,
// started and updated on this machine. The section stacks its parts (Start
// automatically, Updates) on one page. Each part opens with a status line —
// on, off, or needs attention — and its main action right there, so what is
// set up and what is not reads at a glance; the detail sits below and folds
// away when the part is already set up and healthy.

import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { methodInfo, selfUpdateDraft, selfUpdateLoads, selfUpdateMeta } from './config-self-update.js';

const html = htm.bind(h);

const STATE_LABELS = { on: 'On', off: 'Off', warn: 'Needs attention', unknown: 'Unknown' };

// InstallPart is one part of the Installation section: a status card (title,
// state, one-line status, main actions, a details toggle) over its detail.
// The detail stays mounted while folded, so its Config inputs still load and
// save, and a settings search shows it whatever the fold says.
// idp prefixes its element ids ('cfg-' in Config).
export function InstallPart({ idp = 'cfg-', id, title, state = 'unknown', status, actions = null, collapsed = false, onToggle = null, children }) {
  const bodyId = `${idp}install-${id}-body`;
  return html`<div class="cfg-inst-part" id=${`${idp}install-${id}`} data-cfg-install-part=${id} data-cfg-inst-state=${state}>
    <div class="cfg-section cfg-inst-head" role="group" aria-labelledby=${`${idp}install-${id}-title`}>
      <h3 class="cfg-inst-title" id=${`${idp}install-${id}-title`}>${title}
        <span class="ui-tag cfg-inst-state" data-state=${state} id=${`${idp}install-${id}-state`}>${STATE_LABELS[state] || state}</span></h3>
      <div class="cfg-inst-status" id=${`${idp}install-${id}-status`}>${status}</div>
      ${(actions || (children && onToggle)) && html`<div class="cfg-inst-actions">
        ${actions}
        ${children && onToggle && html`<${Button} size="sm" variant="ghost" class="cfg-inst-toggle" id=${`${idp}install-${id}-toggle`}
          aria-expanded=${String(!collapsed)} aria-controls=${bodyId} onClick=${onToggle}>${collapsed ? 'Show details' : 'Hide details'}<//>`}
      </div>`}
    </div>
    ${children && html`<div class="cfg-inst-body" id=${bodyId} data-collapsed=${collapsed ? '' : undefined}>${children}</div>`}
  </div>`;
}

// useFold is a part's fold: null follows `healthy` as it was once the part's
// data was read (`epoch` changed) and is `ready` to judge, so an edit never
// folds the part away under the operator; a toggle fixes it until the next
// read.
export function useFold(healthy, epoch, ready = true) {
  const [auto, setAuto] = useState(false);
  const [expanded, setExpanded] = useState(null);
  const [decided, setDecided] = useState(0);
  useEffect(() => {
    if (!epoch || !ready || decided === epoch) return;
    setDecided(epoch);
    setAuto(!!healthy);
    setExpanded(null);
  }, [epoch, ready]);
  const collapsed = expanded === null ? auto : !expanded;
  return [collapsed, () => setExpanded(collapsed), () => setExpanded(true)];
}

// afterRender runs fn once a state change has been painted, so a control
// just unfolded can be scrolled to and focused.
export function afterRender(fn) {
  const raf = globalThis.requestAnimationFrame;
  if (raf) raf(() => fn()); else globalThis.setTimeout(fn, 0);
}

function capitalize(s) { return s ? s[0].toUpperCase() + s.slice(1) : s; }

// updatesStatus is the Updates part's status from the form's (unsaved)
// values: state, a sentence, and its main action — 'choose' a method,
// 'schedule' updates, or turn them 'off'.
export function updatesStatus(draft = selfUpdateDraft.value, meta = selfUpdateMeta.value) {
  if (!draft.method) {
    return { state: 'off', text: 'Not updating automatically. No update method is chosen, so nothing updates on its own.', action: 'choose' };
  }
  const m = methodInfo(draft.method, meta);
  const label = m?.label || draft.method;
  if (m?.available === false) {
    return { state: 'warn', text: `${label} cannot run here${m.unavailable_reason ? `: ${m.unavailable_reason}` : ''}. Updates fail until that is fixed or another method is chosen.`, action: 'choose' };
  }
  const window = `${draft.windowStart || '03:00'}–${draft.windowEnd || '05:00'}`;
  const when = [];
  if (draft.frequency === 'daily') when.push(`daily ${window}`);
  if (draft.frequency === 'weekly') when.push(`every ${capitalize(draft.weekday || 'monday')} ${window}`);
  if (draft.onStart) when.push('when agentd starts');
  if (!when.length) {
    return { state: 'off', text: `Not updating automatically. The method is ${label}, but no schedule or start-up update is on.`, action: 'schedule' };
  }
  if (m?.notify_only) return { state: 'on', text: `Checks for updates ${when.join(' and ')} and tells you; installs nothing.`, action: 'off' };
  return { state: 'on', text: `Updates itself with ${label}: ${when.join(' and ')}.`, action: 'off' };
}

// setFormControl changes a Config input as if the operator had, so the form
// marks itself dirty and the change is saved with Save changes.
// from is the clicked button: the control is looked up in its own form, which
// need not be in the document.
function formControl(from, id) {
  return from?.closest?.('.config-form')?.querySelector(`#${id}`) || globalThis.document?.getElementById(id) || null;
}

function setFormControl(from, id, apply) {
  const el = formControl(from, id);
  if (!el) return;
  apply(el);
  el.dispatchEvent(new Event('change', { bubbles: true }));
}

// pick selects a <select>'s option by value: clear every option, then select
// the one wanted (as the adapter's setSelectValue does), which every DOM
// honours, test DOMs included.
function pick(select, value) {
  const options = [...select.querySelectorAll('option')];
  for (const option of options) {
    option.removeAttribute('selected');
    option.selected = false;
  }
  const wanted = options.find((option) => (option.getAttribute('value') ?? option.value ?? '') === value);
  if (wanted) {
    wanted.setAttribute('selected', '');
    wanted.selected = true;
  }
}

function UpdatesActions({ action, expand }) {
  if (action === 'choose') {
    return html`<${Button} variant="primary" size="sm" id="cfg-install-updates-choose" onClick=${(e) => {
      expand();
      const select = formControl(e.currentTarget, 'cfg-selfupdate-method');
      afterRender(() => {
        select?.scrollIntoView?.({ block: 'center' });
        select?.focus?.();
      });
    }}>Choose an update method…<//>`;
  }
  if (action === 'schedule') {
    return html`<${Button} variant="primary" size="sm" id="cfg-install-updates-on"
      title="Turns on a daily update in the 03:00–05:00 window; change it below. Saved with Save changes."
      onClick=${(e) => setFormControl(e.currentTarget, 'cfg-selfupdate-frequency', (el) => pick(el, 'daily'))}>Update daily<//>`;
  }
  return html`<${Button} size="sm" id="cfg-install-updates-off"
    title="Turns off scheduled and start-up updates and keeps the method. Saved with Save changes."
    onClick=${(e) => {
      const from = e.currentTarget;
      setFormControl(from, 'cfg-selfupdate-frequency', (el) => pick(el, 'off'));
      setFormControl(from, 'cfg-selfupdate-on-start', (el) => { el.checked = false; });
    }}>Turn off<//>`;
}

// UpdatesPart is the Updates part around the Updates detail (the Config
// form's updates.self inputs, passed as children).
export function UpdatesPart({ children }) {
  const s = updatesStatus(selfUpdateDraft.value, selfUpdateMeta.value);
  // Judged once the install detection is in too: it says whether the
  // method can run here, and a method that cannot must not fold as healthy.
  const meta = selfUpdateMeta.value;
  const [collapsed, toggle, expand] = useFold(s.state === 'on', selfUpdateLoads.value, meta !== null && !meta.error);
  return html`<${InstallPart} id="updates" title="Updates" state=${s.state} collapsed=${collapsed} onToggle=${toggle}
    status=${html`<span>${s.text}</span> <span class="muted">Changes here are saved with Save changes.</span>`}
    actions=${html`<${UpdatesActions} action=${s.action} expand=${expand} />`}>
    ${children}
  <//>`;
}
