import { h } from 'preact';
import htm from 'htm';

const html = htm.bind(h);

/* Long per-mode help copy belongs in a hover tooltip plus a [?] disclosure, not
   in a paragraph that permanently pads the spawn and profile dialogs. The one
   exception is a caveat: help that carries a "⚠" is warning the operator that
   the selected mode can deadlock a detached agent or drop guardrails, and
   burying that behind a click would be a safety regression. helpCaveat pulls
   the warning out so it can stay visible while the rest of the copy collapses.

   Everything from the ⚠ to the end of the string is the caveat. The harness
   help puts the neutral description first and the warning last precisely so
   this split works; truncating at the first sentence break instead would cut
   "…auto-approve everything." off from "No deadlocks but no guardrails", which
   is the half that matters. */
export function helpCaveat(help) {
  const text = String(help || '');
  const start = text.indexOf('⚠');
  return start < 0 ? '' : text.slice(start).trim();
}

/* A labelled <select> whose help copy is reachable three ways: the native title
   tooltip on hover, the [?] button for keyboard and touch, and — only when the
   help carries a ⚠ — a persistent caveat line under the control.

   `open`/`setOpen` are lifted so only one field's disclosure shows at a time;
   the caller keys them by field id. onChange receives the raw event, matching
   the plain <select> rows these replaced.

   The caveat renders as a sibling of .spawn-field-with-help rather than inside
   it: that element is the containing block for the absolutely-positioned
   description, so growing it would push the popover up off its own control.

   `busy` disables the select but never the [?] — a spawn in flight is no reason
   to stop the operator reading what they picked.

   Help can be transiently empty (the sandbox-profile preview arrives from an
   async fetch), so the trigger and its description are both gated on it: an
   empty description would be a focusable, unnamed, blank tooltip in the tab
   order. The CSS reserves the trigger column unconditionally so the select
   does not resize when the help lands. */
export function HelpField({
  id, descriptionID = `${id}-hint`, label, title, value, options,
  onChange, help, open, setOpen, disabled = false, busy = false,
}) {
  const caveat = helpCaveat(help);
  /* A browser focuses the button on mousedown, which would open the disclosure
     before onClick ran and make the click read as a toggle-closed. Suppressing
     the default mousedown keeps click a plain toggle while Tab still opens it
     via onFocus. */
  const swallowFocus = (event) => event.preventDefault();
  return html`<div class="cron-create-row" id=${`${id}-row`} title=${title} hidden=${disabled}>
    <label class="cron-create-label" for=${id}>${label}</label>
    <div class="cron-create-target spawn-field-help-column">
      <div class="spawn-field-with-help">
        <select id=${id} value=${value} title=${help} aria-describedby=${help ? descriptionID : null} disabled=${busy}
          onChange=${onChange}>
          ${options.map((option) => html`<option key=${option.value} value=${option.value}>${option.label}</option>`)}
        </select>
        ${help && html`<button type="button" class="spawn-field-help-trigger" aria-label=${`Show ${label} help`}
          aria-controls=${descriptionID} aria-expanded=${open ? 'true' : 'false'} title=${`Show ${label} help`}
          onMouseDown=${swallowFocus}
          onClick=${() => setOpen(open ? '' : id)}
          onFocus=${() => setOpen(id)}>?</button>`}
        ${help && html`<span id=${descriptionID} class="spawn-field-description" role="tooltip" tabindex="0"
          aria-live="polite" onFocus=${() => setOpen(id)}>${help}</span>`}
      </div>
      ${caveat && html`<div class="spawn-field-hint warn spawn-field-caveat" id=${`${id}-caveat`}
        aria-hidden="true">${caveat}</div>`}
    </div>
  </div>`;
}
