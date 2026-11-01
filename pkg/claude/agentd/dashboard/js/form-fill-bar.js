// form-fill-bar.js -- the part of a fill the operator sees (form-fill.js does the
// work): a highlight on each filled field, and beside submit what was filled, by
// whom, a warning for what widened, and one click to undo it all.
// dashboard-imperative-boundary: browser-io

import { h } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { claimField, FILL_FORMS, filledIn, fillSummary, onFillsChange, undoFill, watchForm } from './form-fill.js';

const html = htm.bind(h);

function markFields(root, fields) {
  const byId = new Map(fields.map((field) => [field.id, field]));
  for (const el of root.querySelectorAll('[data-agent-filled]')) {
    if (!byId.has(el.id)) el.removeAttribute('data-agent-filled');
  }
  for (const [id, field] of byId) {
    root.ownerDocument.getElementById(id)?.setAttribute('data-agent-filled', field.widens ? 'widens' : 'plain');
  }
}

// FormFillBar sits beside a form's submit. It also owns the watch, so a form has
// one for as long as it is open. generation is a signal that restarts it when the
// form reloads, read here so that no larger part of a form subscribes to it, and
// a form with one takes no fill until it has loaded. A form that is not fillable
// has no watch, so a fill finds it closed.
export function FormFillBar({ form, generation, keepValues = false, fillable = true, claims = [], win = globalThis.window }) {
  const ref = useRef(null);
  const rootOf = () => ref.current?.closest(FILL_FORMS[form].root) || null;
  const loaded = generation?.value;
  useEffect(() => {
    const root = rootOf();
    const waiting = generation && !loaded;
    return root && fillable && !waiting ? watchForm(form, root, { win, keepValues }) : undefined;
  }, [form, loaded, fillable]);
  // After the watch starts: these are fields the operator holds through something
  // that is not a field.
  useEffect(() => { for (const id of claims) claimField(form, id); }, [form, loaded, fillable, claims.join()]);
  const [, redraw] = useState(0);
  useEffect(() => onFillsChange(() => redraw((count) => count + 1)), []);
  const fields = filledIn(form);
  // A render can replace a field, so this runs after each one.
  useLayoutEffect(() => {
    const root = rootOf();
    if (root) markFields(root, fields);
  });
  if (!fields.length) return html`<div ref=${ref} class="form-fill-anchor" hidden></div>`;
  const { filled, widened } = fillSummary(fields);
  return html`<div ref=${ref} id=${`form-fill-${form}`} class="form-fill-bar" role="status">
    <div class="form-fill-line">
      <span>${filled}: ${fields.map((field) => field.label).join(', ')}</span>
      <button type="button" class="form-fill-undo" onClick=${() => { void undoFill(form, { win }); }}>Undo all</button>
    </div>
    ${widened.map((line) => html`<div key=${line} class="form-fill-warn" role="alert">⚠ ${line}</div>`)}
  </div>`;
}
