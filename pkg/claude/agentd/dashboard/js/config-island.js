import { h, render } from 'preact';
import { useLayoutEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { DiffLines } from './diff-lines.js';
import { Dialog } from './ui-dialog.js';
import { ConfigFormMarkup } from './config-form-markup.js';
import { bindConfigActivation, configLineDiff, configureConfigAdapter, configureConfigLifecycle, followSourceChoice, handleConfigEvent } from './config-form-adapter.js';
import { SOURCE_CHOICE_EVENT } from './knowledge-source-island.js';
import { configNodeName, setSecretEditHook } from './config-access.js';

const html = htm.bind(h);

function ConfigDiffModal({ model, close }) {
  const confirmRef = useRef(null);
  if (!model) return null;
  const diff = configLineDiff(model.beforeRaw, model.afterRaw);
  const adds = diff.filter(line => line.t === 'add').length;
  const dels = diff.filter(line => line.t === 'del').length;
  const sign = { add: '+', del: '-', ctx: ' ' };
  const node = configNodeName();
  return html`<${Dialog} id="config-diff-modal" dialogClass="config-diff-modal" title=${node ? `Confirm config changes on ${node} (remote)` : 'Confirm config changes'}
    initialFocusRef=${confirmRef} onClose=${() => close(false)} cancel="Cancel"
    footer=${html`<${Button} variant="primary" domRef=${confirmRef} id="config-diff-confirm"
      onClick=${() => close(true)}>${model.malformed ? 'Replace corrupt config.json' : node ? `Save to ${node}` : 'Save to config.json'}<//>`}>
    ${model.malformed && html`<div id="config-diff-warn" class="config-diff-warn" style="display:block">⚠ config.json on disk is corrupt and could not be parsed. The form shows DEFAULT values, not your previous settings. Saving replaces the corrupt file entirely — anything it contained is lost. The diff below is against defaults.</div>`}
    <p id="config-diff-sub" class="cfg-diff-sub">${adds} line(s) added, ${dels} removed — writing to ${model.path}</p>
    ${model.secrets?.length ? html`<p id="config-diff-secrets" class="cfg-diff-sub">Withheld secrets this save sets or clears (their values are never shown): ${model.secrets.join(', ')}</p>` : null}
    <div id="config-diff-body" class="config-diff"><${DiffLines} diff=${diff} sign=${sign} unreadable=${!!model.malformed} /></div>
  <//>`;
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
    setSecretEditHook((panel) => state.markDirty(panel));
    const unbind = bindConfigActivation();
    document.addEventListener(SOURCE_CHOICE_EVENT, followSourceChoice);
    return () => {
      document.removeEventListener(SOURCE_CHOICE_EVENT, followSourceChoice);
      unbind?.();
      state.cancelDiff(false);
    };
  }, []);
  // Do not subscribe this uncontrolled form subtree to Signals during render:
  // phase/dirty changes must never reconcile away imperative list rows or live
  // input values. State remains observable through the feature registry.
  const panelOf = (node) => node?.closest?.('[data-cfg-panel]')?.getAttribute('data-cfg-panel');
  const changeList = (id, values) => {
    state.listController.set(id, values);
    state.markDirty(panelOf(root.current?.querySelector(`#${id}`)));
  };
  const routeFormEvent = (event) => {
    // The settings search lives in the rail, outside every panel; typing a
    // query is not an edit.
    const editsConfig = event.target?.id !== 'cfg-filter';
    if (editsConfig && (event.type === 'input' || event.type === 'change')) state.markDirty(panelOf(event.target));
    handleConfigEvent(event);
  };
  return html`<div ref=${root} class="config-island">
    <${ConfigFormMarkup} lists=${lists} onListChange=${changeList} onFormEvent=${routeFormEvent} dirtyPanels=${state.dirtyPanels} loads=${state.metadata} />
    <${ConfigDiffModal} model=${state.diff.value} close=${state.cancelDiff} />
  </div>`;
}

export function mountConfigIsland({ host, state, dependencies, registerCleanup }) {
  render(html`<${ConfigApp} state=${state} dependencies=${dependencies} />`, host);
  registerCleanup(() => { configureConfigLifecycle({}); render(null, host); });
}
