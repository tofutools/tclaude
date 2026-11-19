// process-run-waits.js -- a run's waiting wait steps in the run views
// (tk_96aee1ac): how long a wait has left, ticking, or for a wait until a
// condition, which check it is on, the last answer and when the next check
// is ("check 3 of 20 · next in 4m"). Someone who may manage the run can skip
// the wait (it goes on as if its time came, or as met) or make the next check
// now.
//
// The run view (GET /v1/process/runs/{id}) carries each waiting step's
// command with `wait`: its deadline (dueAt), and for a check wait its
// progress (check), limits (maxChecks, givesUpAt) and whether a check is out
// (checking). The desktop run focus and the phone run sheet both render
// WaitingSteps through RunOversight; the phone's run card uses waitShort.

import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { shellConfirm } from './shell-state.js';
import { waitingCommands, waitLine } from './process-run-wait-words.js';

const html = htm.bind(h);

// useNow re-renders every second while `active`, for a ticking countdown.
export function useNow(active) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return undefined;
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [active]);
  return now;
}

// WaitingSteps lists a run's waiting wait steps, each with its countdown and,
// when the run may be steered, Skip wait and (for a check wait between
// checks) Check now.
export function WaitingSteps({ view, runId, api, labelOf = (id) => id, onChanged, wizard = false, compact = false }) {
  const waits = waitingCommands(view);
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const now = useNow(waits.length > 0);
  if (!waits.length) return null;
  const act = async (command, action) => {
    if (busy) return;
    const label = labelOf(String(command.nodeId)) || command.nodeId;
    if (action === 'skip') {
      const ok = await shellConfirm({
        title: `Skip the wait at ${label}?`,
        body: command.wait?.check
          ? 'The run goes on now as if the condition held (along its met route).'
          : 'The run goes on now as if its time had come.',
        okLabel: 'Skip wait',
      });
      if (!ok) return;
    }
    setBusy(`${command.nodeId}:${action}`);
    setError('');
    try {
      if (action === 'skip') await api.skipWait(runId, { nodeId: command.nodeId });
      else await api.checkNow(runId, { nodeId: command.nodeId });
      onChanged?.();
    } catch (cause) {
      setError(cause?.message || String(cause));
      onChanged?.();
    } finally {
      setBusy('');
    }
  };
  return html`<ul class=${`prw-waits${compact ? ' is-compact' : ''}`} data-run-waits>${waits.map((command) => {
    const label = labelOf(String(command.nodeId)) || command.nodeId;
    const line = waitLine(command, now);
    const between = command.kind === 'wait' && !!command.wait?.check;
    return html`<li key=${command.commandId} class="prw-wait" data-run-wait=${command.nodeId}>
      <div class="prw-line"><b>${line.check ? '↻' : '⏳'} ${label}</b>${' '}<span class=${line.failing ? 'ui-error' : 'prw-status'} data-run-wait-status>${line.check ? (wizard ? 'watches' : 'waits until its condition holds') : (wizard ? 'slumbers' : 'waits')}: ${line.text}</span></div>
      ${(api?.skipWait || api?.checkNow) && html`<div class="prw-actions">
        ${between && api?.checkNow && html`<${Button} size="sm" disabled=${!!busy} data-run-wait-check-now
          title=${`Make ${label}'s next check now; its count and limits stay as they are`}
          onClick=${() => void act(command, 'check')}>${busy === `${command.nodeId}:check` ? '…' : '↻ Check now'}<//>`}
        ${command.kind === 'wait' && api?.skipWait && html`<${Button} size="sm" disabled=${!!busy} data-run-wait-skip
          title=${command.wait?.check ? `Go on past ${label} now, as met` : `Go on past ${label} now, as if its time had come`}
          onClick=${() => void act(command, 'skip')}>${busy === `${command.nodeId}:skip` ? '…' : '⤼ Skip wait'}<//>`}
      </div>`}
    </li>`;
  })}
    ${error && html`<li class="grf-error ui-error" role="alert">${error}</li>`}
  </ul>`;
}
