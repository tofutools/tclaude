import { h } from 'preact';
import htm from 'htm';

const html = htm.bind(h);

/* Long per-mode help copy belongs in a hover tooltip plus a [?] disclosure, not
   in a paragraph that permanently pads the spawn and profile dialogs. The one
   exception is a caveat: help that carries a "⚠" is warning the operator that
   the selected mode can deadlock a detached agent or drop guardrails, and
   burying that behind a click would be a safety regression. helpCaveat pulls
   just the warning sentence out so it can stay visible while the rest of the
   copy collapses.

   The caveat ends at the first sentence break — a terminator followed by
   whitespace and a capital letter or an opening paren. Terminators inside a
   clause (e.g. "…skip-permissions): auto-approve everything.") do not split,
   and help with no trailing sentence yields the whole remainder. */
export function helpCaveat(help) {
  const text = String(help || '');
  const start = text.indexOf('⚠');
  if (start < 0) return '';
  const rest = text.slice(start);
  const brk = /[.!?](?=\s+[A-Z(])/.exec(rest);
  return (brk ? rest.slice(0, brk.index + 1) : rest).trim();
}

/* A labelled <select> whose help copy is reachable three ways: the native title
   tooltip on hover, the [?] button for keyboard and touch, and — only when the
   help carries a ⚠ — a persistent caveat line under the control.

   `open`/`setOpen` are lifted so only one field's disclosure shows at a time;
   the caller keys them by field id. onChange receives the raw event, matching
   the plain <select> rows these replaced. */
export function HelpField({
  id, descriptionID = `${id}-hint`, label, title, value, options,
  onChange, help, open, setOpen, disabled = false, busy = false,
}) {
  const caveat = helpCaveat(help);
  return html`<div class="cron-create-row" id=${`${id}-row`} title=${title} hidden=${disabled}>
    <label class="cron-create-label" for=${id}>${label}</label>
    <div class="cron-create-target spawn-field-with-help">
      <select id=${id} value=${value} title=${help} aria-describedby=${descriptionID} disabled=${busy}
        onChange=${onChange}>
        ${options.map((option) => html`<option key=${option.value} value=${option.value}>${option.label}</option>`)}
      </select>
      <button type="button" class="spawn-field-help-trigger" aria-label=${`Show ${label} help`}
        aria-controls=${descriptionID} aria-expanded=${open ? 'true' : 'false'} title=${`Show ${label} help`}
        disabled=${busy}
        onClick=${() => setOpen(open ? '' : id)}
        onFocus=${() => setOpen(id)}>?</button>
      <span id=${descriptionID} class="spawn-field-description" role="tooltip" tabindex="0"
        aria-live="polite" onFocus=${() => setOpen(id)}>${help}</span>
      ${caveat && html`<div class="spawn-field-hint warn spawn-field-caveat" id=${`${id}-caveat`}
        aria-live="polite">${caveat}</div>`}
    </div>
  </div>`;
}
