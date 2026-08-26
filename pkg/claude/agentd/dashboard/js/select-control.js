// Shared non-native Select primitive. The popup is ordinary HTML in the
// browser's top layer, so it never asks the window manager to create or place a
// platform-native select menu. Floating UI owns viewport collision handling;
// this component owns the select/listbox interaction and accessible semantics.

import { h } from 'preact';
import { useId, useLayoutEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  autoUpdate, computePosition, flip, offset, shift, size,
} from '@floating-ui/dom';

const html = htm.bind(h);
const VIEWPORT_PADDING = 8;
const MIN_POPUP_WIDTH = 180;

function isPopoverOpen(element) {
  if (!element?.matches) return false;
  try { return element.matches(':popover-open'); } catch { return false; }
}

function showPopover(element, trigger) {
  if (!element) return;
  element.hidden = false;
  if (typeof element.showPopover === 'function') {
    if (!isPopoverOpen(element)) {
      try { element.showPopover({ source: trigger }); } catch { element.showPopover(); }
    }
    return;
  }
  // DOM shims and older engines get a functional in-document fallback. Modern
  // target browsers take the top-layer path above, which is the i3/Linux fix.
  element.dataset.fallbackOpen = 'true';
}

function hidePopover(element) {
  if (!element) return;
  if (typeof element.hidePopover === 'function' && isPopoverOpen(element)) {
    element.hidePopover();
  }
  delete element.dataset.fallbackOpen;
  element.hidden = true;
}

function enabledIndexes(options) {
  const indexes = [];
  for (let i = 0; i < options.length; i += 1) {
    if (!options[i].disabled) indexes.push(i);
  }
  return indexes;
}

function nextEnabled(options, current, delta) {
  const indexes = enabledIndexes(options);
  if (!indexes.length) return -1;
  const at = indexes.indexOf(current);
  if (at < 0) return delta < 0 ? indexes[indexes.length - 1] : indexes[0];
  return indexes[(at + delta + indexes.length) % indexes.length];
}

function initialHighlight(options, value) {
  const selected = options.findIndex((option) => !option.disabled && option.value === value);
  return selected >= 0 ? selected : nextEnabled(options, -1, 1);
}

