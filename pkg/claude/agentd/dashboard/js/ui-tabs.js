// Tabs: the standard tab row (ui.css .ui-tabs / .ui-tab). It owns what every
// hand-written tab row repeated: role="tablist" / role="tab" with
// aria-selected, a roving tabindex, Arrow / Home / End to move between tabs,
// Space to pick, and for link tabs (href) a plain click handled in place
// while a modified click (Ctrl/Cmd/Shift/middle) falls through to the href
// so the view opens in a new browser tab.
//
// items: [{ key, label, href?, title?, disabled?, class?, attrs? }]; attrs
// spreads onto the tab (data-* hooks, aria-controls). onSelect(key, event)
// runs on a plain activation (event is the click; a keyboard pick passes
// none); value is the selected key.

import { h } from 'preact';
import htm from 'htm';
import { isModifiedClick } from './helpers.js';

const html = htm.bind(h);

const MOVES = { ArrowRight: 1, ArrowDown: 1, ArrowLeft: -1, ArrowUp: -1 };

function join(...parts) {
  return parts.filter(Boolean).join(' ');
}

export function Tabs({ items, value, onSelect, label, class: extra, className, domRef, ...rest }) {
  const pick = (event, item) => {
    if (item.disabled) return;
    if (item.href && event.type === 'click' && isModifiedClick(event)) return;
    event.preventDefault();
    onSelect?.(item.key, event.type === 'click' ? event : undefined);
  };
  const keyDown = (event, item) => {
    if (event.key === ' ' || event.key === 'Spacebar' || (event.key === 'Enter' && !item.href)) {
      pick(event, item);
      return;
    }
    // Modified keys (Alt+Left is browser Back) belong to the browser.
    if (event.altKey || event.ctrlKey || event.metaKey) return;
    const tabs = [...event.currentTarget.parentElement.querySelectorAll('[role="tab"]:not(:disabled):not([aria-disabled="true"])')];
    const at = tabs.indexOf(event.currentTarget);
    let next = -1;
    if (event.key in MOVES) next = (at + MOVES[event.key] + tabs.length) % tabs.length;
    else if (event.key === 'Home') next = 0;
    else if (event.key === 'End') next = tabs.length - 1;
    if (next < 0 || !tabs[next]) return;
    event.preventDefault();
    tabs[next].focus();
    const target = items.find((candidate) => candidate.key === tabs[next].dataset.tabKey);
    if (target && !target.disabled && target.key !== value) onSelect?.(target.key);
  };
  // Roving tabindex: the selected tab is the one Tab stop; with none selected,
  // the first enabled tab is.
  const stop = items.some((item) => item.key === value) ? value : items.find((item) => !item.disabled)?.key;
  return html`<div role="tablist" aria-label=${label} ...${rest} ref=${domRef} class=${join('ui-tabs', extra, className)}>
    ${items.map((item) => {
      const selected = item.key === value;
      const props = {
        ...item.attrs,
        key: item.key,
        role: 'tab',
        class: join('ui-tab', item.class, selected && 'active'),
        title: item.title,
        'aria-selected': selected ? 'true' : 'false',
        tabindex: item.key === stop ? '0' : '-1',
        'data-tab-key': item.key,
        onClick: (event) => pick(event, item),
        onKeyDown: (event) => keyDown(event, item),
      };
      return item.href
        ? html`<a ...${props} href=${item.href} aria-disabled=${item.disabled ? 'true' : undefined}>${item.label}</a>`
        : html`<button data-ui-exempt type="button" ...${props} disabled=${item.disabled}>${item.label}</button>`;
    })}
  </div>`;
}
