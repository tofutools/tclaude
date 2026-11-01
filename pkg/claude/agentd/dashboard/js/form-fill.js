// form-fill.js -- lets an agent propose values in an open form (tcl-3ta7bl,
// `tclaude agent guide fill`). A fill never submits and presses nothing: the
// operator reads what landed and acts on it.
//
// Each value goes through the control's own handler, as native events, so the
// form's state, validation and cascades run as they do for typing: the spawn
// draft for spawn, and the uncontrolled inputs with their dirty tracking for
// Config. Ownership comes from trusted events: once the operator has touched a
// field, or a change of theirs has moved it, a fill leaves it alone.
// dashboard-imperative-boundary: browser-io

import { MASKED } from './capture-snapshot.js';

// What a spawn can have filled, and the word a warning uses for what can widen
// access. Any change to these warns: whether a value is wider depends on the
// harness, so the form does not try to rank them. A profile cannot be filled:
// clearing one resets most of the form, and no undo can give that back.
const SPAWN_FIELDS = new Map(Object.entries({
  name: '', 'init-msg': '', descr: '', task: '', role: '', 'wt-branch': '', 'wt-base': '', worktree: '', 'wt-fetch': '', 'wt-sync': '',
  model: '', 'model-codex': '', 'model-custom': '', effort: '', 'fast-mode': '', 'auto-compact-window': '', 'context-window-max': '', 'group-context': '', focus: '', 'auto-memory': '',
  harness: 'harness', group: 'group', cwd: 'directory', 'wt-repo': 'directory',
  owner: 'owner', sandbox: 'sandbox', 'sandbox-impl': 'sandbox', 'sandbox-profile': 'sandbox', 'allow-unenforced-sandbox': 'sandbox',
  approval: 'approval', 'approval-reviewer': 'approval', 'ask-timeout': 'approval', tools: 'tools', 'trust-dir': 'directory trust',
  'remote-control': 'remote control', 'peer-messaging': 'peer messaging', 'usage-profile': 'budget', 'copilot-api': 'API access',
  'codex-app-server': 'app server', 'ssh-workaround': 'SSH access',
}).map(([id, widens]) => [`agent-spawn-${id}`, widens]));

const spawnIds = (...names) => names.map((name) => `agent-spawn-${name}`);
const SPAWN_WORKTREE = spawnIds('worktree', 'wt-branch', 'wt-base');
// What a field resets as it changes (agent-spawn-model.js). A fill of one of these
// is refused while any of them is the operator's, since it would overwrite it.
const SPAWN_RESETS = new Map(Object.entries({
  harness: spawnIds('model', 'model-codex', 'model-custom', 'effort', 'fast-mode', 'sandbox', 'sandbox-impl', 'approval', 'approval-reviewer', 'tools', 'ask-timeout', 'trust-dir', 'remote-control'),
  group: [...SPAWN_WORKTREE, ...spawnIds('remote-control', 'sandbox-profile', 'group-context')],
  cwd: SPAWN_WORKTREE,
  'wt-repo': SPAWN_WORKTREE,
  model: spawnIds('effort'),
}).map(([id, resets]) => [`agent-spawn-${id}`, resets]));

// Config warns unless a section is known to hold nothing about access. A setting
// missed here costs one needless warning, where a list of risky ones would miss
// the next setting without a word.
const CONFIG_COSMETIC = new Set(['appearance', 'voice']);

function configWidens(el) {
  const panel = el.closest('[data-cfg-panel]');
  if (CONFIG_COSMETIC.has(panel?.getAttribute('data-cfg-panel'))) return '';
  const title = panel?.querySelector('h2')?.textContent.replace(/[^\p{L}\p{N}& ]/gu, '').replace(/\s+/g, ' ').trim();
  return title ? `${title} settings` : 'settings';
}

export const FILL_FORMS = Object.freeze({
  spawn: {
    root: '#agent-spawn-modal', showsAll: false,
    accepts: (id) => SPAWN_FIELDS.has(id),
    widens: (el) => SPAWN_FIELDS.get(el.id) || '',
    resets: (id) => SPAWN_RESETS.get(id) || [],
  },
  // Config's sections stay mounted while hidden, and the rail flags the ones a
  // fill changed, so a field can be filled in a section that is not open.
  config: {
    root: '.config-form', showsAll: true,
    accepts: (id) => id.startsWith('cfg-') && id !== 'cfg-filter',
    widens: configWidens,
    resets: () => [],
  },
});

// A change settles after Preact's after-paint effects, which can wait 100ms.
const SETTLE_MS = 120;
const defaultTick = () => new Promise((resolve) => { setTimeout(resolve, SETTLE_MS); });

const NOT_A_VALUE = new Set(['button', 'submit', 'reset', 'image', 'file', 'hidden']);
const isField = (el) => /^(INPUT|SELECT|TEXTAREA)$/.test(el.tagName) && !NOT_A_VALUE.has(el.type);
const fieldsIn = (root) => [...root.querySelectorAll('input[id], select[id], textarea[id]')].filter(isField);

export function readField(el) {
  return el.type === 'checkbox' || el.type === 'radio' ? String(el.checked) : el.value;
}

