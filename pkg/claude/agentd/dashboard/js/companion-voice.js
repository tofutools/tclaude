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
  let text = String(markdown || '');
  text = text.replace(/```[\s\S]*?(```|$)/g, ' (code omitted) ');
  text = text.split('\n').filter((line) => !/^\s*\|.*\|\s*$/.test(line)).join('\n');
  text = text.replace(/!\[([^\]]*)\]\([^)]*\)/g, '$1');
  text = text.replace(/\[([^\]]+)\]\([^)]*\)/g, '$1');
  text = text.replace(/https?:\/\/\S+/g, 'a link');
  text = text.replace(/`([^`]*)`/g, '$1');
  text = text.replace(/^\s{0,3}#{1,6}\s+/gm, '');
  text = text.replace(/^\s*(?:[-*+]|\d+[.)])\s+/gm, '');
  text = text.replace(/(\*\*|__|\*|_|~~)(?=\S)([^*_~]+?)\1/g, '$2');
  text = text.replace(/\s*\n\s*/g, '. ').replace(/\.(\s*\.)+/g, '.').replace(/\s+/g, ' ').trim();
  text = text.replace(/^\.\s*/, '');
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
export function createSpeaker(win = globalThis.window, { rate = 1.05 } = {}) {
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

// createVoiceReader keeps one cursor per agent and fetches what it said since.
export function createVoiceReader(fetchImpl) {
  const cursors = new Map();
  async function read(agentId, since) {
    const response = await fetchImpl(`/api/companion/${encodeURIComponent(agentId)}/speech?since=${since}`, { credentials: 'same-origin' });
    if (!response.ok) throw new Error(`${response.status}`);
    const body = await response.json();
    if (body.now) cursors.set(agentId, String(body.now));
    return Array.isArray(body.items) ? body.items : [];
  }
  return {
    // prime starts an agent's cursor at the server's now.
    async prime(agentId) {
      if (cursors.has(agentId)) return;
      await read(agentId, PRIME_CURSOR);
    },
    forget(agentId) { cursors.delete(agentId); },
    // latest returns the agent's last text since the cursor ('' if none).
    async latest(agentId) {
      if (!cursors.has(agentId)) {
        await read(agentId, PRIME_CURSOR);
        return '';
      }
      const items = await read(agentId, cursors.get(agentId));
      return items.length ? items[items.length - 1].text : '';
    },
  };
}