export function SelectControl({
  id,
  className = '',
  popupClassName = '',
  value = '',
  options = [],
  open = false,
  busy = false,
  loading = false,
  error = '',
  ariaLabel,
  title,
  children,
  onOpenChange,
  onValueChange,
}) {
  const generatedID = useId();
  const listboxID = `${id || generatedID}-listbox`;
  const triggerRef = useRef(null);
  const popupRef = useRef(null);
  const wasOpenRef = useRef(false);
  const typeaheadRef = useRef({ text: '', timer: null });
  const [highlighted, setHighlighted] = useState(() => initialHighlight(options, value));
  const optionIndexes = useMemo(() => enabledIndexes(options), [options]);

  const requestOpen = (next, reason, restoreFocus = false) => {
    onOpenChange?.(next, { reason, restoreFocus });
    if (!next && restoreFocus) queueMicrotask(() => triggerRef.current?.focus());
  };

  useLayoutEffect(() => {
    if (!open) return;
    setHighlighted(initialHighlight(options, value));
  }, [open, value, options.length]);

  useLayoutEffect(() => {
    const trigger = triggerRef.current;
    const popup = popupRef.current;
    if (!trigger || !popup) return undefined;
    if (!open) {
      hidePopover(popup);
      return undefined;
    }

    showPopover(popup, trigger);
    let live = true;
    const place = () => computePosition(trigger, popup, {
      strategy: 'fixed',
      placement: 'bottom-end',
      middleware: [
        offset(4),
        flip({ padding: VIEWPORT_PADDING }),
        shift({ padding: VIEWPORT_PADDING }),
        size({
          padding: VIEWPORT_PADDING,
          apply({ availableHeight, availableWidth, elements, rects }) {
            const width = Math.min(availableWidth, Math.max(MIN_POPUP_WIDTH, rects.reference.width));
            Object.assign(elements.floating.style, {
              minWidth: `${Math.max(0, width)}px`,
              maxWidth: `${Math.max(0, availableWidth)}px`,
              maxHeight: `${Math.max(0, availableHeight)}px`,
            });
          },
        }),
      ],
    }).then(({ x, y }) => {
      if (!live) return;
      Object.assign(popup.style, { left: `${x}px`, top: `${y}px` });
    });
    const stop = autoUpdate(trigger, popup, place);
    queueMicrotask(() => popupRef.current?.focus());
    return () => {
      live = false;
      stop();
      hidePopover(popup);
    };
  }, [open, options.length]);

  useLayoutEffect(() => {
    const popup = popupRef.current;
    if (!open && wasOpenRef.current && popup?.contains(popup.ownerDocument.activeElement)) {
      queueMicrotask(() => triggerRef.current?.focus());
    }
    wasOpenRef.current = open;
  }, [open]);

  useLayoutEffect(() => () => {
    if (typeaheadRef.current.timer) clearTimeout(typeaheadRef.current.timer);
  }, []);

  const choose = (index) => {
    const option = options[index];
    if (!option || option.disabled || busy) return;
    onValueChange?.(option.value, option);
  };

  const move = (delta) => {
    const next = nextEnabled(options, highlighted, delta);
    if (next >= 0) setHighlighted(next);
  };

  const typeahead = (key) => {
    const state = typeaheadRef.current;
    state.text += key.toLocaleLowerCase();
    if (state.timer) clearTimeout(state.timer);
    state.timer = setTimeout(() => { state.text = ''; state.timer = null; }, 500);
    if (!optionIndexes.length) return;
    const start = Math.max(0, optionIndexes.indexOf(highlighted));
    for (let step = 1; step <= optionIndexes.length; step += 1) {
      const index = optionIndexes[(start + step) % optionIndexes.length];
      if (String(options[index].label || '').toLocaleLowerCase().startsWith(state.text)) {
        setHighlighted(index);
        return;
      }
    }
  };

  const onListboxKeyDown = (event) => {
    switch (event.key) {
      case 'ArrowDown': event.preventDefault(); move(1); break;
      case 'ArrowUp': event.preventDefault(); move(-1); break;
      case 'Home':
        event.preventDefault();
        if (optionIndexes.length) setHighlighted(optionIndexes[0]);
        break;
      case 'End':
        event.preventDefault();
        if (optionIndexes.length) setHighlighted(optionIndexes[optionIndexes.length - 1]);
        break;
      case 'Enter':
      case ' ':
        event.preventDefault(); choose(highlighted); break;
      case 'Escape':
        event.preventDefault(); event.stopPropagation(); requestOpen(false, 'escape', true); break;
      case 'Tab': requestOpen(false, 'tab'); break;
      default:
        if (event.key.length === 1 && !event.ctrlKey && !event.metaKey && !event.altKey) {
          typeahead(event.key);
        }
        break;
    }
  };

  const activeID = highlighted >= 0 ? `${listboxID}-option-${highlighted}` : undefined;
  const errorID = error ? `${listboxID}-error` : undefined;
  return html`<span class="tc-select-root">
    <button ref=${triggerRef} type="button" id=${id}
      class=${`${className} tc-select-trigger`.trim()}
      aria-label=${ariaLabel} aria-haspopup="listbox" aria-expanded=${open ? 'true' : 'false'}
      aria-controls=${listboxID} aria-invalid=${error ? 'true' : undefined}
      aria-describedby=${errorID} title=${title} disabled=${busy}
      onClick=${() => requestOpen(!open, 'trigger')}
      onKeyDown=${(event) => {
        if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp') return;
        event.preventDefault();
        if (!open) requestOpen(true, 'keyboard');
        else popupRef.current?.focus();
      }}
    >${children}<span class="tc-select-caret" aria-hidden="true">▾</span></button>
    <div ref=${popupRef} id=${listboxID} popover="auto" hidden=${!open}
      class=${`tc-select-popover${popupClassName ? ` ${popupClassName}` : ''}`}
      role="listbox" tabindex="-1" aria-label=${ariaLabel}
      aria-busy=${loading ? 'true' : undefined} aria-activedescendant=${activeID}
      onKeyDown=${onListboxKeyDown}
      onToggle=${(event) => {
        if (event.newState === 'closed' && open) requestOpen(false, 'dismiss');
      }}
    >
      ${options.map((option, index) => html`<div key=${option.key || option.value || index}
        id=${`${listboxID}-option-${index}`} role="option"
        class=${`tc-select-option${index === highlighted ? ' highlighted' : ''}${option.disabled ? ' disabled' : ''}`}
        aria-selected=${option.value === value ? 'true' : 'false'}
        aria-disabled=${option.disabled ? 'true' : undefined}
        onMouseMove=${() => { if (!option.disabled) setHighlighted(index); }}
        onMouseDown=${(event) => event.preventDefault()}
        onClick=${() => choose(index)}
      ><span class="tc-select-check" aria-hidden="true">${option.value === value ? '✓' : ''}</span
        ><span class="tc-select-option-label">${option.label}</span></div>`)}
      ${loading && !options.length ? html`<div class="tc-select-status" role="status">Loading…</div>` : null}
      ${error ? html`<div id=${errorID} class="tc-select-error" role="alert">⚠ ${error}</div>` : null}
    </div>
  </span>`;
}
