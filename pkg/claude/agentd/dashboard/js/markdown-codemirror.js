// markdown-codemirror.js -- the Markdown editor's CodeMirror half: loading the
// vendored bundle on demand and mounting a view behind the small host
// interface markdown-editor.js drives (doc, lineAt, selection, apply, insert,
// focus, destroy). The editor component never touches CodeMirror itself, so
// its tests swap this module for a stub and drive the textarea fallback.
//
// The bundle (third_party/codemirror, ~326 KB) is fetched with a dynamic
// import the first time an editor mounts, never on a dashboard load.

import { TOOLS } from './markdown-edits.js';

let loading = null;

// loadCodeMirror resolves to the bundle's exports. A failed load is not
// cached: the next editor tries again.
export function loadCodeMirror() {
  if (!loading) {
    loading = import('codemirror').catch((error) => {
      loading = null;
      throw error;
    });
  }
  return loading;
}

// EXTERNAL marks a change setDoc made: the text came from the form.
const EXTERNAL = 'tclaude.external';

// A line that is a list item: Tab nests it, Shift+Tab takes it out a level.
// Anywhere else Tab moves focus as usual, so the editor never traps it.
const LIST_LINE = /^\s*(?:[-*+]|\d{1,9}[.)])\s/;

// canHostCodeMirror is whether a view can be built in parent: CodeMirror
// needs the DOM selection of the document (or shadow root) it lives in.
export function canHostCodeMirror(parent) {
  const root = parent?.getRootNode?.();
  const doc = parent?.ownerDocument;
  return typeof (root && typeof root.getSelection === 'function' ? root : doc)?.getSelection === 'function';
}

