// companion-voice.js -- voice output for the companion (tcl-q3qcfw): pinned
// agents with voice on speak their reply aloud when a turn ends, and say when
// they need the operator. Text stays canonical; this only reads it out, using
// the browser's own speech synthesis (local, nothing to install).
//
// A reply is read from GET /api/companion/{agent}/speech?since=<cursor>: the
// agent's text since the last read, from its own transcript. The cursor is
// the server's clock from the previous read, so nothing is spoken twice and
// nothing from before voice was switched on is spoken at all.

export const VOICE_PREF = 'tclaude.dash.companion.voice';
export const VOICE_SPOKEN_MAX = 900;
// A far-future cursor: the first read returns no items, only the server's now.
const PRIME_CURSOR = '9000000000000000000';

// speakableText turns an agent's markdown reply into something worth hearing:
// code blocks and tables are summarised, links and markup reduced to their
// words, and a long reply cut at a sentence with a pointer to the window.
export function speakableText(markdown, max = VOICE_SPOKEN_MAX) {
  let text = String(markdown || '').replace(/\r\n?/g, '\n');
  text = text.replace(/```[\s\S]*?(```|$)/g, '\n\n(code omitted)\n\n');
  text = text.split('\n')
    .filter((line) => !/^\s*\|.*\|\s*$/.test(line) && !/^\s*([-*_]\s*){3,}$/.test(line))
    .map((line) => line.replace(/^\s*>\s?/, ''))
    .join('\n');
  text = text.replace(/!\[([^\]]*)\]\([^)]*\)/g, '$1');
  text = text.replace(/\[([^\]]+)\]\([^)]*\)/g, '$1');
  text = text.replace(/https?:\/\/\S+/g, 'a link');
  text = text.replace(/`([^`]*)`/g, '$1');
  // A heading is a sentence of its own; so is each list item.
  text = text.replace(/^\s{0,3}#{1,6}\s+(.*)$/gm, '$1\n');
  text = text.replace(/^\s*(?:[-*+]|\d+[.)])\s+/gm, '\u0001');
  // Emphasis only at word boundaries, so snake_case and paths survive.
  text = text.replace(/(^|[^\w*])(\*\*|\*|__|_|~~)(?=\S)(.+?)(?<=\S)\2(?![\w*])/g, '$1$3');
  // Blank lines and list items break sentences; a single newline inside a
  // paragraph (hard-wrapped prose) is a space.
  const sentences = [];
  for (const para of text.split(/\n\s*\n/)) {
    let current = '';
    for (const raw of para.split('\n')) {
      const item = raw.startsWith('\u0001');
      const line = raw.replace(/\u0001/g, '').trim();
      if (!line) continue;
      if (item && current) {
        sentences.push(current);
        current = line;
      } else {
        current = current ? `${current} ${line}` : line;
      }
    }
    if (current) sentences.push(current);
  }
  text = sentences.reduce((acc, part) => (!acc ? part : /[.!?:;,]$/.test(acc) ? `${acc} ${part}` : `${acc}. ${part}`), '');
  text = text.replace(/\s+/g, ' ').trim();
  if (text.length <= max) return text;
  const cut = text.slice(0, max);
  const end = Math.max(cut.lastIndexOf('. '), cut.lastIndexOf('? '), cut.lastIndexOf('! '));
  return `${end > max / 3 ? cut.slice(0, end + 1) : cut} The rest is in the window.`;
}

export function parseVoiceAgents(raw) {
  try {
    const list = JSON.parse(raw || '[]');
    return Array.isArray(list) ? [...new Set(list.filter((id) => /^agt_[0-9a-f]{32}$/.test(id)))] : [];
  } catch {
    return [];
  }
}

export function localVoice(synth, language = 'en') {
  const voices = (synth?.getVoices?.() || []).filter((v) => v.localService);
  const lang = String(language || 'en').toLowerCase().slice(0, 2);
  const matches = (v) => String(v.lang || '').toLowerCase().startsWith(lang);
  const preferred = voices.find((v) => v.default);
  if (preferred && matches(preferred)) return preferred;
  return voices.find(matches) || voices[0] || null;
}

// ---- who holds the microphone ----
//
// A page has one microphone to give. A dictation takes it from the wake
// listener, and speech playing keeps the listener from hearing itself.

const micListeners = new Set();
let speechCount = 0;
let wakeHold = null;
const micChanged = () => { for (const listener of micListeners) listener(); };
const speechStarted = () => { speechCount += 1; micChanged(); };
const speechFinished = () => { speechCount = Math.max(0, speechCount - 1); micChanged(); };
const speechStopped = () => { speechCount = 0; micChanged(); };

// setLocalSpeech records that the browser can recognise on this device; a
// dictation then uses it (voice-local.js).
let localSpeech = false;
export function setLocalSpeech(available) {
  localSpeech = available;
}

// onMicChange calls listener whenever a dictation or speech starts or ends,
// and returns how to stop.
export function onMicChange(listener) {
  micListeners.add(listener);
  return () => micListeners.delete(listener);
}

export function speechPlaying() {
  return speechCount > 0;
}

// holdMicForWake registers the wake listener; holder.release(then) gives the
// mic up and calls then once it is free.
export function holdMicForWake(holder) {
  wakeHold = holder;
  return () => { if (wakeHold === holder) wakeHold = null; };
}

// createSpeaker wraps window.speechSynthesis. Utterances queue in order;
// cancel() stops speech at once (barge-in).
export function createSpeaker(win = globalThis.window, { rate = 1.05, onBlocked = () => {} } = {}) {
  const synth = win?.speechSynthesis;
  const Utterance = win?.SpeechSynthesisUtterance;
  const supported = !!(synth && Utterance);
  return {
    supported,
    speak(text) {
      if (!supported || !text) return false;
      const utterance = new Utterance(text);
      utterance.rate = rate;
      // Prefer a voice that runs on this machine: some browsers' network
      // voices send the text to a cloud service.
      const voice = localVoice(synth, win?.navigator?.language);
      if (voice) utterance.voice = voice;
      // Browsers refuse speech until the page has had a click or key press.
      let playing = true;
      speechStarted();
      const finished = () => {
        if (playing) speechFinished();
        playing = false;
      };
      utterance.onend = finished;
      utterance.onerror = (event) => {
        finished();
        if (event?.error === 'not-allowed') onBlocked();
      };
      synth.speak(utterance);
      return true;
    },
    cancel() {
      if (supported) synth.cancel();
      speechStopped();
    },
  };
}

// voiceTransitions compares agents' companion statuses with the previous
// ones and returns what to say: a turn that ended ('reply') or an agent that
// started waiting on the operator ('needs').
export function voiceTransitions(prev, next) {
  const out = [];
  for (const [agentId, status] of next) {
    const before = prev.get(agentId);
    if (before === undefined || before === status) continue;
    if (status === 'needs') out.push({ agentId, kind: 'needs' });
    else if (before === 'working' && status === 'idle') out.push({ agentId, kind: 'reply' });
  }
  return out;
}

// decisionArrivals compares each agent's newest open decision id with the
// previous one and returns the agents that asked another decision since: a
// question put while the agent already waits on the operator changes no
// status, so voiceTransitions alone would let it pass unspoken. The first
// sight of an agent is silent.
export function decisionArrivals(prev, next) {
  const out = [];
  for (const [agentId, id] of next) {
    const before = prev.get(agentId);
    if (before !== undefined && id > before) out.push(agentId);
  }
  return out;
}

// createVoiceReader keeps one cursor per agent and fetches what it said
// since. Reads for one agent run one at a time, so a reply is never handed
// out twice; the cursor moves only to the newest text actually returned (a
// read that found nothing, or a transcript not readable yet, leaves it), and
// a forgotten agent's late answer is dropped.
export function createVoiceReader(fetchImpl) {
  const cursors = new Map();
  const chains = new Map();
  const generations = new Map();
  function serial(agentId, task) {
    const run = (chains.get(agentId) || Promise.resolve()).then(task, task);
    chains.set(agentId, run.catch(() => {}));
    return run;
  }
  async function read(agentId, since) {
    const generation = generations.get(agentId) || 0;
    const response = await fetchImpl(`/api/companion/${encodeURIComponent(agentId)}/speech?since=${since}`, { credentials: 'same-origin' });
    if (!response.ok) throw new Error(`${response.status}`);
    const body = await response.json();
    if ((generations.get(agentId) || 0) !== generation) return null;
    return body;
  }
  const prime = (agentId) => serial(agentId, async () => {
    if (cursors.has(agentId)) return;
    const body = await read(agentId, PRIME_CURSOR);
    if (body?.now) cursors.set(agentId, String(body.now));
  });
  return {
    // prime starts an agent's cursor at the server's now.
    prime,
    forget(agentId) {
      cursors.delete(agentId);
      generations.set(agentId, (generations.get(agentId) || 0) + 1);
    },
    // latest returns {text, at} for the agent's last text since the cursor,
    // or null when there is none.
    async latest(agentId) {
      await prime(agentId);
      return serial(agentId, async () => {
        if (!cursors.has(agentId)) return null;
        const body = await read(agentId, cursors.get(agentId));
        const items = Array.isArray(body?.items) ? body.items.filter((it) => it?.text && it.at_ns) : [];
        if (!items.length) return null;
        const last = items[items.length - 1];
        cursors.set(agentId, String(last.at_ns));
        return { text: last.text, at: String(last.at_ns) };
      });
    },
  };
}

// ---- voice input (tcl-sqxkb0) ----

export function speechRecognitionCtor(win = globalThis.window) {
  return win?.SpeechRecognition || win?.webkitSpeechRecognition || null;
}

// sendPhraseRest looks for the spoken send phrase at the very end of one
// finished phrase and returns the words before it, or null when it isn't
// there. No pause is needed before it, because the recognizer only finishes a
// phrase at a long one, so a common word would fire mid-sentence: hence a
// distinctive phrase, and only at the end. Case and the punctuation the
// recognizer adds don't count. An empty send phrase never matches.
export function sendPhraseRest(phrase, sendPhrase) {
  const pattern = phrasePattern(sendPhrase);
  if (!pattern) return null;
  const match = new RegExp(`(?:^|[\\s\\p{P}])${pattern}[\\s\\p{P}]*$`, 'iu').exec(String(phrase || ''));
  return match ? phrase.slice(0, match.index).replace(/[\s,;:]+$/, '') : null;
}

// phrasePattern is a spoken phrase as a regular expression source, or '' for
// an empty phrase: its words as text, whatever punctuation lies between them.
export function phrasePattern(phrase) {
  const words = String(phrase || '').split(/\s+/)
    .map((word) => word.replace(/^\p{P}+|\p{P}+$/gu, '').replace(/[.*+?^${}()|[\]\\/]/g, '\\$&'))
    .filter(Boolean);
  return words.join('[\\s\\p{P}]+');
}

// Errors that say nothing against on-device speech.
const NOT_LOCAL_FAULTS = new Set(['no-speech', 'aborted', 'not-allowed', 'service-not-allowed']);

// A page has one microphone to give: the dictation that holds it, whichever
// terminal it belongs to.
let openDictation = null;

// dictationOpen reports whether any dictation on this page is listening, so
// nothing is spoken into the open mic.
export function dictationOpen() {
  return !!openDictation?.active;
}

// createDictation wraps the browser's speech recognition: interim and final
// text arrive through onText (the whole utterance so far), onState reports
// listening on/off, and onSend fires when a finished phrase ends with the send
// phrase, which sendPhrase() returns ('' for none). Interim text never sends:
// it is still changing, so "send now please" would fire early. Starting one
// dictation drops any other on the page. stop() still delivers the words
// already heard but never sends; abort() drops them. Chrome's recognizer runs
// in Google's cloud unless on-device speech is installed; the listening state
// says which. If on-device fails before any words, the dictation falls back
// and onRoute says so: it is a click and a short burst, unlike the wake
// listener. start({strictRoute: true}), for a dictation nobody clicked, ends
// with the error instead.
export function createDictation(win = globalThis.window, { onText, onState, onSend, onRoute, sendPhrase = () => '', lang } = {}) {
  const Ctor = speechRecognitionCtor(win);
  let recognition = null;
  let stopped = false;
  let opening = false;
  let strictRoute = false;
  const dictation = {
    supported: !!Ctor,
    get active() { return !!recognition || opening; },
    start(options = {}) {
      if (!Ctor || recognition || opening) return;
      strictRoute = !!options.strictRoute;
      if (openDictation !== dictation) openDictation?.abort();
      openDictation = dictation;
      opening = true;
      if (wakeHold) wakeHold.release(begin);
      else begin();
    },
    stop() {
      stopped = true;
      recognition?.stop();
    },
    abort() {
      opening = false;
      const r = recognition;
      recognition = null;
      r?.abort?.();
      if (r) onState?.(false);
      micChanged();
    },
  };
  function begin() {
    if (!opening) return;
    opening = false;
    open(localSpeech);
  }
  function open(local) {
    const r = new Ctor();
    r.continuous = true;
    r.interimResults = true;
    r.lang = lang || win?.navigator?.language || 'en-US';
    if (local) r.processLocally = true;
    const finals = [];
    let sent = false;
    let heard = false;
    let fallBack = false;
    stopped = false;
    r.onresult = (event) => {
      // Words after a send would bring the sent text back.
      if (recognition !== r || sent) return;
      heard = true;
      let interim = '';
      for (let i = event.resultIndex; i < event.results.length; i++) {
        const result = event.results[i];
        const phrase = result[0].transcript;
        if (!result.isFinal) {
          interim += phrase;
          continue;
        }
        const rest = stopped ? null : sendPhraseRest(phrase, sendPhrase());
        if (rest !== null) {
          finals.push(rest);
          const text = finals.join(' ').replace(/\s+/g, ' ').trim();
          sent = true;
          onText?.(text);
          onSend?.(text);
          r.stop();
          return;
        }
        finals.push(phrase);
      }
      onText?.(`${finals.join(' ')} ${interim}`.replace(/\s+/g, ' ').trim());
    };
    r.onerror = (event) => {
      fallBack = local && !strictRoute && !heard && !stopped && !NOT_LOCAL_FAULTS.has(event?.error);
      if (!fallBack) onState?.(false, event?.error || 'error');
    };
    r.onend = () => {
      const again = fallBack && recognition === r;
      if (recognition === r) recognition = null;
      if (again) {
        open(false);
        return;
      }
      onState?.(false);
      micChanged();
    };
    recognition = r;
    r.start();
    onRoute?.(local, fallBack || (!local && localSpeech));
    onState?.(true);
    micChanged();
  }
  return dictation;
}
