import { h, Fragment } from 'preact';
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

/* The [?] trigger and its popover on their own, for controls that are not the
   labelled <select> HelpField wraps — checkbox rows, section legends. The two
   nodes stay DOM siblings because the open state is expressed purely in CSS as
   `trigger[aria-expanded="true"] + description`.

   `help` is the plain-text copy used for the native tooltip and as the popover
   body; `content` overrides only the body when the copy is worth structuring
   (paths in <code>, a highlighted warning) while the tooltip stays flat text.
   Both empty means no trigger at all: an empty popover would be a focusable,
   unnamed blank in the tab order. */
export function HelpDisclosure({ id, descriptionID = `${id}-hint`, label, help, content = null, open, setOpen }) {
  if (!help && !content) return null;
  /* A browser focuses the button on mousedown, which would open the disclosure
     before onClick ran and make the click read as a toggle-closed. Suppressing
     the default mousedown keeps click a plain toggle while Tab still opens it
     via onFocus. */
  const swallowFocus = (event) => event.preventDefault();
  return html`<${Fragment}>
    <button type="button" class="spawn-field-help-trigger" aria-label=${`Show ${label} help`}
      aria-controls=${descriptionID} aria-expanded=${open ? 'true' : 'false'} title=${`Show ${label} help`}
      onMouseDown=${swallowFocus}
      onClick=${() => setOpen(open ? '' : id)}
      onFocus=${() => setOpen(id)}>?</button>
    <span id=${descriptionID} class="spawn-field-description" role="tooltip" tabindex="0"
      aria-live="polite" onFocus=${() => setOpen(id)}>${content || help}</span>
  <//>`;
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
   async fetch), so HelpDisclosure drops the trigger and its description
   together: an empty description would be a focusable, unnamed, blank tooltip
   in the tab order. The CSS reserves the trigger column unconditionally so the
   select does not resize when the help lands. */
export function HelpField({
  id, descriptionID = `${id}-hint`, label, title, value, options,
  onChange, help, open, setOpen, disabled = false, busy = false,
}) {
  const caveat = helpCaveat(help);
  return html`<div class="cron-create-row" id=${`${id}-row`} title=${title} hidden=${disabled}>
    <label class="cron-create-label" for=${id}>${label}</label>
    <div class="cron-create-target spawn-field-help-column">
      <div class="spawn-field-with-help">
        <select id=${id} value=${value} title=${help} aria-describedby=${help ? descriptionID : null} disabled=${busy}
          onChange=${onChange}>
          ${options.map((option) => html`<option key=${option.value} value=${option.value}>${option.label}</option>`)}
        </select>
        <${HelpDisclosure} id=${id} descriptionID=${descriptionID} label=${label} help=${help}
          open=${open} setOpen=${setOpen} />
      </div>
      ${caveat && html`<div class="spawn-field-hint warn spawn-field-caveat" id=${`${id}-caveat`}
        aria-hidden="true">${caveat}</div>`}
    </div>
  </div>`;
}
