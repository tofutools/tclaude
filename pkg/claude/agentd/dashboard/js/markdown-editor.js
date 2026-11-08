// markdown-editor.js -- the Markdown editor of the Task board's forms: a
// task's brief (New task, Edit task) and a comment (new and edited). One
// component, two sizes:
//
//   full     toolbar (heading, emphasis, lists, quote, code block, table,
//            rule, link, attach, help) and Write / Preview / Split
//   compact  the comment's: the everyday tools, Write / Preview
//
// The text is edited in CodeMirror, loaded on demand by markdown-codemirror.js
// (never on a dashboard load). Until it has loaded, or when it cannot be, the
// editor is a plain textarea with the same toolbar, so it is never not an
// editor. Preview renders with MarkdownDocument, the same renderer as the
// task's hover card and its comments, so what Preview shows is what readers
// see.
//
// Files: a paste or a drop of files on the editor is taken by the dialog's
// attachment intake, as anywhere in the form; the dialog then calls
// editorRef.current.insertFiles(names, event), which writes a reference to
// each where the caret (or the drop) is: `![name](name.png)` for an image,
// `[name](name)` for anything else. The renderer resolves the name against
// the task's files (`attachments`).

import { h } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { MarkdownDocument } from './markdown-document.js';
import { mountCodeMirror } from './markdown-codemirror.js';
import { applyEdit, fileLinksEdit, headingEdit, headingLevel, HELP, ICONS, mapPos, TOOLS } from './markdown-edits.js';

const html = htm.bind(h);

// EMITTED_KEEP bounds the editor's memory of the texts it reported.
const EMITTED_KEEP = 64;

export const MODES = [
  { key: 'write', label: 'Write' },
  { key: 'preview', label: 'Preview' },
  { key: 'split', label: 'Split', full: true },
];

function readMode(key, fallback) {
  if (!key) return fallback;
  try {
    const stored = globalThis.localStorage?.getItem(key);
    return MODES.some((mode) => mode.key === stored) ? stored : fallback;
  } catch {
    return fallback;
  }
}

function writeMode(key, mode) {
  if (!key) return;
  try { globalThis.localStorage?.setItem(key, mode); } catch { /* a convenience only */ }
}

function Icon({ name }) {
  return html`<svg class="md-ed-icon" viewBox="0 0 24 24" aria-hidden="true" focusable="false">
    ${(ICONS[name] || []).map(([tag, attrs], i) => h(tag, { ...attrs, key: i }))}
  </svg>`;
}

// shortcutLabel spells a binding for a tooltip: Mod-b is Ctrl+B (⌘B on a Mac).
export function shortcutLabel(key, mac = /Mac|iPhone|iPad/.test(globalThis.navigator?.platform || '')) {
  if (!key) return '';
  const letter = key.split('-').pop().toUpperCase();
  return mac ? `⌘${letter}` : `Ctrl+${letter}`;
}

// submitKeyLabel spells Ctrl/Cmd+Enter, the dialogs' submit key.
export function submitKeyLabel(mac = /Mac|iPhone|iPad/.test(globalThis.navigator?.platform || '')) {
  return mac ? '⌘Enter' : 'Ctrl+Enter';
}

// textareaHost drives the fallback textarea through the same interface as
// the CodeMirror host, so the toolbar works before (or without) CodeMirror.
function textareaHost(textarea, onChange) {
  return {
    kind: 'textarea',
    doc: () => textarea().value,
    lineAt: (pos) => {
      const doc = textarea().value;
      const start = doc.lastIndexOf('\n', pos - 1) + 1;
      const end = doc.indexOf('\n', pos);
      return doc.slice(start, end === -1 ? doc.length : end);
    },
    selection: () => ({ from: textarea().selectionStart || 0, to: textarea().selectionEnd || 0 }),
    apply: (edit) => {
      const el = textarea();
      const before = el.value;
      const after = applyEdit(before, edit);
      const sel = edit.selection || { anchor: mapPos(el.selectionStart || 0, edit), head: mapPos(el.selectionEnd || 0, edit) };
      el.value = after;
      onChange(after);
      el.focus?.();
      el.setSelectionRange?.(Math.min(sel.anchor, sel.head), Math.max(sel.anchor, sel.head));
    },
    posAt: () => null,
    focus: () => textarea().focus?.(),
  };
}

