// groups-taskboard-tracker.js -- "+ New task › From a tracker…" (docs/taskboard.md):
// a task made from a ticket elsewhere (awb, Linear, GitHub, or any link).
//
//   One ticket     paste its URL (or an id: tcl-a1b2c3, LIN-123, owner/repo#12).
//                  The title is looked up through the tracker, as an import
//                  takes it, and stays editable; a label (the ref's shown
//                  name, as the Task column's), a brief and Idea or Next go
//                  with it. `taskboard add <group> "title" --ref <url>
//                  --ref-label … --body … --resolve-ref [--next]`.
//   Several ids    the bulk import: one task per id, titled from the tracker
//                  (`add --from-awb` / `--from-linear` / `--from-github`).
//
// A title left empty is the tracker's: the ticket is imported, and the server
// titles it (it then takes the brief and the label as an edit).

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Button, Input, Seg, Select, Textarea } from './ui-components.js';
import { Dialog } from './ui-dialog.js';
import { shellConfirmDiscard, shellToast } from './shell-state.js';
import { importRefs, isTrackerLine } from './taskboard-actions.js';
import { MarkdownEditor, submitKeyLabel } from './markdown-editor.js';
import { StagePicker } from './groups-taskboard-stage.js';

const html = htm.bind(h);

// LOOKUP_DELAY_MS is the pause after typing before a ticket is looked up: a
// paste looks up at once, typing an id does not look up every prefix.
export const LOOKUP_DELAY_MS = 350;

const KIND_NAME = { awb: 'awb', linear: 'Linear', github: 'GitHub', url: 'link' };

// importSummary says what an import did beyond the count: the items that
// kept their id as the title (with the server's note) and the refused ones.
export function importSummary(out) {
  const results = out?.results || [];
  const notes = results.filter((result) => result.task && result.note);
  const refused = results.filter((result) => result.error);
  const parts = [];
  if (notes.length) parts.push(`${notes.length} kept the id as the title (${notes.map((result) => `${result.input}: ${result.note}`).join('; ')})`);
  if (refused.length) parts.push(`${refused.length} refused (${refused.map((result) => `${result.input}: ${result.code || result.error}`).join('; ')})`);
  return parts.join(' · ');
}

// sameRef finds a live task of the board that already carries the ticket
// the lookup describes (by its link, or by its tracker id).
export function sameRef(tasks, found) {
  if (!found) return null;
  const url = String(found.url || '').replace(/\/+$/, '');
  return (tasks || []).find((task) => {
    const ref = task.ref;
    if (!ref || task.status === 'done' || task.status === 'dropped') return false;
    if (url && String(ref.url || '').replace(/\/+$/, '') === url) return true;
    return (found.kind === 'awb' || found.kind === 'linear') && ref.kind === found.kind && !!found.label && ref.label === found.label;
  }) || null;
}

// trackerTaskPlan is the one-ticket form's write. With a title, one add that
// carries the ref (linked through its tracker), its label, the brief and the
// stage. Without one, an import, which takes the tracker's title; the brief
// and the label follow as an edit. `link` is the ticket's URL as the lookup
// found it: the add sends it, so a label never stands in for the tracker id
// (a ref with no link is named by its id, and a label would replace it).
export function trackerTaskPlan(draft) {
  const link = String(draft.link || '').trim();
  const ref = link || String(draft.ref || '').trim();
  const title = String(draft.title || '').trim();
  const refLabel = /^https?:\/\//i.test(ref) ? String(draft.label || '').trim() : '';
  const body = String(draft.body || '').trim();
  const next = draft.start === 'next';
  if (title) {
    return { kind: 'add', task: { title, ref, refLabel, body, stage: next ? 'next' : 'idea', resolveRef: true } };
  }
  return { kind: 'import', ref, refLabel, body, next };
}

