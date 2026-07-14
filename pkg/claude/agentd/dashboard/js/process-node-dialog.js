// process-node-dialog.js -- the structured node editing surface (TCL-298):
// logical zoom into a node opens its stages as a dialog. ONE component, two
// modes: mode 'edit' stages changes in a private node draft, then Save commits
// that draft through ProcessEditModel.updateNode's single undo gate, and
// mode 'view' renders the exact same markup as a read-only detail card for
// the live viewer (design §8b) — the same controls, disabled, so the §9
// unlock phase flips a flag rather than growing a second component.
//
// The performer editor is ONE shared sub-component keyed by kind (uniform
// performer contract, §2). Kind-specific fields come from the
// PERFORMER_FIELDS table in process-node-form.js — never from per-kind
// branches here. Program performers are command execution; the dialog says
// so next to the command field (§10).
//
// Template content is untrusted at render time regardless of authoring
// surface: every value lands via textContent / input.value (the h() helper),
// never via HTML string injection.

import {
  PERFORMER_KINDS, RETRY_ON_FAIL_MODES, PLAN_APPROVAL_MODES,
  performerFieldsFor, defaultPerformer, setPerformerKind, setPerformerField,
  setChoiceOutcome,
  setContactField, setRetryField, setStageEnabled, setPlanApproval,
  addCheck, removeCheck, moveCheck, setCheckID,
  setCaptures, setWaitField, setNodeText, formatLines,
} from './process-node-form.js';
import { bindDialogFocus } from './dialog-focus-core.js';
import { isTopmostOverlay } from './overlay-stack.js';

function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs)) {
    if (value === undefined || value === null) continue;
    if (key === 'class') el.className = value;
    else if (key === 'text') el.textContent = value;
    else if (key.startsWith('on') && typeof value === 'function') el.addEventListener(key.slice(2), value);
    else el.setAttribute(key, String(value));
  }
  for (const child of children) if (child) el.append(child);
  return el;
}