// mountCodeMirror replaces nothing: it builds a view inside `parent` holding
// `doc` and returns its host. Options:
//
//   label        the editing area's accessible name
//   placeholder  shown while it is empty
//   selection    { anchor, head } to start with (the textarea's, so a caret
//                placed before the load is kept)
//   onChange(text)       every edit
//   onSelection()        the selection moved (the toolbar's heading control)
//   onSubmit()           Ctrl/Cmd+Enter; without it the key does nothing here
//                        and the dialog around the editor takes it
//   onTool(id)           a toolbar shortcut (Mod-b, Mod-i, Mod-e, Mod-k)
//   disabled             start read-only (the form is sending)
export async function mountCodeMirror(parent, doc, options = {}) {
  // CodeMirror reads the DOM selection while its view is being built. A
  // document without one (a DOM shim, not a browser) makes the constructor
  // throw half way, after its plugins started their timers: a view that is
  // never finished nor destroyed, whose background parse later dispatches
  // into it and throws "Calls to EditorView.update are not allowed while an
  // update is in progress". So no view is begun there; the textarea stays.
  if (!canHostCodeMirror(parent)) throw new Error('markdown editor: this document has no DOM selection');
  const cm = await loadCodeMirror();
  const {
    EditorView, EditorState, EditorSelection, keymap, placeholder, Prec, Compartment,
    defaultKeymap, history, historyKeymap, indentMore, indentLess,
    syntaxHighlighting, classHighlighter, tagHighlighter, tags, markdown, GFM,
  } = cm;
  const { label = 'Markdown', selection, onChange = () => {}, onSelection = () => {}, onSubmit = null, onTool = () => {} } = options;
  const listLine = (state) => LIST_LINE.test(state.doc.lineAt(state.selection.main.head).text);
  const toolKeys = TOOLS.filter((tool) => tool && tool.key).map((tool) => ({
    key: tool.key, preventDefault: true, run: () => { onTool(tool.id); return true; },
  }));
  const own = [
    // Ctrl/Cmd+Enter submits the form. CodeMirror's default binds it to
    // "insert a blank line"; reserving it here keeps the text as it is and
    // lets the keydown reach the dialog (which submits) unless the editor
    // has its own submit, which then takes it.
    { key: 'Mod-Enter', run: () => { onSubmit?.(); return true; }, stopPropagation: !!onSubmit },
    { key: 'Tab', run: (view) => (listLine(view.state) ? indentMore(view) : false) },
    { key: 'Shift-Tab', run: (view) => (listLine(view.state) ? indentLess(view) : false) },
    ...toolKeys,
  ];
  const editable = new Compartment();
  const hint = new Compartment();
  // The form hears of an edit inside CodeMirror's update, and a render it
  // runs there can write back (the placeholder, disabled, the text): such a
  // write waits for the update to end, since CodeMirror refuses a dispatch
  // during one.
  let reporting = false;
  let destroyed = false;
  const write = (fn) => {
    if (destroyed) return;
    if (!reporting) { fn(); return; }
    queueMicrotask(() => { if (!destroyed) fn(); });
  };
  const start = Math.min(selection?.anchor ?? doc.length, doc.length);
  const head = Math.min(selection?.head ?? start, doc.length);
  const view = new EditorView({
    parent,
    state: EditorState.create({
      doc,
      selection: EditorSelection.single(start, head),
      extensions: [
        history(),
        editable.of(EditorView.editable.of(!options.disabled)),
        Prec.highest(keymap.of(own)),
        keymap.of([...defaultKeymap, ...historyKeymap]),
        EditorView.lineWrapping,
        EditorView.contentAttributes.of({ 'aria-label': label, 'aria-multiline': 'true', spellcheck: 'true', autocapitalize: 'sentences' }),
        hint.of(placeholder(options.placeholder || '')),
        // Files belong to the form's attachment intake, which the event still
        // bubbles to. CodeMirror's own handlers would also paste a dropped
        // text file's contents, or a pasted file's text flavour, into the
        // text; the textarea fallback does neither.
        EditorView.domEventHandlers({
          drop: (event) => Boolean(event.dataTransfer?.files?.length),
          paste: (event) => [...(event.clipboardData?.items || [])].some((item) => item.kind === 'file'),
        }),
        syntaxHighlighting(classHighlighter),
        // classHighlighter has no class for these Markdown tags.
        syntaxHighlighting(tagHighlighter([
          { tag: tags.strikethrough, class: 'tok-strikethrough' },
          { tag: tags.quote, class: 'tok-quote' },
          { tag: tags.monospace, class: 'tok-monospace' },
          { tag: tags.contentSeparator, class: 'tok-contentSeparator' },
        ])),
        // html stays off: the dashboard renders raw HTML as text, and the
        // bundle carries no HTML grammar to highlight it with.
        markdown({ extensions: [GFM], completeHTMLTags: false }),
        EditorView.updateListener.of((update) => {
          // setDoc's own change is the form's text arriving, not an edit:
          // reporting it back would hand the form its own value again.
          const external = update.transactions.every((tr) => !tr.docChanged || tr.isUserEvent(EXTERNAL));
          reporting = true;
          try {
            if (update.docChanged && !external) onChange(update.state.doc.toString());
            if (update.docChanged || update.selectionSet) onSelection();
          } finally {
            reporting = false;
          }
        }),
      ],
    }),
  });
  return {
    kind: 'code-mirror',
    doc: () => view.state.doc.toString(),
    lineAt: (pos) => view.state.doc.lineAt(Math.min(pos, view.state.doc.length)).text,
    selection: () => view.state.selection.main,
    // apply takes an edit from markdown-edits.js.
    apply: (edit) => write(() => {
      view.dispatch({ changes: edit.changes.map((c) => ({ from: c.from, to: c.to ?? c.from, insert: c.insert })), selection: edit.selection, scrollIntoView: true });
      view.focus();
    }),
    // setDoc replaces the whole text (the form reset it, or the brief arrived).
    setDoc: (text) => write(() => {
      if (text === view.state.doc.toString()) return;
      view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: text }, userEvent: EXTERNAL });
    }),
    // posAt is the text position under a pointer event, for a drop.
    posAt: (event) => view.posAtCoords({ x: event.clientX, y: event.clientY }),
    setPlaceholder: (text) => write(() => { view.dispatch({ effects: hint.reconfigure(placeholder(text || '')) }); }),
    setDisabled: (disabled) => write(() => { view.dispatch({ effects: editable.reconfigure(EditorView.editable.of(!disabled)) }); }),
    focus: () => view.focus(),
    hasFocus: () => view.hasFocus,
    destroy: () => { destroyed = true; view.destroy(); },
  };
}
