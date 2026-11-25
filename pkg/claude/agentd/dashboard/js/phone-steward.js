// phone-steward.js -- the Steward's floating ✦ button on the phone's Now and
// Agents views (tk_6f591b11), the phone's way to the companion the desktop
// shows as its chip.
//
// A tap opens the Steward full screen, in the phone terminal every agent's
// "open ›" docks (phone-terminal-island.js), with its message sheet and key
// strip. A Steward that is not running opens a sheet that starts one, as the
// desktop's start tab does: Resume the one in the seat, or Start fresh. With
// Stewards turned off in Config and none running, there is no button.
//
// The button wears what the Steward has for the operator, read from the
// snapshot as the desktop chip reads it: its open decisions and the access
// requests it waits on count in the badge, and an amber ring shows while it
// has any or stands at a prompt.
//
// The views it floats over keep room at their end (data-phone-fab, phone.css)
// so the last row's "open ›" scrolls clear of it, and it shrinks while the
// operator scrolls down.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  approvalsFor, companionAgents, companionStatus, companionStatusLabel, decisionsFor, isAgentId, stewardAgent, stewardToResume,
} from './companion-model.js';
import { requestStewardSummon, resumeAgent } from './companion-approval.js';
import { stewardFreshNote, stewardReturnNote } from './companion-home.js';
import { closePhoneSheet, openPhoneSheet } from './phone-sheet.js';
import { ButtonRow, SheetTitle } from './phone-ui.js';

const html = htm.bind(h);

// The views the button floats over: the phone shell's slots.
export const STEWARD_FAB_SLOTS = Object.freeze(['now', 'agents']);

// stewardFab is what the button shows, or null for no button.
//   agent      the Steward in the seat (companion-model's), or null
//   label      its name, the operator's for the Steward if set
//   count      its open decisions plus the access requests it waits on
//   attention  count > 0, or it stands at a prompt
//   status     its companion status ('needs', 'working', …, 'offline')
//   returning  the roster's Steward that Resume brings back, or null
export function stewardFab(snapshot) {
  if (!snapshot) return null;
  const agent = stewardAgent(companionAgents(snapshot));
  const online = !!agent?.online;
  if (snapshot.stewards_disabled === true && !online) return null;
  const status = companionStatus(agent);
  const count = online ? decisionsFor(snapshot, agent).length + approvalsFor(snapshot, agent).length : 0;
  const entry = online ? null : stewardToResume(snapshot.steward_roster);
  const label = agent?.name || String(snapshot.steward_name || '') || 'Steward';
  return {
    agent: online ? agent : null,
    label,
    count,
    attention: count > 0 || status === 'needs',
    status,
    returning: entry ? {
      agentId: entry.agent_id,
      name: entry.agent_id === agent?.agentId ? label : String(entry.name || label),
      lastRan: entry.last_ran || '',
      keeps: entry.keeps || [],
    } : null,
  };
}

// STEWARD_START_MS is how long a Steward started here may take to show as
// running before the button stops waiting for it (the desktop companion's
// PENDING_MS).
export const STEWARD_START_MS = 30000;

// fabLabel is the button's accessible name: who, and what waits there.
export function fabLabel(fab, starting = false) {
  if (!fab.agent && starting) return `${fab.label}: starting`;
  if (!fab.agent) return `${fab.label}: not running. Start it`;
  const waits = fab.count > 0 ? `${fab.count} waiting for you` : companionStatusLabel(fab.status);
  return `${fab.label}: ${waits}. Open it`;
}

// SHRINK_PX is how far a scroll goes in one direction before the button
// changes size: a jitter at rest must not make it pulse.
export const SHRINK_PX = 12;

// nextShrunk says whether the button is small after a scroll from top `from`
// to `to` (px): down shrinks it, up restores it, and near the top it is
// always full size.
export function nextShrunk(shrunk, from, to) {
  if (to <= SHRINK_PX) return false;
  if (to - from >= SHRINK_PX) return true;
  if (from - to >= SHRINK_PX) return false;
  return shrunk;
}

// useScrollShrink follows the scrolling of whatever scrolls under the button
// (the own view's .phone-view, a tab's <main>, or the page): one listener in
// the capture phase, as a scroll does not bubble.
function useScrollShrink(doc) {
  const [shrunk, setShrunk] = useState(false);
  const tops = useRef(new WeakMap());
  const shrunkRef = useRef(shrunk);
  shrunkRef.current = shrunk;
  useEffect(() => {
    if (!doc) return undefined;
    const onScroll = (event) => {
      const target = event.target === doc ? doc.scrollingElement : event.target;
      if (!target || typeof target.scrollTop !== 'number') return;
      const from = tops.current.get(target) ?? 0;
      const to = target.scrollTop;
      const next = nextShrunk(shrunkRef.current, from, to);
      // Only a move that decided something is the new reference point.
      if (next !== shrunkRef.current || Math.abs(to - from) >= SHRINK_PX || to <= SHRINK_PX) tops.current.set(target, to);
      if (next !== shrunkRef.current) setShrunk(next);
    };
    doc.addEventListener('scroll', onScroll, { capture: true, passive: true });
    return () => doc.removeEventListener('scroll', onScroll, { capture: true });
  }, [doc]);
  return shrunk;
}