// submitTrackerTask runs the plan: { task, warning }. A refusal before the
// task landed throws; one after it is the warning.
export async function submitTrackerTask(api, groupName, draft) {
  const plan = trackerTaskPlan(draft);
  if (plan.kind === 'add') return { task: await api.create(groupName, plan.task), warning: '' };
  const out = await api.importRefs(groupName, [plan.ref], { next: plan.next });
  const result = out?.results?.[0];
  if (out?.failed) throw new Error(out.failed.message);
  if (!result?.task?.id) throw new Error(result?.error || 'the ticket was not added');
  let task = result.task;
  const warning = result.note ? `kept the id as the title: ${result.note}` : '';
  if (plan.body || plan.refLabel) {
    try {
      const change = {};
      if (plan.body) change.body = plan.body;
      if (plan.refLabel && task.ref?.url) { change.ref = task.ref.url; change.refLabel = plan.refLabel; }
      task = await api.edit(task.id, change) || task;
    } catch (cause) {
      return { task, warning: `added as ${task.id}, but its ${plan.body ? 'brief' : 'label'} was not saved: ${cause?.message || cause}` };
    }
  }
  return { task, warning };
}

// useRefLookup looks the ticket up while it is being written: { state:
// 'idle' | 'looking' | 'done' | 'failed', ref, result, error }. Only the
// answer for the ticket in the box counts.
function useRefLookup(api, groupName, ref) {
  const [lookup, setLookup] = useState({ state: 'idle', ref: '' });
  const wanted = String(ref || '').trim();
  useEffect(() => {
    if (!wanted || !isTrackerLine(wanted) || typeof api?.lookupRef !== 'function') {
      setLookup({ state: 'idle', ref: wanted });
      return undefined;
    }
    let live = true;
    const timer = globalThis.setTimeout(() => {
      setLookup({ state: 'looking', ref: wanted });
      api.lookupRef(groupName, wanted).then((result) => {
        if (live) setLookup({ state: 'done', ref: wanted, result });
      }).catch((cause) => {
        if (live) setLookup({ state: 'failed', ref: wanted, error: cause?.message || String(cause), code: cause?.code || '' });
      });
    }, LOOKUP_DELAY_MS);
    return () => { live = false; globalThis.clearTimeout(timer); };
  }, [wanted]);
  return lookup.ref === wanted ? lookup : { state: 'idle', ref: wanted };
}

function refHint(draft, lookup, existing) {
  const ref = draft.ref.trim();
  if (!ref) return 'a ticket URL: awb, Linear, GitHub or any link; or an id such as tcl-a1b2c3, LIN-123 or owner/repo#12';
  if (!isTrackerLine(ref)) return 'one URL or id, no spaces (Several ids takes a list)';
  if (existing) return `already on the board as ${existing.id}: “${existing.title}”`;
  switch (lookup.state) {
    case 'looking': return 'looking up its title…';
    case 'failed': return lookup.code === 'invalid_arg' ? lookup.error : `could not look it up (${lookup.error}); give a title, or leave it empty to import it`;
    case 'done': {
      const r = lookup.result || {};
      const what = `${KIND_NAME[r.kind] || r.kind || 'ref'}${r.label && r.label !== r.url ? ` ${r.label}` : ''}`;
      return r.resolved ? `${what} · title from the tracker` : `${what} · ${r.note || 'not looked up'}`;
    }
    default: return '';
  }
}

const TRACKER_PLACES = [
  { key: 'idea', label: 'Idea', wizard: 'Vision' },
  { key: 'next', label: 'Next', wizard: 'Next' },
];

const EMPTY = { ref: '', title: '', label: '', body: '', start: 'idea' };

