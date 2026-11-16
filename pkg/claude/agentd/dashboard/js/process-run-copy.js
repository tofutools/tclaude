// process-run-copy.js -- a running run's own copy of its template in the
// graph editor (tk_6cf0cd94). The steps the run has reached are locked and
// marked: they stay as they are, except that one still being worked on may be
// rerouted (a step inserted after it). Saving does not make a template
// version: it applies the change to this run, or proposes it to the operator
// (POST /v1/process/runs/{id}/amendments with the editor's model).

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';

const html = htm.bind(h);

// authoredRoot is the authored step a prepared node id belongs to.
export const authoredRoot = (id) => String(id || '').split(/[./]/)[0];

// runCopyConfig is the editor config for a run's copy: what is locked, how
// locked steps are marked, and the save that amends the run.
export function runCopyConfig({ runId, reached = [], reroutable = [], direct = false, send, onClose, onSaved }) {
  const locked = new Set(reached);
  const reroute = new Set(reroutable);
  const isLocked = (id) => locked.has(authoredRoot(id));
  return {
    mode: 'run',
    nodeEditable: (id) => !isLocked(id),
    // The start stays where it is; a reached step's ways out stay, unless it
    // may still be rerouted.
    edgeEditable: (edge) => !!edge?.from && (!isLocked(edge.from) || reroute.has(authoredRoot(edge.from))),
    canInsert: true,
    decorateNode: (node) => {
      if (!isLocked(node.id)) return;
      node.locked = true;
      node.stateOverlay = {
        glyph: '🔒',
        label: reroute.has(authoredRoot(node.id)) ? 'reached · can reroute' : 'reached',
      };
    },
    saveAction: (body) => send(body),
    onSaved,
    runCopy: {
      label: direct ? 'Apply to this run' : 'Propose to the operator',
      title: direct
        ? 'Apply this change to this run only; the template stays as it is'
        : 'Send this change to the operator, who applies or rejects it',
      badge: `run copy · ${runId}`,
      hint: 'This run\'s own copy of its template. Steps the run has reached are locked.',
      onClose,
    },
  };
}

// editViewForRunCopy is the editor's view of the copy: never a saved
// version, so no CAS base and no head to watch.
export function editViewForRunCopy(editView) {
  return { ...editView, sourceHash: '', semanticHash: '', currentRef: '', diagnostics: [] };
}

// RunCopyEditor is the full-screen editor over the run view.
export function RunCopyEditor({ runId, api, onChanged, onClose, onYaml, openEditor = null }) {
  const mountRef = useRef(null);
  const noteRef = useRef('');
  const [note, setNote] = useState('');
  const [error, setError] = useState('');
  const [info, setInfo] = useState(null);
  noteRef.current = note;
  useEffect(() => {
    let live = true;
    let editor = null;
    (async () => {
      try {
        const got = await api.runTemplate(runId);
        if (!live) return;
        if (!got.editView?.template) throw new Error('This run\'s template cannot be drawn; edit it as YAML instead.');
        setInfo({ reached: got.reached || [], direct: !!got.canAmendDirectly });
        const open = openEditor || (await import('./process-editor.js')).openTemplateEditor;
        if (!live || !mountRef.current) return;
        editor = await open(mountRef.current, {
          id: got.editView.template.id,
          view: editViewForRunCopy(got.editView),
          config: runCopyConfig({
            runId, reached: got.reached, reroutable: got.reroutable, direct: !!got.canAmendDirectly,
            send: async (body) => {
              const { sourceHash, ...edit } = body;
              const result = await api.amendRun(runId, { edit, note: noteRef.current.trim() });
              const amendment = result?.amendment;
              return { status: amendment?.state === 'applied' ? 'Applied to this run.' : 'Proposed to the operator.', amendment };
            },
            onClose: () => onClose?.(),
            onSaved: () => { onChanged?.(); onClose?.(); },
          }),
        });
        if (!live) editor?.destroy?.();
      } catch (cause) {
        if (live) setError(cause?.message || String(cause));
      }
    })();
    return () => { live = false; editor?.destroy?.(); };
  }, [runId]);
  return html`<div class="prc-overlay" role="dialog" aria-label=${`Amend run ${runId}`} data-run-copy-editor>
    <div class="prc-bar">
      <span class="prc-title">✎ Amending <b>${runId}</b>: only this run changes; the template stays as it is.</span>
      ${info && html`<span class="prc-legend grf-muted">🔒 ${info.reached.length} reached step${info.reached.length === 1 ? '' : 's'} locked</span>`}
      <input class="pov-note prc-note" type="text" maxlength="4096" aria-label="Why (optional)"
        placeholder=${info?.direct === false ? 'why (shown to the operator and in the log)' : 'why (optional, recorded in the log)'}
        value=${note} onInput=${(event) => setNote(event.currentTarget.value)} />
      ${onYaml && html`<${Button} size="sm" data-run-copy-yaml onClick=${() => onYaml()}>Edit as YAML instead<//>`}
    </div>
    ${error && html`<div class="grf-error ui-error" role="alert">${error}</div>`}
    <div class="prc-mount" ref=${mountRef}></div>
  </div>`;
}
