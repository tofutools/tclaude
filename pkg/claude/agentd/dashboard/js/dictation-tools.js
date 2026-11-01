// dictation-tools.js -- the floating tools and draft field shared by every
// terminal the operator can talk to: the companion window
// (companion-island.js) and the Terminals tab (terminal-dictation.js). The
// speech recognition itself lives in companion-voice.js.

import { h } from 'preact';
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { AnnotateOverlay, composeAnnotation, drawInk } from './companion-annotate.js';
import { COMPANION_MESSAGE_MAX, companionMessageFits, composeCompanionMessage, markDictated } from './companion-model.js';
import { createDictation, phraseLeadRest, saysStop, sendPhraseRest } from './companion-voice.js';
import { renderPage } from './dashboard-capture.js';
import { dictationBlock } from './insecure-origin.js';
import { SILENCE_SEND_DEFAULT, voicePhrase } from './voice-config.js';
import { localSpeechState, speechRouteLabel } from './voice-local.js';
import { openStewardDoor, registerWakeTarget, reserveStewardDoor, stewardRefusal } from './voice-wake.js';

const html = htm.bind(h);

const dictationFailure = (error) => (error === 'not-allowed' ? 'The browser blocked the microphone for this page.' : `Dictation stopped: ${error}`);

const TAB_HIDDEN = 'Continuous mode stopped because the tab was hidden.';

const joinDraft = (base, heard) => [base.trim(), heard.trim()].filter(Boolean).join(' ');

