// group-picker.js — the dashboard's choice of a group (tcl-kduiih). A node
// can hold dozens to hundreds of groups; every group <select> is this
// type-to-filter choice instead (ChoiceCombobox, on FilterCombobox).
//
//   groupItems(groups)  group names, or { value, label?, hint?, disabled?,
//                       reason? } choices, as FilterCombobox items
//   snapshotGroups(s)   the snapshot's groups, as group names
//   GroupCombobox       the choice itself

import { h } from 'preact';
import { useMemo } from 'preact/hooks';
import htm from 'htm';
import { ChoiceCombobox } from './choice-combobox.js';

const html = htm.bind(h);

// groupItems turns group choices into FilterCombobox items. A choice is a
// group name, or { value, label, hint, disabled, reason } for a site whose
// options say more (a count, why one is off). A nested group sits under its
// parent, so a long list narrows by "group:<parent>".
export function groupItems(groups = []) {
  return groups.filter((group) => group !== null && group !== undefined && group !== '').map((group) => {
    if (typeof group !== 'object') return { key: String(group), label: String(group), hint: '' };
    const key = String(group.value ?? group.name ?? group.id ?? '');
    return {
      key, label: group.label || key, hint: group.hint || '', group: group.parent || '',
      keywords: group.keywords || '', disabled: !!group.disabled, reason: group.reason || '',
    };
  }).filter((item) => item.key);
}

// snapshotGroups are the snapshot's groups (or only the active ones) by name.
export function snapshotGroups(snapshotValue, { activeOnly = false } = {}) {
  return (snapshotValue?.groups || [])
    .filter((group) => group?.name && (!activeOnly || !group.archived))
    .map((group) => group.name);
}

// GroupCombobox is a type-to-filter choice of one group. groups is what
// groupItems takes; every other prop is ChoiceCombobox's.
export function GroupCombobox({ groups = [], placeholder = 'Type to find a group…', emptyHint = 'No groups.', ariaLabel = 'Group', ...props }) {
  const items = useMemo(() => groupItems(groups), [groups]);
  return html`<${ChoiceCombobox} ...${props} items=${items} placeholder=${placeholder} emptyHint=${emptyHint} ariaLabel=${ariaLabel} missing="no such group" />`;
}