// StewardStart is the sheet a tap opens while no Steward runs: Resume the
// one in the seat, or Start fresh (Summon when there is none to resume).
function StewardStart({ fab, start, close }) {
  const [busy, setBusy] = useState(false);
  const go = (how) => async () => {
    setBusy(true);
    try {
      await start(how);
      close();
    } finally {
      setBusy(false);
    }
  };
  const back = fab.returning;
  return html`<div class="phone-steward-start">
    <${SheetTitle}>${back ? `Bring back ${back.name}` : `Summon ${fab.label}`}<//>
    <p class="phone-steward-note">${back ? stewardReturnNote(back) : 'Your helper for all of tclaude. It opens here once it is up.'}</p>
    <${ButtonRow} buttons=${back
      ? [{ key: 'resume', label: 'Resume', tone: 'pri', disabled: busy, onClick: go('resume') },
        { key: 'fresh', label: 'Start fresh', disabled: busy, onClick: go('summon') }]
      : [{ key: 'summon', label: 'Summon', tone: 'pri', disabled: busy, onClick: go('summon') }]} />
    ${back && html`<p class="phone-steward-note">${stewardFreshNote(back)}</p>`}
  </div>`;
}

// StewardFab is the button. snapshot is the dashboard's latest; actions has
// openTerminal(agent) and notify(text, error?).
export function StewardFab({ snapshot, actions, fetchImpl = globalThis.fetch, doc = globalThis.document }) {
  const fab = stewardFab(snapshot);
  const shrunk = useScrollShrink(doc);
  // A Steward started from here opens once the snapshot shows it running,
  // within STEWARD_START_MS. Until then a tap offers no second start: Start
  // fresh would make two Stewards.
  const [starting, setStarting] = useState('');
  useEffect(() => {
    if (!starting || fab?.agent?.agentId !== starting) return;
    setStarting('');
    actions.openTerminal(fab.agent);
  }, [starting, fab?.agent?.agentId]);
  useEffect(() => {
    if (!starting) return undefined;
    const timer = setTimeout(() => {
      setStarting('');
      actions.notify?.(`${fab?.label || 'The Steward'} did not come up. Try again, or look in Agents.`, true);
    }, STEWARD_START_MS);
    return () => clearTimeout(timer);
  }, [starting]);
  // The views keep room at their end while the button floats over them.
  useEffect(() => {
    if (!fab || !doc) return undefined;
    doc.documentElement.dataset.phoneFab = '';
    return () => { delete doc.documentElement.dataset.phoneFab; };
  }, [!!fab, doc]);
  if (!fab) return null;

  async function start(how) {
    try {
      if (how === 'resume') {
        await resumeAgent(fetchImpl, fab.returning.agentId);
        setStarting(fab.returning.agentId);
        actions.notify?.(`Resuming ${fab.returning.name}…`);
        return;
      }
      const result = await requestStewardSummon(fetchImpl);
      if (!isAgentId(result.agent_id)) throw new Error('the summon returned no agent id');
      setStarting(result.agent_id);
      actions.notify?.(`${result.reused ? 'Reopening' : 'Summoning'} ${fab.label}…`);
    } catch (error) {
      actions.notify?.(`Could not start ${fab.label}: ${error?.message || error}`, true);
    }
  }

  const onClick = () => {
    if (fab.agent) {
      closePhoneSheet();
      actions.openTerminal(fab.agent);
      return;
    }
    if (starting) {
      actions.notify?.(`${fab.label} is starting…`);
      return;
    }
    openPhoneSheet({ label: `Start ${fab.label}`, Body: StewardStart, props: { fab, start } });
  };
  const cls = ['phone-steward-fab', fab.attention && 'attention', shrunk && 'shrunk', !fab.agent && 'offline', !fab.agent && starting && 'starting'].filter(Boolean).join(' ');
  return html`<button data-ui-exempt type="button" class=${cls} aria-label=${fabLabel(fab, !!starting)} title=${fab.label}
    aria-haspopup=${fab.agent || starting ? undefined : 'dialog'} aria-busy=${!fab.agent && starting ? 'true' : undefined} onClick=${onClick}>
    <span class="phone-steward-fab-glyph" aria-hidden="true">✦</span>
    ${fab.count > 0 && html`<span class="ui-badge ui-badge-warn phone-steward-fab-badge" aria-hidden="true">${fab.count}</span>`}
  </button>`;
}
