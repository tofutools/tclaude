import test from 'node:test';
import { readFileSync } from 'node:fs';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// The editor's CodeMirror half (markdown-codemirror.js) cannot run in
// LinkeDOM, so each test decides what it does: refuse to load (the textarea
// fallback every operator gets while the bundle is on its way), or hand back
// a recording host, which is what the editor drives once it has loaded.

const NO_CODEMIRROR = `
  export function loadCodeMirror() { return Promise.reject(new Error('offline')); }
  export function mountCodeMirror() { return Promise.reject(new Error('offline')); }
`;

// A host over a plain string, recording what the editor asked of it.
const RECORDING_CODEMIRROR = `
  import { applyEdit } from './markdown-edits.js';
  export function loadCodeMirror() { return Promise.resolve({}); }
  export function mountCodeMirror(parent, doc, options) {
    let text = doc;
    let sel = { from: text.length, to: text.length };
    const host = {
      kind: 'code-mirror', calls: [], options,
      doc: () => text,
      lineAt: () => '',
      selection: () => sel,
      select: (from, to) => { sel = { from, to }; },
      apply: (edit) => { host.calls.push(['apply', edit]); text = applyEdit(text, edit); options.onChange(text); },
      setDoc: (next) => { if (next !== text) { host.calls.push(['setDoc', next]); text = next; } },
      setDisabled: (off) => host.calls.push(['setDisabled', off]),
      posAt: () => 0,
      focus: () => {},
      destroy: () => host.calls.push(['destroy']),
    };
    globalThis.__mdHost = host;
    return Promise.resolve(host);
  }
`;

async function settle(harness) {
  for (let i = 0; i < 4; i += 1) {
    await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  }
}

async function setup(t, codemirror = NO_CODEMIRROR) {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/markdown-codemirror.js', codemirror);
  const { MarkdownEditor } = await harness.importDashboardModule('js/markdown-editor.js');
  return { harness, MarkdownEditor };
}

// Controlled like the forms hold it: the editor's onInput sets the value.
function controlled(harness, MarkdownEditor, props = {}, initial = '') {
  const seen = [];
  function Host() {
    const [value, setValue] = harness.hooks.useState(initial);
    return harness.html`<${MarkdownEditor} value=${value} onInput=${(next) => { seen.push(next); setValue(next); }} ...${props} />`;
  }
  return { Host, seen };
}

test('until CodeMirror loads the editor is a textarea, and the toolbar works on it', async (t) => {
  const { harness, MarkdownEditor } = await setup(t);
  const { Host, seen } = controlled(harness, MarkdownEditor, { label: 'Brief' });
  const mounted = await harness.mount(harness.html`<${Host} />`);
  await settle(harness);
  const root = mounted.container.querySelector('.md-ed');
  assert.equal(root.getAttribute('data-md-editor'), 'textarea');
  const box = root.querySelector('textarea');
  assert.equal(box.getAttribute('aria-label'), 'Brief');
  await harness.input(box, 'make this bold');
  box.selectionStart = 10;
  box.selectionEnd = 14;
  await harness.act(() => harness.fireEvent(root.querySelector('[data-md-tool="bold"]'), 'click'));
  assert.equal(box.value, 'make this **bold**');
  assert.equal(seen.at(-1), 'make this **bold**', 'a toolbar edit reaches the form like typing does');
  await mounted.unmount();
});

test('Preview renders what readers will see, task lists included', async (t) => {
  const { harness, MarkdownEditor } = await setup(t);
  const { Host } = controlled(harness, MarkdownEditor, {}, '## Scope\n\n- [x] done\n- [ ] next');
  const mounted = await harness.mount(harness.html`<${Host} />`);
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('[data-md-mode="preview"]'), 'click'));
  await settle(harness);
  const preview = mounted.container.querySelector('.md-ed-view');
  assert.ok(preview, 'the preview shows');
  assert.equal(mounted.container.querySelector('.md-ed-edit').hidden, true, 'and the text is hidden behind it');
  assert.equal(preview.querySelector('h2')?.textContent, 'Scope');
  const boxes = [...preview.querySelectorAll('input.markdown-task-box')];
  assert.deepEqual(boxes.map((box) => box.hasAttribute('checked')), [true, false]);
  assert.ok(boxes.every((box) => box.hasAttribute('disabled')), 'a rendered task box cannot be ticked');
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('[data-md-mode="split"]'), 'click'));
  assert.ok(mounted.container.querySelector('.md-ed-view') && !mounted.container.querySelector('.md-ed-edit').hidden, 'Split shows both');
  await mounted.unmount();
});

// tk_b6c22fcc: the root's mode class (md-ed-preview) once named the preview
// pane too, so Preview gave the whole box the pane's flex: 1 1 0 and
// min-height: 0 and it collapsed to a few pixels. The box keeps its height
// (and its resize grip its hold) only while no mode class is also a part's.
test('Preview keeps the Write height: the box never takes a pane\'s class', async (t) => {
  const { harness, MarkdownEditor } = await setup(t);
  const { Host } = controlled(harness, MarkdownEditor, {}, '## Scope');
  const mounted = await harness.mount(harness.html`<${Host} />`);
  const root = mounted.container.querySelector('.md-ed');
  const css = readFileSync(new URL('../dashboard/dashboard.css', import.meta.url), 'utf8');
  for (const mode of ['write', 'preview', 'split']) {
    await harness.act(() => harness.fireEvent(mounted.container.querySelector(`[data-md-mode="${mode}"]`), 'click'));
    await settle(harness);
    assert.ok(root.classList.contains(`md-ed-${mode}`), mode);
    const inner = new Set([...root.querySelectorAll('[class]')].flatMap((el) => [...el.classList]));
    const shared = [...root.classList].filter((name) => inner.has(name));
    assert.deepEqual(shared, [], `in ${mode} the box shares no class with its parts`);
    // Nor does a rule for the mode class alone size the box.
    const own = new RegExp(`(^|[},])\\s*\\.md-ed-${mode}\\s*[,{]`, 'm');
    assert.equal(own.test(css), false, `no rule sizes .md-ed-${mode} by itself`);
  }
  await mounted.unmount();
});

