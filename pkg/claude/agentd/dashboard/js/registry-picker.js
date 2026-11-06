// registry-picker.js — the dashboard's choice of a named entry from a list
// that grows: a group template, a role or a saved prompt (tcl-986r2g), or a
// federation peer, node or board, or a harness's model (tcl-saada2). Each
// grows to tens of entries (federation and OpenCode's models without bound),
// so every such <select> is this type-to-filter choice instead (ChoiceCombobox, on FilterCombobox). Profiles have their
// own (profile-picker.js), as do groups (group-picker.js).
//
//   namedItems(entries, opts)  entries as FilterCombobox items
//   TemplateCombobox           the choice of a group template
//   RoleCombobox               the choice of a role
//   PromptCombobox             the choice of a saved prompt
//   PeerCombobox               the choice of a peer (or pool)
//   NodeCombobox               the choice of a node
//   BoardCombobox              the choice of a board
//   ModelCombobox              the choice of a model (the spawn dialog's stays a
//                              <select>: an agent can fill it, tcl-alhauf)

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

// named builds one registry's choice: what it is called, for the box's words,
// and how many rows it lists at once (cap; ChoiceCombobox's by default).
function named(noun, nouns, { cap: listCap } = {}) {
  return function NamedCombobox({ entries = [], hint = null, items = null, cap = listCap, placeholder = `Type to find a ${noun}…`, emptyHint = `No ${nouns}.`, ariaLabel = noun[0].toUpperCase() + noun.slice(1), missing = 'missing', ...props }) {
    const built = useMemo(() => items || namedItems(entries, { hint }), [items, entries, hint]);
    return html`<${ChoiceCombobox} ...${props} ...${cap === undefined ? {} : { cap }} items=${built} placeholder=${placeholder} emptyHint=${emptyHint} ariaLabel=${ariaLabel} missing=${missing} />`;
  };
}

export const TemplateCombobox = named('template', 'templates');
export const RoleCombobox = named('role', 'roles');
export const PromptCombobox = named('prompt', 'saved prompts');
export const PeerCombobox = named('peer', 'peers');
export const NodeCombobox = named('node', 'nodes');
export const BoardCombobox = named('board', 'boards');
// A model list has no groups to narrow by, so nothing typed lists every model,
// as the <select> did, down to a "Custom model id…" row after them.
export const ModelCombobox = named('model', 'models', { cap: Infinity });
