// relaunch-policy-dialog.js — the per-agent relaunch policy editor (launch
// slice 11): what a later resume or reincarnation of the agent re-reads and
// what it keeps, row by row, over the global table in Config › Launch.
//
// It is the permissions editor's twin (perm-edit-modal, perm-row,
// perm-row-eff): a pure buffer editor. The rows belong to the spawn form or
// the profile editor, and Save hands them back through descriptor.onSave.
//
// Each row offers Default, then (for a spawn) Follow global, then the row's
// own modes. Default leaves the row to the tiers below: for a spawn its
// profiles, then the global table; for a profile, the global table. The
// effective column says what the row comes to and where from. Nothing is
// resolved here: descriptor.inherited is the daemon's answer for the row
// with no explicit choice (GET /api/relaunch-policy?preview=1), and
// descriptor.rows carries the global table.
import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { Dialog } from './ui-dialog.js';
import { HelpDisclosure } from './help-field.js';
import { useGuardedOverlayClose } from './management-overlay.js';
import { MODE_LABELS, READONLY_SECTIONS } from './relaunch-policy-table.js';

const html = htm.bind(h);

export const FOLLOW_GLOBAL = 'inherit';

const modeLabel = (mode) => MODE_LABELS[mode] || mode;

// relaunchRowEffect is what one row comes to under selection: {value, source}.
// A chosen mode is this spawn's (or this profile's); Follow global is the
// global table's; Default is whatever the daemon resolved without a choice.
export function relaunchRowEffect(row, selection, inherited, ownSource) {
  const chosen = selection?.[row.field];
  if (chosen === FOLLOW_GLOBAL) return { value: row.mode, source: 'global' };
  if (chosen) return { value: chosen, source: ownSource };
  return inherited?.[row.field] || { value: row.mode, source: 'global' };
}

// relaunchRowsSummary is the one-line summary the spawn dialog and the
// profile editor show: the rows whose effect differs from the row's default,
// with where each comes from.
export function relaunchRowsSummary(rows, selection, inherited, ownSource) {
  const changed = rows.map((row) => ({ row, effect: relaunchRowEffect(row, selection, inherited, ownSource) }))
    .filter(({ row, effect }) => effect.value !== row.default || effect.source !== 'global');
  if (!changed.length) return 'Every row follows the global table at its default';
  return changed.map(({ row, effect }) => `${row.label} → ${modeLabel(effect.value)} (${effect.source})`).join(' · ');
}

function sectionList(members) {
  return members.map((f) => f.label).join(' · ');
}

// InlineHelp is a heading-sized [?] (help-field.js) whose copy opens below it.
function InlineHelp(props) {
  return html`<span class="sbx-section-help sbx-section-legend"><${HelpDisclosure} ...${props} /></span>`;
}

