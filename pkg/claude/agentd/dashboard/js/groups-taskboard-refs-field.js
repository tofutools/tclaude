// groups-taskboard-refs-field.js -- a task's refs in a form: the edit form
// and the New task form (tk_c447158c). One row per ref, with ↑ to make it
// the first (the task's ref for --ref and the List, tk_39630a0f) and ✕ to
// take it off, and a box to add one.

import { h } from 'preact';
import htm from 'htm';
import { Button, Input } from './ui-components.js';
import { OneRef, refKey, refsOf } from './groups-taskboard-card.js';

const html = htm.bind(h);

// MAX_REFS is the most refs a task has (db.MaxGroupTaskRefs).
export const MAX_REFS = 10;

// draftRefs is the refs the draft ends with: its list, and the add box's
// text as if Add was pressed.
export function draftRefs(draft) {
  const out = [];
  for (const raw of [...draft.refs, draft.newRef || '']) {
    const ref = String(raw).trim();
    if (ref && !out.includes(ref)) out.push(ref);
  }
  return out;
}

// multiRefs is whether the task's node takes several refs: a node from
// before that sends no `refs`, and would drop add_refs unsaid.
export const multiRefs = (task) => Array.isArray(task?.refs);

// refSub is a ref's kind and where it lives, under its title in the form,
// and why it has no link when its node could not link it.
function refSub(ref) {
  const sub = refKindSub(ref);
  return !ref.url && ref.link_note ? `${sub} · not linked: ${ref.link_note}` : sub;
}

function refKindSub(ref) {
  if (ref.pr) return `${/gitlab/i.test(ref.url || '') ? 'GitLab merge request' : 'GitHub pull request'}${repoOf(ref.url)}`;
  switch (ref.kind) {
    case 'awb': return 'awb issue';
    case 'linear': return 'Linear issue';
    case 'github': return `GitHub issue${repoOf(ref.url)}`;
    default: return ref.url ? hostOf(ref.url) : ref.kind;
  }
}

function repoOf(url) {
  const m = /^https?:\/\/[^/]+\/(.+?)\/(?:-\/)?(?:pull|issues|merge_requests)\//.exec(url || '');
  return m ? ` · ${m[1]}` : '';
}

function hostOf(url) {
  try { return new URL(url).host; } catch { return url; }
}

// madeFirst is refs with key first and the rest as the daemon keeps them
// after a first_ref: the task's stored refs in their order, then the new
// ones. Only the first is the form's to choose, so a second ↑ never shows an
// order Save cannot write.
export function madeFirst(task, refs, key) {
  const stored = refsOf(task).map(refKey).filter((k) => refs.includes(k));
  const rest = [...stored, ...refs.filter((k) => !stored.includes(k))];
  return [key, ...rest.filter((k) => k !== key)];
}

// RefsField is the form's refs: one row per ref (its chip, its title, ↑ to
// make it the first, and ✕ to take it off on Save) and a box to add one. A
// new ref is a row of its text until Save links it. The New task form uses
// it too, with no task (every ref is new).
export function RefsField({ task = { refs: [] }, draft, busy, looked = {}, pending = 'added on Save', onChange, onNewRef }) {
  const known = new Map(refsOf(task).map((ref) => [refKey(ref), ref]));
  const full = draftRefs({ refs: draft.refs, newRef: '' }).length >= (multiRefs(task) ? MAX_REFS : 1);
  const add = () => {
    const ref = (draft.newRef || '').trim();
    if (!ref || full) return;
    if (!draft.refs.includes(ref)) onChange([...draft.refs, ref]);
    onNewRef('');
  };
  return html`<div class="tb-edit-refs">
    ${draft.refs.length > 0 && html`<ul class="tb-refs-list">${draft.refs.map((key, i) => {
      const ref = known.get(key);
      const seen = ref && looked[key];
      const shown = seen ? { ...ref, tracker: { ...(ref.tracker || {}), ...seen } } : ref;
      const title = shown ? (shown.pr?.title || shown.tracker?.title || '') : '';
      return html`<li class="tb-ref-row" key=${key}>
        <span class="tb-ref-row-chip">${shown ? html`<${OneRef} of=${shown} />` : html`<span class="tb-ref tb-ref-new">${key}</span>`}</span>
        <span class="tb-ref-row-text"><span class="tb-ref-row-title" title=${title}>${shown ? title || key : pending}</span>
          ${shown && html`<span class="tb-ref-row-sub">${refSub(shown)}</span>`}</span>
        <span class="tb-ref-row-actions">${multiRefs(task) && i > 0 && html`<${Button} variant="ghost" size="sm" class="tb-ref-first" aria-label=${`Make ${key} the first ref`}
          title="make it the first: the task's ref for --ref and the List" disabled=${busy}
          onClick=${() => onChange(madeFirst(task, draft.refs, key))}>↑<//>`}
        <${Button} variant="ghost" size="sm" class="tb-ref-rm" aria-label=${`Remove ${key}`} title="remove on Save" disabled=${busy}
          onClick=${() => onChange(draft.refs.filter((k) => k !== key))}>✕<//></span>
      </li>`;
    })}</ul>`}
    <span class="tb-ref-add"><${Input} class="tb-edit-ref" aria-label="Add a ref" value=${draft.newRef || ''} disabled=${busy || full}
      placeholder=${full ? 'no more refs' : 'add a ref: URL, owner/repo#12, tcl-a1b2c3 or LIN-123'}
      onInput=${(event) => onNewRef(event.currentTarget.value)}
      onKeyDown=${(event) => { if (event.key === 'Enter' && !event.isComposing) { event.preventDefault(); add(); } }} />
      <${Button} size="sm" class="tb-ref-add-btn" disabled=${busy || full || !(draft.newRef || '').trim()} onClick=${add}>+ Add<//></span>
  </div>`;
}