// buildNodeDetail renders the shared node card: header, per-type sections,
// and the edges summary. `commit(mutate)` routes every edit through the edit
// dialog draft and re-renders; in view mode commit is absent and every control
// renders disabled.
export function buildNodeDetail(model, nodeId, { mode = 'edit', commit = null, onClose = null } = {}) {
  const node = model.node(nodeId);
  const readOnly = mode !== 'edit' || !commit;
  const root = h('div', { class: `process-node-detail${readOnly ? ' is-readonly' : ''}` });
  if (!node) {
    root.append(h('p', { class: 'process-node-missing', text: `Node ${nodeId} no longer exists.` }));
    return root;
  }
  const type = node.type || 'task';

  const header = h('div', { class: 'process-node-detail-head' },
    h('span', { class: 'process-inspector-kind', text: `${type} node` }),
    h('span', { class: 'process-inspector-id', text: nodeId }),
    readOnly ? h('span', { class: 'process-node-readonly-badge', text: 'read-only' }) : null,
    h('span', { class: 'spacer' }),
    onClose ? h('button', {
      class: 'process-node-close', type: 'button', text: '✕',
      title: 'Close', 'aria-label': 'Close node dialog', onclick: () => onClose(),
    }) : null,
  );
  root.append(header);

  const control = (el) => {
    if (readOnly) el.disabled = true;
    return el;
  };

  // Associate a commit with the control whose raw DOM value produced it.
  // The transactional wrapper uses this identity to retain rejected values
  // (for example a duplicate check id) until that same control is corrected.
  const applyFromControl = (controlEl, apply, value, restore) => {
    if (!apply) return true;
    commit.activeControl = controlEl;
    try {
      const accepted = apply(value) !== false;
      if (accepted || commit.lastFailure === 'blocked') {
        controlEl.removeAttribute('aria-invalid');
        if (!accepted) restore?.();
      } else controlEl.setAttribute('aria-invalid', 'true');
      return accepted;
    } finally {
      commit.activeControl = null;
    }
  };

  const textField = (label, value, apply, { multiline = false, hint = '', placeholder = '' } = {}) => {
    const input = control(h(multiline ? 'textarea' : 'input', {
      class: multiline ? 'process-node-textarea' : 'process-node-input',
      spellcheck: 'false', placeholder,
      ...(multiline ? { rows: '3' } : { type: 'text' }),
    }));
    input.value = value || '';
    if (apply && !readOnly) input.addEventListener('change', (event) => {
      if (!applyFromControl(input, apply, input.value, () => { input.value = value || ''; })) event.preventDefault();
    });
    const field = h('label', { class: 'field process-node-field' },
      h('span', { class: 'process-node-field-label', text: label }), input);
    if (hint) field.title = hint;
    return field;
  };

  const selectField = (label, options, value, apply, { blankLabel = null } = {}) => {
    const select = control(h('select', { class: 'process-node-select', 'aria-label': label }));
    const selectedValue = options.includes(value) ? value : (blankLabel !== null ? '' : options[0]);
    if (blankLabel !== null) select.append(h('option', {
      value: '', text: blankLabel, selected: selectedValue === '' ? '' : undefined,
    }));
    for (const option of options) select.append(h('option', {
      value: option, text: option, selected: option === selectedValue ? '' : undefined,
    }));
    if (apply && !readOnly) select.addEventListener('change', (event) => {
      if (!applyFromControl(select, apply, select.value, () => {
        for (const option of select.querySelectorAll('option')) {
          option.selected = option.value === selectedValue;
        }
      })) event.preventDefault();
    });
    return h('label', { class: 'field process-node-field' },
      h('span', { class: 'process-node-field-label', text: label }), select);
  };

  const section = (title, ...children) => h('section', { class: 'process-node-section' },
    h('h4', { class: 'process-node-section-title', text: title }),
    ...children);

  // The ONE shared performer editor. `locate(draft)` addresses this slot's
  // performer inside the draft node (work / plan / check[i] / review /
  // decider) so the same component edits every slot uniformly.
  const performerEditor = (performer, locate, { choiceRouting = true } = {}) => {
    const wrap = h('div', { class: 'process-performer-editor' });
    const kind = typeof performer?.kind === 'string' && performer.kind ? performer.kind : 'agent';
    const known = PERFORMER_KINDS.includes(kind);
    // An unrecognized stored kind renders AS ITSELF (never silently coerced
    // to agent — the card must not assert a kind the model rejects); picking
    // a supported kind normalizes it through setPerformerKind.
    wrap.append(selectField('kind', known ? PERFORMER_KINDS : [...PERFORMER_KINDS, kind], kind,
      (value) => commit((draft) => setPerformerKind(locate(draft), value))));
    if (!known) {
      wrap.append(h('p', { class: 'process-node-empty', text: `Unknown performer kind ${kind}: validation rejects it — pick a supported kind.` }));
    }
    for (const field of known ? performerFieldsFor(kind) : []) {
      if (field.outcomeMap) {
        if (!choiceRouting || !(performer?.choices || []).length) continue;
        const routes = h('div', { class: 'process-choice-outcomes', title: field.hint },
          h('span', { class: 'process-node-field-label', text: field.label }));
        for (const label of performer.choices) {
          routes.append(selectField(label, ['pass', 'fail'], performer.choiceOutcomes?.[label] || 'pass',
            (value) => commit((draft) => setChoiceOutcome(locate(draft), label, value))));
        }
        wrap.append(routes);
        continue;
      }
      const value = field.list ? formatLines(performer?.[field.key]) : performer?.[field.key];
      wrap.append(textField(field.label, value,
        (text) => commit((draft) => setPerformerField(locate(draft), field.key, text, { choiceRouting })),
        { multiline: !!(field.multiline || field.list), hint: field.hint }));
      if (field.key === 'run') {
        wrap.append(h('p', {
          class: 'process-node-security-note',
          text: '⚠ Program performers are command execution: this command runs on the host when the node activates.',
        }));
      }
    }
    const contact = performer?.contact || {};
    wrap.append(h('div', { class: 'process-node-contact', title: 'Contact schedule for this slot: nudge cadence, budget, escalation target' },
      h('span', { class: 'process-node-field-label', text: 'contact schedule' },),
      textField('cadence', contact.cadence, (value) => commit((draft) => setContactField(locate(draft), 'cadence', value)), { placeholder: '30m' }),
      textField('budget', contact.budget === undefined ? '' : String(contact.budget), (value) => commit((draft) => setContactField(locate(draft), 'budget', value)), { placeholder: '5' }),
      textField('escalate to', contact.escalationTarget, (value) => commit((draft) => setContactField(locate(draft), 'escalationTarget', value)), { placeholder: 'human:operator' }),
    ));
    return wrap;
  };

  const stageToggle = (label, enabled, apply) => {
    const checkbox = control(h('input', { type: 'checkbox', class: 'process-node-toggle' }));
    checkbox.checked = enabled;
    if (!readOnly) checkbox.addEventListener('change', (event) => {
      if (!applyFromControl(checkbox, apply, checkbox.checked, () => { checkbox.checked = enabled; })) event.preventDefault();
    });
    return h('label', { class: 'process-node-stage-toggle' }, checkbox,
      h('span', { text: label }));
  };

  // Shared prose fields. Start/end nodes are label/doc only (the spec's
  // whole dialog for them); the richer types get description too.
  const meta = [textField('label', node.name, (value) => commit((draft) => setNodeText(draft, 'name', value)))];
  if (type !== 'start' && type !== 'end') {
    meta.push(textField('description', node.description, (value) => commit((draft) => setNodeText(draft, 'description', value))));
  }
  meta.push(textField('doc', node.doc, (value) => commit((draft) => setNodeText(draft, 'doc', value)), { multiline: true }));
  root.append(section(type === 'start' || type === 'end' ? 'label / doc' : 'node', ...meta));

  if (type === 'task') {
    const plan = section('plan',
      stageToggle('plan before work', !!node.plan, (enabled) => commit((draft) => setStageEnabled(draft, 'plan', enabled))));
    if (node.plan) {
      plan.append(selectField('approval', PLAN_APPROVAL_MODES, node.plan.approval || 'auto',
        (value) => commit((draft) => setPlanApproval(draft, value))));
      plan.append(performerEditor(node.plan.performer, (draft) => draft.plan.performer));
    }
    root.append(plan);

    if (!node.performer && !readOnly) {
      // A task minted from the palette has no performer yet; give the editor
      // a real slot to fill in (the model requires one to validate).
      root.append(section('work', h('button', {
        class: 'process-action process-node-add', type: 'button', text: 'add work performer',
        onclick: () => commit((draft) => { draft.performer = defaultPerformer('agent'); }),
      })));
    } else {
      root.append(section('work', performerEditor(node.performer || defaultPerformer('agent'), (draft) => {
        if (!draft.performer) draft.performer = defaultPerformer('agent');
        return draft.performer;
      })));
    }

    const checks = section('checks');
    (node.checks || []).forEach((check, index) => {
      const row = h('div', { class: 'process-node-check' },
        h('div', { class: 'process-node-check-head' },
          textField('check id', check.id, (value) => commit((draft) => setCheckID(draft, index, value))),
          h('span', { class: 'spacer' }),
          readOnly ? null : h('button', { class: 'process-action', type: 'button', text: '↑', title: 'Move check up', 'aria-label': `Move check ${check.id} up`, onclick: () => commit((draft) => moveCheck(draft, index, -1)) }),
          readOnly ? null : h('button', { class: 'process-action', type: 'button', text: '↓', title: 'Move check down', 'aria-label': `Move check ${check.id} down`, onclick: () => commit((draft) => moveCheck(draft, index, 1)) }),
          readOnly ? null : h('button', { class: 'process-action process-action-danger', type: 'button', text: 'remove', 'aria-label': `Remove check ${check.id}`, onclick: () => commit((draft) => removeCheck(draft, index)) }),
        ),
        performerEditor(check.performer, (draft) => draft.checks[index].performer));
      checks.append(row);
    });
    if (!(node.checks || []).length) checks.append(h('p', { class: 'process-node-empty', text: 'No checks: work settles without gate verdicts.' }));
    if (!readOnly) {
      checks.append(h('button', {
        class: 'process-action process-node-add', type: 'button', text: '+ add check',
        onclick: () => commit((draft) => addCheck(draft)),
      }));
    }
    root.append(checks);

    const review = section('review',
      stageToggle('review gate after checks', !!node.review, (enabled) => commit((draft) => setStageEnabled(draft, 'review', enabled))));
    if (node.review) review.append(performerEditor(node.review.performer, (draft) => draft.review.performer));
    root.append(review);

    root.append(section('retry policy',
      textField('max attempts', node.retry?.maxAttempts === undefined ? '' : String(node.retry.maxAttempts),
        (value) => commit((draft) => setRetryField(draft, 'maxAttempts', value)), { placeholder: 'unset' }),
      selectField('on fail', RETRY_ON_FAIL_MODES, node.retry?.onFail,
        // Unset onFail resolves to model.DefaultRetryMode: fresh-attempt (the
        // conservative choice — never trust a possibly-poisoned context).
        (value) => commit((draft) => setRetryField(draft, 'onFail', value)), { blankLabel: 'default (fresh-attempt)' })));

    root.append(section('captures',
      textField('published outputs', formatLines(node.captures),
        (value) => commit((draft) => setCaptures(draft, value)),
        { multiline: true, hint: 'Names of outputs this node publishes for downstream nodes, one per line', placeholder: 'one-name-per-line' })));
  }

  if (type === 'decision') {
    // The displayed performer and the one the first edit mints must agree:
    // a missing decider renders (and is created) as human, so the field set
    // shown is the field set written to.
    root.append(section('decider', performerEditor(node.performer || defaultPerformer('human'), (draft) => {
      if (!draft.performer) draft.performer = defaultPerformer('human');
      return draft.performer;
    }, { choiceRouting: false })));
  }

  if (type === 'wait') {
    root.append(section('wait / timer',
      textField('duration', node.wait?.duration, (value) => commit((draft) => setWaitField(draft, 'duration', value)), { placeholder: '30m', hint: 'Sleep for a Go duration' }),
      textField('until', node.wait?.until, (value) => commit((draft) => setWaitField(draft, 'until', value)), { hint: 'Wait until a point in time' }),
      textField('signal', node.wait?.signal, (value) => commit((draft) => setWaitField(draft, 'signal', value)), { hint: 'Wait for a named external signal' })));
  }

  // Outgoing edges, read-only in every mode: topology is edited on the
  // canvas. For decision nodes this doubles as the choices → edge mapping.
  if (type !== 'end') {
    const edges = model.outgoingEdges(nodeId);
    const list = h('div', { class: 'process-node-edges' });
    for (const edge of edges) {
      list.append(h('div', { class: 'process-node-edge-row' },
        h('span', { class: 'process-node-edge-outcome', text: edge.outcome }),
        h('span', { class: 'process-node-edge-arrow', text: '→' }),
        h('span', { class: 'process-node-edge-target', text: edge.to })));
    }
    if (!edges.length) list.append(h('p', { class: 'process-node-empty', text: 'No outgoing edges yet.' }));
    root.append(section(type === 'decision' ? 'choices → edges' : 'edges',
      list, h('p', { class: 'process-node-edges-note', text: 'Edges are edited on the canvas, not in this dialog.' })));
  }

  return root;
}