// postOperatorMessage sends operator mail to an agent and throws what the
// daemon said when it refused.
export async function postOperatorMessage(fetchImpl, { to, subject = '', body, attachmentToken = '' }) {
  const response = await fetchImpl('/api/operator-message', {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ to, subject, body, ...(attachmentToken ? { attachment_token: attachmentToken } : {}) }),
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
// if that setting is off, which leaves the first message to the send phrase. It stays on this target, and anything but speech ends
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
    const seconds = latest.current.silenceSend || (loop.current && latest.current.continuous ? SILENCE_SEND_DEFAULT : 0);
    if (!seconds) return;
    const started = Date.now();
    silence.current = setInterval(() => {
      if (!own()) {
        clearSilence();
        return;
      }
      const quiet = Date.now() - started;
      if (quiet >= seconds * 1000) {
        const text = latest.current.draft;
        clearSilence();
        if (!text.trim()) return;
        // The recognizer hasn't finished the stop phrase yet, and it never sends.
        if (saysStop(lastHeard.current.interim, latest.current.continuous?.stopPhrase, latest.current.sendPhrase)) {
          placeDraft(joinDraft(base.current, lastHeard.current.settled || ''));
          stopListening();
          return;
        }
        // Nor do the words of a switch to the Steward go to this agent.
        const switching = phraseLeadRest(lastHeard.current.interim, loop.current ? latest.current.continuous?.stewardPhrase : '');
        if (switching !== null) {
          placeDraft(joinDraft(base.current, lastHeard.current.settled || ''));
          dictationRef.current.abort?.();
          switchToSteward(switching);
          return;
        }
        void voiceSend(text, () => dictationRef.current.abort?.());
      } else if (quiet >= 500) {
        setSilenceLeft(Math.ceil(seconds - quiet / 1000));
      }
    }, 250);
  }
  // The one way out of a loop that keeps listening: the operator named the
  // Steward. It ends here, and starts there if the phrase was said in a loop.
  function switchToSteward(heard) {
    const refused = stewardRefusal();
    if (refused) {
      latest.current.notify(refused, true);
      return false;
    }
    const carry = !!loop.current;
    clearSilence();
    reserveStewardDoor();
    endLoop(undefined, true);
    dictationRef.current.abort?.();
    openStewardDoor(heard, carry);
    return true;
  }
  // The draft a caller needs next, ahead of the render that brings it.
  function placeDraft(text) {
    latest.current.setDraft(owner.current, text);
    latest.current.draft = text;
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
  function endLoop(message, keepMic = false) {
    const live = loop.current;
    if (!live) return;
    loop.current = null;
    clearTimeout(live.idle);
    clearTimeout(live.reopen);
    clearSilence();
    setContinuing(false);
    if (!keepMic) dictationRef.current?.keep?.(false);
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
      if (win?.document?.hidden) {
        endLoop(TAB_HIDDEN);
        return;
      }
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
    // A hidden tab shows no indicator, so no loop starts in one.
    if (win?.document?.hidden) {
      endLoop();
      stopMic();
      void latest.current.send(text);
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
      placeDraft(joinDraft(base.current, heard));
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
    stewardPhrase: () => (loop.current ? latest.current.continuous?.stewardPhrase || '' : ''),
    onSteward: switchToSteward,
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
    const onVisibility = () => { if (doc.hidden) endLoop(TAB_HIDDEN); };
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
  opener.current = (heard) => {
    if (block || listeningRef.current || !target.current) return;
    // A wake phrase carries its words here, send phrase and all.
    const words = sendPhraseRest(heard, latest.current.sendPhrase) ?? heard;
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
    // reserve holds the mic through a switch to this target, which handover
    // then starts on, once the target is this one. carry starts a loop here.
    reserve: () => dictation.keep?.(true),
    release: () => dictation.keep?.(false),
    handover(words, carry) {
      const looping = carry && latest.current.continuous && !block && target.current;
      if (looping) beginLoop();
      opener.current(words);
      if (!looping) dictation.keep?.(false);
    },
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

// uploadMarkupImage stages the image as a message attachment and returns its
// token.
async function uploadMarkupImage(fetchImpl, image) {
  const form = new FormData();
  form.append('file', image, 'markup.png');
  const response = await fetchImpl('/api/spawn-attachments', { method: 'POST', credentials: 'same-origin', body: form });
  const result = await response.json().catch(() => ({}));
  if (!response.ok || !result.token) throw new Error(result.message || result.error || `${response.status} ${response.statusText}`);
  return result.token;
}

// useAnnotation runs the page mark-up for one agent at a time: start(target)
// opens the drawing layer for {agentId, name, fetchImpl, remote}, and overlay
// renders it. The operator's Send is what lets this one image of the view go,
// so it takes no capture grant, and it is rendered as a capture is, secrets
// masked. Strokes go as an image only to this node's agents: a peer's operator
// mail is text. Without an image, the lines still go and the notice says so.
export function useAnnotation({ win, notify, viewNow, render = renderPage }) {
  const [annotating, setAnnotating] = useState(null);
  useEffect(() => {
    win?.document?.body?.classList?.toggle?.('companion-annotating', !!annotating);
    return () => win?.document?.body?.classList?.remove?.('companion-annotating');
  }, [annotating, win]);
  async function sendAnnotation({ note, lines, strokes = [] }) {
    const target = annotating;
    if (!target) return;
    const view = viewNow();
    let image = null;
    let imageRefused = target.remote && strokes.length ? 'an agent on another node gets no image' : '';
    if (strokes.length && !target.remote) {
      try {
        image = await render({ win, fetchImpl: globalThis.fetch.bind(globalThis), draw: (ctx, scale) => drawInk(ctx, strokes, scale) });
      } catch (error) {
        imageRefused = `the image failed: ${error?.message || error}`;
      }
    }
    const text = composeAnnotation({ note, lines, image: !!image });
    // Clipped, the agent would miss marks while the operator is told it all
    // went out. The overlay stays open with the strokes intact.
    if (!companionMessageFits(text, view)) {
      notify('That markup is too long to send. Undo some marks or shorten the note, then send again.', true);
      return;
    }
    try {
      let attachmentToken = '';
      let body = composeCompanionMessage(text, view);
      if (image) {
        try {
          attachmentToken = await uploadMarkupImage(target.fetchImpl, image);
        } catch (error) {
          imageRefused = `the image failed: ${error?.message || error}`;
          body = composeCompanionMessage(composeAnnotation({ note, lines }), view);
        }
      }
      const post = (message, token) => postOperatorMessage(target.fetchImpl, { to: target.agentId, subject: '[annotation]', body: message, attachmentToken: token });
      try {
        await post(body, attachmentToken);
      } catch (error) {
        // An agent away from its home node takes less than the upload did.
        if (!attachmentToken) throw error;
        imageRefused = `the image failed: ${error?.message || error}`;
        await post(composeCompanionMessage(composeAnnotation({ note, lines }), view), '');
      }
      setAnnotating(null);
      notify(imageRefused ? `Sent your markup to ${target.name} as text: ${imageRefused}.` : `Sent your markup to ${target.name}.`, !!imageRefused);
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
  dictation, micLabel, micTitle, withContext = null, onContext = () => {}, contextNote = '', annotateLabel = '', onAnnotate = null, children = null,
}) {
  const { listening, block, toggle, continuing } = dictation;
  return html`<div class=${`companion-floating${listening ? ' listening' : ''}`} role="group" aria-label="Message tools">
      ${!block && withContext !== null && html`<label class="companion-context" title="Attach the dashboard view you are on to dictated messages, so the agent knows what you are looking at">
        <input type="checkbox" checked=${withContext} onChange=${(event) => onContext(event.currentTarget.checked)} /> view
        ${contextNote && html`<span class="companion-context-off" title=${contextNote}>capture off</span>`}
      </label>`}
      <button type="button" class=${`companion-tool companion-mic${listening ? ' on' : ''}${continuing ? ' steady' : ''}`} aria-pressed=${listening ? 'true' : 'false'}
        aria-label=${micLabel} disabled=${!!block}
        title=${block?.why || micTitle} onClick=${toggle}>${listening ? '⏺' : '🎤'}</button>
      ${onAnnotate && html`<button type="button" class="companion-tool companion-annotate" aria-label=${annotateLabel}
        title="Draw on the page and send the marked-up view (Ctrl+Shift+.)" onClick=${onAnnotate}>✎</button>`}
      ${children}
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
