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
export const VOICE_RATE_PREF = 'tclaude.dash.companion.voice-rate';
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
      utterance.onerror = (event) => { if (event?.error === 'not-allowed') onBlocked(); };
      synth.speak(utterance);
      return true;
    },
    cancel() {
      if (supported) synth.cancel();
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

// splitSendCommand reports whether dictated text ends with the spoken
// command "send" and returns the text without it.
export function splitSendCommand(text) {
  const match = /^(.*?)[\s,.]*\bsend(?: it)?[.!]?\s*$/is.exec(String(text || ''));
  if (!match) return { text: String(text || '').trim(), send: false };
  return { text: match[1].trim(), send: true };
}

// createDictation wraps the browser's speech recognition: interim and final
// text arrive through onText (the whole utterance so far), onState reports
// listening on/off, and onSend fires when the operator ends with "send".
// Chrome's recognizer runs in Google's cloud; the button says so.
export function createDictation(win = globalThis.window, { onText, onState, onSend, lang } = {}) {
  const Ctor = speechRecognitionCtor(win);
  let recognition = null;
  return {
    supported: !!Ctor,
    get active() { return !!recognition; },
    start() {
      if (!Ctor || recognition) return;
      const r = new Ctor();
      r.continuous = true;
      r.interimResults = true;
      r.lang = lang || win?.navigator?.language || 'en-US';
      let finals = '';
      r.onresult = (event) => {
        let interim = '';
        for (let i = event.resultIndex; i < event.results.length; i++) {
          const result = event.results[i];
          if (result.isFinal) finals += `${result[0].transcript} `;
          else interim += result[0].transcript;
        }
        const heard = `${finals}${interim}`.replace(/\s+/g, ' ').trim();
        const command = splitSendCommand(finals);
        if (command.send && !interim.trim()) {
          onText?.(command.text);
          onSend?.(command.text);
          r.stop();
          return;
        }
        onText?.(heard);
      };
      r.onerror = (event) => onState?.(false, event?.error || 'error');
      r.onend = () => {
        recognition = null;
        onState?.(false);
      };
      recognition = r;
      r.start();
      onState?.(true);
    },
    stop() {
      recognition?.stop();
    },
  };
}