// openNodeDialog wraps the shared detail card in a modal on the editor's
// established .modal-overlay styling — an owned overlay, never the shared
// global confirm singleton (which only offers two fixed buttons and must not
// be double-booked). Returns a dispose function (resolving dialog close) so
// the editor can slot it into its single-modal discipline.
export function openNodeDialog({
  model, nodeId, mode = 'edit', onMutated = null, onClosed = null,
  confirmDiscard = async () => false,
}) {
  const body = h('div', { class: 'process-node-dialog-body' });
  const cancelButton = h('button', { class: 'process-node-cancel', type: 'button', text: mode === 'edit' ? 'Cancel' : 'Close' });
  const saveButton = mode === 'edit'
    ? h('button', { class: 'primary process-node-save', type: 'button', text: 'Save' }) : null;
  const actions = h('div', { class: 'modal-buttons process-node-dialog-actions' }, cancelButton, saveButton);
  const dialog = h('div', { class: 'modal process-node-dialog', role: 'dialog', 'aria-modal': 'true', 'aria-label': `Node ${nodeId}` }, body, actions);
  const overlay = h('div', { class: 'modal-overlay show process-node-modal' },
    dialog);
  const status = h('p', { class: 'process-node-status', role: 'status' });

  const original = structuredClone(model.node(nodeId));
  let draft = structuredClone(original);
  let disposed = false;
  let confirming = false;
  const invalidControls = new Set();
  let releaseDialogFocus = () => {};
  const isDirty = () => mode === 'edit'
    && (invalidControls.size > 0 || JSON.stringify(draft) !== JSON.stringify(original));
  const dispose = () => {
    if (disposed) return;
    disposed = true;
    overlay.remove();
    document.removeEventListener('keydown', onKey, true);
    releaseDialogFocus();
    onClosed?.();
  };
  dispose.isDirty = isDirty;

  const draftModel = {
    node: (id) => id === nodeId ? draft : model.node(id),
    outgoingEdges: (id) => model.outgoingEdges(id),
  };

  const flushActiveControl = () => {
    const active = document.activeElement;
    if (active && body.contains(active) && active.matches?.('input, textarea, select')) {
      active.dispatchEvent(new Event('change', { bubbles: true, cancelable: true }));
    }
    return invalidControls.size === 0;
  };

  const suspendForConfirmation = (suspended) => {
    overlay.inert = suspended;
    overlay.toggleAttribute('inert', suspended);
    if (suspended) overlay.setAttribute('aria-hidden', 'true');
    else overlay.removeAttribute('aria-hidden');
    dialog.setAttribute('aria-modal', suspended ? 'false' : 'true');
  };

  const requestCancel = async () => {
    flushActiveControl();
    if (disposed || confirming) return false;
    if (!isDirty()) {
      dispose();
      return true;
    }
    confirming = true;
    suspendForConfirmation(true);
    let discard = false;
    try {
      discard = await confirmDiscard();
    } catch (error) {
      status.textContent = error.message || 'Could not confirm discard.';
      status.classList.add('is-error');
    } finally {
      confirming = false;
      if (!disposed) suspendForConfirmation(false);
    }
    if (discard && !disposed) dispose();
    else if (!disposed) (saveButton || cancelButton).focus();
    return !!discard;
  };

  const replaceNode = (target, source) => {
    for (const key of Object.keys(target)) delete target[key];
    Object.assign(target, structuredClone(source));
  };

  const save = () => {
    if (!flushActiveControl()) return false;
    if (disposed || mode !== 'edit') return false;
    try {
      const changed = model.updateNode(nodeId, (node) => replaceNode(node, draft));
      if (changed) onMutated?.();
      dispose();
      return true;
    } catch (error) {
      status.textContent = error.message;
      status.classList.add('is-error');
      return false;
    }
  };

  const onKey = (event) => {
    // The shared discard confirmation owns the keyboard while it is open. A
    // document-capture shortcut must neither save behind it nor close this
    // dialog in response to keys from another overlay.
    if (confirming || !overlay.contains(event.target)) return;
    if (mode === 'edit' && event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      event.stopImmediatePropagation();
      save();
    }
  };

  // commit routes one mutation through the private draft, then re-renders the
  // card. A rejected mutation surfaces in the status line and leaves both the
  // draft and edit model untouched. Save is the only model mutation path.
  const commit = (mutate) => {
    const source = commit.activeControl;
    commit.lastFailure = null;
    if (invalidControls.size && !invalidControls.has(source)) {
      commit.lastFailure = 'blocked';
      status.textContent = 'Correct the highlighted invalid field first; this change was not applied.';
      status.classList.add('is-error');
      return false;
    }
    let changed = false;
    try {
      const next = structuredClone(draft);
      mutate(next);
      changed = JSON.stringify(next) !== JSON.stringify(draft);
      if (changed) draft = next;
    } catch (error) {
      commit.lastFailure = 'invalid';
      if (source) invalidControls.add(source);
      status.textContent = error.message;
      status.classList.add('is-error');
      return false;
    }
    if (source) invalidControls.delete(source);
    status.textContent = '';
    status.classList.remove('is-error');
    render();
    return changed;
  };

  // Re-rendering replaces every control, so restore the scroll position and
  // put focus back on the control at the same tab position — a change event
  // fired from a focused select/checkbox must not dump keyboard users back
  // at the top of the dialog after every single edit. (On structural edits
  // the index can shift by the inserted/removed controls; landing on a
  // neighbor is the acceptable degradation.)
  const focusables = () => Array.from(body.querySelectorAll('input, select, textarea, button'));
  const render = () => {
    const active = document.activeElement;
    const focusIndex = active && body.contains(active) ? focusables().indexOf(active) : -1;
    const scrollTop = body.scrollTop;
    const detail = buildNodeDetail(draftModel, nodeId, {
      mode, onClose: requestCancel,
      commit: mode === 'edit' ? commit : null,
    });
    body.replaceChildren(detail, status);
    body.scrollTop = scrollTop;
    if (focusIndex >= 0) focusables()[focusIndex]?.focus();
  };

  render();
  // A text input's blur/change re-renders the scroll body. Claim pointer
  // dismissal before that blur can replace the header close button; keyboard
  // activation still follows the button's ordinary click handler.
  body.addEventListener('pointerdown', (event) => {
    if (!event.target.closest?.('.process-node-close')) return;
    event.preventDefault();
    void requestCancel();
  });
  cancelButton.addEventListener('click', () => { void requestCancel(); });
  saveButton?.addEventListener('click', save);
  overlay.addEventListener('click', (event) => { if (event.target === overlay) void requestCancel(); });
  document.addEventListener('keydown', onKey, true);
  document.body.append(overlay);
  releaseDialogFocus = bindDialogFocus({
    dialog,
    initialFocus: () => overlay.querySelector('.process-node-input:not(:disabled)') || cancelButton,
    onEscape: () => { void requestCancel(); },
    shouldHandle: () => !confirming && isTopmostOverlay(overlay),
  });
  dispose.requestClose = requestCancel;
  return dispose;
}
