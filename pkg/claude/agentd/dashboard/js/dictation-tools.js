// dictation-tools.js -- the floating tools and draft field shared by every
// terminal the operator can talk to: the companion window
// (companion-island.js) and the Terminals tab (terminal-dictation.js). The
// speech recognition itself lives in companion-voice.js.

import { h } from 'preact';
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { AnnotateOverlay, composeAnnotation } from './companion-annotate.js';
import { COMPANION_MESSAGE_MAX, companionMessageFits, composeCompanionMessage, markDictated } from './companion-model.js';
import { createDictation, isWholePhrase } from './companion-voice.js';
import { dictationBlock } from './insecure-origin.js';
import { SILENCE_SEND_DEFAULT, voicePhrase } from './voice-config.js';
import { localSpeechState, speechRouteLabel } from './voice-local.js';
import { registerWakeTarget } from './voice-wake.js';

const html = htm.bind(h);

const dictationFailure = (error) => (error === 'not-allowed' ? 'The browser blocked the microphone for this page.' : `Dictation stopped: ${error}`);

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

// dictationSendPhrase is the configured send phrase, '' when it is off.
export function dictationSendPhrase(snapshot) {
  return voicePhrase(snapshot, 'send');
}

export function micTitle(sendPhrase) {
  const send = sendPhrase ? ` (say “${sendPhrase}” at the end to send, or press Enter)` : ' (press Enter to send)';
  return `Dictate${send}. Uses the browser's speech recognition; in Chrome the audio is recognised by Google.`;
}

