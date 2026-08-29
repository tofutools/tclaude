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
   unnamed blank in the tab order.

   `warn` swaps the glyph to [!] and colours the trigger, for help that carries
   a caveat rather than a description. It is what lets a caveat come out of the
   dialog body without becoming invisible: the operator still sees at a glance
   that this control has something to say, and one click says it. */
export function HelpDisclosure({
  id, descriptionID = `${id}-hint`, label, help, content = null, open, setOpen, warn = false,
}) {
  if (!help && !content) return null;
  /* Focus alone is passive form traversal, not a request to cover the dialog
     with help. Once explicitly opened, however, the trigger and description
     act as one focus group: Tab may enter the copy, and leaving both closes it. */
  const closeAfterTrigger = (event) => {
    if (!open || event.currentTarget.nextElementSibling?.contains(event.relatedTarget)) return;
    setOpen('');
  };
  const closeAfterDescription = (event) => {
    if (!open || event.currentTarget.contains(event.relatedTarget)
      || event.currentTarget.previousElementSibling?.contains(event.relatedTarget)) return;
    setOpen('');
  };
  const closeOnEscape = (event) => {
    if (!open || event.key !== 'Escape') return;
    event.preventDefault();
    event.stopPropagation();
    setOpen('');
    const trigger = event.currentTarget.classList.contains('spawn-field-help-trigger')
      ? event.currentTarget : event.currentTarget.previousElementSibling;
    trigger?.focus();
  };
  /* aria-label wins over content, so the [!] glyph is not announced. Naming the
     warn state here is what gives a screen-reader user the same "this control
     has a caveat" cue the colour gives a sighted one. */
  const name = warn ? `Show ${label} warning` : `Show ${label} help`;
  return html`<${Fragment}>
    <button type="button" class=${`spawn-field-help-trigger${warn ? ' warn' : ''}`}
      aria-label=${name}
      aria-controls=${descriptionID} aria-expanded=${open ? 'true' : 'false'} title=${name}
      onClick=${() => setOpen(open ? '' : id)}
      onBlur=${closeAfterTrigger} onKeyDown=${closeOnEscape}>${warn ? '!' : '?'}</button>
    <span id=${descriptionID} class="spawn-field-description" role="tooltip" tabindex=${open ? '0' : '-1'}
      aria-live="polite" onBlur=${closeAfterDescription} onKeyDown=${closeOnEscape}>${content || help}</span>
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
