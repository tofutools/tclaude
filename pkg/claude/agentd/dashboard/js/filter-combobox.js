// filter-combobox.js — the shared inline type-to-filter dropdown.
//
// It is the agent picker's combobox (role=combobox, aria-activedescendant,
// ↑↓ / Enter, the add-member classes) lifted into one component that renders
// in place rather than in its own dialog, ranked by palette-score.js — the
// same ranking as the Ctrl-K palette — so a picker over hundreds of values
// never renders a long flat list:
//
//   - at most `cap` rows show, grouped by kind and then group, with the true
//     match count and a hint to refine when the list is cut;
//   - opened with nothing typed over more than `cap` items, it shows a
//     summary of the groups (counts, never members) instead of rows;
//   - "kind:" and "group:" query prefixes narrow by kind or group, so
//     "board:rel" or "group:infra rel" refine a long result.
//
// Props:
//   items        [{ key, label, hint?, kind?, group?, keywords?, disabled?, reason? }]
//   value        key[] when multi, else one key ('' for none)
//   multi        true (default): choosing toggles a key in value
//   onChange     (next) → void — key[] when multi, else the key
//   cap          the most rows rendered (default 8)
//   kindLabels   { kind: 'Heading' } for the kind headings
//   freeText     opt-in: (text) → key | '' — Enter with no highlighted row
//                commits the typed text through it ('' refuses)
//   placeholder, emptyHint, ariaLabel, id
import { h } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { rankCommands } from './palette-score.js';

const html = htm.bind(h);

let nextComboboxID = 0;

// parseComboQuery splits "kind:" and "group:" prefixes off a query. A prefix
// that names no known kind stays part of the text, so a typed "board:plan"
// both narrows to boards and searches for "plan".
export function parseComboQuery(query, kinds = []) {
  const out = { text: [], kind: '', group: '' };
  for (const token of String(query || '').trim().split(/\s+/).filter(Boolean)) {
    const match = /^([a-z_]+):(.*)$/i.exec(token);
    const prefix = match?.[1].toLowerCase();
    if (prefix === 'group') { out.group = match[2].toLowerCase(); continue; }
    if (prefix && kinds.includes(prefix)) {
      out.kind = prefix;
      if (match[2]) out.text.push(match[2]);
      continue;
    }
    out.text.push(token);
  }
  return { kind: out.kind, group: out.group, text: out.text.join(' ') };
}

// filterComboItems is the ranked match list for a query, before the cap.
export function filterComboItems(items, query) {
  const kinds = [...new Set(items.map((item) => item.kind).filter(Boolean))];
  const parsed = parseComboQuery(query, kinds);
  const narrowed = items.filter((item) => (!parsed.kind || item.kind === parsed.kind)
    && (!parsed.group || String(item.group || '').toLowerCase().includes(parsed.group)));
  const commands = narrowed.map((item) => ({
    label: item.label || item.key, hint: `${item.key} ${item.hint || ''}`,
    keywords: `${item.group || ''} ${item.keywords || ''}`, item,
  }));
  return rankCommands(commands, parsed.text).map((command) => command.item);
}

// comboSections groups the visible rows by kind, then group, keeping the
// order of each section's best-ranked row. index is the row's position in
// the flat list the keyboard moves through.
export function comboSections(visible) {
  const sections = [];
  const byKey = new Map();
  visible.forEach((item, index) => {
    const id = `${item.kind || ''}\u0000${item.group || ''}`;
    let section = byKey.get(id);
    if (!section) {
      section = { kind: item.kind || '', group: item.group || '', rows: [] };
      byKey.set(id, section);
      sections.push(section);
    }
    section.rows.push({ item, index });
  });
  // Kinds stay together, in the order their best row ranked.
  const kindOrder = [...new Set(sections.map((section) => section.kind))];
  return kindOrder.flatMap((kind) => sections.filter((section) => section.kind === kind));
}