// useDictation fills a draft from the microphone. A dictation belongs to the
// target it started for: a change of target drops it, and results that land
// after a switch are dropped too. An empty sendPhrase turns spoken send off.
// Stopping with the button hands focus on through onRelease; a recognizer that
// ends by itself leaves it on the draft, where Enter still sends. silenceSend
// sends after that many seconds with no new speech, counting down where the
// draft shows it; 0 is off. pauses are the {comma, period} seconds that
// punctuate a dictation, null for none. outgoing(text) is a message as it should go to an
// agent: with mark on, a draft holding dictated words gets the marker, however
// the operator edited it since, until it is emptied. Typed drafts don't, and
// the marker stays out of the draft. continuous is continuous mode's
// {stopPhrase, idleSeconds, held}, null where it can't run: after a voice send
// the mic reopens for the next message, each sent on silence, 3 seconds of it
// if that setting is off. It stays on this target, and anything but speech ends
// it: a change of target, a hidden tab, Esc, Stop, typing, or a failed send.
// held() names a reason not to send, which also ends it. wakeHost offers this target to the wake listener:
// it returns the target's element while it can dictate, else null. The
// callbacks read the latest props through a ref.
export function useDictation({
  win, targetId, draft, setDraft, send, notify, sendPhrase = '', silenceSend = 0, pauses = null, mark = false, continuous = null, wakeHost = () => null,
  onStart = () => {}, onRelease = () => {}, dictationImpl = null,
}) {
  const [listening, setListening] = useState(false);
  const listeningRef = useRef(false);
  const base = useRef('');
  const owner = useRef('');
  const target = useRef('');
  const latest = useRef(null);
  latest.current = { draft, setDraft, send, notify, sendPhrase, silenceSend, pauses, continuous, wakeHost, onStart, onRelease };
  const [local, setLocal] = useState(false);
  const [silenceLeft, setSilenceLeft] = useState(0);
  const silence = useRef(null);
  const [continuing, setContinuing] = useState(false);
  const loop = useRef(null);
  const lastHeard = useRef({});
  function clearSilence() {
    if (silence.current) clearInterval(silence.current);
    silence.current = null;
    setSilenceLeft(0);
  }
  // The countdown shows after a moment's quiet, not between every two words.
  function armSilence() {
    clearSilence();
    const seconds = latest.current.silenceSend || (latest.current.continuous ? SILENCE_SEND_DEFAULT : 0);
    if (!seconds) return;
    const started = Date.now();
    silence.current = setInterval(() => {
      const quiet = Date.now() - started;
      if (quiet >= seconds * 1000) {
        const text = latest.current.draft;
        clearSilence();
        if (!text.trim()) return;
        // The recognizer hasn't finished the stop phrase yet, and it never sends.
        if (isWholePhrase(lastHeard.current.interim, latest.current.continuous?.stopPhrase)) {
          latest.current.setDraft(owner.current, joinDraft(base.current, lastHeard.current.settled || ''));
          stopListening();
          return;
        }
        void voiceSend(text, () => dictationRef.current.abort?.());
      } else if (quiet >= 500) {
        setSilenceLeft(Math.ceil(seconds - quiet / 1000));
      }
    }, 250);
  }
  function armIdle() {
    const live = loop.current;
    if (!live) return;
    clearTimeout(live.idle);
    const seconds = latest.current.continuous?.idleSeconds || 0;
    if (seconds) live.idle = setTimeout(() => endLoop('Continuous mode stopped after a long silence.'), seconds * 1000);
  }
  function beginLoop() {
    if (!loop.current) {
      loop.current = { sending: false, sent: false, heard: false, quick: 0, openedAt: 0, idle: null, reopen: null };
      setContinuing(true);
      dictationRef.current.keep?.(true);
      armIdle();
    }
    return loop.current;
  }
  function endLoop(message) {
    const live = loop.current;
    if (!live) return;
    loop.current = null;
    clearTimeout(live.idle);
    clearTimeout(live.reopen);
    setContinuing(false);
    dictationRef.current?.keep?.(false);
    dictationRef.current?.abort?.();
    listeningRef.current = false;
    setListening(false);
    if (message) latest.current.notify(message, true);
  }
  function stopListening() {
    const looping = !!loop.current;
    clearSilence();
    endLoop();
    dictationRef.current?.abort?.();
    if (looping) latest.current.onRelease();
  }
  // A short gap, so an ended recognizer has let go of the mic.
  function reopen() {
    const live = loop.current;
    if (!live) return;
    if (!latest.current.continuous) {
      endLoop();
      return;
    }
    const from = live.sent ? '' : latest.current.draft;
    live.sent = false;
    live.reopen = setTimeout(() => {
      if (loop.current !== live) return;
      live.openedAt = Date.now();
      live.heard = false;
      opener.open(from, { strictRoute: true });
    }, 200);
  }
  // A recognizer ends by itself after a quiet spell: start the next, unless it
  // keeps ending at once.
  function recognizerEnded(live, error) {
    if (error && error !== 'no-speech' && error !== 'aborted') {
      endLoop(dictationFailure(error));
      return;
    }
    if (live.sending || dictationRef.current.recognizing) return;
    live.quick = Date.now() - live.openedAt < 1000 && !live.heard ? live.quick + 1 : 0;
    if (live.quick >= 5) endLoop('Continuous mode stopped: speech recognition keeps ending.');
    else reopen();
  }
  async function voiceSend(text, stopMic = () => {}) {
    const mode = latest.current.continuous;
    if (!mode) {
      stopMic();
      void latest.current.send(text);
      return;
    }
    const held = mode.held?.();
    if (held) {
      endLoop();
      stopMic();
      latest.current.notify(held, true);
      return;
    }
    const live = beginLoop();
    live.sending = true;
    clearSilence();
    stopMic();
    const sent = await latest.current.send(text);
    if (loop.current !== live) return;
    live.sending = false;
    if (sent === false) {
      endLoop();
      return;
    }
    live.sent = true;
    if (!dictationRef.current.recognizing) reopen();
  }
  const dictationRef = useRef(null);
  target.current = targetId || '';
  const dictatedDrafts = useRef(new Set());
  if (!draft.trim()) dictatedDrafts.current.delete(target.current);
  const own = () => owner.current && owner.current === target.current;
  const dictation = useMemo(() => (dictationImpl || ((callbacks) => createDictation(win, callbacks)))({
    onText: (heard, parts = {}) => {
      if (!own()) return;
      lastHeard.current = parts;
      if (heard.trim()) dictatedDrafts.current.add(owner.current);
      latest.current.setDraft(owner.current, joinDraft(base.current, heard));
      if (!heard.trim()) return;
      armSilence();
      if (loop.current) {
        loop.current.heard = true;
        armIdle();
      }
    },
    onRoute: (onDevice, fellBack) => {
      setLocal(onDevice);
      if (fellBack) latest.current.notify(`On-device speech isn’t working, so this dictation uses ${speechRouteLabel(false, win)}.`, true);
    },
    onState: (on, error) => {
      if (!on && loop.current) {
        recognizerEnded(loop.current, error);
        return;
      }
      listeningRef.current = on;
      setListening(on);
      if (!on) clearSilence();
      if (error && error !== 'no-speech' && error !== 'aborted') {
        latest.current.notify(dictationFailure(error), true);
      }
    },
    sendPhrase: () => latest.current.sendPhrase,
    pauses: () => latest.current.pauses,
    stopPhrase: () => latest.current.continuous?.stopPhrase || '',
    onStopPhrase: stopListening,
    onTaken: () => endLoop(),
    onSend: (heard) => {
      if (!own()) return;
      const text = joinDraft(base.current, heard);
      if (!text) return;
      if (heard.trim()) dictatedDrafts.current.add(owner.current);
      // The draft takes the final words first: a send clears the draft it
      // sent, and only when the message really went out.
      latest.current.setDraft(owner.current, text);
      void voiceSend(text);
    },
  }), [dictationImpl, win]);
  dictationRef.current = dictation;
  useEffect(() => { void localSpeechState(win); }, [win]);
  useEffect(() => () => {
    clearSilence();
    endLoop();
    dictation.abort?.();
  }, [targetId, dictation]);
  useEffect(() => {
    const doc = win?.document;
    if (!continuing || !doc) return undefined;
    const onKey = (event) => {
      if (event.key !== 'Escape') return;
      event.preventDefault();
      event.stopPropagation();
      stopListening();
    };
    const onVisibility = () => { if (doc.hidden) endLoop('Continuous mode stopped because the tab was hidden.'); };
    doc.addEventListener('keydown', onKey, true);
    doc.addEventListener('visibilitychange', onVisibility);
    return () => {
      doc.removeEventListener('keydown', onKey, true);
      doc.removeEventListener('visibilitychange', onVisibility);
    };
  }, [continuing, win]);
  const block = dictationBlock(dictation.supported, win);
  const opener = useRef(null);
  opener.open = (from, options) => {
    latest.current.onStart();
    lastHeard.current = {};
    base.current = from;
    owner.current = target.current;
    dictation.start(options);
  };
  opener.current = (words) => {
    if (block || listeningRef.current || !target.current) return;
    const text = joinDraft(latest.current.draft, words);
    if (words) {
      dictatedDrafts.current.add(target.current);
      latest.current.setDraft(target.current, text);
    }
    // Nobody clicked, so this one mustn't fall back to a speech service.
    opener.open(text, { strictRoute: true });
    if (words) armSilence();
  };
  useEffect(() => registerWakeTarget({
    host: () => latest.current.wakeHost(),
    start: (words) => opener.current(words),
  }), []);
  return {
    block,
    listening,
    listeningRef,
    sendPhrase,
    route: speechRouteLabel(local, win),
    silenceLeft,
    continuing,
    stopPhrase: continuous?.stopPhrase || '',
    outgoing: (text) => (mark && dictatedDrafts.current.has(target.current) ? markDictated(text) : text),
    // Cancelling is final: words still arriving must not start it again.
    cancelSilence: stopListening,
    abort: () => {
      endLoop();
      dictation.abort?.();
    },
    toggle() {
      if (loop.current) {
        stopListening();
        return;
      }
      if (listening) {
        dictation.stop();
        onRelease();
        return;
      }
      opener.open(draft);
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
  const { listening, block, toggle, continuing } = dictation;
  return html`<div class=${`companion-floating${listening ? ' listening' : ''}`} role="group" aria-label="Message tools">
      ${!block && withContext !== null && html`<label class="companion-context" title="Attach the dashboard view you are on to dictated messages, so the agent knows what you are looking at">
        <input type="checkbox" checked=${withContext} onChange=${(event) => onContext(event.currentTarget.checked)} /> view
      </label>`}
      <button type="button" class=${`companion-tool companion-mic${listening ? ' on' : ''}${continuing ? ' steady' : ''}`} aria-pressed=${listening ? 'true' : 'false'}
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
  const { listening, listeningRef, abort, sendPhrase, route, silenceLeft, cancelSilence, continuing, stopPhrase } = dictation;
  const input = useRef(null);
  useLayoutEffect(() => { if (listening) input.current?.focus(); }, [listening]);
  const bindInput = useCallback((element) => {
    input.current = element;
    fieldRef?.(element);
  }, [fieldRef]);
  // The field is one line: Shift+Enter has no newline to add, and must not send.
  const onKeyDown = (event) => {
    if (event.key === 'Enter' && event.shiftKey) event.preventDefault();
    if (event.key === 'Escape' && silenceLeft) {
      event.preventDefault();
      event.stopPropagation();
      cancelSilence();
    }
  };
  return html`<form class=${`companion-input${listening ? ' listening' : ''}`} onSubmit=${onSubmit}>
    <input type="text" class="companion-draft" ref=${bindInput} maxLength=${COMPANION_MESSAGE_MAX}
      placeholder=${listening ? 'Listening…' : ''} onFocus=${onFocus} onBlur=${onBlur} onKeyDown=${onKeyDown}
      aria-label=${label} value=${draft} onInput=${(event) => { if (listeningRef.current) abort(); onEdit(event.currentTarget.value); }} />
    ${continuing ? html`<span class="companion-send-hint companion-continuous" role="status">⏺ ${silenceLeft ? `Sending in ${silenceLeft}s` : 'Continuous mode'}${stopPhrase ? ` · say “${stopPhrase}” to stop` : ''} · ${route}</span>
      <button type="button" class="companion-silence-cancel" title="Stop continuous mode and keep the draft (Esc)" onClick=${cancelSilence}>Stop</button>`
      : silenceLeft ? html`<span class="companion-send-hint companion-silence" role="status">Sending in ${silenceLeft}s</span>
      <button type="button" class="companion-silence-cancel" title="Stop the mic and keep the draft (Esc)" onClick=${cancelSilence}>Cancel</button>`
      : listening && html`<span class="companion-send-hint">${sendPhrase ? `say “${sendPhrase}” to send · ` : ''}${route}</span>`}
    <button type="submit" title=${submitTitle} disabled=${sending || !draft.trim()}>${submitLabel}</button>
  </form>`;
}
