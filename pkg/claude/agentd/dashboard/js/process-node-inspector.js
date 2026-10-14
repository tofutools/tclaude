// process-node-inspector.js -- the editor's docked node inspector (mock E3,
// tcl-oypy1p), replacing the modal node dialog. Selecting one node opens it
// beside the canvas as a stack of blocks (process-node-blocks.js): required
// blocks for the node kind, optional blocks that carry data, and blocks the
// author added. Blocks reorder by drag or ↑/↓ (a per-kind UI preference) and
// optional ones remove by clearing their slice.
//
// There is no draft: every edit commits straight through the editor's
// `editNode` gate (ProcessEditModel.updateNode inside the editor's mutate),
// so it is one undoable step and is validated live like any canvas edit. A
// refused edit leaves the model untouched and snaps the field back.

import { h } from 'preact';
import { useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { dashPrefs } from './prefs.js';
import {
  RETRY_ON_FAIL_MODES, PLAN_APPROVAL_MODES,
  defaultPerformer, setStageEnabled, setPlanApproval, addCheck, removeCheck, moveCheck, setCheckID,
  setRetryField, setCaptures, setWaitField, setNodeText, formatLines,
} from './process-node-form.js';
import {
  nodeBlock, visibleBlocks, addableBlocks, removeBlock, moveKey, moveKeyBefore, mergeOrder,
  upstreamInputs, stageSummary,
  setSubprocessPort, setSubprocessRef,
} from './process-node-blocks.js';
import { NodeField, NodeSelect, PerformerEditor } from './process-node-fields.js';

const html = htm.bind(h);

export const BLOCK_ORDER_PREF = 'tclaude.dash.processInspector.blockOrder.';

// Prompt and Bounds own these performer fields; Performer shows the rest.
const PROMPT_FIELDS = ['ask', 'prompt'];
const BOUNDS_FIELDS = ['timeout'];

export function readBlockOrder(kind) {
  try {
    const parsed = JSON.parse(dashPrefs.getItem(BLOCK_ORDER_PREF + kind) || '[]');
    return Array.isArray(parsed) ? parsed.filter((key) => typeof key === 'string') : [];
  } catch {
    return [];
  }
}

function writeBlockOrder(kind, order) {
  dashPrefs.setItem(BLOCK_ORDER_PREF + kind, JSON.stringify(order));
}

// The node's main performer slot: a task's work performer, a decision's
// decider. Reading it never mints one; editing through it does.
function mainPerformer(node) {
  return node.performer || defaultPerformer(node.type === 'decision' ? 'human' : 'agent');
}

function locateMain(type) {
  return (draft) => {
    if (!draft.performer) draft.performer = defaultPerformer(type === 'decision' ? 'human' : 'agent');
    return draft.performer;
  };
}

function PerformerBlock({ node, commit, readOnly }) {
  const type = node.type || 'task';
  if (type === 'task' && !node.performer && !readOnly) {
    return html`<button class="process-action process-node-add" type="button"
      onClick=${() => commit('work.add', (draft) => { draft.performer = defaultPerformer('agent'); })}>add work performer</button>`;
  }
  return html`<${PerformerEditor} path="performer" locate=${locateMain(type)} performer=${mainPerformer(node)}
    commit=${commit} readOnly=${readOnly} choiceRouting=${type !== 'decision'} omit=${[...PROMPT_FIELDS, ...BOUNDS_FIELDS]} />`;
}

function PromptBlock({ node, commit, readOnly }) {
  return html`<${PerformerEditor} path="performer" locate=${locateMain(node.type || 'task')} performer=${mainPerformer(node)}
    commit=${commit} readOnly=${readOnly} showKind=${false} only=${PROMPT_FIELDS} />`;
}

function BoundsBlock({ node, commit, readOnly }) {
  return html`<${PerformerEditor} path="performer" locate=${locateMain(node.type || 'task')} performer=${mainPerformer(node)}
    commit=${commit} readOnly=${readOnly} showKind=${false} only=${BOUNDS_FIELDS} contact=${true} />`;
}

function StagesBlock({ node, commit, readOnly }) {
  const field = (key, label, value, mutate, options = {}) => html`<${NodeField} fieldKey=${key} label=${label} value=${value || ''}
    readOnly=${readOnly} apply=${(fieldKey, raw) => commit(fieldKey, (draft) => mutate(draft, raw))} ...${options} />`;
  const toggle = (key, label, stage) => html`<label class="process-node-stage-toggle">
    <input type="checkbox" class="process-node-toggle" checked=${!!node[stage]} disabled=${readOnly} data-node-field=${key}
      onChange=${(event) => commit(key, (draft) => setStageEnabled(draft, stage, event.currentTarget.checked))} />
    <span>${label}</span>
  </label>`;
  return html`<div class="process-node-stages">
    <div class="pni-stage-chips" aria-label="Work pipeline">${stageSummary(node).map((stage, i) => html`${i > 0 && html`<span class="pni-stage-arrow" aria-hidden="true">→</span>`}<span key=${stage.key} class=${`pni-stage-chip pni-stage-${stage.key.split(':')[0]}`} title=${stage.detail}>${stage.label}</span>`)}</div>
    ${toggle('plan.enabled', 'plan before work', 'plan')}
    ${node.plan && html`<${NodeSelect} fieldKey="plan.approval" label="plan approval" options=${PLAN_APPROVAL_MODES}
        value=${node.plan.approval || 'auto'} readOnly=${readOnly}
        apply=${(key, value) => commit(key, (draft) => setPlanApproval(draft, value))} />
      <${PerformerEditor} path="plan.performer" locate=${(draft) => draft.plan.performer} performer=${node.plan.performer}
        commit=${commit} readOnly=${readOnly} contact=${true} />`}
    <div class="process-node-checks">
      <span class="process-node-field-label">checks</span>
      ${(node.checks || []).map((check, index) => html`<div key=${index} class="process-node-check">
        <div class="process-node-check-head">
          ${field(`checks.${index}.id`, 'check id', check.id, (draft, value) => setCheckID(draft, index, value))}<span class="spacer"></span>
          ${!readOnly && html`<button class="process-action" type="button" aria-label=${`Move check ${check.id} up`} onClick=${() => commit(`checks.${index}.up`, (draft) => moveCheck(draft, index, -1))}>↑</button><button class="process-action" type="button" aria-label=${`Move check ${check.id} down`} onClick=${() => commit(`checks.${index}.down`, (draft) => moveCheck(draft, index, 1))}>↓</button><button class="process-action process-action-danger" type="button" aria-label=${`Remove check ${check.id}`} onClick=${() => commit(`checks.${index}.remove`, (draft) => removeCheck(draft, index))}>remove</button>`}
        </div>
        <${PerformerEditor} path=${`checks.${index}.performer`} locate=${(draft) => draft.checks[index].performer} performer=${check.performer}
          commit=${commit} readOnly=${readOnly} contact=${true} />
      </div>`)}
      ${!readOnly && html`<button class="process-action process-node-add" type="button" onClick=${() => commit('checks.add', (draft) => addCheck(draft))}>+ add check</button>`}
    </div>
    ${toggle('review.enabled', 'review gate after checks', 'review')}
    ${node.review && html`<${PerformerEditor} path="review.performer" locate=${(draft) => draft.review.performer} performer=${node.review.performer}
      commit=${commit} readOnly=${readOnly} contact=${true} />`}
  </div>`;
}

function InputsBlock({ inputs }) {
  return html`<div class="pni-inputs">
    ${inputs.captures.length
    ? html`<ul class="pni-input-list">${inputs.captures.map((capture) => html`<li key=${`${capture.from}:${capture.name}`}><code>${capture.name}</code> ← ${capture.fromName}</li>`)}</ul>`
    : html`<p class="process-node-empty">No upstream captures reach this node.</p>`}
    ${inputs.params.length > 0 && html`<p class="pni-params">params: ${inputs.params.map((name, i) => html`${i > 0 && ', '}<code key=${name}>${name}</code>`)}</p>`}
  </div>`;
}

function RetryBlock({ node, commit, readOnly }) {
  return html`<div class="pni-row">
    <${NodeField} fieldKey="retry.maxAttempts" label="max attempts" placeholder="unset" readOnly=${readOnly}
      value=${node.retry?.maxAttempts === undefined ? '' : String(node.retry.maxAttempts)}
      apply=${(key, raw) => commit(key, (draft) => setRetryField(draft, 'maxAttempts', raw))} />
    <${NodeSelect} fieldKey="retry.onFail" label="on fail" options=${RETRY_ON_FAIL_MODES} value=${node.retry?.onFail}
      blankLabel="default (fresh-attempt)" readOnly=${readOnly}
      apply=${(key, raw) => commit(key, (draft) => setRetryField(draft, 'onFail', raw))} />
  </div>`;
}

function OutputsBlock({ node, commit, readOnly }) {
  return html`<${NodeField} fieldKey="captures" label="published outputs" multiline=${true} placeholder="one-name-per-line"
    value=${formatLines(node.captures)} readOnly=${readOnly}
    apply=${(key, raw) => commit(key, (draft) => setCaptures(draft, raw))} />`;
}

function WaitBlock({ node, commit, readOnly }) {
  return html`<div class="pni-row">${[['duration', '30m'], ['until', ''], ['signal', '']].map(([key, placeholder]) => html`<${NodeField}
    key=${key} fieldKey=${`wait.${key}`} label=${key} placeholder=${placeholder} value=${node.wait?.[key] || ''} readOnly=${readOnly}
    apply=${(fieldKey, raw) => commit(fieldKey, (draft) => setWaitField(draft, key, raw))} />`)}</div>`;
}

function OutcomesBlock({ edges, nameOf }) {
  return html`<div class="process-node-edges">
    ${edges.map((edge) => html`<div key=${edge.outcome} class="process-node-edge-row"><span class="process-node-edge-outcome">${edge.outcome}</span><span class="process-node-edge-arrow">→</span><span class="process-node-edge-target">${nameOf(edge.to)}</span></div>`)}
    ${!edges.length && html`<p class="process-node-empty">No outgoing edges yet.</p>`}
    <p class="process-node-edges-note">Edges are drawn and relabelled on the canvas.</p>
  </div>`;
}

function SubprocessBlock({ node, nodeId, controller, commit, readOnly, edges }) {
  const sub = node.subprocess || {};
  const isRef = !!sub.templateRef;
  const inlineNodes = Object.entries(sub.nodes || {});
  const params = isRef ? Object.keys(sub.inputs || {}) : Object.keys(sub.params || {});
  const ends = isRef ? Object.keys(sub.outputs || {}) : inlineNodes.filter(([, n]) => n?.type === 'end').map(([id]) => id);
  const outcomes = edges.map((edge) => edge.outcome);
  return html`<div class="pni-subprocess">
    ${isRef ? html`<div class="pni-row">
        <${NodeField} fieldKey="subprocess.templateRef" label="template" value=${sub.templateRef} readOnly=${readOnly}
          apply=${(key, raw) => commit(key, (draft) => setSubprocessRef(draft, 'templateRef', raw))} />
        <${NodeField} fieldKey="subprocess.version" label="version" placeholder="head at run start" value=${sub.version || ''} readOnly=${readOnly}
          apply=${(key, raw) => commit(key, (draft) => setSubprocessRef(draft, 'version', raw))} />
      </div>
      ${controller.options?.onOpenTemplate && html`<button type="button" class="process-action" data-subprocess-open-template
        onClick=${() => controller.options.onOpenTemplate(sub.templateRef)}>open template ↗</button>`}`
    : html`<div class="pni-subprocess-inline"><span>Inline body · ${inlineNodes.length} node${inlineNodes.length === 1 ? '' : 's'}</span>
        <button type="button" class="process-action primary" data-subprocess-enter disabled=${!controller.model.canEnterScope(nodeId)}
          title="Open the nested graph (double-click the node does the same)" onClick=${() => controller.enterSubprocess(nodeId)}>enter ▸</button></div>`}
    <div class="pni-ports">
      <span class="process-node-field-label">in-ports · bind body params</span>
      ${params.length ? params.map((param) => html`<${NodeField} key=${param} fieldKey=${`subprocess.inputs.${param}`} label=${`${param} ←`}
          placeholder="literal or {{ params.x }}" value=${sub.inputs?.[param] || ''} readOnly=${readOnly}
          apply=${(key, raw) => commit(key, (draft) => setSubprocessPort(draft, 'inputs', param, raw))} />`)
        : html`<p class="process-node-empty">${isRef ? 'Bindings for the referenced template\'s params appear once set.' : 'The body declares no params.'}</p>`}
      <span class="process-node-field-label">out-ports · body end → outcome</span>
      ${ends.length ? ends.map((end) => html`<${NodeSelect} key=${end} fieldKey=${`subprocess.outputs.${end}`} label=${`${end} →`}
          options=${outcomes} value=${sub.outputs?.[end] || ''} blankLabel=${outcomes.length === 1 ? `default (${outcomes[0]})` : 'unmapped'} readOnly=${readOnly}
          apply=${(key, raw) => commit(key, (draft) => setSubprocessPort(draft, 'outputs', end, raw))} />`)
        : html`<p class="process-node-empty">No end nodes to map.</p>`}
    </div>
  </div>`;
}

function AboutBlock({ node, commit, readOnly }) {
  const type = node.type || 'task';
  return html`<div class="pni-about">
    ${type !== 'start' && type !== 'end' && html`<${NodeField} fieldKey="meta.description" label="description" value=${node.description || ''}
      readOnly=${readOnly} apply=${(key, raw) => commit(key, (draft) => setNodeText(draft, 'description', raw))} />`}
    <${NodeField} fieldKey="meta.doc" label="doc" multiline=${true} value=${node.doc || ''} readOnly=${readOnly}
      apply=${(key, raw) => commit(key, (draft) => setNodeText(draft, 'doc', raw))} />
  </div>`;
}

const BLOCK_BODIES = {
  subprocess: SubprocessBlock,
  performer: PerformerBlock,
  prompt: PromptBlock,
  stages: StagesBlock,
  inputs: InputsBlock,
  retry: RetryBlock,
  captures: OutputsBlock,
  wait: WaitBlock,
  outcomes: OutcomesBlock,
  bounds: BoundsBlock,
  about: AboutBlock,
};

function Block({ blockKey, index, count, readOnly, removable = true, onMove, onRemove, onDragStart, onDrop, children }) {
  const block = nodeBlock(blockKey);
  const [over, setOver] = useState(false);
  // A div, not a <section>: dashboard.css hides bare sections (tab panes).
  return html`<div class=${`pni-block${over ? ' is-drop-target' : ''}`} data-node-block=${blockKey}
      role="group" aria-label=${block.label}
      onDragOver=${(event) => { if (event.dataTransfer?.types?.includes?.('application/x-tclaude-node-block')) { event.preventDefault(); setOver(true); } }}
      onDragLeave=${() => setOver(false)}
      onDrop=${(event) => { setOver(false); const key = event.dataTransfer?.getData?.('application/x-tclaude-node-block'); if (key) { event.preventDefault(); onDrop(key, blockKey); } }}>
    <div class="pni-block-head" title=${block.hint}>
      <span class="pni-grip" draggable="true" aria-hidden="true" title="Drag to reorder"
        onDragStart=${(event) => { event.dataTransfer?.setData?.('application/x-tclaude-node-block', blockKey); onDragStart?.(blockKey); }}>⠿</span>
      <span class="pni-block-icon" aria-hidden="true">${block.icon}</span>
      <span class="pni-block-title">${block.label}</span>
      <span class="spacer"></span>
      <button type="button" class="pni-block-move" aria-label=${`Move ${block.label} up`} disabled=${index === 0} onClick=${() => onMove(blockKey, -1)}>↑</button>
      <button type="button" class="pni-block-move" aria-label=${`Move ${block.label} down`} disabled=${index === count - 1} onClick=${() => onMove(blockKey, 1)}>↓</button>
      ${!block.required && !readOnly && removable && html`<button type="button" class="pni-block-remove" aria-label=${`Remove ${block.label}`}
        title=${block.clear ? `Remove ${block.label} and clear its settings` : `Hide ${block.label}`} onClick=${() => onRemove(blockKey)}>✕</button>`}
    </div>
    <div class="pni-block-body">${children}</div>
  </div>`;
}

// Closing or deleting from the inspector unmounts the focused button; hand
// focus back to the canvas rather than dropping it on <body>.
function refocusCanvas(controller) {
  queueMicrotask(() => controller.graph?.focusKeyboardTarget?.());
}

export function NodeInspector({ controller, view }) {
  const nodeId = view.selection?.type === 'node' ? view.selection.id : '';
  const node = view.selectedNode;
  const root = useRef(null);
  const [opened, setOpened] = useState({});
  const [, setOrderRevision] = useState(0);
  useLayoutEffect(() => {
    if (!view.nodeFocusRequest) return;
    const label = root.current?.querySelector('[data-node-field="meta.name"]');
    (label && !label.disabled ? label : root.current?.querySelector('.pni-close'))?.focus?.();
  }, [view.nodeFocusRequest]);
  if (!nodeId || !node) return null;
  const model = controller.model;
  const type = node.type || 'task';
  const readOnly = !model.config.nodeEditable(nodeId);
  const pending = view.pending.externalDecision || view.pending.externalReload;
  const commit = (key, mutate) => (controller.editNode(nodeId, mutate) ? { accepted: true } : { blocked: true });
  const inputs = upstreamInputs(model, nodeId);
  const order = readBlockOrder(type);
  const nodeOpened = opened[nodeId] || [];
  const visible = visibleBlocks(node, { opened: nodeOpened, order, context: { inputs } });
  const addable = readOnly ? [] : addableBlocks(node, visible);
  const fromData = visibleBlocks(node, { order, context: { inputs } });
  const saveOrder = (next) => {
    writeBlockOrder(type, mergeOrder(type, order, next));
    setOrderRevision((value) => value + 1);
  };
  const open = (key) => setOpened((prev) => ({ ...prev, [nodeId]: [...(prev[nodeId] || []), key] }));
  const remove = (key) => {
    if (nodeBlock(key)?.clear && !controller.editNode(nodeId, (draft) => removeBlock(draft, key))) return;
    setOpened((prev) => ({ ...prev, [nodeId]: (prev[nodeId] || []).filter((candidate) => candidate !== key) }));
  };
  const nameOf = (id) => model.node(id)?.name || id;
  const diagnostics = (view.issues?.entries || []).filter((entry) => entry.node === nodeId);
  const extraActions = controller.options?.nodeInspectorActions?.({ nodeId, node, readOnly }) || [];
  const bodyProps = { node, nodeId, controller, commit, readOnly, inputs, edges: model.outgoingEdges(nodeId), nameOf };
  return html`<aside ref=${root} class="process-node-inspector process-node-detail process-scroll-surface${readOnly ? ' is-readonly' : ''}"
      aria-label=${`Node ${nodeId}`} data-node-inspector=${nodeId} inert=${pending}>
    <div class="pni-head">
      <div class="pni-head-row">
        <${NodeField} fieldKey="meta.name" label="label" value=${node.name || ''} readOnly=${readOnly}
          apply=${(key, raw) => (controller.renameNode(nodeId, raw) === undefined ? { blocked: true } : { accepted: true })} />
        <button type="button" class="pni-close process-node-close" title="Close the inspector" aria-label="Close node inspector"
          onClick=${() => { controller.setSelection(null); refocusCanvas(controller); }}>✕</button>
      </div>
      <div class="pni-meta"><span class="process-inspector-kind">${type} node</span> · <span class="process-inspector-id">${nodeId}</span>
        ${readOnly && html` <span class="process-node-readonly-badge">read-only</span>`}</div>
      <div class="pni-actions">
        ${view.selectedNodeIncoming > 1 && html`<select class="process-inspector-select" aria-label="Join semantics" disabled=${readOnly}
          value=${node.join || ''} onChange=${(event) => controller.setJoin(nodeId, event.currentTarget.value || null)}>
          <option value="">join: unset</option><option value="all">join: all</option><option value="any">join: any</option></select>`}
        ${view.model.start !== nodeId && type !== 'end' && !readOnly && html`<button class="process-action" type="button" title="Make this node the process entry point" onClick=${() => controller.setStart(nodeId)}>set as start</button>`}
        <button class="process-action" type="button" disabled=${!view.snippets.canSaveSelection} title="Save this node as a reusable custom snippet" onClick=${() => controller.saveSelectionAsSnippet()}>save as snippet…</button>
        ${extraActions.map((action) => html`<button key=${action.key || action.label} class="process-action" type="button" disabled=${!!action.disabled} title=${action.title || ''} onClick=${action.run}>${action.label}</button>`)}
        ${!readOnly && html`<button class="process-action process-action-danger" type="button" onClick=${() => { controller.deleteSelection(); refocusCanvas(controller); }}>delete node</button>`}
      </div>
      ${diagnostics.length > 0 && html`<ul class="pni-diagnostics" aria-label="Issues on this node">${diagnostics.map((entry, i) => html`<li key=${i} class=${`pni-diag is-${entry.severity}`}>${entry.severity === 'error' ? '✕' : '⚠'} ${entry.message}</li>`)}</ul>`}
    </div>
    ${visible.map((key, index) => {
      const Body = BLOCK_BODIES[key];
      // A block with nothing to clear that is present from data (Inputs with
      // upstream captures) cannot be hidden: its ✕ would do nothing.
      const removable = !!nodeBlock(key)?.clear || !fromData.includes(key);
      return html`<${Block} key=${key} blockKey=${key} index=${index} count=${visible.length} readOnly=${readOnly} removable=${removable}
        onMove=${(blockKey, delta) => saveOrder(moveKey(visible, blockKey, delta))}
        onDrop=${(dragged, before) => saveOrder(moveKeyBefore(visible, dragged, before))}
        onRemove=${remove}><${Body} ...${bodyProps} /></${Block}>`;
    })}
    ${addable.length > 0 && html`<div class="pni-add" role="group" aria-label="Add block">
      <span class="pni-add-label">+ add block:</span>
      ${addable.map((block) => html`<button key=${block.key} type="button" class="pni-add-block" data-add-block=${block.key}
        title=${block.hint} onClick=${() => open(block.key)}>${block.icon} ${block.label}</button>`)}
    </div>`}
  </aside>`;
}