// TrackerTaskDialog is the form. `initial` is a lanes cell's start state:
// an import lands in Ideas or Next, so Next for any cell but Ideas and Done.
export function TrackerTaskDialog({ group, entry, api, wizard = false, initial = {}, onClose, onDone = () => {} }) {
  const start = initial.start && initial.start !== 'idea' && initial.start !== 'done' ? 'next' : 'idea';
  const [mode, setMode] = useState('one');
  const [draft, setDraft] = useState({ ...EMPTY, start });
  // The title is the tracker's until it is typed in.
  const [titleTyped, setTitleTyped] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [note, setNote] = useState('');
  const sending = useRef(false);
  const refInput = useRef(null);
  const [kind, setKind] = useState('awb');
  const [ids, setIds] = useState('');
  const lookup = useRefLookup(api, group.name, mode === 'one' ? draft.ref : '');
  const found = lookup.state === 'done' ? lookup.result : null;
  // The looked-up title fills the title until the operator types one.
  useEffect(() => {
    if (titleTyped) return;
    const looked = found?.resolved ? found.title || '' : '';
    setDraft((current) => (current.title === looked ? current : { ...current, title: looked }));
  }, [found, titleTyped]);
  const existing = sameRef(entry?.tasks, found);
  // A ref with no link (a bare id its tracker did not resolve) is named by
  // that id: a label would replace it.
  const unlinked = !!found && !found.url && !/^https?:\/\//i.test(draft.ref.trim());
  const set = (key) => (value) => setDraft((current) => ({ ...current, [key]: value }));
  const refs = importRefs(kind, ids);
  const noun = wizard ? 'quest' : 'task';
  // A ticket already on the board is not added twice (the import refused it
  // as a duplicate; an add with a title would not).
  const oneReady = !existing && isTrackerLine(draft.ref) && lookup.state !== 'looking' && !(lookup.state === 'failed' && lookup.code === 'invalid_arg');
  const ready = mode === 'one' ? oneReady : refs.length > 0;
  const dirty = mode === 'one' ? Object.keys(EMPTY).some((key) => key !== 'start' && draft[key].trim()) : ids.trim().length > 0;

  const submitOne = async () => {
    const out = await submitTrackerTask(api, group.name, { ...draft, link: found?.url || '' });
    if (out.warning) shellToast(out.warning, true);
    else shellToast(`Added ${out.task?.id || ''} to ${group.name}`);
    onDone(out);
    onClose();
  };
  const submitMany = async () => {
    setNote(`looking up ${refs.length} title${refs.length === 1 ? '' : 's'}…`);
    const out = await api.importRefs(group.name, refs, { next: draft.start === 'next' });
    onDone(out);
    const more = importSummary(out);
    if (!more && !out?.failed) { shellToast(`Added ${out?.created || 0} to ${group.name}`); onClose(); return; }
    // Only the ids that were not sent stay in the box, so Import again
    // never adds the landed ones twice.
    setIds(out?.failed ? out.failed.remaining.join(', ') : '');
    setNote(`${out?.created || 0} added${more ? ` · ${more}` : ''}${out?.failed
      ? ` · stopped: ${out.failed.message}; ${out.failed.remaining.length} not sent, left in the box` : ''}`);
  };
  const submit = async () => {
    if (!ready || sending.current) return;
    sending.current = true;
    setBusy(true);
    setError('');
    try {
      if (mode === 'one') await submitOne(); else await submitMany();
    } catch (cause) {
      setError(cause?.message || String(cause));
      setNote('');
    } finally {
      sending.current = false;
      setBusy(false);
    }
  };

  const row = (label, field, hint = null) => html`<div class="cron-create-row tb-new-row"><span class="cron-create-label">${label}</span>
    <span class="tb-new-field">${field}${hint && html`<span class="ui-hint tb-new-hint">${hint}</span>`}</span></div>`;
  const enter = (event) => { if (event.key === 'Enter' && !event.isComposing) { event.preventDefault(); void submit(); } };
  // An import lands in Ideas or Next only, so the picker offers those two.
  const startPick = html`<${StagePicker} id="taskboard-tracker-stage" value=${draft.start} onChange=${(key) => set('start')(key || 'idea')}
    places=${TRACKER_PLACES} wizard=${wizard} disabled=${busy} ariaLabel="Starts in" />`;
  const plan = mode === 'one' ? trackerTaskPlan(draft) : null;
  const cli = mode === 'one'
    ? plan.kind === 'add' ? `tclaude agent taskboard add ${group.name} "…" --ref … --resolve-ref` : `tclaude agent taskboard add ${group.name} --from-… <id>`
    : `tclaude agent taskboard add ${group.name} --from-${kind} …`;
  const submitLabel = busy ? 'Adding…' : mode === 'one' ? `Add ${noun}` : `Import ${refs.length || ''}`.trim();
  return html`<${Dialog} id="taskboard-tracker-modal" title=${`${wizard ? 'New quest' : 'New task'} from a tracker · ${group.name}`}
    onClose=${onClose} busy=${busy} dirty=${dirty} confirmDiscard=${shellConfirmDiscard} initialFocusRef=${refInput}
    onSubmitHotkey=${() => void submit()} resizeKey="tclaude.dash.modalSize.taskboard-tracker" error=${error}
    cancel="Cancel"
    footerStart=${html`<span class="ui-hint tb-new-cli"><code>${cli}</code></span>`}
    footer=${html`<${Button} variant="primary" id="taskboard-tracker-submit" busy=${busy} disabled=${!ready || busy} onClick=${() => void submit()}>${submitLabel}<//>`}>
    <div class="modal-meta tb-tracker-head">
      <${Seg} aria-label="How many" class="tb-tracker-mode">
        <${Button} size="sm" aria-pressed=${mode === 'one' ? 'true' : 'false'} data-tracker-mode="one" disabled=${busy}
          onClick=${() => { setMode('one'); setNote(''); }}>One ticket<//>
        <${Button} size="sm" aria-pressed=${mode === 'many' ? 'true' : 'false'} data-tracker-mode="many" disabled=${busy}
          onClick=${() => { setMode('many'); setNote(''); }}>Several ids<//>
      <//>
      <span>${mode === 'one'
        ? `Paste the ticket's URL; its title comes from the tracker, and you can change it.`
        : `One ${noun} per id, titled from the tracker.`}</span>
    </div>
    ${mode === 'one' ? html`
      ${row('Ticket', html`<${Input} class="tb-tracker-ref" aria-label="Ticket URL or id" required value=${draft.ref} disabled=${busy}
        domRef=${refInput} placeholder="https://… or tcl-a1b2c3, LIN-123, owner/repo#12"
        onInput=${(event) => set('ref')(event.currentTarget.value)} onKeyDown=${enter} />`, refHint(draft, lookup, existing))}
      ${row('Title', html`<${Input} class="tb-tracker-title" aria-label="Title" maxLength=${200} value=${draft.title} disabled=${busy}
        placeholder=${found && !found.resolved ? `empty: “${found.title}” (the ticket's id)` : 'from the tracker, or type your own'}
        onInput=${(event) => { setTitleTyped(event.currentTarget.value.trim() !== ''); set('title')(event.currentTarget.value); }} onKeyDown=${enter} />`)}
      ${row('Label', html`<${Input} class="tb-tracker-label" aria-label="Label" maxLength=${80} value=${draft.label} disabled=${busy || unlinked}
        placeholder=${unlinked ? `none: with no link, the ref shows as its id “${found.label}”` : found?.label ? `optional; shown instead of “${found.label}”` : 'optional: the name the ref shows as'}
        onInput=${(event) => set('label')(event.currentTarget.value)} onKeyDown=${enter} />`)}
      ${row('Brief', html`<${MarkdownEditor} class="tb-tracker-body" label="Brief" value=${draft.body} disabled=${busy}
        placeholder="optional: what the assignee needs to know beyond the ticket (Markdown)" onInput=${set('body')}
        modeKey="tclaude.dash.mdEditor.brief" hint=${`${submitKeyLabel()} adds`} />`)}`
    : html`
      ${row('Tracker', html`<${Select} class="tb-tracker-kind" aria-label="Tracker" value=${kind} disabled=${busy} onChange=${(event) => setKind(event.currentTarget.value)}>
        <option value="awb">awb</option><option value="linear">Linear</option><option value="github">GitHub</option>
      <//>`)}
      ${row('Ids', html`<${Textarea} class="tb-import-ids" rows="4" aria-label="Tracker ids" value=${ids} disabled=${busy}
        placeholder=${kind === 'github' ? 'owner/repo#12, or issue / PR URLs' : kind === 'linear' ? 'LIN-12, LIN-34' : 'tcl-a1b2c3, tcl-d4e5f6'}
        onInput=${(event) => setIds(event.currentTarget.value)} />`, note || 'separated by spaces, commas or lines; a URL works too')}`}
    ${row('Starts in', startPick, initial.to || initial.start === 'blocked' || initial.start === 'done'
      ? `a ${noun} from a tracker lands in Ideas or Next; assign, block or close it on the board afterwards` : null)}
  <//>`;
}