test('a compact editor has the everyday tools and no Split', async (t) => {
  const { harness, MarkdownEditor } = await setup(t);
  const mounted = await harness.mount(harness.html`<${MarkdownEditor} compact value="" />`);
  const root = mounted.container.querySelector('.md-ed');
  assert.equal(root.querySelector('[data-md-mode="split"]'), null);
  assert.equal(root.querySelector('[data-md-tool="table"]'), null);
  assert.ok(root.querySelector('[data-md-tool="bold"]'));
  assert.equal(root.querySelector('.md-ed-heading'), null);
  await mounted.unmount();
});

test('Ctrl+Enter sends from an editor with its own submit, and goes no further', async (t) => {
  const { harness, MarkdownEditor } = await setup(t);
  let sent = 0;
  let reachedDialog = 0;
  const mounted = await harness.mount(harness.html`<div onKeyDown=${() => { reachedDialog += 1; }}>
    <${MarkdownEditor} value="hi" onSubmit=${() => { sent += 1; }} /></div>`);
  const box = mounted.container.querySelector('textarea');
  await harness.act(() => harness.fireEvent(box, 'keydown', { key: 'Enter', ctrlKey: true }));
  assert.equal(sent, 1);
  assert.equal(reachedDialog, 0, 'the dialog does not also submit');
  await mounted.unmount();
});

test('once loaded, CodeMirror is the editor: the toolbar, outside changes and files all go through it', async (t) => {
  const { harness, MarkdownEditor } = await setup(t, RECORDING_CODEMIRROR);
  const editorRef = { current: null };
  let setOutside;
  const seen = [];
  function Host() {
    const [value, setValue] = harness.hooks.useState('first');
    setOutside = setValue;
    return harness.html`<${MarkdownEditor} value=${value} editorRef=${editorRef} onInput=${(next) => { seen.push(next); setValue(next); }} />`;
  }
  const mounted = await harness.mount(harness.html`<${Host} />`);
  await settle(harness);
  const host = globalThis.__mdHost;
  const root = mounted.container.querySelector('.md-ed');
  assert.equal(root.getAttribute('data-md-editor'), 'code-mirror');
  assert.equal(root.querySelector('textarea').hidden, true, 'the fallback textarea steps aside');
  assert.equal(host.options.label, 'Markdown');

  host.select(0, 5);
  await harness.act(() => harness.fireEvent(root.querySelector('[data-md-tool="italic"]'), 'click'));
  assert.equal(host.doc(), '*first*');
  assert.equal(seen.at(-1), '*first*');

  await harness.act(() => setOutside('reset from outside'));
  assert.deepEqual(host.calls.at(-1), ['setDoc', 'reset from outside'], 'a value the form sets reaches CodeMirror');

  host.select(host.doc().length, host.doc().length);
  await harness.act(() => editorRef.current.insertFiles(['shot.png']));
  assert.match(host.doc(), /!\[shot\]\(shot\.png\)\n$/);

  await mounted.unmount();
  assert.deepEqual(host.calls.at(-1), ['destroy'], 'unmounting destroys the view');
});

test('a render carrying the text from a keystroke ago never rewinds the editor', async (t) => {
  const { harness, MarkdownEditor } = await setup(t, RECORDING_CODEMIRROR);
  let setOutside;
  // The form sets its value only when the test says so, so a render can lag
  // behind the typing exactly as a busy dashboard's does.
  function Host() {
    const [value, setValue] = harness.hooks.useState('');
    setOutside = setValue;
    return harness.html`<${MarkdownEditor} value=${value} onInput=${() => {}} />`;
  }
  const mounted = await harness.mount(harness.html`<${Host} />`);
  await settle(harness);
  const host = globalThis.__mdHost;
  // The mount's own sync of the (empty) value has run before anyone types.
  for (let i = 0; i < 20 && !host.calls.some(([kind]) => kind === 'setDisabled'); i += 1) await settle(harness);
  assert.ok(host.calls.some(([kind]) => kind === 'setDisabled'), 'the editor settled after mounting');
  // Two keystrokes: the editor reports 'x', then 'xa'.
  host.apply({ changes: [{ from: 0, insert: 'x' }] });
  host.apply({ changes: [{ from: 1, insert: 'a' }] });
  const before = host.calls.length;
  await harness.act(() => setOutside('x'));
  assert.equal(host.doc(), 'xa', 'the lagging render does not undo the second keystroke');
  await harness.act(() => setOutside('xa'));
  assert.equal(host.calls.slice(before).filter(([kind]) => kind === 'setDoc').length, 0, 'catching up writes nothing');
  // Caught up: the form clearing the text (a sent comment) applies, even
  // though the editor once reported '' itself.
  await harness.act(() => setOutside(''));
  assert.equal(host.doc(), '');
  await mounted.unmount();
});
