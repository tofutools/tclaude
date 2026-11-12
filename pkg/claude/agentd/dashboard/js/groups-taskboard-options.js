// groups-taskboard-options.js -- a board's Board options drawer
// (docs/taskboard.md "Board options", docs/dashboard.md): a side drawer over
// the board with the tabs Triggers (the idle check and the event messages)
// and Columns (the board's columns and which are idle-checked). Every value
// says where it comes from (default, global, set on this board) and, where
// the board sets it, has a ↺ that makes it inherit again. Changes are a
// draft until Save, which replaces the board's options document at the
// version read (PATCH /v1/groups/{g}/taskboard/options with only the keys
// it changed, as the CLI's `taskboard options set|reset`); Cancel, Esc or the backdrop with unsaved
// changes ask first.
//
// Ways in: ⚙ Board options in the section's toolbar (list and lanes), the
// icon-only ⚙ on the folded section line, a custom column's ⋯ "Column in
// Board options", and a link, /groups?board-options=<group>#idle (a linked
// node's board: inst_…:<group>), which is an overlay route, so Back closes
// the drawer.
//
// Reading needs taskboard.read; without taskboard.write (a linked node's
// board this node may only read) every control is read-only and the drawer
// says why. A linked node's board reads and saves through that node's peer
// view, as the rest of its board does.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Dialog, useDialogClose } from './ui-dialog.js';
import { Button, Check, Input, OptionalSelect, Switch } from './ui-components.js';
import { Tabs } from './ui-tabs.js';
import { RowMenu } from './row-menu.js';
import { shellConfirmDiscard, shellToast } from './shell-state.js';
import { openBoardOptions } from './groups-taskboard-options-route.js';
import { AddColumnButton, COLUMNS_CAP, columnMenuItems, useColumnNaming } from './groups-taskboard-columns.js';
import {
  EVENT_TO, eventRows, IDLE_TELL, RECIPIENT_LABEL, RECIPIENT_TITLE, SECTION_TAB, TABS,
  AT_LIMITS, IN_PROGRESS, PER,
  changedKeys, draftErrors, effectiveOf, eventKey, flatOptions, inheritedOf, laneKey, laneLimit,
  optionsSummary, patchOf, rebaseDraft, resetOption, setOption, sourceText, valueText,
} from './groups-taskboard-options-model.js';

const html = htm.bind(h);

// BoardOptionsButton is the toolbar's ⚙ Board options; iconOnly is the
// folded section line's ⚙.
export function BoardOptionsButton({ group, node = '', iconOnly = false, wizard = false }) {
  const label = wizard ? 'Quest board options' : 'Board options';
  const open = (event) => { event.stopPropagation(); openBoardOptions({ group, node }); };
  if (iconOnly) {
    return html`<${Button} size="sm" icon=${true} class="tb-options-btn" aria-label=${label}
      title=${`${label}: the idle check, the board's messages and its columns`} onClick=${open}>⚙<//>`;
  }
  return html`<${Button} size="sm" class="tb-options-btn" title="The idle check, the board's messages and its columns"
    onClick=${open}>⚙ ${label}<//>`;
}

// BoardOptionsSummary is the section line's note of what the board's
// options change from the inherited behaviour (the folded line shows it
// too). It sits inside the section's toggle, so it is text, not controls.
const SUMMARY_ICON = { idle: '⏱ ', events: '✉ ', limits: '' };

export function BoardOptionsSummary({ options, columns = [] }) {
  const parts = optionsSummary(options, columns);
  if (!parts.length) return null;
  return html`<span class="tb-options-summary">${parts.map((part) => html`<span key=${part.key}
    class=${`ui-tag tb-options-chip${part.fill ? ` tb-lane-${part.fill}` : ''}`} data-tb-options-summary=${part.section}
    data-tb-options-lane=${part.lane} title=${part.title}>${SUMMARY_ICON[part.section] ?? ''}${part.text}</span>`)}</span>`;
}

