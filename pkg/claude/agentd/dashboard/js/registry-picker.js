// registry-picker.js — the dashboard's choice of a named registry entry: a
// group template, a role or a saved prompt (tcl-986r2g). Each registry grows
// to tens of entries, so every such <select> is this type-to-filter choice
// instead (ChoiceCombobox, on FilterCombobox). Profiles have their own
// (profile-picker.js), as do groups (group-picker.js).
//
//   namedItems(entries, opts)  entries as FilterCombobox items
//   TemplateCombobox           the choice of a group template
//   RoleCombobox               the choice of a role
//   PromptCombobox             the choice of a saved prompt

import { h } from 'preact';
import { useMemo } from 'preact/hooks';
import htm from 'htm';
import { ChoiceCombobox } from './choice-combobox.js';

const html = htm.bind(h);

// oneLine flattens a description for a row's hint.
const oneLine = (text) => String(text || '').replace(/\s+/g, ' ').trim();

// namedItems turns registry entries into FilterCombobox items. An entry is a
// name, or an object with a name and maybe a label, a descr (the row's hint
// and found by), a group (rows group under it), or unpickable (why it cannot
// be chosen). hint, when given, says what a row's hint is instead.
export function namedItems(entries = [], { hint = null } = {}) {
  const items = [];
  for (const entry of entries || []) {
    if (entry === null || entry === undefined || entry === '') continue;
    const e = typeof entry === 'object' ? entry : { name: String(entry) };
    if (!e.name) continue;
    const descr = oneLine(e.descr || e.description || String(e.body || '').slice(0, 200));
    items.push({
      key: e.name, label: e.label || e.name, hint: hint ? hint(e) : descr, keywords: descr,
      group: e.group || '', disabled: !!e.unpickable, reason: e.unpickable || '',
    });
  }
  return items;
}

// named builds one registry's choice: what it is called, for the box's words.
function named(noun, nouns) {
  return function NamedCombobox({ entries = [], hint = null, items = null, placeholder = `Type to find a ${noun}…`, emptyHint = `No ${nouns}.`, ariaLabel = noun[0].toUpperCase() + noun.slice(1), missing = 'missing', ...props }) {
    const built = useMemo(() => items || namedItems(entries, { hint }), [items, entries, hint]);
    return html`<${ChoiceCombobox} ...${props} items=${built} placeholder=${placeholder} emptyHint=${emptyHint} ariaLabel=${ariaLabel} missing=${missing} />`;
  };
}

export const TemplateCombobox = named('template', 'templates');
export const RoleCombobox = named('role', 'roles');
export const PromptCombobox = named('prompt', 'saved prompts');