export function RelaunchPolicyDialog({ descriptor, state, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const rows = (descriptor.rows || []).filter((row) => row.settable !== false);
  const fields = descriptor.fields || [];
  const forProfile = descriptor.scope === 'profile';
  const ownSource = forProfile ? 'this profile' : 'this spawn';
  const [baseline] = useState(() => ({ ...(descriptor.selection || {}) }));
  const [selection, setSelection] = useState(() => ({ ...baseline }));
  // One help popover open at a time, keyed by its id.
  const [helpOpen, setHelpOpen] = useState('');
  const helpProps = (id) => ({ id, open: helpOpen === id, setOpen: setHelpOpen });
  const current = (field) => selection[field] || '';
  const dirty = JSON.stringify(Object.entries(selection).sort()) !== JSON.stringify(Object.entries(baseline).sort());
  const choose = (field, next) => setSelection((before) => {
    const draft = { ...before };
    // Sparse, like the other buffer editors: Default is an absent row.
    if (!next) delete draft[field];
    else draft[field] = next;
    return draft;
  });
  const submit = () => {
    descriptor.onSave?.({ ...selection });
    state.close();
  };
  const segment = (row, value, label, title) => html`<${Button} size="sm" data-mode=${value || 'default'}
    pressed=${current(row.field) === value} class=${current(row.field) === value ? 'active' : ''} title=${title}
    onClick=${() => choose(row.field, value)}>${label}<//>`;
  const defaultTitle = (row) => {
    const effect = descriptor.inherited?.[row.field] || { value: row.mode, source: 'global' };
    return `Leave it to ${forProfile ? 'the global table' : 'the spawn profiles, then the global table'}: ${modeLabel(effect.value)} (${effect.source})`;
  };
  // The intro is behind the title's [?]: the controls and what each row comes
  // to carry the dialog on their own.
  const subject = forProfile ? 'an agent spawned from this profile' : 'this agent';
  const below = forProfile ? 'the global table' : 'the spawn profiles, then the global table';
  const introText = `What a later resume or reincarnation of ${subject} re-reads, and what it keeps. `
    + `Default leaves a row to ${below}${forProfile ? '' : '; Follow global takes the global table even where a profile sets the row'}. `
    + 'The global table is in Config › Launch.';
  const intro = html`What a later resume or reincarnation of ${subject} re-reads, and what it keeps.${' '}
    <strong>Default</strong> leaves a row to ${below}${forProfile ? '' : html`; <strong>Follow global</strong> takes the global table even where a profile sets the row`}.${' '}
    The global table is in Config › Launch.`;
  const title = html`<span id="relaunch-policy-title-text">On resume</span>${' '}<${InlineHelp}
    ...${helpProps('relaunch-policy-help')} label="On resume" help=${introText} content=${intro} />`;
  const layerSections = READONLY_SECTIONS.filter((s) => s.kind !== 'once')
    .map((section) => ({ section, members: fields.filter((f) => !f.configurable && section.layers.includes(f.layer)) }))
    .filter(({ members }) => members.length);
  return html`<${Dialog} id="relaunch-policy-modal" dialogClass="perm-edit-modal relaunch-policy-modal" onClose=${state.close} dirty=${dirty} confirmDiscard=${confirmDiscard}
    guard=${{ requestClose, registerClose }} title=${title} labelledby="relaunch-policy-title-text">
    <p class="perm-edit-subtitle" id="relaunch-policy-subtitle">${
      descriptor.label ? `${forProfile ? 'Profile' : 'New agent'} “${descriptor.label}”` : (forProfile ? 'Profile' : 'New agent')} · copied at spawn</p>
    <div class="perm-edit-toolbar">
      <${Button} id="relaunch-policy-reset" title="Set every row back to Default" onClick=${() => setSelection((before) => {
        const draft = { ...before };
        for (const row of rows) delete draft[row.field]; // rows the editor does not show are kept
        return draft;
      })}>all default<//>
    </div>
    <div id="relaunch-policy-list" class="perm-edit-list">${rows.map((row) => {
      const effect = relaunchRowEffect(row, selection, descriptor.inherited, ownSource);
      return html`<div class="perm-row" key=${row.field} data-relaunch-field=${row.field}>
        <div class="perm-row-info">
          <span class="perm-row-slug" title=${row.why || ''}>${row.label}${row.why ? html`${' '}<${InlineHelp}
            ...${helpProps(`relaunch-policy-help-${row.field}`)} label=${row.label} help=${row.why} />` : ''}</span>
        </div>
        <div class="ui-seg perm-tristate">
          ${segment(row, '', 'Default', defaultTitle(row))}
          ${!forProfile && segment(row, FOLLOW_GLOBAL, 'Follow global', `The global table, even where a profile sets the row: ${modeLabel(row.mode)}`)}
          ${(row.modes || []).map((mode) => segment(row, mode, modeLabel(mode), ''))}
        </div>
        <span class="perm-row-eff" data-relaunch-effect title=${row.gap || ''}>${modeLabel(effect.value)} · ${effect.source}</span>
      </div>`;
    })}</div>
    ${layerSections.length > 0 && html`<details class="relaunch-policy-layers" id="relaunch-policy-layers">
      <summary>Every other input follows its layer: <span data-relaunch-layers-summary>${
        layerSections.map(({ section, members }) => `${members.length} ${section.pill.toLowerCase()}`).join(' · ')}</span></summary>
      <ul>${layerSections.map(({ section, members }) => html`<li key=${section.kind} data-relaunch-layer=${section.kind}>
          <span class=${`ui-tag rlp-pill rlp-pill-${section.kind}`}>${section.pill}</span> ${sectionList(members)}</li>`)}</ul>
    </details>`}
    <div class="modal-buttons">
      <${Button} id="relaunch-policy-cancel" onClick=${() => { void requestClose(); }}>Cancel<//>
      <span class="spacer"></span>
      <${Button} variant="primary" id="relaunch-policy-submit" onClick=${submit}>Save<//>
    </div>
  <//>`;
}