const valuesIn = (root) => new Map(fieldsIn(root).map((el) => [el.id, readField(el)]));

// writeField sends a value through the control's own handlers. watch.writing
// tells the watch this is not the operator: a checkbox's click() arrives trusted.
function writeField(win, watch, el, value) {
  watch.sweep();
  watch.writing = true;
  try {
    if (el.type === 'checkbox' || el.type === 'radio') {
      if (String(el.checked) !== value) el.click();
      return;
    }
    el.value = value;
    el.dispatchEvent(new win.Event('input', { bubbles: true }));
    el.dispatchEvent(new win.Event('change', { bubbles: true }));
  } finally {
    watch.writing = false;
  }
}

// optionValue accepts what a capture prints, an option's label, or its value.
function optionValue(el, value) {
  // An option that opens something rather than holding a value is not one.
  const options = [...el.options].filter((option) => !option.hasAttribute?.('data-no-fill'));
  const match = options.find((option) => option.value === value) || options.find((option) => option.label === value || option.textContent.trim() === value);
  return match?.value;
}

// fills is what each open form has had filled, by form. Filled fields stay in it
// until the operator edits them, undoes the fill, or the form closes.
const fills = new Map();
const watches = new Map();
const listeners = new Set();

export const filledIn = (form) => fills.get(form) || [];

