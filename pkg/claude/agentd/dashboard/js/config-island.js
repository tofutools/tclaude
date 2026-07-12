import { h, render } from 'preact';
import { useLayoutEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { useDialogFocus } from './dialog-focus.js';
import { ConfigFormMarkup } from './config-form-markup.js';
import { bindConfigActivation, configLineDiff, configureConfigAdapter, configureConfigLifecycle, handleConfigEvent } from './config-form-adapter.js';

const html = htm.bind(h);

function ConfigDiffModal({ model, close }) {
  const confirmRef = useRef(null);
  const { dialogRef } = useDialogFocus({
    open: !!model, initialFocusRef: confirmRef, onEscape: () => close(false),
  });
  if (!model) return null;
  const diff = configLineDiff(model.beforeRaw, model.afterRaw);
  const adds = diff.filter(line => line.t === 'add').length;
  const dels = diff.filter(line => line.t === 'del').length;
  const sign = { add: '+', del: '-', ctx: ' ' };
  const cancelOutside = (event) => { if (event.target === event.currentTarget) close(false); };
  return html`<div ref=${dialogRef} id="config-diff-modal" class="modal-overlay show" role="dialog" aria-modal="true"
    aria-labelledby="config-diff-title" onClick=${cancelOutside}>
    <div class="config-diff-modal">
      <h3 id="config-diff-title">Confirm config changes</h3>
      ${model.malformed && html`<div id="config-diff-warn" class="config-diff-warn" style="display:block">⚠ config.json on disk is corrupt and could not be parsed. The form shows DEFAULT values, not your previous settings. Saving replaces the corrupt file entirely — anything it contained is lost. The diff below is against defaults.</div>`}
      <p id="config-diff-sub" class="cfg-diff-sub">${adds} line(s) added, ${dels} removed — writing to ${model.path}</p>
      <div id="config-diff-body" class="config-diff">${diff.map((line, index) => html`<span key=${index} class=${`dl ${line.t}`}>${sign[line.t]} ${line.s}</span>`)}</div>
      <div class="modal-buttons">
        <button id="config-diff-cancel" type="button" onClick=${() => close(false)}>Cancel</button>
        <span class="spacer"></span>
        <button ref=${confirmRef} id="config-diff-confirm" class="primary" type="button"
          onClick=${() => close(true)}>${model.malformed ? 'Replace corrupt config.json' : 'Save to config.json'}</button>
      </div>
    </div>
  </div>`;
}

// The large form deliberately uses uncontrolled browser inputs. Config loads
// populate them only on explicit load/reload/save, so typing and focus cannot
// be overwritten by unrelated snapshot renders. Signals own lifecycle/dirty
// state; the adapter preserves the exact historical JSON round-trip semantics.
export function ConfigApp({ state, dependencies = {} }) {
  const root = useRef(null);
  const lists = state.lists.value;
  useLayoutEffect(() => {
    configureConfigAdapter({
      ...dependencies,
      lists: state.listController,
      confirmDiff: dependencies.confirmDiff || state.confirmDiff,
    });
    configureConfigLifecycle(state.lifecycle);
    const unbind = bindConfigActivation();
    return () => {
      unbind?.();
      state.cancelDiff(false);
    };
  }, []);
  // Do not subscribe this uncontrolled form subtree to Signals during render:
  // phase/dirty changes must never reconcile away imperative list rows or live
  // input values. State remains observable through the feature registry.
  const changeList = (id, values) => {
    state.listController.set(id, values);
    state.markDirty();
  };
  const routeFormEvent = (event) => {
    if (event.type === 'input' || event.type === 'change') state.markDirty();
    handleConfigEvent(event);
  };
  return html`<div ref=${root} class="config-island">
    <${ConfigFormMarkup} lists=${lists} onListChange=${changeList} onFormEvent=${routeFormEvent} />
    <${ConfigDiffModal} model=${state.diff.value} close=${state.cancelDiff} />
  </div>`;
}

export function mountConfigIsland({ host, state, dependencies, registerCleanup }) {
  render(html`<${ConfigApp} state=${state} dependencies=${dependencies} />`, host);
  registerCleanup(() => { configureConfigLifecycle({}); render(null, host); });
}
