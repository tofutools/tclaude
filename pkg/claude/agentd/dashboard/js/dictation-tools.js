// dictation-tools.js -- the floating tools and draft field shared by every
// terminal the operator can talk to: the overlay chat windows
// (companion-island.js) and the Terminals tab (terminal-dictation.js). The
// speech recognition itself lives in companion-voice.js.

import { h } from 'preact';
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { AnnotateOverlay, composeAnnotation } from './companion-annotate.js';
import { COMPANION_MESSAGE_MAX, companionMessageFits, composeCompanionMessage } from './companion-model.js';
import { DEFAULT_SEND_PHRASE, createDictation } from './companion-voice.js';
import { dictationBlock } from './insecure-origin.js';

const html = htm.bind(h);

const joinDraft = (base, heard) => [base.trim(), heard.trim()].filter(Boolean).join(' ');

// postOperatorMessage sends operator mail to an agent and throws what the
// daemon said when it refused.
export async function postOperatorMessage(fetchImpl, { to, subject = '', body }) {
  const response = await fetchImpl('/api/operator-message', {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ to, subject, body }),
  });
  const result = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(result.message || result.error || `${response.status} ${response.statusText}`);
}

// dictationSendPhrase is the configured send phrase, '' when it is off. A
// snapshot from before the setting, or none yet, means the default.
export function dictationSendPhrase(snapshot) {
  return snapshot?.dictation_send_phrase ?? DEFAULT_SEND_PHRASE;
}

export function micTitle(sendPhrase) {
  const send = sendPhrase ? ` (say “${sendPhrase}” at the end to send, or press Enter)` : ' (press Enter to send)';
  return `Dictate${send}. Uses the browser's speech recognition; in Chrome the audio is recognised by Google.`;
}

// useDictation fills a draft from the microphone. A dictation belongs to the
// target it started for: a change of target drops it, and results that land
// after a switch are dropped too. An empty sendPhrase turns spoken send off.
// Stopping with the button hands focus on through onRelease; a recognizer that
// ends by itself leaves it on the draft, where Enter still sends. The
// callbacks read the latest props through a ref.
export function useDictation({
  win, targetId, draft, setDraft, send, notify, sendPhrase = '', onStart = () => {}, onRelease = () => {}, dictationImpl = null,
}) {
  const [listening, setListening] = useState(false);
  const listeningRef = useRef(false);
  const base = useRef('');
  const owner = useRef('');
  const target = useRef('');
  const latest = useRef(null);
  latest.current = { setDraft, send, notify, sendPhrase };
  target.current = targetId || '';
  const own = () => owner.current && owner.current === target.current;
  const dictation = useMemo(() => (dictationImpl || ((callbacks) => createDictation(win, callbacks)))({
    onText: (heard) => { if (own()) latest.current.setDraft(owner.current, joinDraft(base.current, heard)); },
    onState: (on, error) => {
      listeningRef.current = on;
      setListening(on);
      if (error && error !== 'no-speech' && error !== 'aborted') {
        latest.current.notify(error === 'not-allowed' ? 'The browser blocked the microphone for this page.' : `Dictation stopped: ${error}`, true);
      }
    },
    sendPhrase: () => latest.current.sendPhrase,
    onSend: (heard) => {
      if (!own()) return;
      const text = joinDraft(base.current, heard);
      if (!text) return;
      // The draft takes the final words first: a send clears the draft it
      // sent, and only when the message really went out.
      latest.current.setDraft(owner.current, text);
      void latest.current.send(text);
    },
  }), [dictationImpl, win]);
  useEffect(() => () => dictation.abort?.(), [targetId, dictation]);
  return {
    block: dictationBlock(dictation.supported, win),
    listening,
    listeningRef,
    sendPhrase,
    abort: () => dictation.abort?.(),
    toggle() {
      if (listening) {
        dictation.stop();
        onRelease();
        return;
      }
      onStart();
      base.current = draft;
      owner.current = targetId || '';
      dictation.start();
    },
  };
}