// onFillsChange calls back when any form's fills change, and returns a stop
// function. This module stays free of Preact because the capture channel loads it.
export function onFillsChange(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

function setFilled(form, fields) {
  if (fields.length) fills.set(form, fields);
  else fills.delete(form);
  for (const fn of listeners) fn();
}

// watchForm starts tracking who owns a form's fields. It returns a stop function,
// which forgets the fill. keepValues is for a form reopened with the operator's
// own earlier values: they are theirs from the start.
export function watchForm(form, root, { win = globalThis.window, setTimeoutImpl = (fn) => win.setTimeout(fn, SETTLE_MS), keepValues = false } = {}) {
  const owned = new Set();
  const seen = new Map();
  const look = () => { for (const el of fieldsIn(root)) seen.set(el.id, readField(el)); };
  const release = (id) => {
    owned.add(id);
    const held = filledIn(form);
    if (held.some((field) => field.id === id)) setFilled(form, held.filter((field) => field.id !== id));
  };
  let sweeping = false;
  // A change settles after its handlers, and what moved by then is the operator's.
  // Anything written after that is not, so a write runs this first.
  const sweep = () => {
    if (!sweeping) return;
    sweeping = false;
    for (const el of fieldsIn(root)) {
      if (seen.get(el.id) !== readField(el)) release(el.id);
    }
    look();
  };
  const watch = { root, owned, look, sweep, writing: false };
  const onEdit = (event) => {
    if (!event.isTrusted || watch.writing || !event.target?.id) return;
    look();
    release(event.target.id);
    if (event.type !== 'change') return;
    // A profile pick or a harness change moves other fields: those are the
    // operator's now too.
    sweeping = true;
    setTimeoutImpl(sweep);
  };
  look();
  if (keepValues) {
    for (const el of fieldsIn(root)) {
      if (el.type === 'checkbox' || el.type === 'radio' || el.value) owned.add(el.id);
    }
  }
  root.addEventListener('input', onEdit, true);
  root.addEventListener('change', onEdit, true);
  watches.set(form, watch);
  return () => {
    root.removeEventListener('input', onEdit, true);
    root.removeEventListener('change', onEdit, true);
    if (watches.get(form) === watch) watches.delete(form);
    setFilled(form, []);
  };
}


// claimField is for a change that fires no event, such as a directory chosen in
// a browse dialog: that field is the operator's.
export function claimField(form, id) {
  watches.get(form)?.owned.add(id);
  const held = filledIn(form);
  if (held.some((field) => field.id === id)) setFilled(form, held.filter((field) => field.id !== id));
}

function refusal(form, watch, el, id) {
  if (!form.accepts(id) || (el && !isField(el))) return 'not_fillable';
  if (!el) return 'not_available';
  if (el.matches(MASKED) || el.closest('[data-capture-mask]')) return 'sensitive';
  if (el.disabled || el.readOnly) return 'disabled';
  if (!form.showsAll && !el.getClientRects().length) return 'not_available';
  if (watch.owned.has(id) || form.resets(id).some((reset) => watch.owned.has(reset))) return 'operator_edited';
  return '';
}

const isOpen = (name, watch) => watches.get(name) === watch && watch.root.isConnected;

// keepOwned puts back any field of the operator's that a write moved, which
// nothing should have, but a form can grow a cascade this file does not know.
async function keepOwned(win, watch, values, tick) {
  for (const id of watch.owned) {
    const el = win.document.getElementById(id);
    if (el && values.has(id) && readField(el) !== values.get(id)) {
      writeField(win, watch, el, values.get(id));
      await tick();
      watch.look();
    }
  }
}

// record adds what a write changed to the form's fills. Each entry keeps what was
// there before the first fill, and leaves once it is back to that.
function record(name, form, win, agent, changes) {
  let held = filledIn(name);
  for (const { el, before, after, cascade } of changes) {
    const earlier = held.find((field) => field.id === el.id);
    const first = earlier ? earlier.before : before;
    held = held.filter((field) => field.id !== el.id);
    if (first !== after) held.push({ id: el.id, before: first, after, cascade: cascade && (earlier?.cascade ?? true), widens: form.widens(el), agent, label: fillLabel(el) });
  }
  setFilled(name, held);
}

// applyFill puts values into the open form, one at a time so that each field's
// cascade has run before the next, which may depend on it (a harness, then its
// model). Whatever a write moves counts as filled, the named field or not, so the
// warning and the undo cover it. It answers what became of each field.
export async function applyFill({ win = globalThis.window, request, tick = defaultTick }) {
  const form = FILL_FORMS[request.form];
  const watch = watches.get(request.form);
  if (!form || !watch || !isOpen(request.form, watch) || !watch.root.getClientRects().length) return { error: `the ${request.form} form is not open` };
  const outcomes = [];
  for (const { field: id, value: asked } of request.fields) {
    watch.sweep();
    const found = win.document.getElementById(id);
    const el = found && watch.root.contains(found) ? found : null;
    let status = refusal(form, watch, el, id);
    let value = asked;
    if (!status && el.tagName === 'SELECT') {
      value = optionValue(el, asked);
      if (value === undefined) status = 'not_an_option';
    }
    if (!status && (el.type === 'checkbox' || el.type === 'radio') && asked !== 'true' && asked !== 'false') status = 'not_a_boolean';
    if (status) {
      outcomes.push({ field: id, status });
      continue;
    }
    if (readField(el) === value) {
      outcomes.push({ field: id, status: 'unchanged' });
      continue;
    }
    const before = valuesIn(watch.root);
    writeField(win, watch, el, value);
    await tick();
    if (!isOpen(request.form, watch)) {
      outcomes.push({ field: id, status: 'form_closed' });
      break;
    }
    await keepOwned(win, watch, before, tick);
    // The operator can get in during the wait.
    if (watch.owned.has(id)) {
      watch.look();
      outcomes.push({ field: id, status: 'operator_edited' });
      continue;
    }
    const after = valuesIn(watch.root);
    const changes = [...after].filter(([field, now]) => before.has(field) && before.get(field) !== now)
      .map(([field, now]) => ({ el: win.document.getElementById(field), before: before.get(field), after: now, cascade: field !== id }));
    watch.look();
    if (!changes.some((change) => change.el === el)) {
      outcomes.push({ field: id, status: 'rejected' });
      if (changes.length) record(request.form, form, win, request.agent, changes);
      continue;
    }
    record(request.form, form, win, request.agent, changes);
    const words = [...new Set(changes.map((change) => form.widens(change.el)).filter(Boolean))];
    outcomes.push({
      field: id, status: readField(el) === value ? 'filled' : 'filled_changed',
      widens: words.join(', ') || undefined, moved: changes.filter((change) => change.cascade).map((change) => change.el.id),
    });
  }
  return { fields: outcomes };
}

// fillLabel names a field for the bar. A label can wrap its control, and a
// select's text is every option.
function fillLabel(el) {
  const label = el.labels?.[0]?.cloneNode(true);
  for (const control of label?.querySelectorAll('input, select, textarea, button') || []) control.remove();
  return String(el.getAttribute('aria-label') || label?.textContent || el.id).replace(/\s+/g, ' ').trim().slice(0, 60);
}

// undoFill restores what was there. The fields an agent named go first, newest
// first, since that can move others, and the rest are then set back directly.
// Undoing a harness resets the fields it controls, so the operator's are put back
// afterwards. A field that could not be restored stays listed, warning and all.
export async function undoFill(name, { win = globalThis.window, tick = defaultTick } = {}) {
  const watch = watches.get(name);
  if (!watch) return;
  watch.sweep();
  const mine = valuesIn(watch.root);
  const held = filledIn(name);
  const restore = async (field) => {
    const el = win.document.getElementById(field.id);
    if (!el || readField(el) === field.before) return;
    writeField(win, watch, el, field.before);
    await tick();
    watch.look();
  };
  for (const field of held.filter((each) => !each.cascade).reverse()) await restore(field);
  for (const field of held) await restore(field);
  await keepOwned(win, watch, mine, tick);
  watch.look();
  setFilled(name, held.filter((field) => {
    const el = win.document.getElementById(field.id);
    return el && readField(el) !== field.before;
  }));
}

// fillSummary is what the bar says. A form can have been filled by several agents.
export function fillSummary(fields) {
  const agents = [...new Set(fields.map((field) => field.agent))];
  const count = fields.length === 1 ? '1 field' : `${fields.length} fields`;
  const widened = agents.flatMap((agent) => {
    const words = [...new Set(fields.filter((field) => field.agent === agent && field.widens).map((field) => field.widens))];
    return words.length ? [`${agent} widened: ${words.join(', ')}`] : [];
  });
  return { filled: `${agents.join(', ')} filled ${count}`, widened };
}