// Source is a value's provenance and, where the board sets it, its ↺. A
// value changed and not yet saved has a dot. compact (the event table) says
// "set here" for "set on this board".
function Source({ view, draft, saved, keys, canWrite, onReset, label, compact = false }) {
  const set = keys.filter((key) => Object.hasOwn(draft, key));
  const pick = (obj) => Object.fromEntries(keys.filter((k) => Object.hasOwn(obj, k)).map((k) => [k, obj[k]]));
  const changed = changedKeys(pick(saved), pick(draft)).length > 0;
  const inherited = keys.map((key) => inheritedOf(view, key, draft));
  let text;
  if (set.length) {
    const from = inherited[0];
    text = compact ? 'set here' : keys.length === 1 ? `set on this board · ${from.source}: ${valueText(from.value)}` : 'set on this board';
  } else {
    text = [...new Set(inherited.map((v) => v.source))].map(sourceText).join(' · ');
  }
  return html`<span class=${`tb-opt-src${set.length ? ' board' : ''}${changed ? ' changed' : ''}`} data-tb-opt-src=${keys.join(' ')}>
    ${changed && html`<span class="tb-opt-dot" title="Changed, not saved yet" aria-label="unsaved">●</span>`}
    <span class="tb-opt-src-text">${text}</span>
    ${set.length > 0 && canWrite && html`<${Button} size="sm" variant="ghost" icon=${true} class="tb-opt-reset"
      aria-label=${`Reset ${label}`} title=${`Inherit it again: ${inherited.map((v) => `${valueText(v.value)} (${sourceText(v.source)})`).join(', ')}`}
      onClick=${() => onReset(keys)}>↺<//>`}
  </span>`;
}

// IdleSection is the Triggers tab's idle check.
function IdleSection({ view, draft, saved, columns, canWrite, set, reset, errors }) {
  const eff = (key) => effectiveOf(view, draft, key);
  const globallyOff = !!view?.resolved?.idle?.globally_off;
  const enabled = eff('idle.enabled').value;
  const tell = eff('idle.tell').value || [];
  const checked = eff('idle.check_columns').value || [];
  const src = (key, label) => html`<${Source} view=${view} draft=${draft} saved=${saved} keys=${[key]} canWrite=${canWrite} onReset=${reset} label=${label} />`;
  const muted = !enabled || globallyOff;
  return html`<section class="tb-opt-section" data-tb-opt-section="idle" aria-labelledby="tb-opt-idle-title">
    <div class="tb-opt-head">
      <label class="tb-opt-switch">
        <${Switch} checked=${enabled && !globallyOff} disabled=${!canWrite || globallyOff} aria-label="Idle check"
          onChange=${(event) => set('idle.enabled', event.currentTarget.checked)} />
        <b id="tb-opt-idle-title">Idle check</b>
      </label>
      <span class=${`ui-tag tb-opt-state${enabled && !globallyOff ? ' on' : ''}`}>${globallyOff ? 'off on every board' : enabled ? 'on' : 'off'}</span>
      ${src('idle.enabled', 'the idle check')}
    </div>
    ${globallyOff && html`<p class="tb-opt-note ui-hint" role="note" data-tb-opt-globally-off="">
      Idle check disabled globally: <code>taskboard.idle_grace</code> is 0, which turns it off on every board whatever a board sets.</p>`}
    <p class="tb-opt-desc ui-hint">When an assigned, started task's agent sits idle without reporting for the grace period, nudge it and tell
      the recipients below. Off by default on every board.</p>
    <div class=${`tb-opt-grid${muted ? ' muted' : ''}`}>
      <label class="tb-opt-label" for="tb-opt-grace">Grace period</label>
      <span class="tb-opt-ctl">
        <${Input} id="tb-opt-grace" size="sm" class="tb-opt-grace" value=${eff('idle.grace').value} disabled=${!canWrite}
          invalid=${!!errors['idle.grace']} aria-describedby=${errors['idle.grace'] ? 'tb-opt-grace-error' : undefined}
          spellcheck="false" autocomplete="off" onInput=${(event) => set('idle.grace', event.currentTarget.value.trim())} />
        ${errors['idle.grace'] && html`<span id="tb-opt-grace-error" class="ui-error">${errors['idle.grace']}</span>`}
      </span>
      ${src('idle.grace', 'the grace period')}

      <span class="tb-opt-label">Nudge</span>
      <span class="tb-opt-ctl">
        <${Check} checked=${eff('idle.nudge').value} disabled=${!canWrite} onChange=${(event) => set('idle.nudge', event.currentTarget.checked)}>
          the idle assignee<//>
        <span class="ui-hint">an inbox nudge with its report commands</span>
      </span>
      ${src('idle.nudge', 'the nudge')}

      <span class="tb-opt-label" id="tb-opt-tell-label">Tell</span>
      <span class="tb-opt-ctl" role="group" aria-labelledby="tb-opt-tell-label">
        ${IDLE_TELL.map((who) => html`<${Check} key=${who} title=${RECIPIENT_TITLE[who]} checked=${tell.includes(who)} disabled=${!canWrite}
          onChange=${(event) => set('idle.tell', event.currentTarget.checked ? [...tell, who] : tell.filter((r) => r !== who))}>${who === 'owners' ? 'group owners' : who}<//>`)}
        ${tell.length === 0 && html`<span class="ui-hint">no one</span>`}
      </span>
      ${src('idle.tell', 'who is told')}

      <span class="tb-opt-label" id="tb-opt-skip-label">Not idle while the task is…</span>
      <span class="tb-opt-ctl tb-opt-skips" role="group" aria-labelledby="tb-opt-skip-label">
        ${columns.length > 0
          ? html`<span class="tb-opt-skip-line">in a custom column:
              ${columns.map((column) => html`<${Check} key=${column.slug} checked=${!checked.includes(column.slug)} disabled=${!canWrite}
                title=${`Unticked, a task in ${column.label} is idle-checked as one in progress is`}
                onChange=${(event) => set('idle.check_columns', event.currentTarget.checked ? checked.filter((s) => s !== column.slug) : [...checked, column.slug])}>${column.label}<//>`)}
              <span class="ui-hint">(waiting on a reviewer or CI)</span></span>`
          : html`<span class="ui-hint">in a custom column (the board has none)</span>`}
        <span class="tb-opt-skip-line ui-hint">waiting on an unfinished dependency, or blocked: always</span>
      </span>
      ${src('idle.check_columns', 'the checked columns')}
    </div>
  </section>`;
}

// EventsSection is the Triggers tab's event messages: one row per event, a
// switch and its recipients.
function EventsSection({ view, draft, saved, canWrite, set, reset, setMany }) {
  const events = eventRows(view);
  const anyOn = events.some(({ name }) => effectiveOf(view, draft, eventKey(name, 'enabled')).value);
  const anySet = Object.keys(draft).some((key) => key.startsWith('events.'));
  return html`<section class="tb-opt-section" data-tb-opt-section="events" aria-labelledby="tb-opt-events-title">
    <div class="tb-opt-head">
      <b id="tb-opt-events-title">Event messages</b>
      <span class="ui-hint">the defaults are today's behaviour</span>
      <span class="spacer"></span>
      ${canWrite && html`<${Button} size="sm" variant="link" class="tb-opt-all-off" disabled=${!anyOn}
        title="Turn every event message off on this board" onClick=${() => setMany(events.map(({ name }) => [eventKey(name, 'enabled'), false]))}>all off<//>
        <${Button} size="sm" variant="link" class="tb-opt-defaults" disabled=${!anySet}
        title="Every event message back to its default" onClick=${() => reset(['events.'])}>↺ defaults<//>`}
    </div>
    <div class="tb-opt-table-wrap">
      <table class="ui-compact tb-opt-events">
        <thead><tr>
          <th scope="col">On</th><th scope="col">When</th>
          ${EVENT_TO.map((who) => html`<th key=${who} scope="col" title=${RECIPIENT_TITLE[who]}>${RECIPIENT_LABEL[who]}</th>`)}
          <th scope="col"><span class="tb-opt-sr">Source</span></th>
        </tr></thead>
        <tbody>${events.map(({ name, label, title }) => {
          const on = effectiveOf(view, draft, eventKey(name, 'enabled')).value;
          const to = effectiveOf(view, draft, eventKey(name, 'to')).value || [];
          return html`<tr key=${name} class=${on ? '' : 'off'} data-tb-opt-event=${name}>
            <td><${Switch} checked=${on} disabled=${!canWrite} aria-label=${`Send the ${label} message`}
              onChange=${(event) => set(eventKey(name, 'enabled'), event.currentTarget.checked)} /></td>
            <td class="tb-opt-when" title=${title}>${label}</td>
            ${EVENT_TO.map((who) => html`<td key=${who}><${Check} checked=${to.includes(who)} disabled=${!canWrite}
              aria-label=${`${label}: ${RECIPIENT_LABEL[who]}`}
              onChange=${(event) => set(eventKey(name, 'to'), event.currentTarget.checked ? [...to, who] : to.filter((r) => r !== who))} /></td>`)}
            <td><${Source} view=${view} draft=${draft} saved=${saved} keys=${[eventKey(name, 'enabled'), eventKey(name, 'to')]}
              canWrite=${canWrite} onReset=${() => reset([`events.${name}.`])} label=${`the ${label} message`} compact=${true} /></td>
          </tr>`;
        })}</tbody>
      </table>
    </div>
    <p class="ui-hint tb-opt-foot">Operator is the dashboard's Messages. Whoever caused an event is never told of it, and nobody hears twice.</p>
  </section>`;
}

// LimitInput is a lane's max: a number, empty for no limit.
function LimitInput({ view, draft, lane, label, canWrite, set, errors }) {
  const key = laneKey(lane, 'max');
  const max = effectiveOf(view, draft, key).value;
  const [text, setText] = useState(max > 0 ? String(max) : '');
  // Follow the draft when it changes from elsewhere (a reset, a reread).
  useEffect(() => {
    if (Number.isNaN(max)) return; // a typo being fixed: keep what was typed
    const shown = text.trim() === '' ? 0 : Number(text);
    if (shown !== max) setText(max > 0 ? String(max) : '');
  }, [max]);
  const id = `tb-opt-max-${lane}`;
  return html`<span class="tb-opt-limit">
    <${Input} id=${id} size="sm" class="tb-opt-max" inputmode="numeric" value=${text} placeholder="—" disabled=${!canWrite}
      aria-label=${`${label} limit`} invalid=${!!errors[key]} aria-describedby=${errors[key] ? `${id}-error` : undefined}
      autocomplete="off" onInput=${(event) => {
        const raw = event.currentTarget.value.trim();
        setText(event.currentTarget.value);
        set(key, raw === '' ? 0 : /^\d+$/.test(raw) ? Number(raw) : Number.NaN);
      }} />
    ${errors[key] && html`<span id=${`${id}-error`} class="ui-error">${errors[key]}</span>`}
  </span>`;
}

// AtLimitSelect is a lane's own at-limit choice, or the board's.
function AtLimitSelect({ view, draft, lane, label, canWrite, set, reset }) {
  const key = laneKey(lane, 'at_limit');
  const board = effectiveOf(view, draft, 'limits.at_limit').value;
  const own = Object.hasOwn(draft, key) ? draft[key] : '';
  const name = (value) => AT_LIMITS.find(([k]) => k === value)?.[1].toLowerCase() || value;
  return html`<${OptionalSelect} size="sm" class="tb-opt-at" value=${own} disabled=${!canWrite} aria-label=${`${label}: at the limit`}
    unset=${`board (${name(board)})`} options=${AT_LIMITS.map(([k, text]) => [k, text.toLowerCase()])}
    onChange=${(value) => (value ? set(key, value) : reset([key]))} />`;
}

// LimitsTab is the board's work-in-progress limits: a cap on In progress
// and on each custom column, what a full lane does, and whether a cap
// counts the lane or each assignee in it.
function LimitsTab({ view, draft, saved, columns, entry, canWrite, set, reset, errors }) {
  const lanes = [{ lane: IN_PROGRESS, label: 'In progress', sub: 'started tasks of agents' }, ...columns.map((c) => ({ lane: c.slug, label: c.label, sub: c.slug }))];
  const per = effectiveOf(view, draft, 'limits.per').value;
  const atLimit = effectiveOf(view, draft, 'limits.at_limit').value;
  const now = (lane) => {
    const limit = laneLimit(entry?.options, lane);
    if (!limit) return '';
    return per === 'assignee' && limit.per === 'assignee' ? `now ${limit.count} (fullest)` : limit.per === 'board' ? `now ${limit.count}` : '';
  };
  const src = (keys, label) => html`<${Source} view=${view} draft=${draft} saved=${saved} keys=${keys} canWrite=${canWrite} onReset=${reset} label=${label} />`;
  return html`<section class="tb-opt-section" data-tb-opt-section="limits" aria-label="Limits">
    <p class="tb-opt-desc ui-hint">How many tasks a lane holds at once: In progress, and each custom column. Empty is no limit.
      When a full lane refuses the operator's change, the dashboard offers to go over it; agents follow the rule.</p>
    <div class="tb-opt-grid">
      ${lanes.map(({ lane, label, sub }) => html`
        <span key=${`l-${lane}`} class="tb-opt-label" data-tb-opt-lane=${lane}>${label} <span class="ui-hint">${sub}</span></span>
        <span key=${`c-${lane}`} class="tb-opt-ctl">
          <${LimitInput} view=${view} draft=${draft} lane=${lane} label=${label} canWrite=${canWrite} set=${set} errors=${errors} />
          <span class="ui-hint" data-tb-opt-now=${lane}>${now(lane)}</span>
        </span>
        <span key=${`s-${lane}`}>${src([laneKey(lane, 'max')], `the ${label} limit`)}</span>`)}

      <span class="tb-opt-label" id="tb-opt-per-label">Count per</span>
      <span class="tb-opt-ctl" role="radiogroup" aria-labelledby="tb-opt-per-label">
        ${PER.map(([value, text]) => html`<${Check} key=${value} type="radio" name="tb-opt-per" checked=${per === value} disabled=${!canWrite}
          onChange=${() => set('limits.per', value)}>${text}<//>`)}
        <span class="ui-hint">${per === 'assignee' ? 'each agent has its own N' : 'every task in the lane counts'}</span>
      </span>
      ${src(['limits.per'], 'what a limit counts')}
    </div>
    <div class="tb-opt-head">
      <b id="tb-opt-at-title">At the limit</b>
      <span class="ui-hint">one choice for the board; a lane can override it on the Columns tab</span>
      <span class="spacer"></span>
      ${src(['limits.at_limit'], 'what a full lane does')}
    </div>
    <div class="tb-opt-at-choices" role="radiogroup" aria-labelledby="tb-opt-at-title">
      ${AT_LIMITS.map(([value, text, says]) => html`<${Check} key=${value} type="radio" name="tb-opt-at" checked=${atLimit === value} disabled=${!canWrite}
        labelClass="tb-opt-at-choice" onChange=${() => set('limits.at_limit', value)}><b>${text}</b> <span class="ui-hint">${says}</span><//>`)}
    </div>
  </section>`;
}

// ColumnsTab is the board's columns: the built-in ones, and each custom
// column with whether it is idle-checked and its ⋯ (rename, move, remove).
// Column changes apply at once (taskboard columns …, taskboard.columns);
// the idle-check choice is part of the options draft.
function ColumnsTab({ group, view, draft, saved, columns, canWrite, set, reset, actions, focusColumn, errors }) {
  const naming = useColumnNaming(group, canWrite ? actions : null);
  const checked = effectiveOf(view, draft, 'idle.check_columns').value || [];
  const limit = (lane, label) => html`<td><${LimitInput} view=${view} draft=${draft} lane=${lane} label=${label} canWrite=${canWrite} set=${set} errors=${errors} /></td>
    <td><${AtLimitSelect} view=${view} draft=${draft} lane=${lane} label=${label} canWrite=${canWrite} set=${set} reset=${reset} /></td>`;
  const builtin = (label, idle) => html`<tr class="tb-opt-builtin"><td>${label}</td><td class="ui-hint">—</td><td></td><td class="ui-hint">${idle}</td><td class="ui-hint">built-in</td></tr>`;
  return html`<section class="tb-opt-section" data-tb-opt-section="columns" aria-label="Columns">
    ${naming.dialog}
    <table class="ui-compact tb-opt-columns">
      <thead><tr><th scope="col">Column</th><th scope="col">Limit</th><th scope="col">At limit</th><th scope="col">Idle check</th><th scope="col"><span class="tb-opt-sr">Actions</span></th></tr></thead>
      <tbody>
        ${builtin('Next · Ideas', '—')}
        <tr data-tb-opt-column=${IN_PROGRESS} class=${focusColumn === IN_PROGRESS ? 'focus' : ''}>
          <td>In progress</td>${limit(IN_PROGRESS, 'In progress')}<td class="ui-hint">checked</td><td class="ui-hint">built-in</td></tr>
        ${columns.map((column) => html`<tr key=${column.slug} data-tb-opt-column=${column.slug} class=${focusColumn === column.slug ? 'focus' : ''}>
          <td>${column.label} <span class="ui-hint">${column.slug}</span></td>
          ${limit(column.slug, column.label)}
          <td><${Check} checked=${!checked.includes(column.slug)} disabled=${!canWrite}
            title=${`Ticked, a task in ${column.label} never stalls: its agent waits on a review or CI`}
            onChange=${(event) => set('idle.check_columns', event.currentTarget.checked ? checked.filter((s) => s !== column.slug) : [...checked, column.slug])}>skip<//></td>
          <td>${canWrite && actions && html`<${RowMenu} label=${`the ${column.label} column`} buttonClass="tb-col-more"
            items=${columnMenuItems({ column, columns, group, actions, onRename: (c) => naming.open('rename', c) })} />`}</td>
        </tr>`)}
        ${builtin('Blocked · Done · Dropped', '—')}
      </tbody>
    </table>
    <p class="ui-hint tb-opt-foot">
      ${columns.length} of ${COLUMNS_CAP} custom columns. Adding, renaming, moving and removing a column applies at once
      (<code>taskboard columns …</code>); removing one moves its tasks back to Next. Skip saves with the options.</p>
    <div class="tb-opt-col-foot">
      ${canWrite && html`<${AddColumnButton} naming=${naming} columns=${columns} />`}
      <span class="spacer"></span>
      <span class="ui-hint">Skip:</span>
      <${Source} view=${view} draft=${draft} saved=${saved} keys=${['idle.check_columns']} canWrite=${canWrite} onReset=${reset} label="the checked columns" />
    </div>
  </section>`;
}

// DrawerClose is the drawer's ✕: the dialog's own guarded close, so unsaved
// changes ask first as Esc and Cancel do.
function DrawerClose() {
  const close = useDialogClose();
  return html`<${Button} size="sm" variant="ghost" icon=${true} class="tb-opt-close" aria-label="Close board options" title="Close (Esc)"
    onClick=${() => { void close(); }}>✕<//>`;
}

// BoardOptionsDrawer is the drawer itself. group is the board's own name,
// shownName the name the view shows (name@node in the fused view); api is
// the write client (null without taskboard.write) and readApi the read one;
// entry is the board's context entry, whose options.version moving rereads
// the options; actions are the board's column actions.
export function BoardOptionsDrawer({ group, shownName = group.name, nodeName = '', entry, api = null, readApi = null, actions = null,
  section = '', column = '', wizard = false, onSaved = null, onClose }) {
  const canWrite = !!api && typeof api.saveOptions === 'function';
  const reader = readApi || api;
  const [view, setView] = useState(null);
  const [loadError, setLoadError] = useState('');
  const [saved, setSaved] = useState({});
  const [draft, setDraft] = useState({});
  const [tab, setTab] = useState(SECTION_TAB[section] || 'triggers');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [stale, setStale] = useState(false);
  const dirtyRef = useRef(false);
  const bodyRef = useRef(null);
  const changed = changedKeys(saved, draft);
  dirtyRef.current = changed.length > 0;
  const columns = entry?.columns || [];
  const slugs = columns.map((c) => c.slug);
  const version = entry?.options?.version;

  const take = (next) => {
    const flat = flatOptions(next.options);
    setView(next);
    setSaved(flat);
    setDraft(flat);
    setStale(false);
  };
  // Read on open, and again when the board's options move (the CLI, a
  // linked node): silently while nothing is edited, else the drawer says so
  // and Save carries the edits onto the newer version.
  // A reread is due when the board's options moved past the version shown,
  // or when the drawer said so and its edits are gone again.
  const behind = !!view && version !== undefined && version !== view.version;
  const clean = changed.length === 0;
  useEffect(() => {
    if (!reader || typeof reader.options !== 'function') {
      setLoadError("This board's node does not serve board options; update tclaude there.");
      return undefined;
    }
    if (view && !behind) return undefined;
    if (view && dirtyRef.current) { setStale(true); return undefined; }
    let live = true;
    reader.options(group.name).then((next) => {
      if (!live) return;
      // Edits begun while it was read win: say the options moved instead.
      if (view && dirtyRef.current) { setStale(true); return; }
      take(next);
      setLoadError('');
    }).catch((cause) => { if (live) setLoadError(cause?.status === 404 ? "This board's node does not serve board options; update tclaude there." : cause?.message || String(cause)); });
    return () => { live = false; };
  }, [group.name, version, behind && clean]);
  // A link or a column's ⋯ names a section: show it.
  useEffect(() => {
    if (section && SECTION_TAB[section]) setTab(SECTION_TAB[section]);
  }, [section, column]);
  useEffect(() => {
    if (!view) return;
    const where = column ? `[data-tb-opt-column="${CSS.escape?.(column) ?? column}"]` : section ? `[data-tb-opt-section="${section}"]` : '';
    if (where) bodyRef.current?.querySelector(where)?.scrollIntoView?.({ block: 'nearest' });
  }, [view, tab, section, column]);

  const set = (key, value) => setDraft((d) => setOption(view, saved, d, key, value, slugs));
  const setMany = (pairs) => setDraft((d) => pairs.reduce((acc, [key, value]) => setOption(view, saved, acc, key, value, slugs), d));
  const reset = (keys) => setDraft((d) => keys.reduce((acc, key) => resetOption(acc, key), d));
  const errors = draftErrors(draft);
  const invalid = Object.keys(errors).length > 0;

  const save = async () => {
    if (!canWrite || busy || invalid || !view || !changed.length) return;
    setBusy(true);
    setError('');
    try {
      const next = await api.saveOptions(group.name, patchOf(saved, draft, slugs), view.version);
      take(next);
      shellToast(`Saved ${shownName}'s board options`);
      onSaved?.();
      onClose();
    } catch (cause) {
      const current = cause?.status === 409 && cause?.body?.current;
      if (current) {
        // Changed elsewhere since it was read: carry this draft onto the
        // newer options and let the operator look before saving again.
        const newer = flatOptions(current.options);
        const rebased = rebaseDraft(saved, draft, newer);
        setView(current);
        setSaved(newer);
        setDraft(rebased);
        setStale(false);
        setError('These options changed elsewhere while you edited them. Your changes are now applied over the newer version: check them and Save again.');
      } else {
        setError(cause?.message || String(cause));
      }
    } finally {
      setBusy(false);
    }
  };

  const title = `${wizard ? 'Quest board' : 'Board'} options · ${shownName}`;
  const where = nodeName || 'this node';
  const readOnly = !canWrite && html`<p class="tb-opt-readonly" role="note" data-tb-opt-readonly="">
    Read-only: ${nodeName ? `no taskboard.write on ${nodeName}` : 'you cannot change this board (taskboard.write)'}.</p>`;
  const count = changed.length;
  const footerStart = count
    ? html`<span class="ui-hint tb-opt-unsaved" data-tb-opt-unsaved=${count}>${count} unsaved ${count === 1 ? 'change' : 'changes'}</span>`
    : html`<span class="ui-hint tb-opt-cli">Stored with the board · <code>tclaude agent taskboard options show ${group.name}</code></span>`;
  const props = { view, draft, saved, columns, canWrite, set, setMany, reset, errors };
  return html`<${Dialog} id="taskboard-options-modal" overlayClass="tb-opt-overlay" dialogClass="cron-create-modal tb-opt-drawer"
    titleId="taskboard-options-title" title=${html`<span aria-hidden="true">⚙</span> ${title} <span class="ui-tag tb-opt-node">${where}</span>`}
    onClose=${onClose} busy=${busy} dirty=${() => dirtyRef.current} confirmDiscard=${shellConfirmDiscard}
    onSubmitHotkey=${() => void save()} error=${error || undefined}
    cancel=${canWrite ? 'Cancel' : 'Close'} footerStart=${footerStart}
    footer=${canWrite && html`<${Button} variant="primary" id="taskboard-options-save" busy=${busy} disabled=${busy || invalid || !count || !view}
      onClick=${() => void save()}>Save<//>`}>
    <${DrawerClose} />
    ${readOnly}
    ${stale && html`<p class="tb-opt-note ui-hint" role="status" data-tb-opt-stale="">
      These options changed elsewhere since you opened them. Save applies your changes over the newer version.</p>`}
    <${Tabs} label="Board options" variant="seg" class="tb-opt-tabs" value=${tab} onSelect=${setTab}
      items=${TABS.map(([key, label]) => ({ key, label, attrs: { 'data-tb-opt-tab': key } }))} />
    <div class="tb-opt-body" ref=${bodyRef}>
      ${loadError && html`<p class="ui-error-box" role="alert">${loadError}</p>`}
      ${!view && !loadError && html`<p class="ui-hint" aria-busy="true">Reading the board's options…</p>`}
      ${view && tab === 'triggers' && html`<${IdleSection} ...${props} /><${EventsSection} ...${props} />`}
      ${view && tab === 'limits' && html`<${LimitsTab} ...${props} entry=${entry} />`}
      ${view && tab === 'columns' && html`<${ColumnsTab} group=${group} ...${props} actions=${actions} focusColumn=${column} />`}
    </div>
  <//>`;
}
