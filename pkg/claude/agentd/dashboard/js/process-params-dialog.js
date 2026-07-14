// Structured template-param editor. Param ids are the map keys referenced by
// {{ params.<id> }}; changing/deleting one intentionally does not rewrite
// performer text, so the existing live-validation loop reports stale refs.

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

function clone(value) { return structuredClone(value); }

function defaultText(value) {
  if (value === undefined) return '';
  if (typeof value === 'string') return value;
  if (typeof value === 'number' || typeof value === 'boolean') return String(value);
  try { return JSON.stringify(value); } catch { return String(value); }
}

function parseDefault(value, type) {
  if (type === 'boolean' && (value === 'true' || value === 'false')) return value === 'true';
  if (type === 'number' && value.trim() !== '') {
    const number = Number(value);
    if (Number.isFinite(number)) return number;
  }
  return value;
}

function nextParamName(rows) {
  const names = new Set(rows.map((row) => row.name));
  if (!names.has('param')) return 'param';
  for (let n = 2; ; n += 1) if (!names.has(`param${n}`)) return `param${n}`;
}

export function openProcessParamsDialog({ model, onMutated = () => {}, onClosed = () => {} } = {}) {
  if (!model) throw new Error('process param editor requires a model');
  let rows = Object.entries(model.template.params || {}).sort(([a], [b]) => a.localeCompare(b, 'en'))
    .map(([name, param]) => ({
      name, param: clone(param), type: param.type || 'string', description: param.description || '',
      required: param.required === true, requiredChanged: false, hasDefault: param.default !== undefined,
      defaultValue: defaultText(param.default), defaultChanged: false,
    }));
  let closed = false;
  const error = h('div', { class: 'process-param-error', role: 'alert' });
  const list = h('div', { class: 'process-param-list' });
  const addButton = h('button', { class: 'process-action', type: 'button', text: '+ add param' });
  const applyButton = h('button', { class: 'primary', type: 'button', text: 'Apply' });
  const cancelButton = h('button', { type: 'button', text: 'Cancel' });
  const overlay = h('div', { class: 'modal-overlay show process-param-modal' },
    h('div', { class: 'modal process-param-dialog', role: 'dialog', 'aria-modal': 'true', 'aria-labelledby': 'process-param-title' },
      h('h3', { id: 'process-param-title', text: 'Template parameters' }),
      h('p', { class: 'muted', text: 'Declare values referenced as {{ params.name }}. Renamed or deleted references are reported by live validation.' }),
      h('datalist', { id: 'process-param-types' }, h('option', { value: 'string' }), h('option', { value: 'number' }), h('option', { value: 'boolean' })),
      list,
      h('div', { class: 'process-param-toolbar' }, addButton, h('span', { class: 'spacer' }), error),
      h('div', { class: 'modal-buttons' }, cancelButton, applyButton)));

  const finish = (applied) => {
    if (closed) return;
    closed = true;
    overlay.remove();
    document.removeEventListener('keydown', onKey, true);
    onClosed(applied);
  };
  const onKey = (event) => {
    if (event.key !== 'Escape') return;
    event.preventDefault(); event.stopImmediatePropagation(); finish(false);
  };

  const render = () => {
    list.replaceChildren();
    if (!rows.length) list.append(h('div', { class: 'process-param-empty', text: 'No parameters declared.' }));
    rows.forEach((row, index) => {
      const name = h('input', { class: 'process-param-name', type: 'text', spellcheck: 'false', 'aria-label': `Parameter ${index + 1} name` });
      name.value = row.name;
      name.addEventListener('input', () => { row.name = name.value.trim(); error.textContent = ''; });
      const type = h('input', { class: 'process-param-type', type: 'text', spellcheck: 'false', list: 'process-param-types', 'aria-label': `Parameter ${row.name || index + 1} type` });
      type.value = row.type;
      type.addEventListener('input', () => { row.type = type.value.trim(); row.defaultChanged = true; });
      const description = h('input', { class: 'process-param-description', type: 'text', spellcheck: 'true', placeholder: 'Description', 'aria-label': `Parameter ${row.name || index + 1} description` });
      description.value = row.description;
      description.addEventListener('input', () => { row.description = description.value; });
      const defaultEnabled = h('input', { class: 'process-param-default-enabled', type: 'checkbox', 'aria-label': `Parameter ${row.name || index + 1} has default` });
      defaultEnabled.checked = row.hasDefault;
      const defaultValue = h('input', { class: 'process-param-default', type: 'text', spellcheck: 'false', placeholder: 'No default', 'aria-label': `Parameter ${row.name || index + 1} default` });
      defaultValue.value = row.defaultValue;
      defaultValue.disabled = !row.hasDefault;
      defaultEnabled.addEventListener('change', () => { row.hasDefault = defaultEnabled.checked; row.defaultChanged = true; defaultValue.disabled = !row.hasDefault; });
      defaultValue.addEventListener('input', () => { row.defaultValue = defaultValue.value; row.defaultChanged = true; });
      const required = h('input', { class: 'process-param-required', type: 'checkbox', 'aria-label': `Parameter ${row.name || index + 1} required` });
      required.checked = row.required;
      required.addEventListener('change', () => { row.required = required.checked; row.requiredChanged = true; });
      const remove = h('button', { class: 'process-action process-action-danger', type: 'button', text: 'remove', 'aria-label': `Remove parameter ${row.name || index + 1}` });
      remove.addEventListener('click', () => { rows.splice(index, 1); render(); });
      list.append(h('div', { class: 'process-param-row', 'data-process-param': row.name },
        h('label', {}, h('span', { text: 'Name' }), name),
        h('label', {}, h('span', { text: 'Type' }), type),
        h('label', { class: 'process-param-description-field' }, h('span', { text: 'Description' }), description),
        h('label', { class: 'process-param-default-field' }, h('span', { text: 'Default' }), h('span', { class: 'process-param-default-control' }, defaultEnabled, defaultValue)),
        h('label', { class: 'process-param-check' }, required, h('span', { text: 'Required' })), remove));
    });
  };

  addButton.addEventListener('click', () => {
    rows.push({ name: nextParamName(rows), param: {}, type: 'string', description: '', required: false, requiredChanged: false, hasDefault: false, defaultValue: '', defaultChanged: false });
    render();
    list.lastElementChild?.querySelector('.process-param-name')?.focus();
  });
  cancelButton.addEventListener('click', () => finish(false));
  applyButton.addEventListener('click', () => {
    const names = rows.map((row) => row.name);
    if (names.some((name) => !name)) { error.textContent = 'Every parameter needs a name.'; return; }
    if (new Set(names).size !== names.length) { error.textContent = 'Parameter names must be unique.'; return; }
    // A null prototype keeps valid-looking-but-dangerous draft names such as
    // "__proto__" inert until live validation reports the invalid id.
    const params = Object.create(null);
    for (const row of rows) {
      const param = clone(row.param);
      param.type = row.type || 'string';
      if (row.description) param.description = row.description; else delete param.description;
      if (row.requiredChanged) {
        if (row.required) param.required = true; else delete param.required;
      }
      if (row.hasDefault) param.default = row.defaultChanged ? parseDefault(row.defaultValue, row.type) : row.param.default;
      else delete param.default;
      params[row.name] = param;
    }
    const changed = model.setParams(params);
    if (changed) onMutated();
    finish(true);
  });
  overlay.addEventListener('click', (event) => { if (event.target === overlay) finish(false); });
  document.addEventListener('keydown', onKey, true);
  document.body.append(overlay);
  render();
  (list.querySelector('input') || addButton).focus();
  return finish;
}
