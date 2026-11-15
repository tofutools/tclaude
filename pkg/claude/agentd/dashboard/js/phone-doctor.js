// phone-doctor.js -- Diagnostics › Doctor on a phone (data-layout="phone").
//
// The desktop Doctor is a category sidebar beside a list of long finding
// cards; on a phone that sidebar became a second row of tabs that ran off the
// screen, and a long path in a finding ran out of its card (B12). A phone
// shows one list instead, grouped by severity (decisions, blocking, warnings),
// each finding a two-line row (its title, then its category and detail) that
// opens the finding as a page of its own: the whole detail, the fix to copy,
// and the choice or repair it offers. The checks that pass fold into one line
// at the end. It runs on the desktop's state and actions (doctor-state.js,
// doctor-island.js), so loading, polling and the badges are the desktop's.

import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { DOCTOR_CATEGORIES } from './doctor-state.js';
import { SandboxDefaultControl, copyFix } from './doctor-island.js';
import { HarnessConfigRepairControl } from './harness-config-repair.js';
import { usePhonePage } from './phone-page.js';
import { Section } from './phone-ui.js';
import { Button } from './ui-components.js';

const html = htm.bind(h);

// GROUPS are the severities that ask something of the operator, in the
// order the operator should look at them.
export const GROUPS = Object.freeze([
  { severity: 'decision', title: 'Decisions' },
  { severity: 'blocking', title: 'Blocking' },
  { severity: 'warning', title: 'Warnings' },
]);

const DOT = { decision: '🔵', blocking: '🔴', warning: '🟡', info: '⚪', ok: '✅' };
const SEVERITY = { decision: 'Decision', blocking: 'Blocking', warning: 'Warning', info: 'Info', ok: 'OK' };

// kicker is the line over a finding's title: its severity, and its category
// unless that says the same ("Decision · Decisions").
export function kicker(check) {
  const severity = SEVERITY[check.severity] || check.severity || '';
  const label = check.category === 'decisions' ? '' : categoryLabel(check);
  return [severity, label].filter(Boolean).join(' · ');
}

export const categoryLabel = (check) => DOCTOR_CATEGORIES.find((c) => c.id === check.category)?.label || '';

// groupChecks splits the checks into the GROUPS, then the rest (passing or
// only informational), each in the order the server sent them.
export function groupChecks(checks) {
  const groups = GROUPS.map((group) => ({ ...group, checks: checks.filter((check) => check.severity === group.severity) }));
  const asking = new Set(GROUPS.map((group) => group.severity));
  return { groups: groups.filter((group) => group.checks.length), passing: checks.filter((check) => !asking.has(check.severity)) };
}

function CheckRow({ check, onOpen }) {
  const sub = [categoryLabel(check), check.detail].filter(Boolean).join(' · ');
  return html`<li><button data-ui-exempt type="button" class=${`phone-doctor-row sev-${check.severity}`} data-check=${check.id}
    onClick=${() => onOpen(check.id)}>
    <span class="phone-doctor-dot" role="img" aria-label=${SEVERITY[check.severity] || check.severity}>${DOT[check.severity] || '•'}</span>
    <span class="phone-doctor-text">
      <span class="phone-doctor-title">${check.title}</span>
      ${sub && html`<span class="phone-doctor-sub">${sub}</span>`}
    </span>
    <span class="phone-doctor-more" aria-hidden="true">›</span>
  </button></li>`;
}

// CheckPage is one finding, pushed over the list: Back (the bar's ‹ or the
// browser's) returns to the list.
function CheckPage({ check, state, actions, notify, back }) {
  usePhonePage({ title: 'Doctor', onBack: back, hideNav: true, actions: [] }, true);
  useEffect(() => { globalThis.scrollTo?.(0, 0); }, [check?.id]);
  if (!check) {
    return html`<div class="phone-doctor-page">
      <p class="ui-empty phone-empty">✅ This check has cleared.</p>
      <${Button} class="phone-btn phone-wide" onClick=${back}>Back to the Doctor<//>
    </div>`;
  }
  return html`<div class=${`phone-doctor-page sev-${check.severity}`} data-check=${check.id}>
    <p class="ui-eyebrow phone-doctor-kicker"><span aria-hidden="true">${DOT[check.severity] || '•'}</span> ${kicker(check)}</p>
    <h1 class="phone-doctor-heading">${check.title}</h1>
    ${check.detail && html`<p class="phone-doctor-detail">${check.detail}</p>`}
    ${check.fix && html`<div class="phone-doctor-fix">
      <code>${check.fix}</code>
      <${Button} class="phone-btn" onClick=${() => copyFix(check.fix, notify)}>Copy<//>
    </div>`}
    ${check.action === 'sandbox-default' && html`<${SandboxDefaultControl} check=${check} state=${state} actions=${actions} />`}
    ${check.action === 'harness-config-repair' && html`<${HarnessConfigRepairControl} notify=${notify} onDone=${() => actions.load({ refresh: true })} />`}
  </div>`;
}

// PhoneDoctor is the Doctor on a phone: what needs the operator, grouped,
// then the checks that pass, one line until opened.
export function PhoneDoctor({ state, actions, notify }) {
  const [openId, setOpenId] = useState(null);
  const [showPassing, setShowPassing] = useState(false);
  const active = state.active.value;
  useEffect(() => {
    if (active) actions.load({ refresh: true });
    else setOpenId(null);
  }, [active]);
  const checks = state.checks.value;
  const phase = state.phase.value;
  if (openId !== null && active) {
    return html`<${CheckPage} check=${checks.find((check) => check.id === openId)} state=${state} actions=${actions}
      notify=${notify} back=${() => setOpenId(null)} />`;
  }
  const { groups, passing } = groupChecks(checks);
  const asking = groups.reduce((n, group) => n + group.checks.length, 0);
  return html`<div class="phone-doctor">
    <div class="phone-doctor-head">
      <p class="phone-doctor-lede">${phase !== 'ready' ? '' : asking === 0 ? '✅ Nothing needs you.' : `${asking} need${asking === 1 ? 's' : ''} you`}</p>
      <${Button} class="phone-btn" disabled=${phase === 'loading'} onClick=${() => actions.load({ refresh: true })}>↻ Re-check<//>
    </div>
    ${phase === 'error' && html`<div class="ui-error-box" role="alert">Could not load checks: ${state.error.value}</div>`}
    ${phase === 'loading' && !checks.length && html`<p class="ui-empty phone-empty">Checking…</p>`}
    ${groups.map((group) => html`<${Section} key=${group.severity} title=${group.title} count=${group.checks.length}>
      <ul class="phone-list phone-doctor-list">${group.checks.map((check) => html`<${CheckRow} key=${check.id} check=${check} onOpen=${setOpenId} />`)}</ul>
    <//>`)}
    ${passing.length > 0 && html`<${Section} title="All good" count=${passing.length}>
      <button data-ui-exempt type="button" class="phone-doctor-fold" aria-expanded=${showPassing ? 'true' : 'false'}
        onClick=${() => setShowPassing(!showPassing)}>${showPassing ? 'Hide' : 'Show'} the ${passing.length} passing check${passing.length === 1 ? '' : 's'}
        <span aria-hidden="true">${showPassing ? '⌄' : '›'}</span></button>
      ${showPassing && html`<ul class="phone-list phone-doctor-list">${passing.map((check) => html`<${CheckRow} key=${check.id} check=${check} onOpen=${setOpenId} />`)}</ul>`}
    <//>`}
  </div>`;
}
