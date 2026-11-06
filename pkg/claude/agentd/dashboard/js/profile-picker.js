// profile-picker.js — the dashboard's choice of a profile (tcl-986r2g): a
// spawn profile, a sandbox or usage profile, or a federation node profile. A
// node's profile libraries grow to tens of entries, so every profile <select>
// is this type-to-filter choice instead (ChoiceCombobox, on FilterCombobox).
//
//   profileItems(profiles, opts)  profiles as FilterCombobox items
//   ProfileCombobox               the choice of one profile

import { h } from 'preact';
import { useMemo } from 'preact/hooks';
import htm from 'htm';
import { ChoiceCombobox } from './choice-combobox.js';

const html = htm.bind(h);

// profileSummary is a spawn profile's launch at a glance: harness · model ·
// effort.
function profileSummary(profile) {
  return [profile.harness, profile.model, profile.effort && `effort ${profile.effort}`].filter(Boolean).join(' · ');
}

// profileStatus is what a spawn profile's state adds to its row: disabled
// (with why) or operator only.
function profileStatus(profile) {
  if (profile.disabled) return `🚫 disabled${profile.disabled_reason ? `: ${String(profile.disabled_reason).replace(/\s+/g, ' ').trim()}` : ''}`;
  return profile.operator_only ? '👤 operator only' : '';
}

// profileItems turns profiles into FilterCombobox items. A profile is a name,
// or an object with a name (and, for a spawn profile, harness / model /
// effort / aliases / disabled / operator_only). Each row is found by its name,
// its aliases and its launch; with aliases, an alias is a row of its own
// ("alias → name"), as the spawn profile <select>s listed them. hint, when
// given, says what a row's hint is instead (a node profile's trust).
export function profileItems(profiles = [], { aliases = false, hint = null } = {}) {
  const items = [];
  for (const profile of profiles || []) {
    if (profile === null || profile === undefined || profile === '') continue;
    const p = typeof profile === 'object' ? profile : { name: String(profile) };
    if (!p.name) continue;
    const summary = profileSummary(p);
    const status = profileStatus(p);
    const rowHint = hint ? hint(p) : [summary, status].filter(Boolean).join(' · ');
    const keywords = [summary, ...(p.aliases || []), p.descr].filter(Boolean).join(' ');
    items.push({ key: p.name, label: p.name, hint: rowHint, keywords, group: p.group || '', disabled: !!p.unpickable, reason: p.unpickable || '' });
    if (!aliases) continue;
    for (const alias of p.aliases || []) {
      items.push({ key: alias, label: `${alias} → ${p.name}`, hint: rowHint, keywords: `${p.name} ${keywords}`, group: p.group || '' });
    }
  }
  return items;
}

// ProfileCombobox is a type-to-filter choice of one profile. profiles and
// aliases are profileItems'; items, when given, are used as they are (a site
// that builds its own rows); every other prop is ChoiceCombobox's.
export function ProfileCombobox({ profiles = [], aliases = false, hint = null, items = null, placeholder = 'Type to find a profile…', emptyHint = 'No profiles.', ariaLabel = 'Profile', missing = 'missing', ...props }) {
  const built = useMemo(() => items || profileItems(profiles, { aliases, hint }), [items, profiles, aliases, hint]);
  return html`<${ChoiceCombobox} ...${props} items=${built} placeholder=${placeholder} emptyHint=${emptyHint} ariaLabel=${ariaLabel} missing=${missing} />`;
}
