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
//   literal      opt-in: (text) → bool — typed text of this shape (a whole
//                kind:id, say) is meant as typed, so Enter commits it through
//                freeText instead of the fuzzy-highlighted row
//   commitOnBlur opt-in, with freeText: leaving the box with text in it adds
//                that text, as Enter would (the catalogue's row when it names
//                one exactly, else as typed), so a typed value is not lost
//   onQuery      (text) → void — the typed text as it changes, so a caller can
//                treat what is left in the box as an uncommitted value
//   inputId      the input's id, so a <label for> can name it
//   initialQuery the box's text on mount: a caller that keeps uncommitted
//                text (onQuery) hands it back, so a remount does not hide it
//   disabled     the input takes no typing and opens no list
//   alwaysOpen   the list always shows (a picker dialog's own list), and
//                Escape is left to the dialog
//   autoFocus    focus the input on mount
//   selectedLabel  a single choice that stands in for a <select>: the box
//                shows this (the chosen value's label) while it is not being
//                typed in, drops what was typed when it loses focus, and opens
//                on a click, ↓ or typing, not on focus alone (a dialog that
//                focuses it on open does not cover its own fields)
//   overlay      the list opens over what follows instead of pushing it down
//                (a toolbar or a form row); without it the list is in the flow,
//                which a scroll box cannot clip
//   busy         marks the list aria-busy (still loading, or a write in
//                flight); with disabled, nothing can be chosen meanwhile
//
// The highlight is a row's key, not its place: a poll that reorders the
// items keeps it on the same row, and one that removes that row leaves
// nothing highlighted (Enter does nothing) until ↑↓ or a hover picks again.
// Typing, or a fresh list, highlights the first row.
// An item may also carry lead and tail: what renders before its label (an
// online dot) and after its hint (badges), for a picker whose rows say more.
//   placeholder, emptyHint, ariaLabel, id (the listbox's)
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
  items = [], value, multi = true, onChange, cap = 8, kindLabels = {}, freeText = null, literal = null, onQuery = null,
  placeholder = 'Type to filter…', emptyHint = 'No matches.', ariaLabel = 'Filter', id, inputId, initialQuery = '', disabled = false,
  alwaysOpen = false, autoFocus = false, busy = false, selectedLabel = null, overlay = false, commitOnBlur = false,
}) {
  const [query, setQueryState] = useState(initialQuery || '');
  const setQuery = (text) => { setQueryState(text); onQuery?.(text); };
  const [openState, setOpen] = useState(false);
  const open = alwaysOpen || openState;
  // null: the first row, whichever it is; '': none; else the row's key.
  const [highlightKey, setHighlightKey] = useState(null);
  const resetHighlight = () => setHighlightKey(null);
  const inputRef = useRef(null);
  const highlightedRef = useRef(null);
  const listID = useMemo(() => id || `filter-combobox-${++nextComboboxID}`, [id]);
  const chosen = new Set(multi ? (value || []) : (value ? [value] : []));
  const matches = useMemo(() => filterComboItems(items, query), [items, query]);
  const summarize = !query.trim() && items.length > cap;
  // The rows show in sections (comboSections), so the keyboard walks them in
  // that order, not the ranking's: ↓ always goes to the row below.
  const visible = summarize ? [] : comboSections(matches.slice(0, cap)).flatMap((section) => section.rows.map((row) => row.item));
  const choosable = (item) => item && !item.disabled;
  const bounded = highlightKey === null ? (visible.length ? 0 : -1) : visible.findIndex((item) => item.key === highlightKey);
  const activeID = open && bounded >= 0 ? `${listID}-option-${bounded}` : undefined;
  // The highlighted row went away: nothing is highlighted, and a later list
  // that has it again does not bring the highlight back unasked.
  useEffect(() => { if (highlightKey && bounded < 0) setHighlightKey(''); }, [highlightKey, bounded]);
  const highlightAt = (index) => setHighlightKey(visible[index]?.key ?? '');
  useEffect(() => { highlightedRef.current?.scrollIntoView?.({ block: 'nearest' }); }, [bounded, open]);
  useEffect(() => { if (autoFocus) inputRef.current?.focus?.(); }, []);

  const choose = (item) => {
    if (!choosable(item) || disabled) return;
    if (multi) {
      const next = chosen.has(item.key) ? [...chosen].filter((key) => key !== item.key) : [...chosen, item.key];
      onChange(next);
    } else onChange(chosen.has(item.key) ? '' : item.key);
    setQuery(''); resetHighlight();
    if (!multi) setOpen(false);
  };
  const add = (item) => {
    if (!choosable(item) || disabled) return;
    if (!chosen.has(item.key)) {
      if (multi) onChange([...chosen, item.key]); else onChange(item.key);
    }
    setQuery(''); resetHighlight();
    if (!multi) setOpen(false);
  };
  const commitFree = () => {
    if (disabled) return false;
    const key = freeText?.(query.trim()) || '';
    if (!key) return false;
    if (multi) { if (!chosen.has(key)) onChange([...chosen, key]); }
    else onChange(key);
    setQuery(''); resetHighlight();
    return true;
  };
  // commitTyped adds the typed text as a leaving Enter does: the row whose
  // key or label it names exactly (when only one does), else as typed.
  const commitTyped = () => {
    const typed = query.trim();
    if (!typed) return false;
    const exacts = items.filter((item) => !item.disabled
      && (item.key === typed || String(item.label || '').toLowerCase() === typed.toLowerCase()));
    const exact = exacts.find((item) => item.key === typed) || (exacts.length === 1 ? exacts[0] : null);
    if (exact) { add(exact); return true; }
    return commitFree();
  };
  const onKeyDown = (event) => {
    if (event.isComposing || event.keyCode === 229) return;
    if (event.key === 'ArrowDown') {
      event.preventDefault();
      if (!open) { setOpen(true); return; }
      highlightAt(bounded < 0 ? 0 : Math.min(bounded + 1, visible.length - 1));
    } else if (event.key === 'ArrowUp') {
      event.preventDefault(); highlightAt(bounded < 0 ? visible.length - 1 : Math.max(bounded - 1, 0));
    } else if (event.key === 'Enter') {
      // Enter only ever adds. An exact key or label wins over the ranking, a
      // literal-shaped entry is taken as typed, and otherwise the highlighted
      // row is the one added. Taking a value away is the ✓ row's or the
      // chip's, never Enter's: a stray Enter must not empty a dimension.
      const typed = query.trim();
      // A label two rows share (two agents both titled "reviewer") names
      // neither: the highlighted row is the one meant.
      const exacts = typed ? items.filter((item) => !item.disabled
        && (item.key === typed || String(item.label || '').toLowerCase() === typed.toLowerCase())) : [];
      const exact = exacts.find((item) => item.key === typed) || (exacts.length === 1 ? exacts[0] : null);
      if (exact) { event.preventDefault(); add(exact); }
      else if (typed && freeText && literal?.(typed)) { event.preventDefault(); commitFree(); }
      else if (open && bounded >= 0) { event.preventDefault(); add(visible[bounded]); }
      else if (typed && freeText) { event.preventDefault(); commitFree(); }
    } else if (event.key === 'Tab' && open && query.trim() && bounded >= 0) {
      // Tab completes the typed text to the highlighted row's label, the way
      // a shell completes; a second Tab moves focus on as usual.
      const label = visible[bounded].label || visible[bounded].key;
      if (label.toLowerCase() !== query.trim().toLowerCase()) { event.preventDefault(); setQuery(label); }
    } else if (event.key === 'Escape' && open && !alwaysOpen) {
      event.preventDefault(); event.stopPropagation(); setOpen(false);
    }
  };
  const kinds = [...new Set(items.map((item) => item.kind).filter(Boolean))];
  const refineExample = [kinds.length > 1 ? `${kinds[0]}:` : '', items.some((item) => item.group) ? 'group:name' : '']
    .filter(Boolean).map((example) => `“${example}”`).join(' or ');
  const summary = summarize ? comboGroupSummary(items) : [];
  const shownGroups = summary.slice(0, 5);
  // A click in the box must not reach a <label> around it: the label would
  // hand it to its first control (a form row's radio), taking the focus.
  const standIn = selectedLabel !== null;
  const shownText = standIn && !openState && !query ? selectedLabel : query;
  return html`<div class=${`filter-combobox${overlay ? ' filter-combobox-floating' : ''}`} onClick=${(event) => event.preventDefault()}>
    <input ref=${inputRef} id=${inputId} disabled=${disabled} type="text" class="add-member-search filter-combobox-input" value=${shownText}
      placeholder=${standIn && openState && selectedLabel ? selectedLabel : placeholder} autocomplete="off" spellcheck=${false}
      role="combobox" aria-label=${ariaLabel} aria-controls=${listID} aria-expanded=${open ? 'true' : 'false'}
      aria-autocomplete="list" aria-activedescendant=${activeID}
      onFocus=${() => { if (!standIn) setOpen(true); }} onClick=${() => setOpen(true)} onBlur=${() => { setOpen(false); if (commitOnBlur && freeText) commitTyped(); else if (standIn && query) setQuery(''); }}
      onInput=${(event) => {
        // A stand-in box shows the chosen label while closed: what is typed
        // into it then is the start of a search, not more of that label.
        const text = event.currentTarget.value;
        const typed = standIn && !openState && selectedLabel && text.startsWith(selectedLabel) ? text.slice(selectedLabel.length) : text;
        setQuery(typed); resetHighlight(); setOpen(true);
      }}
      onKeyDown=${onKeyDown} />
    ${open && html`<div class="add-member-list filter-combobox-list" id=${listID} role="listbox" aria-busy=${busy ? 'true' : 'false'}
      aria-multiselectable=${multi ? 'true' : 'false'}
      onMouseDown=${(event) => event.preventDefault()}>
      ${summarize ? html`<div class="filter-combobox-tip">Type to search ${items.length} by name, id or group.</div>
        <div class="ui-eyebrow filter-combobox-kind">Groups</div>
        ${shownGroups.map(({ group, count }) => html`<div key=${group} class="add-member-row filter-combobox-group-sum"
          data-group=${group} onMouseDown=${() => { if (group) { setQuery(`group:${group} `); resetHighlight(); } }}>
          <span class="rowname">${group || '(no group)'}</span><span class="id">${count}</span></div>`)}
        ${summary.length > shownGroups.length ? html`<div class="filter-combobox-tip">… ${summary.length - shownGroups.length} more groups</div>` : null}`
    : visible.length === 0 ? html`<div class="ui-empty-block filter-combobox-empty">${query.trim() && freeText?.(query.trim())
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
          onMouseMove=${() => { if (index !== bounded) setHighlightKey(item.key); }}
          onMouseDown=${() => choose(item)}>
          <span class="filter-combobox-check" aria-hidden="true">${chosen.has(item.key) ? '✓' : ''}</span>
          ${item.lead || null}
          <span class="rowname">${item.label || item.key}</span>
          <span class="id">${item.hint ?? item.key}</span>
          ${item.tail || null}
          ${item.disabled && item.reason ? html`<span class="descr">${item.reason}</span>` : null}
        </div>`)}</div>`)}
      ${!summarize && matches.length > visible.length ? html`<div class="filter-combobox-foot">Showing ${visible.length} of ${matches.length} matches. Refine your search: type more${refineExample ? `, or narrow it with ${refineExample}` : ''}.</div>` : null}
    </div>`}
  </div>`;
}