// MarkdownEditor props:
//
//   value, onInput(text)   the text, controlled
//   label                  the editing area's accessible name
//   placeholder
//   disabled               the form is sending
//   compact                the comment's smaller toolbar, Write / Preview only
//   onSubmit()             Ctrl/Cmd+Enter here sends (a comment); without it
//                          the key goes on to the dialog, which submits
//   attachments            the task's files, as MarkdownDocument takes them
//                          ({ filename, url, previewable }), for Preview
//   onPickFiles(files)     the toolbar's attach button: the dialog takes the
//                          files and returns the names it accepted, which are
//                          linked at the caret
//   editorRef              receives { insertFiles(names, event), focus() }
//   textareaRef            the fallback textarea (a dialog's initial focus)
//   modeKey                localStorage key the Write/Preview/Split pick is
//                          remembered under
//   hint                   the status line's right-hand note
//   maxLength              shown as a count once the text nears it
//   className
export function MarkdownEditor({
  value = '', onInput = () => {}, label = 'Markdown', placeholder = '', disabled = false, compact = false,
  onSubmit = null, attachments = undefined, onPickFiles = null, editorRef = null, textareaRef = null,
  modeKey = '', hint = '', maxLength = 0, className = '', class: extraClass = '', ...rest
}) {
  const [mode, setModeState] = useState(() => {
    const picked = readMode(modeKey, 'write');
    return compact && picked === 'split' ? 'write' : picked;
  });
  const setMode = (next) => { setModeState(next); writeMode(modeKey, next); };
  const [host, setHost] = useState(null);
  const [, setRevision] = useState(0);
  const [help, setHelp] = useState(false);
  const textarea = useRef(null);
  const mount = useRef(null);
  const fileInput = useRef(null);
  // The latest props, for the callbacks CodeMirror holds from its mount.
  const latest = useRef({});
  latest.current = { onInput, onSubmit, value };
  // The texts CodeMirror reported since the form last set one (see below).
  const emitted = useRef([]);
  const fallback = useRef(null);
  if (!fallback.current) fallback.current = textareaHost(() => textarea.current, (text) => latest.current.onInput(text));
  const active = host || fallback.current;

  const runTool = (id) => {
    const tool = TOOLS.find((t) => t && t.id === id);
    if (!tool || disabled) return;
    const { from, to } = active.selection();
    active.apply(tool.edit(active.doc(), from, to));
  };
  const runToolRef = useRef(runTool);
  runToolRef.current = runTool;

  useLayoutEffect(() => {
    let disposed = false;
    let mounted = null;
    const input = textarea.current;
    mountCodeMirror(mount.current, input?.value ?? latest.current.value, {
      label,
      placeholder,
      disabled,
      selection: input ? { anchor: input.selectionStart || 0, head: input.selectionEnd || 0 } : undefined,
      onChange: (text) => {
        emitted.current.push(text);
        if (emitted.current.length > EMITTED_KEEP) emitted.current.shift();
        latest.current.onInput(text);
      },
      onSelection: () => setRevision((n) => n + 1),
      onSubmit: onSubmit ? () => latest.current.onSubmit?.() : null,
      onTool: (id) => runToolRef.current(id),
    }).then((made) => {
      if (disposed) { made.destroy(); return; }
      mounted = made;
      const focused = globalThis.document?.activeElement === input;
      setHost(made);
      if (focused) made.focus();
    }).catch(() => { /* the textarea stays the editor */ });
    return () => { disposed = true; mounted?.destroy(); };
  }, []);

  // The text can change from outside (the brief arrived, a comment was sent
  // and cleared): CodeMirror follows it. A value the editor itself reported
  // is never pushed back: the form renders behind the typing, so a render can
  // carry the text from a keystroke ago, and writing that into the editor
  // would undo the last keystroke and set the two chasing each other.
  // Once the form has caught up with the last one, the history is dropped,
  // so a later reset to an old text (a sent comment cleared to '') applies.
  useEffect(() => {
    if (!host) return;
    const reported = emitted.current;
    if (reported.length && value === reported[reported.length - 1]) { emitted.current = []; return; }
    if (reported.includes(value)) return;
    emitted.current = [];
    host.setDoc?.(value);
  }, [value, host]);
  useEffect(() => { host?.setDisabled?.(disabled); }, [disabled, host]);

  if (editorRef) {
    editorRef.current = {
      insertFiles: (names, event = null) => {
        if (!names?.length) return;
        const dropped = event?.type === 'drop' ? active.posAt?.(event) : null;
        const { from, to } = dropped != null ? { from: dropped, to: dropped } : active.selection();
        active.apply(fileLinksEdit(active.doc(), from, to, names));
      },
      focus: () => active.focus(),
    };
  }

  const showEditor = mode !== 'preview';
  const showPreview = mode !== 'write';
  const tools = TOOLS.filter((tool) => !compact || tool === null || tool.compact);
  // No separator first, last or twice in a row once the compact set dropped tools.
  const shown = tools.filter((tool, i) => tool !== null || (i > 0 && tools[i - 1] !== null && i < tools.length - 1));
  const level = host || textarea.current ? headingLevel(active.lineAt(active.selection().from)) : 0;
  const editingOff = disabled || !showEditor;
  const keep = (event) => event.preventDefault(); // a toolbar click keeps the caret where it is
  const count = maxLength && value.length > maxLength * 0.8 ? `${value.length} / ${maxLength}` : '';
  return html`<div ...${rest} class=${`md-ed md-ed-${mode}${compact ? ' md-ed-compact' : ''}${host ? ' md-ed-cm' : ''}${className ? ` ${className}` : ''}${extraClass ? ` ${extraClass}` : ''}`}
    data-md-editor=${host ? 'code-mirror' : 'textarea'}>
    <div class="md-ed-bar" role="toolbar" aria-label="Formatting">
      <span class="md-ed-modes" role="tablist" aria-label="Editor view">
        ${MODES.filter((m) => !compact || !m.full).map((m) => html`<button key=${m.key} type="button" data-ui-exempt role="tab"
          class=${`md-ed-mode${mode === m.key ? ' on' : ''}`} aria-selected=${mode === m.key ? 'true' : 'false'}
          data-md-mode=${m.key} onClick=${() => setMode(m.key)}>${m.label}</button>`)}
      </span>
      ${!compact && html`<select class="md-ed-heading" aria-label="Heading" title="Heading" value=${String(level)} disabled=${editingOff}
        onMouseDown=${(event) => event.stopPropagation()}
        onChange=${(event) => {
          const { from, to } = active.selection();
          active.apply(headingEdit(active.doc(), from, to, Number(event.currentTarget.value)));
        }}>
        ${[0, 1, 2, 3].map((n) => html`<option key=${n} value=${String(n)}>${n ? `Heading ${n}` : 'Text'}</option>`)}
      </select>`}
      ${shown.map((tool, i) => (tool === null
        ? html`<span key=${`sep-${i}`} class="md-ed-sep" aria-hidden="true"></span>`
        : html`<button key=${tool.id} type="button" data-ui-exempt class="md-ed-tool" data-md-tool=${tool.id} disabled=${editingOff}
            title=${tool.key ? `${tool.label} (${shortcutLabel(tool.key)})` : tool.label} aria-label=${tool.label}
            onMouseDown=${keep} onClick=${() => runTool(tool.id)}><${Icon} name=${tool.id} /></button>`))}
      ${onPickFiles && html`<span class="md-ed-sep" aria-hidden="true"></span>
        <button type="button" data-ui-exempt class="md-ed-tool" data-md-tool="attach" disabled=${editingOff}
          title="Attach images or files (or paste or drop them here)" aria-label="Attach files" onMouseDown=${keep}
          onClick=${() => fileInput.current?.click()}><${Icon} name="attach" /></button>
        <input ref=${fileInput} type="file" multiple hidden class="md-ed-file-input"
          onChange=${(event) => {
            const names = onPickFiles(event.currentTarget.files) || [];
            event.currentTarget.value = '';
            editorRef?.current?.insertFiles(names);
          }} />`}
      <span class="md-ed-spacer"></span>
      <button type="button" data-ui-exempt class=${`md-ed-tool${help ? ' on' : ''}`} data-md-tool="help" title="Markdown help"
        aria-label="Markdown help" aria-expanded=${help ? 'true' : 'false'} onClick=${() => setHelp(!help)}><${Icon} name="help" /></button>
    </div>
    ${help && html`<div class="md-ed-help" role="note">
      <dl>${HELP.map(([syntax, meaning]) => html`<dt key=${`t-${syntax}`}><code>${syntax}</code></dt><dd key=${`d-${syntax}`}>${meaning}</dd>`)}</dl>
      <p>Ctrl/⌘+B, I, E and K format · Enter continues a list, Tab nests it · Ctrl/⌘+Enter ${onSubmit ? 'sends' : 'saves the form'}</p>
    </div>`}
    <div class="md-ed-panes">
      <div class="md-ed-edit" hidden=${!showEditor}>
        <textarea ref=${(el) => { textarea.current = el; if (textareaRef) textareaRef.current = host ? null : el; }}
          class="md-ed-textarea ui-textarea" aria-label=${label} placeholder=${placeholder} value=${value} disabled=${disabled}
          maxLength=${maxLength || undefined} hidden=${!!host}
          onInput=${(event) => onInput(event.currentTarget.value)}
          onKeyDown=${(event) => {
            if (event.key === 'Enter' && (event.ctrlKey || event.metaKey) && onSubmit) {
              event.preventDefault(); event.stopPropagation(); onSubmit();
              return;
            }
            const tool = !event.altKey && (event.ctrlKey || event.metaKey) && TOOLS.find((t) => t?.key === `Mod-${event.key.toLowerCase()}`);
            if (tool) { event.preventDefault(); runTool(tool.id); }
          }}
          onSelect=${() => setRevision((n) => n + 1)} />
        <div ref=${mount} class="md-ed-cm-mount" hidden=${!host}></div>
      </div>
      ${showPreview && html`<div class="md-ed-preview" aria-label="Preview">
        ${value.trim()
          ? html`<${MarkdownDocument} source=${value} attachments=${attachments} className="markdown-document tb-comment-md md-ed-preview-doc" />`
          : html`<div class="md-ed-preview-empty">Nothing to preview yet.</div>`}
      </div>`}
    </div>
    <div class="md-ed-status">
      <span>Markdown${onPickFiles ? ' · paste or drop images and files' : ''}</span>
      <span>${[hint, count].filter(Boolean).join(' · ')}</span>
    </div>
  </div>`;
}