// useAnnotation runs the page mark-up for one agent at a time: start(target)
// opens the drawing layer for {agentId, name, fetchImpl}, and overlay renders it.
export function useAnnotation({ win, notify, viewNow }) {
  const [annotating, setAnnotating] = useState(null);
  useEffect(() => {
    win?.document?.body?.classList?.toggle?.('companion-annotating', !!annotating);
    return () => win?.document?.body?.classList?.remove?.('companion-annotating');
  }, [annotating, win]);
  async function sendAnnotation({ note, lines }) {
    const target = annotating;
    if (!target) return;
    const view = viewNow();
    const text = composeAnnotation({ note, lines });
    // Clipped, the agent would miss marks while the operator is told it all
    // went out. The overlay stays open with the strokes intact.
    if (!companionMessageFits(text, view)) {
      notify('That markup is too long to send. Undo some marks or shorten the note, then send again.', true);
      return;
    }
    try {
      await postOperatorMessage(target.fetchImpl, { to: target.agentId, subject: '[annotation]', body: composeCompanionMessage(text, view) });
      setAnnotating(null);
      notify(`Sent your markup to ${target.name}.`);
    } catch (error) {
      notify(`Markup to ${target.name} failed: ${error?.message || error}`, true);
    }
  }
  return {
    start: setAnnotating,
    overlay: annotating && html`<${AnnotateOverlay} agentName=${annotating.name} win=${win}
      onSend=${sendAnnotation} onCancel=${() => setAnnotating(null)} />`,
  };
}

// FloatingTools float over the terminal's top right, taking no room: the
// anchor has no height, and only the cluster itself takes the pointer. Render
// before the terminal in the DOM, because Tab typed into a terminal stays
// there. view and ✎ are for agents, so a plain shell leaves them out.
export function FloatingTools({
  dictation, micLabel, micTitle, withContext = null, onContext = () => {}, annotateLabel = '', onAnnotate = null,
}) {
  const { listening, block, toggle } = dictation;
  return html`<div class=${`companion-floating${listening ? ' listening' : ''}`} role="group" aria-label="Message tools">
      ${!block && withContext !== null && html`<label class="companion-context" title="Attach the dashboard view you are on to dictated messages, so the agent knows what you are looking at">
        <input type="checkbox" checked=${withContext} onChange=${(event) => onContext(event.currentTarget.checked)} /> view
      </label>`}
      <button type="button" class=${`companion-tool companion-mic${listening ? ' on' : ''}`} aria-pressed=${listening ? 'true' : 'false'}
        aria-label=${micLabel} disabled=${!!block}
        title=${block?.why || micTitle} onClick=${toggle}>${listening ? '⏺' : '🎤'}</button>
      ${block && html`<span class="companion-mic-note" title=${block.why}>${block.note}</span>`}
      ${onAnnotate && html`<button type="button" class="companion-tool companion-annotate" aria-label=${annotateLabel}
        title="Draw on the page and send the marked-up view (Ctrl+Shift+.)" onClick=${onAnnotate}>✎</button>`}
  </div>`;
}

// DraftField is where dictated words land, to read and fix before sending. It
// takes the keyboard while the mic is open, so Enter sends what was heard.
// Typing in it stops the mic, so the operator's edit is never overwritten.
export function DraftField({
  dictation, draft, sending, label, submitLabel = 'Send', submitTitle = null, fieldRef = null, onEdit, onSubmit, onFocus, onBlur,
}) {
  const { listening, listeningRef, abort, sendPhrase } = dictation;
  const input = useRef(null);
  useLayoutEffect(() => { if (listening) input.current?.focus(); }, [listening]);
  const bindInput = useCallback((element) => {
    input.current = element;
    fieldRef?.(element);
  }, [fieldRef]);
  // The field is one line: Shift+Enter has no newline to add, and must not send.
  const onKeyDown = (event) => { if (event.key === 'Enter' && event.shiftKey) event.preventDefault(); };
  return html`<form class=${`companion-input${listening ? ' listening' : ''}`} onSubmit=${onSubmit}>
    <input type="text" class="companion-draft" ref=${bindInput} maxLength=${COMPANION_MESSAGE_MAX}
      placeholder=${listening ? 'Listening…' : ''} onFocus=${onFocus} onBlur=${onBlur} onKeyDown=${onKeyDown}
      aria-label=${label} value=${draft} onInput=${(event) => { if (listeningRef.current) abort(); onEdit(event.currentTarget.value); }} />
    ${listening && sendPhrase && html`<span class="companion-send-hint">say “${sendPhrase}” to send</span>`}
    <button type="submit" title=${submitTitle} disabled=${sending || !draft.trim()}>${submitLabel}</button>
  </form>`;
}