// comboGroupSummary counts items per group, largest first.
export function comboGroupSummary(items) {
  const counts = new Map();
  for (const item of items) counts.set(item.group || '', (counts.get(item.group || '') || 0) + 1);
  return [...counts.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .map(([group, count]) => ({ group, count }));
}

export function FilterCombobox({
  items = [], value, multi = true, onChange, cap = 8, kindLabels = {}, freeText = null,
  placeholder = 'Type to filter…', emptyHint = 'No matches.', ariaLabel = 'Filter', id,
}) {
  const [query, setQuery] = useState('');
  const [open, setOpen] = useState(false);
  const [highlight, setHighlight] = useState(0);
  const inputRef = useRef(null);
  const highlightedRef = useRef(null);
  const listID = useMemo(() => id || `filter-combobox-${++nextComboboxID}`, [id]);
  const chosen = new Set(multi ? (value || []) : (value ? [value] : []));
  const matches = useMemo(() => filterComboItems(items, query), [items, query]);
  const summarize = !query.trim() && items.length > cap;
  const visible = summarize ? [] : matches.slice(0, cap);
  const choosable = (item) => item && !item.disabled;
  const bounded = Math.max(0, Math.min(highlight, Math.max(0, visible.length - 1)));
  const activeID = open && visible[bounded] ? `${listID}-option-${bounded}` : undefined;
  useEffect(() => { if (bounded !== highlight) setHighlight(bounded); }, [bounded, highlight]);
  useEffect(() => { highlightedRef.current?.scrollIntoView?.({ block: 'nearest' }); }, [bounded, open]);

  const choose = (item) => {
    if (!choosable(item)) return;
    if (multi) {
      const next = chosen.has(item.key) ? [...chosen].filter((key) => key !== item.key) : [...chosen, item.key];
      onChange(next);
    } else onChange(chosen.has(item.key) ? '' : item.key);
    setQuery(''); setHighlight(0);
    if (!multi) setOpen(false);
  };
  const commitFree = () => {
    const key = freeText?.(query.trim()) || '';
    if (!key) return false;
    if (multi) { if (!chosen.has(key)) onChange([...chosen, key]); }
    else onChange(key);
    setQuery(''); setHighlight(0);
    return true;
  };
  const onKeyDown = (event) => {
    if (event.isComposing || event.keyCode === 229) return;
    if (event.key === 'ArrowDown') {
      event.preventDefault();
      if (!open) { setOpen(true); return; }
      setHighlight(Math.min(bounded + 1, visible.length - 1));
    } else if (event.key === 'ArrowUp') {
      event.preventDefault(); setHighlight(Math.max(bounded - 1, 0));
    } else if (event.key === 'Enter') {
      if (open && visible[bounded]) { event.preventDefault(); choose(visible[bounded]); }
      else if (query.trim() && freeText) { event.preventDefault(); commitFree(); }
    } else if (event.key === 'Tab' && open && query.trim() && visible[bounded]) {
      // Tab completes the typed text to the highlighted row's label, the way
      // a shell completes; a second Tab moves focus on as usual.
      const label = visible[bounded].label || visible[bounded].key;
      if (label.toLowerCase() !== query.trim().toLowerCase()) { event.preventDefault(); setQuery(label); }
    } else if (event.key === 'Escape' && open) {
      event.preventDefault(); event.stopPropagation(); setOpen(false);
    }
  };
  const kinds = [...new Set(items.map((item) => item.kind).filter(Boolean))];
  const refineExample = [kinds.length > 1 ? `${kinds[0]}:` : '', items.some((item) => item.group) ? 'group:name' : '']
    .filter(Boolean).map((example) => `“${example}”`).join(' or ');
  const summary = summarize ? comboGroupSummary(items) : [];
  const shownGroups = summary.slice(0, 5);
  return html`<div class="filter-combobox">
    <input ref=${inputRef} type="text" class="add-member-search filter-combobox-input" value=${query}
      placeholder=${placeholder} autocomplete="off" spellcheck=${false}
      role="combobox" aria-label=${ariaLabel} aria-controls=${listID} aria-expanded=${open ? 'true' : 'false'}
      aria-autocomplete="list" aria-activedescendant=${activeID}
      onFocus=${() => setOpen(true)} onClick=${() => setOpen(true)} onBlur=${() => setOpen(false)}
      onInput=${(event) => { setQuery(event.currentTarget.value); setHighlight(0); setOpen(true); }}
      onKeyDown=${onKeyDown} />
    ${open && html`<div class="add-member-list filter-combobox-list" id=${listID} role="listbox"
      aria-multiselectable=${multi ? 'true' : 'false'}
      onMouseDown=${(event) => event.preventDefault()}>
      ${summarize ? html`<div class="filter-combobox-tip">Type to search ${items.length} by name, id or group.</div>
        <div class="ui-eyebrow filter-combobox-kind">Groups</div>
        ${shownGroups.map(({ group, count }) => html`<div key=${group} class="add-member-row filter-combobox-group-sum"
          data-group=${group} onMouseDown=${() => { if (group) { setQuery(`group:${group} `); setHighlight(0); } }}>
          <span class="rowname">${group || '(no group)'}</span><span class="id">${count}</span></div>`)}
        ${summary.length > shownGroups.length ? html`<div class="filter-combobox-tip">… ${summary.length - shownGroups.length} more groups</div>` : null}`
    : visible.length === 0 ? html`<div class="ui-empty-block filter-combobox-empty">${query.trim() && freeText
      ? html`No matches. <kbd>Enter</kbd> adds “${query.trim()}” as typed.` : emptyHint}</div>`
      : comboSections(visible).map((section, at, sections) => html`<div key=${`${section.kind}:${section.group}`} class="filter-combobox-section">
        ${section.kind && sections[at - 1]?.kind !== section.kind
          ? html`<div class="ui-eyebrow filter-combobox-kind">${kindLabels[section.kind] || section.kind}</div>` : null}
        ${section.group || section.kind ? html`<div class="filter-combobox-group">${section.group || '(no group)'}</div>` : null}
        ${section.rows.map(({ item, index }) => html`<div key=${item.key}
          ref=${index === bounded ? highlightedRef : null} id=${`${listID}-option-${index}`}
          role="option" aria-selected=${chosen.has(item.key) ? 'true' : 'false'}
          aria-disabled=${item.disabled ? 'true' : undefined} title=${item.disabled ? item.reason || '' : item.key}
          data-key=${item.key}
          class=${`add-member-row filter-combobox-option${index === bounded ? ' highlighted' : ''}${item.disabled ? ' disabled' : ''}`}
          onMouseDown=${() => choose(item)}>
          <span class="filter-combobox-check" aria-hidden="true">${chosen.has(item.key) ? '✓' : ''}</span>
          <span class="rowname">${item.label || item.key}</span>
          <span class="id">${item.hint || item.key}</span>
          ${item.disabled && item.reason ? html`<span class="descr">${item.reason}</span>` : null}
        </div>`)}</div>`)}
      ${!summarize && matches.length > visible.length ? html`<div class="filter-combobox-foot">Showing ${visible.length} of ${matches.length} matches. Refine your search: type more${refineExample ? `, or narrow it with ${refineExample}` : ''}.</div>` : null}
    </div>`}
  </div>`;
}
