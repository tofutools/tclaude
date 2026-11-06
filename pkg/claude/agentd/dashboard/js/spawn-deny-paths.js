// spawn-deny-paths.js — the spawn dialog's "Deny paths" control: the
// dashboard's --sandbox-deny (tcl-074v5a). Each denied path is a chip; new
// ones are typed into the shared FilterCombobox, so suggestions (a profile's
// grant paths, recent denies) can be offered later through its items without
// changing the control.
//
// The rules live in spawn-deny-paths-model.js. The daemon expands "~" and
// "~/" against its own home, as it does for profile grants, so the dialog
// sends paths as typed. It also refuses what this control only flags: a
// relative path, a deny covering the working directory, or a launch whose
// sandbox would not enforce it.

import { h } from 'preact';
import htm from 'htm';
import { FilterCombobox } from './filter-combobox.js';
import { denyCoversCwd, denyPathProblem, normalizeDenyPath } from './spawn-deny-paths-model.js';

const html = htm.bind(h);

// DenyPathChips is the control. blockedReason, when set, says why this launch
// cannot enforce a deny: the input is disabled, and chips already chosen stay
// (the daemon refuses the spawn rather than dropping them).
export function DenyPathChips({ id, value = [], draftText = '', onChange, onDraft, cwd = '', blockedReason = '', busy = false, items = [] }) {
  const remove = (path) => onChange(value.filter((entry) => entry !== path));
  const problem = denyPathProblem(draftText);
  const covering = value.filter((path) => denyCoversCwd(path, cwd));
  return html`<div class="spawn-deny-paths" id=${id}>
    ${value.length ? html`<span class="perm-scope-chips">
      ${value.map((path) => html`<span key=${path}
        class=${`perm-scope-chip spawn-deny-chip${covering.includes(path) ? ' covers-cwd' : ''}`} title=${path}>${path}
        <button data-ui-exempt type="button" class="x" aria-label=${`Remove denied path ${path}`}
          disabled=${busy} onClick=${() => remove(path)}>×</button></span>`)}
    </span>` : null}
    ${blockedReason ? null : html`<${FilterCombobox} id=${`${id}-list`} inputId=${`${id}-input`} items=${items}
      value=${value} onChange=${onChange} onQuery=${onDraft}
      freeText=${normalizeDenyPath} literal=${(text) => !!normalizeDenyPath(text)}
      placeholder="Add a path, then Enter (/abs or ~/…)" ariaLabel="Add a denied path"
      emptyHint="Type an absolute path or ~/…" />`}
    ${problem ? html`<div class="spawn-field-hint ui-hint warn" role="alert">${problem}</div>` : null}
    ${covering.map((path) => html`<div key=${path} class="spawn-field-hint ui-hint warn" role="alert">
      ${path} covers the working directory ${cwd}; the daemon will refuse this spawn.</div>`)}
    ${blockedReason ? html`<div class="spawn-field-hint ui-hint${value.length ? ' warn' : ''}">${blockedReason}${value.length
      ? ' The daemon will refuse this spawn while these paths are set.' : ''}</div>` : null}
  </div>`;
}
