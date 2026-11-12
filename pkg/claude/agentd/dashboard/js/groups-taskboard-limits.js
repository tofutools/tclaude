// groups-taskboard-limits.js -- a board's work-in-progress limits on the
// board itself (docs/taskboard.md "Work-in-progress limits"): a limited
// lane's head says how full it is ("2 / 3", amber when full, red when over),
// a task waiting in line for a full lane has a ⏳ chip with its place and
// why, and a lane head's ⋯ has "Set limit…", a small dialog that sets the
// lane's limit and what it does when full at once (taskboard options set
// <group> limits.in_progress.max=3 …).
//
// The limits and the lanes' counts come with the board's context entry
// (options.limits); the Board options drawer has every limit on its Limits
// tab.

import { h } from 'preact';
import { useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Dialog } from './ui-dialog.js';
import { Button, Input, OptionalSelect } from './ui-components.js';
import { ordinal } from './taskboard-actions.js';
import {
  AT_LIMITS, IN_PROGRESS, laneFill, laneKey, laneLabel, laneLimit, laneTitle, limitError,
} from './groups-taskboard-options-model.js';

const html = htm.bind(h);

// columnLane is the limits' lane a lanes column stands for: in_progress for
// In progress, a custom column's slug; '' for the others.
export function columnLane(column) {
  if (column?.key === 'progress') return IN_PROGRESS;
  return column?.custom ? column.slug : '';
}

// LaneLimit is a limited lane head's tag: "3 / 3" (per assignee "≤ 3
// each"), amber with "full" at the limit, red with "over" past it, and how
// many wait in line. The count is the board's own: a blocked task keeps its
// slot, though the lanes show it under Blocked.
export function LaneLimit({ entry, column }) {
  const lane = columnLane(column);
  const limit = lane && laneLimit(entry?.options, lane);
  if (!limit) return null;
  const fill = laneFill(limit);
  const title = laneTitle(limit, laneLabel(lane, entry?.columns || []), limit.per);
  return html`<span class=${`ui-tag tb-col-limit${fill ? ` tb-lane-${fill}` : ''}`} data-tb-lane-limit=${lane} data-tb-lane-fill=${fill || 'room'} title=${title}>${
    limit.per === 'assignee' ? `≤ ${limit.max} each` : `${limit.count} / ${limit.max}`}${fill && ` ${fill}`}${
    limit.queued > 0 ? ` · ⏳ ${limit.queued}` : ''}</span>`;
}

// QueuedChip marks a task waiting in line for a full lane: its place and,
// in its tooltip, why ("In progress is full, 1st in line").
export function QueuedChip({ task }) {
  if (!task?.queued_for || !(task.queued_position > 0)) return null;
  const reason = task.queued_reason || `waits for ${task.queued_for}`;
  return html`<span class="ui-tag tb-queued-chip" data-tb-queued=${task.queued_for} title=${`Queued: ${reason}. It goes in when a slot frees.`}>⏳ ${ordinal(task.queued_position)}</span>`;
}

// useLimitDialog drives a lane head's "Set limit…": open(lane, label), and
// the dialog to draw (null when none is up). Without a write client it is
// off.
export function useLimitDialog(group, entry, actions) {
  const [lane, setLane] = useState(null);
  if (!actions || typeof actions.api?.saveOptions !== 'function' || !entry?.options) return { can: false, open: () => {}, dialog: null };
  const dialog = lane && html`<${SetLimitDialog} lane=${lane.lane} label=${lane.label} entry=${entry} busy=${actions.busy === 'limits'}
    onSave=${(patch) => actions.run('limits', `Limit ${lane.label}`, (api) => api.saveOptions(group.name, patch))} onClose=${() => setLane(null)} />`;
  return { can: true, dialog, open: (l, label) => setLane({ lane: l, label }) };
}

// SetLimitDialog sets one lane's limit and what it does when full, at once.
// onSave({ set, reset }) runs the write and resolves true when it landed.
export function SetLimitDialog({ lane, label, entry, busy = false, onSave, onClose }) {
  const limit = laneLimit(entry?.options, lane);
  const [max, setMax] = useState(limit?.max > 0 ? String(limit.max) : '');
  const ownAt = (entry?.options?.set || []).includes(laneKey(lane, 'at_limit'));
  const [at, setAt] = useState(ownAt && limit ? limit.at_limit : '');
  const inputRef = useRef(null);
  const sending = useRef(false);
  const raw = max.trim();
  const value = raw === '' ? 0 : /^\d+$/.test(raw) ? Number(raw) : Number.NaN;
  const why = limitError(value);
  const submit = async () => {
    if (why || busy || sending.current) { inputRef.current?.focus(); return; }
    sending.current = true;
    try {
      const set = { [laneKey(lane, 'max')]: String(value) };
      const reset = [];
      if (at) set[laneKey(lane, 'at_limit')] = at;
      else if (ownAt) reset.push(laneKey(lane, 'at_limit'));
      if (await onSave({ set, reset })) onClose();
    } finally {
      sending.current = false;
    }
  };
  const now = limit ? (limit.per === 'assignee' ? `the fullest assignee has ${limit.count}` : `${limit.count} now`) : '';
  return html`<${Dialog} id="taskboard-limit-modal" dialogClass="cron-create-modal taskboard-limit-modal"
    title=${`Limit ${label}`} onClose=${onClose} onSubmitHotkey=${() => void submit()} onSubmitEnter=${() => void submit()}
    busy=${busy} initialFocusRef=${inputRef} cancel="Cancel"
    footer=${html`<${Button} variant="primary" id="taskboard-limit-submit" busy=${busy} disabled=${busy || !!why} onClick=${() => void submit()}>Set limit<//>`}>
    <p class="modal-hint ui-hint">How many tasks ${label} holds at once${limit?.per === 'assignee' ? ', per assignee' : ''}. Empty for no limit.
      Every limit is in the board's options, Limits tab.</p>
    <label class="cron-create-row">
      <span class="cron-create-label">Limit</span>
      <${Input} domRef=${inputRef} id="taskboard-limit-input" value=${max} invalid=${!!why} inputmode="numeric" placeholder="no limit"
        aria-label=${`${label} limit`} autocomplete="off" data-select-on-focus="" onInput=${(event) => setMax(event.currentTarget.value)} />
      ${now && html`<span class="ui-hint">${now}</span>`}
    </label>
    ${why && html`<p class="ui-error" role="alert">${why}</p>`}
    <label class="cron-create-row">
      <span class="cron-create-label">At the limit</span>
      <${OptionalSelect} id="taskboard-limit-at" value=${at} aria-label=${`${label}: at the limit`} unset="the board's choice"
        options=${AT_LIMITS.map(([k, text]) => [k, text.toLowerCase()])} onChange=${setAt} />
    </label>
  <//>`;
}
