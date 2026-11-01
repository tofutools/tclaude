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

// What a spawn can have filled, and the word a warning uses for what widens
// access. Any change to these warns: whether a value is wider depends on the
// harness, so the form does not try to rank them.
const SPAWN_FIELDS = new Map(Object.entries({
  name: '', 'init-msg': '', group: 'group', 'load-profile': 'profile', descr: '', task: '', role: '', cwd: '', 'wt-repo': '', worktree: '', 'wt-branch': '', 'wt-base': '',
  'wt-fetch': '', 'wt-sync': '', harness: '', model: '', 'model-codex': '', 'model-custom': '', effort: '', 'fast-mode': '', 'auto-compact-window': '', 'context-window-max': '',
  'group-context': '', focus: '', 'auto-memory': '',
  owner: 'owner', sandbox: 'sandbox', 'sandbox-impl': 'sandbox', 'sandbox-profile': 'sandbox', 'allow-unenforced-sandbox': 'sandbox',
  approval: 'approval', 'approval-reviewer': 'approval', 'ask-timeout': 'approval', tools: 'tools', 'trust-dir': 'directory trust',
  'remote-control': 'remote control', 'peer-messaging': 'peer messaging', 'usage-profile': 'budget', 'copilot-api': 'API access',
  'codex-app-server': 'app server', 'ssh-workaround': 'SSH access',
}).map(([id, widens]) => [`agent-spawn-${id}`, widens]));

const CONFIG_WIDENS = [
  [/sudo|permissions/, 'permissions'], [/remote/, 'remote access'], [/dashboardbind|dashboardport|persisttoken/, 'daemon access'],
  [/notif-command/, 'a command it runs'], [/sandbox/, 'sandbox'], [/proxy/, 'proxies'], [/federation/, 'federation'], [/approval/, 'approval'],
];

export const FILL_FORMS = Object.freeze({
  spawn: {
    root: '#agent-spawn-modal', showsAll: false,
    accepts: (id) => SPAWN_FIELDS.has(id),
    widens: (id) => SPAWN_FIELDS.get(id) || '',
  },
  // Config's sections stay mounted while hidden, and the rail flags the ones a
  // fill changed, so a field can be filled in a section that is not open.
  config: {
    root: '.config-form', showsAll: true,
    accepts: (id) => id.startsWith('cfg-') && id !== 'cfg-filter',
    widens: (id) => CONFIG_WIDENS.find(([pattern]) => pattern.test(id))?.[1] || '',
  },
});

// A change settles after Preact's after-paint effects, which can wait 100ms.
const SETTLE_MS = 120;
const defaultTick = () => new Promise((resolve) => { setTimeout(resolve, SETTLE_MS); });

const fieldsIn = (root) => [...root.querySelectorAll('input[id], select[id], textarea[id]')];

export function readField(el) {
  return el.type === 'checkbox' || el.type === 'radio' ? String(el.checked) : el.value;
}

function writeField(win, el, value) {
  if (el.type === 'checkbox' || el.type === 'radio') {
    if (String(el.checked) !== value) el.click();
    return;
  }
  el.value = value;
  el.dispatchEvent(new win.Event('input', { bubbles: true }));
  el.dispatchEvent(new win.Event('change', { bubbles: true }));
}

// optionValue accepts what a capture prints, an option's label, or its value.
function optionValue(el, value) {
  const options = [...el.options];
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
  const onEdit = (event) => {
    if (!event.isTrusted || !event.target?.id) return;
    look();
    release(event.target.id);
    if (event.type !== 'change') return;
    // A profile pick or a harness change moves other fields: those are the
    // operator's now too.
    setTimeoutImpl(() => {
      for (const el of fieldsIn(root)) {
        if (seen.get(el.id) !== readField(el)) release(el.id);
      }
      look();
    });
  };
  look();
  if (keepValues) {
    for (const el of fieldsIn(root)) {
      if (el.type !== 'checkbox' && el.type !== 'radio' && el.value) owned.add(el.id);
    }
  }
  root.addEventListener('input', onEdit, true);
  root.addEventListener('change', onEdit, true);
  const watch = { root, owned, look };
  watches.set(form, watch);
  return () => {
    root.removeEventListener('input', onEdit, true);
    root.removeEventListener('change', onEdit, true);
    if (watches.get(form) === watch) watches.delete(form);
    setFilled(form, []);
  };
}


function refusal(form, watch, el, id) {
  if (!form.accepts(id)) return 'not_fillable';
  if (!el) return 'not_available';
  if (el.matches(MASKED) || el.closest('[data-capture-mask]')) return 'sensitive';
  if (el.disabled || el.readOnly) return 'disabled';
  if (!form.showsAll && !el.getClientRects().length) return 'not_available';
  if (watch.owned.has(id)) return 'operator_edited';
  return '';
}

// applyFill puts values into the open form, one at a time so that each field's
// cascade has run before the next, which may depend on it (a harness, then its
// model). It answers what became of each.
export async function applyFill({ win = globalThis.window, request, tick = defaultTick }) {
  const form = FILL_FORMS[request.form];
  const watch = watches.get(request.form);
  if (!form || !watch?.root.isConnected) return { error: `the ${request.form} form is not open` };
  const outcomes = [];
  for (const { field: id, value: asked } of request.fields) {
    const el = win.document.getElementById(id);
    const inForm = el && watch.root.contains(el) ? el : null;
    let status = refusal(form, watch, inForm, id);
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
    const before = readField(el);
    if (before === value) {
      outcomes.push({ field: id, status: 'unchanged' });
      continue;
    }
    writeField(win, el, value);
    await tick();
    const landed = readField(el);
    // Its own cascade is not the operator's doing.
    watch.look();
    if (landed === before) {
      outcomes.push({ field: id, status: 'rejected' });
      continue;
    }
    const held = filledIn(request.form);
    const earlier = held.find((field) => field.id === id);
    const widens = form.widens(id);
    const entry = { id, before: earlier ? earlier.before : before, after: landed, widens, agent: request.agent, label: fillLabel(el) };
    setFilled(request.form, [...held.filter((field) => field.id !== id), entry]);
    outcomes.push({ field: id, status: landed === value ? 'filled' : 'filled_changed', widens: widens || undefined });
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

// undoFill restores what was there, newest first. A field the operator has since
// edited has already left the list, so their edit stays.
export async function undoFill(formName, { win = globalThis.window, tick = defaultTick } = {}) {
  const watch = watches.get(formName);
  const held = filledIn(formName);
  if (!watch) return;
  for (const field of [...held].reverse()) {
    const el = win.document.getElementById(field.id);
    if (!el || readField(el) !== field.after) continue;
    writeField(win, el, field.before);
    await tick();
    watch.look();
  }
  setFilled(formName, []);
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
