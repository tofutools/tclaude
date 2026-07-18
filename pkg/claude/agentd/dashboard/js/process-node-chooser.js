// Anchored node-type chooser shared by connector-drop flows. Presentation
// comes from TCL-435's canonical node-type command builder and search uses the
// dashboard command palette's ranking rules; this module only owns the small
// combobox/listbox interaction and focus lifecycle.

// dashboard-imperative-boundary: process-graph

import { buildProcessNodeTypeCommands } from './process-command-registry.js';
import { rankCommands } from './palette-score.js';
import { isWizardActive } from './slop.js';

let nextChooserID = 1;

function h(documentRef, tag, attrs = {}, ...children) {
  const element = documentRef.createElement(tag);
  for (const [key, value] of Object.entries(attrs)) {
    if (value === undefined || value === null) continue;
    if (key === 'class') element.className = value;
    else if (key === 'text') element.textContent = value;
    else element.setAttribute(key, String(value));
  }
  for (const child of children) if (child) element.append(child);
  return element;
}

function clamp(value, min, max) {
  return Math.max(min, Math.min(value, max));
}

export function openProcessNodeTypeChooser({
  host,
  anchor,
  onChoose,
  onClose = null,
  restoreFocus = null,
  availability = null,
  wizard = isWizardActive(),
  documentRef = document,
} = {}) {
  if (!host) throw new TypeError('node-type chooser requires a host');
  if (typeof onChoose !== 'function') throw new TypeError('node-type chooser requires onChoose');

  const chooserID = nextChooserID++;
  const listID = `process-node-chooser-list-${chooserID}`;
  const input = h(documentRef, 'input', {
    class: 'process-node-chooser-input', type: 'search', role: 'combobox',
    autocomplete: 'off', spellcheck: 'false', 'aria-autocomplete': 'list',
    'aria-expanded': 'true', 'aria-controls': listID,
    'aria-label': wizard ? 'Choose a rune to connect' : 'Choose a node type to connect',
    placeholder: wizard ? 'Search runes…' : 'Search node types…',
  });
  const list = h(documentRef, 'div', {
    class: 'process-node-chooser-list process-scroll-surface', id: listID, role: 'listbox',
    'aria-label': wizard ? 'Available runes' : 'Available node types',
  });
  const empty = h(documentRef, 'p', {
    class: 'process-node-chooser-empty', text: 'No matching node types.', hidden: '',
  });
  const cancel = h(documentRef, 'button', {
    class: 'process-node-chooser-cancel', type: 'button', text: 'Cancel',
  });
  const chooser = h(documentRef, 'div', {
    class: 'process-node-chooser', role: 'dialog', 'aria-modal': 'false',
    'aria-label': wizard ? 'Conjure connected rune' : 'Create connected node',
  }, h(documentRef, 'div', {
    class: 'process-node-chooser-title', text: wizard ? 'Conjure the next rune' : 'Create connected node',
  }), input, list, empty, cancel);

  const commands = buildProcessNodeTypeCommands({ onCreate: onChoose, availability, wizard });
  let filtered = commands.slice();
  let selected = 0;
  let closed = false;

  const place = () => {
    const x = Number.isFinite(anchor?.x) ? anchor.x : 0;
    const y = Number.isFinite(anchor?.y) ? anchor.y : 0;
    const hostRect = host.getBoundingClientRect?.() || { width: host.clientWidth || 0, height: host.clientHeight || 0 };
    const chooserRect = chooser.getBoundingClientRect?.() || { width: chooser.offsetWidth || 0, height: chooser.offsetHeight || 0 };
    const maxLeft = hostRect.width > 0 ? Math.max(8, hostRect.width - chooserRect.width - 8) : x;
    const left = hostRect.width > 0 ? clamp(x, 8, maxLeft) : x;
    let top = y + 8;
    if (hostRect.height > 0 && chooserRect.height > 0 && top + chooserRect.height > hostRect.height - 8) {
      top = Math.max(8, y - chooserRect.height - 8);
    }
    chooser.style.left = `${Math.round(left)}px`;
    chooser.style.top = `${Math.round(top)}px`;
  };

  const close = (reason, { focus = true } = {}) => {
    if (closed) return false;
    closed = true;
    documentRef.removeEventListener('pointerdown', onOutsidePointerDown, true);
    chooser.remove();
    onClose?.(reason);
    if (focus) restoreFocus?.();
    return true;
  };

  const choose = (index) => {
    const command = filtered[index];
    if (!command || command.enabled === false) return false;
    close('select', { focus: false });
    return command.run();
  };

  const render = () => {
    list.replaceChildren();
    empty.hidden = filtered.length > 0;
    if (!filtered.length) {
      input.removeAttribute('aria-activedescendant');
      return;
    }
    selected = clamp(selected, 0, filtered.length - 1);
    filtered.forEach((command, index) => {
      const optionID = `process-node-chooser-option-${chooserID}-${index}`;
      const option = h(documentRef, 'button', {
        class: `process-node-chooser-option${index === selected ? ' is-selected' : ''}${command.enabled === false ? ' is-disabled' : ''}`,
        type: 'button', role: 'option', id: optionID, tabindex: '-1',
        'aria-selected': index === selected ? 'true' : 'false',
        'aria-disabled': command.enabled === false ? 'true' : 'false',
        'data-command-id': command.id,
      }, h(documentRef, 'span', { class: 'process-node-chooser-icon', text: command.icon }),
      h(documentRef, 'span', { class: 'process-node-chooser-copy' },
        h(documentRef, 'span', { class: 'process-node-chooser-label', text: command.label }),
        h(documentRef, 'span', {
          class: 'process-node-chooser-hint',
          text: command.enabled === false ? command.disabledReason : command.hint,
        })));
      option.addEventListener('pointermove', () => {
        if (selected === index) return;
        selected = index;
        render();
      });
      option.addEventListener('pointerdown', (event) => event.preventDefault());
      option.addEventListener('click', () => choose(index));
      list.append(option);
    });
    input.setAttribute('aria-activedescendant', `process-node-chooser-option-${chooserID}-${selected}`);
    list.children[selected]?.scrollIntoView?.({ block: 'nearest' });
  };

  const move = (delta) => {
    if (!filtered.length) return;
    selected = (selected + delta + filtered.length) % filtered.length;
    render();
  };

  function onOutsidePointerDown(event) {
    if (!chooser.contains(event.target)) close('outside');
  }

  input.addEventListener('input', () => {
    filtered = rankCommands(commands, input.value);
    selected = 0;
    render();
  });
  input.addEventListener('keydown', (event) => {
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault();
      move(event.key === 'ArrowDown' ? 1 : -1);
    } else if (event.key === 'Home' || event.key === 'End') {
      event.preventDefault();
      selected = event.key === 'Home' ? 0 : Math.max(0, filtered.length - 1);
      render();
    } else if (event.key === 'Enter') {
      event.preventDefault();
      choose(selected);
    }
  });
  chooser.addEventListener('keydown', (event) => {
    if (event.key !== 'Escape') return;
    event.preventDefault();
    event.stopPropagation();
    close('escape');
  });
  cancel.addEventListener('click', () => close('cancel'));

  host.append(chooser);
  render();
  place();
  queueMicrotask(() => {
    if (closed) return;
    place();
    input.focus({ preventScroll: true });
  });
  documentRef.addEventListener('pointerdown', onOutsidePointerDown, true);

  const dispose = ({ focus = false } = {}) => close('dispose', { focus });
  dispose.close = () => close('cancel');
  dispose.element = chooser;
  dispose.input = input;
  return dispose;
}
