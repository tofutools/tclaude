// Preact-owned structured template-parameter editor. Draft rows preserve raw
// text until Apply; the live model is mutated atomically only after validation.

import { h, render } from 'preact';
import { useCallback, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';

const html = htm.bind(h);

function clone(value) { return structuredClone(value); }

function defaultText(value) {
  if (value === undefined) return '';
  if (typeof value === 'string') return value;
  if (typeof value === 'number' || typeof value === 'boolean') return String(value);
  try { return JSON.stringify(value); } catch { return String(value); }
}

function parseDefault(value, type) {
  if (type === 'boolean') {
    if (value === 'true' || value === 'false') return { value: value === 'true' };
    return { error: 'must be exactly "true" or "false".' };
  }
  if (type === 'number') {
    if (value.trim() === '') return { error: 'must be a finite number.' };
    const number = Number(value);
    if (Number.isFinite(number)) return { value: number };
    return { error: 'must be a finite number.' };
  }
  return { value };
}

function nextParamName(rows) {
  const names = new Set(rows.map((row) => row.name));
  if (!names.has('param')) return 'param';
  for (let n = 2; ; n += 1) if (!names.has(`param${n}`)) return `param${n}`;
}

let nextRowKey = 1;
function modelRows(model) {
  return Object.entries(model.template.params || {}).sort(([a], [b]) => a.localeCompare(b, 'en'))
    .map(([name, param]) => ({
      key: nextRowKey++, name, rawName: name, param: clone(param), type: param.type || 'string',
      rawType: param.type || 'string', description: param.description || '',
      required: param.required === true, requiredChanged: false, hasDefault: param.default !== undefined,
      defaultValue: defaultText(param.default), defaultChanged: false,
    }));
}

function comparable(rows) {
  return JSON.stringify(rows.map(({ key, ...row }) => row));
}

export function ParamsDialog({ model, onMutated, complete, confirmDiscard, registerHandle }) {
  const baseline = useRef(modelRows(model));
  const rowsRef = useRef(clone(baseline.current));
  const [, redraw] = useState(0);
  const [error, setError] = useState('');
  const sharedClose = useRef(null);
  const list = useRef(null);
  const addButton = useRef(null);
  const isDirty = () => comparable(rowsRef.current) !== comparable(baseline.current);
  const update = (index, fields) => {
    Object.assign(rowsRef.current[index], fields);
    setError('');
    redraw((value) => value + 1);
  };
  const requestClose = useCallback(async () => {
    return sharedClose.current?.() ?? false;
  }, []);
  const apply = () => {
    const rows = rowsRef.current;
    const names = rows.map((row) => row.name);
    if (names.some((name) => !name)) { setError('Every parameter needs a name.'); return false; }
    if (new Set(names).size !== names.length) { setError('Parameter names must be unique.'); return false; }
    const params = Object.create(null);
    for (const row of rows) {
      const param = clone(row.param);
      param.type = row.type || 'string';
      if (row.description) param.description = row.description; else delete param.description;
      if (row.requiredChanged) {
        if (row.required) param.required = true; else delete param.required;
      }
      if (row.hasDefault) {
        if (row.defaultChanged || row.param.default === undefined) {
          const parsed = parseDefault(row.defaultValue, row.type);
          if (parsed.error) { setError(`Default for "${row.name}" ${parsed.error}`); return false; }
          param.default = parsed.value;
        } else param.default = row.param.default;
      } else delete param.default;
      params[row.name] = param;
    }
    const changed = model.setParams(params);
    if (changed) onMutated?.();
    complete(true);
    return true;
  };
  useLayoutEffect(() => {
    const registered = registerHandle;
    const cleanup = registered?.({ isDirty, requestClose });
    return () => {
      if (typeof cleanup === 'function') cleanup();
      else registered?.(null);
    };
  }, [registerHandle, requestClose]);
  const registerSharedClose = useCallback((close) => {
    sharedClose.current = close;
    return () => { if (sharedClose.current === close) sharedClose.current = null; };
  }, []);
  const remove = (index) => {
    rowsRef.current.splice(index, 1);
    redraw((value) => value + 1);
    queueMicrotask(() => {
      const next = Math.min(index, rowsRef.current.length - 1);
      if (next >= 0) list.current?.querySelectorAll('.process-param-name')[next]?.focus();
      else addButton.current?.focus();
    });
  };
  const add = () => {
    const name = nextParamName(rowsRef.current);
    rowsRef.current.push({
      key: nextRowKey++, name, rawName: name, param: {}, type: 'string', rawType: 'string',
      description: '', required: false, requiredChanged: false,
      hasDefault: false, defaultValue: '', defaultChanged: false,
    });
    redraw((value) => value + 1);
    queueMicrotask(() => list.current?.lastElementChild?.querySelector('.process-param-name')?.focus());
  };
  return html`<${Overlay}
    id="process-param-modal" dialogClass="modal process-param-dialog" overlayClass="process-param-modal"
    labelledby="process-param-title" onClose=${complete} dirty=${isDirty} blocked=${false}
    confirmDiscard=${confirmDiscard} onCloseError=${(closeError) => setError(`Discard confirmation failed: ${closeError?.message || String(closeError)}`)}
    registerClose=${registerSharedClose}
  >
    <h3 id="process-param-title">Template parameters</h3>
    <p class="muted">Declare values referenced as {{ params.name }}. Renamed or deleted references are reported by live validation.</p>
    <datalist id="process-param-types"><option value="string"></option><option value="number"></option><option value="boolean"></option></datalist>
    <div ref=${list} class="process-param-list">
      ${!rowsRef.current.length && html`<div class="process-param-empty">No parameters declared.</div>`}
      ${rowsRef.current.map((row, index) => html`<div key=${row.key} class="process-param-row" data-process-param=${row.name}>
        <label><span>Name</span><input class="process-param-name" type="text" spellcheck="false" aria-label=${`Parameter ${index + 1} name`} value=${row.rawName} onInput=${(event) => update(index, { rawName: event.currentTarget.value, name: event.currentTarget.value.trim() })} /></label>
        <label><span>Type</span><input class="process-param-type" type="text" spellcheck="false" list="process-param-types" aria-label=${`Parameter ${row.name || index + 1} type`} value=${row.rawType} onInput=${(event) => update(index, { rawType: event.currentTarget.value, type: event.currentTarget.value.trim(), defaultChanged: true })} /></label>
        <label class="process-param-description-field"><span>Description</span><input class="process-param-description" type="text" spellcheck="true" placeholder="Description" aria-label=${`Parameter ${row.name || index + 1} description`} value=${row.description} onInput=${(event) => update(index, { description: event.currentTarget.value })} /></label>
        <label class="process-param-default-field"><span>Default</span><span class="process-param-default-control"><input class="process-param-default-enabled" type="checkbox" aria-label=${`Parameter ${row.name || index + 1} has default`} checked=${row.hasDefault} onChange=${(event) => update(index, { hasDefault: event.currentTarget.checked })} /><input class="process-param-default" type="text" spellcheck="false" placeholder="No default" aria-label=${`Parameter ${row.name || index + 1} default`} value=${row.defaultValue} disabled=${!row.hasDefault} onInput=${(event) => update(index, { defaultValue: event.currentTarget.value, defaultChanged: true })} /></span></label>
        <label class="process-param-check"><input class="process-param-required" type="checkbox" aria-label=${`Parameter ${row.name || index + 1} required`} checked=${row.required} onChange=${(event) => update(index, { required: event.currentTarget.checked, requiredChanged: true })} /><span>Required</span></label>
        <button class="process-action process-action-danger" type="button" aria-label=${`Remove parameter ${row.name || index + 1}`} onClick=${() => remove(index)}>remove</button>
      </div>`)}
    </div>
    <div class="process-param-toolbar"><button ref=${addButton} class="process-action" type="button" onClick=${add}>+ add param</button><span class="spacer"></span><div class="process-param-error" role="alert">${error}</div></div>
    <div class="modal-buttons"><button type="button" onClick=${requestClose}>Cancel</button><button class="primary" type="button" onClick=${apply}>Apply</button></div>
  </${Overlay}>`;
}

export function openProcessParamsDialog({ model, onMutated, onClosed, confirmDiscard = async () => false } = {}) {
  if (!model) throw new Error('process param editor requires a model');
  const host = document.body.appendChild(document.createElement('div'));
  let handle = null;
  let closed = false;
  const complete = (result) => {
    if (closed) return;
    closed = true;
    render(null, host);
    host.remove();
    onClosed?.(result);
  };
  render(html`<${ParamsDialog} model=${model} onMutated=${onMutated} complete=${complete}
    confirmDiscard=${confirmDiscard} registerHandle=${(value) => { handle = value; }} />`, host);
  const dispose = () => complete(false);
  dispose.isDirty = () => !!handle?.isDirty?.();
  dispose.requestClose = () => handle?.requestClose?.() || Promise.resolve(true);
  return dispose;
}
// dashboard-imperative-boundary: preact-compat
