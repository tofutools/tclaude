import { Fragment, h, render } from 'preact';
import { signal } from '@preact/signals';
import { useCallback, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { PALETTE_PRIMITIVES, PALETTE_SNIPPETS, templateIDEditable } from './process-edit-model.js';
import { selectionItems } from './process-selection.js';
import { severityGlyph } from './process-validation.js';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { PROCESS_SCRIBE_PROMPT_MAX } from './process-scribe.js';
import { NodeDialog } from './process-node-dialog.js';
import { ParamsDialog } from './process-params-dialog.js';

const html = htm.bind(h);

function shortHash(hash) { return hash ? hash.slice(0, 8) : ''; }

// Uncontrolled while focused/composing, synchronized while idle. Polls,
// validation, and graph commits can publish editor snapshots without replacing
// the active control or clobbering an IME composition buffer.
function StableField({
  value = '', multiline = false, blocked = false, onCommit, onInput, class: className,
  ...props
}) {
  const ref = useRef(null);
  const composing = useRef(false);
  const discardBufferedChange = useRef(false);
  const latestValue = useRef(value);
  latestValue.current = value;
  useLayoutEffect(() => {
    const input = ref.current;
    if (blocked) discardBufferedChange.current = true;
    if (!input || composing.current || document.activeElement === input) return;
    if (input.value !== String(value ?? '')) input.value = String(value ?? '');
    if (!blocked) discardBufferedChange.current = false;
  }, [blocked, value]);
  const Tag = multiline ? 'textarea' : 'input';
  return html`<${Tag}
    ...${props}
    ref=${ref}
    class=${className}
    defaultValue=${String(value ?? '')}
    onCompositionStart=${() => { composing.current = true; }}
    onCompositionEnd=${(event) => {
      composing.current = false;
      onInput?.(event);
    }}
    onInput=${onInput}
    onChange=${(event) => {
      if (composing.current || event.isComposing) return;
      if (discardBufferedChange.current) {
        event.currentTarget.value = String(latestValue.current ?? '');
        discardBufferedChange.current = false;
        return;
      }
      onCommit?.(event.currentTarget.value);
    }}
  />`;
}

function Header({ controller, view }) {
  const { model, pending } = view;
  const externalPending = pending.externalDecision || pending.externalReload;
  const showIDInput = templateIDEditable(view.blank, model.sourceHash);
  return html`<div class="process-editor-header">
    <span class="process-editor-identity">
      ${showIDInput ? html`<${StableField}
        class="process-editor-id-input" type="text" spellcheck="false"
        placeholder="template-id" aria-label="Template id"
        value=${model.id} disabled=${pending.save || externalPending}
        onCommit=${(value) => controller.setTemplateID(value)}
      />` : html`<strong class="process-editor-title">${model.name ? `${model.name} (${model.id})` : model.id || 'untitled'}</strong>`}
    </span>
    <span class="process-hash process-editor-version" title=${model.semanticHash || 'This template has never been saved'}>
      ${model.semanticHash ? `v ${shortHash(model.semanticHash)}` : 'unsaved'}
    </span>
    <span class="process-editor-dirty" hidden=${!model.dirty}>● modified</span>
    <span class=${`process-editor-status${view.status.error ? ' is-error' : ''}`} role="status">${view.status.message}</span>
    <span class="spacer"></span>
    <button class="process-action" type="button" disabled=${externalPending} title="Edit template name and description" onClick=${() => controller.setSelection({ type: 'template' })}>template settings…</button>
    <button class="process-action" type="button" disabled=${externalPending || pending.save} title="Declare template parameters" onClick=${() => controller.openParamsSettings()}>params…</button>
    <button class="process-action" type="button" disabled=${externalPending || !model.canUndo} title="Undo (Ctrl+Z)" onClick=${() => controller.applyHistory('undo')}>↶ undo</button>
    <button class="process-action" type="button" disabled=${externalPending || !model.canRedo} title="Redo (Ctrl+Shift+Z)" onClick=${() => controller.applyHistory('redo')}>↷ redo</button>
    <button class="process-action" type="button" disabled=${externalPending} title="Toggle the node palette" onClick=${() => controller.togglePalette()}>⬒ palette</button>
    <button class="process-action" type="button" title="Process commands (Ctrl/Cmd-K)" aria-label="Open process commands (Ctrl/Cmd-K)" onClick=${() => controller.openCommands()}>⌘K commands</button>
    <button class="process-action process-scribe-action" type="button" disabled=${externalPending || pending.save} title="Open an agent scoped to this exact process template" onClick=${() => controller.requestScribe('template')}>
      <span class="process-scribe-plain">Edit with agent</span><span class="process-scribe-wizard">Consult a process scribe</span>
    </button>
    <button class="process-action" type="button" disabled=${externalPending || pending.save} title="Instantiate this exact saved version" onClick=${() => controller.requestInstantiate()}>instantiate…</button>
    <button class="process-action primary" type="button" disabled=${pending.save || externalPending || (!model.dirty && !view.blank)} title="Save a new version" onClick=${() => controller.save()}>${pending.save ? 'Saving…' : 'Save'}</button>
  </div>`;
}

function externalSummary(change) {
  const summary = change.review?.summary;
  if (!summary) return null;
  const parts = [];
  const nodePart = (prefix, label, ids = [], count = ids.length, truncated = false) => {
    if (!count) return;
    const omitted = Math.max(0, count - ids.length);
    const listed = [...ids, ...(truncated ? [`… ${omitted} more IDs omitted`] : [])].join(', ');
    parts.push(`${prefix}${count} ${label}${count === 1 ? '' : 's'} (${listed})`);
  };
  nodePart('+', 'node', summary.addedNodes, summary.addedNodeCount, summary.addedNodesTruncated);
  nodePart('−', 'node', summary.removedNodes, summary.removedNodeCount, summary.removedNodesTruncated);
  nodePart('', 'changed node', summary.changedNodes, summary.changedNodeCount, summary.changedNodesTruncated);
  if (summary.addedEdges) parts.push(`+${summary.addedEdges} edge${summary.addedEdges === 1 ? '' : 's'}`);
  if (summary.removedEdges) parts.push(`−${summary.removedEdges} edge${summary.removedEdges === 1 ? '' : 's'}`);
  if (summary.metadataChanged) parts.push('template settings changed');
  const source = summary.source;
  const limits = source?.truncation ? Object.entries(source.truncation)
    .filter(([, clipped]) => clipped).map(([limit]) => limit === 'bytes' ? 'UTF-8 bytes' : limit) : [];
  return {
    graph: parts.length ? `Graph: ${parts.join(' · ')}` : 'Graph: no semantic changes (layout/source only)',
    source: source ? [
      `Source near line ${source.firstLine}: −${source.removedLines} / +${source.addedLines}`,
      ...source.before.map((line) => `− ${line}`), ...source.after.map((line) => `+ ${line}`),
      ...(source.truncated ? [`… source preview truncated at ${limits.join(', ') || 'configured'} limit${limits.length === 1 ? '' : 's'}`] : []),
      ...(change.kind === 'dirty' ? ['Keep editing preserves this draft; Save still uses CAS and will stop on a 409 conflict.'] : []),
    ].join('\n') : (change.kind === 'dirty'
      ? 'Canonical source preview is unavailable. Keep editing preserves this draft; Save still uses CAS and will stop on a 409 conflict.'
      : 'Canonical source preview is unavailable; the graph summary remains authoritative.'),
  };
}

function ExternalChange({ controller, view }) {
  const change = view.external;
  const visible = change.kind === 'clean' || change.kind === 'dirty';
  if (!visible) return null;
  const actor = change.actorDescription;
  const version = shortHash(String(change.ref || '').split('@sha256:').at(-1));
  const pending = view.pending.externalDecision || view.pending.externalReload;
  const review = externalSummary(change);
  const message = actor
    ? `${actor.label} saved version ${version}${change.kind === 'dirty' ? '; your local edits are untouched' : ''}`
    : `Version ${version} is available${change.kind === 'dirty' ? '; your local edits are untouched' : ''}`;
  return html`<div class=${`process-editor-external${change.kind === 'dirty' ? ' is-dirty' : ''}`} role="status">
    <div class="process-editor-external-bar">
      <span class="process-editor-external-message" title=${change.authoredAt || change.ref || ''}>${message}</span>
      <span class="spacer"></span>
      ${actor?.live && html`<button class="process-action process-external-actor" type="button" onClick=${() => controller.openExternalActor()}>Open ${actor.label}</button>`}
      <button class="process-action" type="button" disabled=${pending || change.reviewPending} onClick=${() => controller.toggleExternalReview()}>
        ${change.reviewPending ? 'Loading review…' : change.reviewOpen ? 'Hide review' : 'Review changes'}
      </button>
      <button class="process-action primary" type="button" disabled=${pending || view.pending.save} onClick=${() => controller.reloadExternalChange()}>
        ${change.kind === 'dirty' ? 'Reload & discard' : 'Apply update'}
      </button>
      ${change.kind === 'dirty' && html`<button class="process-action" type="button" disabled=${pending || view.pending.save} onClick=${() => controller.keepExternalChange()}>Keep editing</button>`}
    </div>
    ${change.reviewOpen && html`<div class="process-external-review">
      <div class="process-external-graph-summary">${review?.graph || (change.reviewPending ? 'Loading the exact polled generation…' : 'Change summary unavailable; retry Review changes.')}</div>
      <pre class="process-external-source-summary">${review?.source || (change.kind === 'dirty' ? 'Your local edits remain untouched. Keep editing preserves this draft; Save still uses CAS and will stop on a 409 conflict.' : '')}</pre>
    </div>`}
  </div>`;
}

function Palette({ controller, hidden }) {
  const card = (payload, label, hint) => html`<div
    key=${JSON.stringify(payload)} class="process-palette-card" draggable="true"
    title=${hint || ''} data-palette-item=${JSON.stringify(payload)}
  ><span class="process-palette-card-label">${label}</span><span class="process-palette-card-hint">${hint || ''}</span></div>`;
  return html`<aside class="process-editor-palette" aria-label="Node palette" hidden=${hidden}
    onDragStart=${(event) => controller.paletteDragStart(event)}
    onDragEnd=${(event) => controller.paletteDragEnd(event)}>
    <div class="process-palette-section">Primitives</div>
    ${PALETTE_PRIMITIVES.map((item) => card({ kind: 'primitive', type: item.type }, item.label, item.hint))}
    <div class="process-palette-section">Snippets</div>
    ${PALETTE_SNIPPETS.map((item) => card({ kind: 'snippet', key: item.key }, item.label, item.hint))}
    <p class="process-palette-help">Drag onto the canvas to add. Drag a port to another node to connect.</p>
  </aside>`;
}

function Inspector({ controller, view }) {
  const root = useRef(null);
  const pending = view.pending.externalDecision || view.pending.externalReload;
  useLayoutEffect(() => {
    if (!view.inspectorFocusRequest) return;
    root.current?.querySelector('input:not(:disabled), textarea:not(:disabled)')?.focus();
  }, [view.inspectorFocusRequest]);
  const selected = selectionItems(view.selection);
  const sel = view.selection;
  if (sel?.type === 'template') return html`<div ref=${root} class="process-editor-inspector" inert=${pending}>
    <span class="process-inspector-kind">template</span>
    <input class="process-inspector-input process-template-id-locked" type="text" value=${view.model.id} disabled title="Template ids are immutable after creation" aria-label="Template id (immutable)" />
    <${StableField} blocked=${pending} class="process-inspector-input" type="text" spellcheck="false" placeholder="display name" aria-label="Template display name" value=${view.model.name} onCommit=${(name) => controller.setTemplateMeta({ name })} />
    <${StableField} blocked=${pending} class="process-inspector-input process-template-description" type="text" spellcheck="true" placeholder="description" aria-label="Template description" value=${view.model.description} onCommit=${(description) => controller.setTemplateMeta({ description })} />
    <${StableField} blocked=${pending} multiline class="process-inspector-input process-template-doc" rows="2" spellcheck="true" placeholder="documentation" aria-label="Template documentation" value=${view.model.doc} onCommit=${(doc) => controller.setTemplateMeta({ doc })} />
  </div>`;
  if (selected.length > 1) {
    const nodes = selected.filter((item) => item.type === 'node').length;
    const edges = selected.length - nodes;
    return html`<div ref=${root} class="process-editor-inspector" inert=${pending}><span class="process-inspector-kind">multiple selection</span><span class="process-inspector-id">${selected.length} items</span><span class="process-inspector-hint">${[nodes ? `${nodes} node${nodes === 1 ? '' : 's'}` : '', edges ? `${edges} edge${edges === 1 ? '' : 's'}` : ''].filter(Boolean).join(' · ')}</span><button class="process-action process-action-danger" type="button" onClick=${() => controller.deleteSelection()}>delete selection</button></div>`;
  }
  if (sel?.type === 'node' && view.selectedNode) {
    const node = view.selectedNode;
    return html`<div ref=${root} class="process-editor-inspector" inert=${pending}>
      <span class="process-inspector-kind">${node.type || 'task'} node</span><span class="process-inspector-id">${sel.id}</span>
      <${StableField} blocked=${pending} class="process-inspector-input" type="text" spellcheck="false" placeholder="label" aria-label="Node label" value=${node.name || ''} onCommit=${(name) => controller.renameNode(sel.id, name)} />
      ${view.selectedNodeIncoming > 1 && html`<select class="process-inspector-select" aria-label="Join semantics" value=${node.metadata?.join || ''} onChange=${(event) => controller.setJoin(sel.id, event.currentTarget.value || null)}><option value="">join: unset</option><option value="all">join: all</option><option value="any">join: any</option></select>`}
      ${view.model.start !== sel.id && node.type !== 'end' && html`<button class="process-action" type="button" title="Make this node the process entry point" onClick=${() => controller.setStart(sel.id)}>set as start</button>`}
      <button class="process-action" type="button" title="Open the structured node editor: stages, performers, retry, captures" onClick=${() => controller.openNodeSettings(sel.id)}>node settings…</button>
      <button class="process-action process-action-danger" type="button" onClick=${() => controller.deleteSelection()}>delete node</button>
    </div>`;
  }
  if (sel?.type === 'edge' && view.selectedEdge) {
    const edge = view.selectedEdge;
    return html`<div ref=${root} class="process-editor-inspector" inert=${pending}><span class="process-inspector-kind">edge</span><span class="process-inspector-id">${edge.from} → ${edge.to}</span><${StableField} blocked=${pending} class="process-inspector-input" type="text" spellcheck="false" placeholder="outcome" aria-label="Edge outcome label" value=${edge.outcome} onCommit=${(outcome) => controller.renameEdgeOutcome(edge.from, edge.outcome, outcome.trim())} /><button class="process-action process-action-danger" type="button" onClick=${() => controller.deleteSelection()}>delete edge</button></div>`;
  }
  return html`<div ref=${root} class="process-editor-inspector" inert=${pending}><span class="process-inspector-hint">Select a node or edge to edit it. Double-click a node to open its stage editor.</span></div>`;
}

function Issues({ controller, issues }) {
  const list = useRef(null);
  useLayoutEffect(() => {
    if (!issues.focusRequest || issues.issueCursor < 0) return;
    list.current?.querySelector(`[data-issue-index="${issues.issueCursor}"]`)?.focus();
  }, [issues.focusRequest]);
  return html`<details class="process-issues-panel" hidden=${issues.hidden} open=${issues.open}
    onToggle=${(event) => controller.setIssuesOpen(event.currentTarget.open)}>
    <summary class="process-issues-summary">${issues.summary}</summary>
    <ul ref=${list} class="process-issues-list">
      ${issues.entries.map((entry, index) => html`<li key=${`${entry.code}:${entry.scope}:${entry.targetId}:${entry.message}`}><button type="button" class=${`process-issue process-issue-${entry.severity}`} data-issue-index=${index} onClick=${() => controller.focusIssueAt(index, false)}><span class="process-issue-glyph">${severityGlyph(entry.severity)}</span><span class="process-issue-target">${entry.scope === 'edge' && entry.edge ? `${entry.edge.from} → (${entry.edge.outcome})` : entry.node || 'template'}</span><span class="process-issue-message" title=${`${entry.code}: ${entry.message}`}>${entry.message}</span></button></li>`)}
    </ul>
  </details>`;
}

function InlineEditor({ controller, inline }) {
  const ref = useRef(null);
  useLayoutEffect(() => {
    if (!inline.open) return;
    ref.current?.focus();
    ref.current?.select();
  }, [inline.token]);
  if (!inline.open) return null;
  return html`<input ref=${ref} class="process-editor-inline-input" type="text" spellcheck="false"
    style=${`left:${Math.round(inline.left)}px;top:${Math.round(inline.top)}px`}
    defaultValue=${inline.value}
    onKeyDown=${(event) => {
      if (event.isComposing || event.keyCode === 229) return;
      if (event.key === 'Enter') { event.preventDefault(); controller.closeInline(true, event.currentTarget.value); }
      if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); controller.closeInline(false); }
    }}
    onBlur=${(event) => controller.closeInline(true, event.currentTarget.value)} />`;
}

export function ChoiceDialog({ descriptor, complete }) {
  const initial = useRef(null);
  const preferred = descriptor.choices.find((choice) => choice.initialFocus || choice.primary);
  return html`<${Overlay}
    id="process-editor-choice-modal"
    dialogClass="modal"
    overlayClass="process-editor-modal"
    onClose=${() => complete(null)}
    dirty=${false}
    confirmDiscard=${async () => false}
    initialFocusRef=${initial}
  >
    <h3>${descriptor.title}</h3>
    <p>${descriptor.body}</p>
    <div class="modal-buttons">
      <button ref=${preferred ? null : initial} type="button" class="process-editor-modal-btn" onClick=${() => complete(null)}>Cancel</button>
      ${descriptor.choices.map((choice) => html`<button
        key=${choice.key}
        ref=${choice === preferred ? initial : null}
        class=${`${choice.primary ? 'primary ' : ''}${choice.danger ? 'confirm-danger ' : ''}process-editor-modal-btn`}
        type="button"
        onClick=${() => complete(choice.key)}
      >${choice.label}</button>`)}
    </div>
  </${Overlay}>`;
}

export function ScribeDialog({ descriptor, complete }) {
  const [prompt, setPrompt] = useState(descriptor.prompt);
  const input = useRef(null);
  return html`<${Overlay}
    id="process-scribe-preview-modal"
    dialogClass="modal process-scribe-preview"
    overlayClass="process-editor-modal process-scribe-preview-overlay"
    labelledby="process-scribe-preview-title"
    onClose=${() => complete(null)}
    dirty=${false}
    confirmDiscard=${async () => false}
    initialFocusRef=${input}
  >
    <h3 id="process-scribe-preview-title">Share editor context with process scribe</h3>
    <p>${descriptor.scribeKind === 'selection' ? 'The selected graph identities below will be shared.'
      : descriptor.scribeKind === 'diagnostic' ? 'The current diagnostic identity below will be shared.'
        : 'A bounded whole-template summary will be shared.'}</p>
    <label class="process-scribe-prompt-label">Your request (editable)</label>
    <textarea ref=${input} class="process-scribe-prompt" rows="4" maxlength=${PROCESS_SCRIBE_PROMPT_MAX}
      aria-label="Request for the process scribe" spellcheck="true" data-select-on-focus
      value=${prompt} onInput=${(event) => setPrompt(event.currentTarget.value)} />
    <span class="process-scribe-prompt-count">${prompt.length} / ${PROCESS_SCRIBE_PROMPT_MAX}</span>
    <div class="process-scribe-context-label">BEGIN BOUNDED EDITOR CONTEXT · read-only · not template source</div>
    <pre class="process-scribe-context-preview" aria-label="Read-only bounded editor context" tabindex="0">${descriptor.context}</pre>
    <div class=${`process-scribe-context-end${descriptor.truncated ? ' is-truncated' : ''}`}>
      ${descriptor.truncated
        ? 'END BOUNDED EDITOR CONTEXT · visibly truncated; the scribe must reread canonical YAML'
        : 'END BOUNDED EDITOR CONTEXT · the scribe must reread canonical YAML'}
    </div>
    <div class="modal-buttons">
      <button type="button" class="process-editor-modal-btn" onClick=${() => complete(null)}>Cancel</button>
      <button class="primary process-editor-modal-btn" type="button" onClick=${() => complete(prompt.trim())}>Send & open scribe</button>
    </div>
  </${Overlay}>`;
}

function EditorModal({ controller, descriptor }) {
  const registerHandle = useCallback(
    (handle) => controller.registerModalHandle(descriptor?.generation, handle),
    [controller, descriptor?.generation],
  );
  if (!descriptor) return null;
  const complete = (result) => controller.finishModal(descriptor.generation, result);
  if (descriptor.kind === 'choice') return html`<${ChoiceDialog} descriptor=${descriptor} complete=${complete} />`;
  if (descriptor.kind === 'scribe') return html`<${ScribeDialog} descriptor=${descriptor} complete=${complete} />`;
  if (descriptor.kind === 'node') return html`<${NodeDialog}
    model=${controller.model} nodeId=${descriptor.nodeId} mode=${descriptor.mode}
    onMutated=${() => controller.refresh()} complete=${complete}
    confirmDiscard=${controller.options.confirmDiscard || (async () => false)}
    registerHandle=${registerHandle} />`;
  if (descriptor.kind === 'params') return html`<${ParamsDialog}
    model=${controller.model} onMutated=${() => controller.refresh()} complete=${complete}
    confirmDiscard=${controller.options.confirmDiscard || (async () => false)}
    registerHandle=${registerHandle} />`;
  return null;
}

export function ProcessEditorApp({ controller }) {
  const view = controller.snapshotSignal.value;
  const graphRef = useCallback((host) => controller.attachGraphHost(host), [controller]);
  const stageRef = useCallback((stage) => { controller.stage = stage; }, [controller]);
  return html`<${Fragment}><div inert=${!!view.modal} class=${`process-editor${view.pending.externalDecision || view.pending.externalReload ? ' is-reloading' : ''}`} onKeyDown=${(event) => controller.onEditorKeyDown(event)}>
    <${Header} controller=${controller} view=${view} />
    <${ExternalChange} controller=${controller} view=${view} />
    <div class="process-editor-body" inert=${view.pending.externalDecision || view.pending.externalReload}>
      <${Palette} controller=${controller} hidden=${view.paletteHidden} />
      <div ref=${stageRef} class="process-editor-stage">
        <div ref=${graphRef} class="process-editor-canvas-host"></div>
        <${InlineEditor} controller=${controller} inline=${view.inline} />
        <${Issues} controller=${controller} issues=${view.issues} />
      </div>
    </div>
    <${Inspector} controller=${controller} view=${view} />
  </div><${EditorModal} controller=${controller} descriptor=${view.modal} /></${Fragment}>`;
}

export function mountProcessEditorIsland(mount, controller) {
  render(html`<${ProcessEditorApp} controller=${controller} />`, mount);
  return () => render(null, mount);
}

export function createProcessEditorPublisher(initial) { return signal(initial); }

function openStandalone(Component, descriptor) {
  const host = document.body.appendChild(document.createElement('div'));
  return new Promise((resolve) => {
    const complete = (result) => {
      render(null, host);
      host.remove();
      resolve(result);
    };
    render(html`<${Component} descriptor=${descriptor} complete=${complete} />`, host);
  });
}

export function openStandaloneChoiceDialog(descriptor) {
  return openStandalone(ChoiceDialog, descriptor);
}

export function openStandaloneScribeDialog(descriptor) {
  return openStandalone(ScribeDialog, { ...descriptor, scribeKind: descriptor.kind });
}

export { StableField };
// dashboard-imperative-boundary: preact-compat
