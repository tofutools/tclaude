// companion-home.js -- the two tabs of the Steward's window that hold no agent:
// where a Steward is summoned or resumed, and the fleet-wide "to decide" queue.
// A chat tab has the rest (chat-window.js).

import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { relativeAge } from './pr-review-model.js';
import { windowControlsLook } from './overlay-window-controls.js';
import { OverlayWindow } from './overlay-window.js';
import { minimizeOverlayWindow } from './overlay-instances.js';
import { Button } from './ui-components.js';
import { postDismiss } from './decision-dismiss.js';

const html = htm.bind(h);

// stewardReturnNote says how far back a Steward's conversation picks up.
export function stewardReturnNote({ lastRan }) {
  const age = relativeAge(lastRan);
  return age ? `Same conversation. Last ran ${age}.` : 'Same conversation.';
}

// stewardFreshNote says what Start fresh leaves behind. Going on the roster takes
// what the role gave, not what the operator gave this agent separately.
export function stewardFreshNote({ name, keeps = [] }) {
  const kept = keeps.length ? ` It keeps what you gave it separately: ${keeps.join(', ')}.` : '';
  return `A new Steward. ${name} goes on the roster.${kept}`;
}

function HomeWindow({ instance, overlay, strip, close, minimize, snapshot, label, children }) {
  const look = windowControlsLook(snapshot.value?.window_controls);
  const onMinimize = minimize || (() => minimizeOverlayWindow(instance.id));
  const title = html`${strip}
    ${look === 'classic' && html`<button data-ui-exempt type="button" class="drawing-board-overlay-close" title="Minimize" aria-label=${`Minimize ${label}`} onClick=${onMinimize}>–</button>`}
    ${look === 'classic' && html`<button data-ui-exempt type="button" class="drawing-board-overlay-close" title=${close.title} aria-label=${`Close ${label}`} onClick=${close.onClose}>×</button>`}`;
  return html`<${OverlayWindow} id=${instance.windowId} window=${overlay} open=${!instance.minimized} className="companion-window chat-window" frameClass="companion-resize-frame"
    titlebarClass=${`drawing-board-overlay-titlebar overlay-look-${look}`} label=${label} title=${title}
    controls=${{ look, closeLabel: `Close ${label}`, closeTitle: close.title, onClose: close.onClose, onMinimize }}>
    <div class="companion-body">${children}</div>
  <//>`;
}

// StartTab is the Steward's window with no Steward in it.
export function StartTab({ instance, overlay, strip, close, minimize, snapshot, steward, shutDown, returning, busy, stewardLabel, customName, onResume, onSummon }) {
  return html`<${HomeWindow} instance=${instance} overlay=${overlay} strip=${strip} close=${close} minimize=${minimize} snapshot=${snapshot} label="Companion">
    <div class="companion-empty">
      <p class="companion-empty-title"><span class="companion-avatar steward" aria-hidden="true">✦</span> ${customName || 'The Steward'}</p>
      ${shutDown && html`<p class="companion-shutdown-note" role="status"><b>${shutDown.name}</b> is shut down. You can pick its conversation back up.</p>`}
      <p>An always-available helper for all of tclaude. Ask what you can do on this page, how your
        groups are set up, or to help you set up groups, profiles, processes or your sandbox.</p>
      <p class="companion-empty-note">Config > Steward sets what it can do without asking. Once it is running, ⚙ in the title bar changes its access.</p>
      ${returning ? html`<${Button} size="sm" variant="primary" class="companion-primary companion-resume-steward" disabled=${busy} onClick=${onResume}>${busy ? 'Working…' : `Resume ${returning.name}`}<//>
        <p class="companion-empty-note">${stewardReturnNote(returning)}</p>
        <${Button} size="sm" class="companion-secondary companion-fresh-steward" disabled=${busy} onClick=${onSummon}>Start fresh<//>
        <p class="companion-empty-note">${stewardFreshNote(returning)}</p>`
      : html`<${Button} size="sm" variant="primary" class="companion-primary" disabled=${busy} onClick=${onSummon}>${busy ? 'Summoning…' : `${steward?.online ? 'Open' : 'Summon'} ${stewardLabel}`}<//>`}
      <p class="companion-empty-note">Or press ＋ to pin any live agent here.</p>
    </div>
  <//>`;
}

// DecideTab is every decision agents are waiting on, whoever asked.
// DismissAll takes every decision in the queue off it, after a one-step
// confirm that names the count. It sends the ids on screen, nothing else.
export function DismissAll({ queue, fetchImpl, notify }) {
  const [phase, setPhase] = useState('idle'); // idle | confirm | busy
  const n = queue.length;
  if (n === 0) return null;
  const noun = `${n} decision${n === 1 ? '' : 's'}`;
  async function run() {
    setPhase('busy');
    try {
      const done = await postDismiss(fetchImpl, queue.map((d) => d.id));
      notify(`Dismissed ${done} of ${noun}; each agent is told.`);
    } catch (error) {
      notify(`Could not dismiss: ${error?.message || error}`, true);
    }
    setPhase('idle');
  }
  if (phase === 'idle') {
    return html`<div class="companion-decide-acts"><${Button} size="sm" onClick=${() => setPhase('confirm')}>Dismiss all (${n})<//></div>`;
  }
  return html`<div class="companion-decide-acts" role="group" aria-label="Confirm dismiss all">
    <span>Dismiss ${noun} without answering?</span>
    <${Button} size="sm" variant="danger" disabled=${phase === 'busy'} onClick=${run}>${phase === 'busy' ? 'Dismissing…' : `Dismiss ${n}`}<//>
    <${Button} size="sm" disabled=${phase === 'busy'} onClick=${() => setPhase('idle')}>Cancel<//>
  </div>`;
}

export function DecideTab({ instance, overlay, strip, close, minimize, snapshot, queue, askerName, fetchImpl, notify, DecisionCard }) {
  return html`<${HomeWindow} instance=${instance} overlay=${overlay} strip=${strip} close=${close} minimize=${minimize} snapshot=${snapshot} label="To decide">
    <div class="companion-decide-queue" role="region" aria-label="To decide">
      <${DismissAll} queue=${queue} fetchImpl=${fetchImpl} notify=${notify} />
      ${queue.map((decision) => html`<${DecisionCard} key=${decision.id} decision=${decision} fetchImpl=${fetchImpl} notify=${notify} from=${askerName(decision)} />`)}
    </div>
  <//>`;
}
