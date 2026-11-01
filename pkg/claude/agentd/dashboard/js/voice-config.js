// voice-config.js -- the spoken phrases and opt-in voice settings, as Config
// shows them and the snapshot carries them (dashboard.voice). A new spoken
// command is one more VOICE_ACTIONS entry here and in the Go defaults
// (config.VoicePhraseDefaults): Config lists it with the others.

export const VOICE_ACTIONS = Object.freeze([
  Object.freeze({
    action: 'send', phrase: 'send now', on: true, title: 'Send phrase',
    summary: 'say this at the very end of a dictated message to send it to an agent',
    more: 'No pause is needed, and the phrase is left out of the message. It works in the companion window and agent terminals, never in a plain shell. Enter and the Send button always work.',
  }),
  Object.freeze({
    action: 'wake', phrase: 'hey agent', on: false, title: 'Focused phrase',
    summary: 'say this to dictate to the focused agent or Steward without a click',
    more: 'Words after the phrase start the draft. Pick something you would not say to an agent: common words may fire by mistake. It must differ from the other phrases that are on. It only does anything with the wake phrase listener on, below.',
  }),
  Object.freeze({
    action: 'steward', phrase: 'hey stewie', on: false, title: 'Steward phrase',
    summary: 'say this to switch to the Steward, whatever has focus, and dictate there',
    more: "It opens or focuses the Steward's window, and words after the phrase start the draft. It never starts a Steward: with none running you get a short notice. It is also the one way to move continuous mode, which ends on the old agent and starts on the Steward. It only does anything with the wake phrase listener on, below.",
  }),
  Object.freeze({
    action: 'stop', phrase: 'stop listening', on: true, title: 'Stop phrase',
    summary: 'say this on its own to end continuous mode',
    more: 'It only counts as a whole phrase, so a sentence that merely contains it is plain text. It is never sent to the agent, and it does nothing with continuous mode off.',
  }),
]);

export const COMMA_PAUSE_DEFAULT = 0.7;
export const PERIOD_PAUSE_DEFAULT = 1.5;
export const VOICE_PAUSE_MIN = 0.3;
export const VOICE_PAUSE_MAX = 10;
export const CONTINUOUS_IDLE_DEFAULT = 120;
export const CONTINUOUS_IDLE_MIN = 30;
export const CONTINUOUS_IDLE_MAX = 3600;
export const SILENCE_SEND_DEFAULT = 3;
export const SILENCE_SEND_MIN = 2;
export const SILENCE_SEND_MAX = 30;

const defaultPhrase = (action) => VOICE_ACTIONS.find((item) => item.action === action)?.phrase ?? '';

// voicePhrase is an action's phrase, '' when it is off. A snapshot from before
// the setting, or none yet, means the default, which may be off.
export function voicePhrase(snapshot, action) {
  const stored = snapshot?.voice?.phrases?.find((item) => item.action === action);
  if (stored) return stored.phrase;
  return VOICE_ACTIONS.find((item) => item.action === action)?.on ? defaultPhrase(action) : '';
}

export function voiceWakeOn(snapshot) {
  return snapshot?.voice?.wake_enabled === true;
}

// voiceMarksDictated is the opt-out for the marker on dictated messages.
export function voiceMarksDictated(snapshot) {
  return snapshot?.voice?.mark_dictated !== false;
}

// voiceContinuous is continuous mode's settings, null when it is off.
export function voiceContinuous(snapshot) {
  if (snapshot?.voice?.continuous_enabled !== true) return null;
  return {
    stopPhrase: voicePhrase(snapshot, 'stop'),
    stewardPhrase: voiceWakeOn(snapshot) ? voicePhrase(snapshot, 'steward') : '',
    idleSeconds: snapshot.voice.continuous_idle_seconds || CONTINUOUS_IDLE_DEFAULT,
  };
}

// harnessTakesMailMidTurn reports whether continuous mode can run on an agent
// of this harness, per the snapshot's catalog.
export function harnessTakesMailMidTurn(snapshot, harness) {
  return !!(snapshot?.harnesses || []).find((item) => item.name === harness)?.can_mail_mid_turn;
}

export function voiceSilenceSeconds(snapshot) {
  return snapshot?.voice?.silence_send_seconds || 0;
}

// voicePauses is the pause, in seconds, that adds a comma and a period; 0 is off.
export function voicePauses(snapshot) {
  return { comma: snapshot?.voice?.comma_pause_seconds || 0, period: snapshot?.voice?.period_pause_seconds || 0 };
}

// voicePausesProblem says what is wrong with the form's pause settings, each
// 0 when off, or returns ''. A pause that reaches the next one could never
// add its mark.
export function voicePausesProblem({ comma, period, silence }) {
  if (comma && period && comma >= period) return 'The comma pause must be shorter than the period pause.';
  if (silence && Math.max(comma, period) >= silence) return 'A punctuation pause must be shorter than send after silence.';
  return '';
}

const sameWords = (a, b) => a.trim().split(/\s+/).join(' ').toLowerCase() === b.trim().split(/\s+/).join(' ').toLowerCase();

// voicePhrasesClash reports whether two phrases would answer to the same words.
export function voicePhrasesClash(a, b) {
  return !!a.trim() && sameWords(a, b);
}

// voiceClashProblem says which two of the form's active phrases share words,
// or returns ''.
export function voiceClashProblem(phrases) {
  const active = VOICE_ACTIONS.filter(({ action }) => phrases[action].on);
  for (const [index, first] of active.entries()) {
    for (const second of active.slice(index + 1)) {
      if (voicePhrasesClash(phrases[first.action].phrase, phrases[second.action].phrase)) {
        return `The ${first.title.toLowerCase()} and the ${second.title.toLowerCase()} must differ.`;
      }
    }
  }
  return '';
}

// voiceFormPhrase is what Config shows for an action, from the stored
// dashboard config: its text and whether it is on. An empty phrase, the older
// way to turn one off, shows as off with the default text to come back to. The
// send phrase's earlier key still counts until a save moves it into the list.
export function voiceFormPhrase(dashboard, action) {
  const known = VOICE_ACTIONS.find((item) => item.action === action);
  const stored = dashboard?.voice?.phrases?.find((item) => item.action === action);
  const legacy = action === 'send' ? dashboard?.dictation_send_phrase : undefined;
  const phrase = (stored ? stored.phrase ?? '' : legacy ?? known.phrase).trim();
  return { phrase: phrase || known.phrase, on: !!phrase && (stored?.enabled ?? known.on) };
}

// storedVoicePhrases is the config list for the form's phrases, a map of
// action to { phrase, on }: only what differs from the defaults.
export function storedVoicePhrases(phrases) {
  return VOICE_ACTIONS.filter(({ action, phrase, on }) => phrases[action].phrase !== phrase || phrases[action].on !== on)
    .map(({ action }) => ({ action, phrase: phrases[action].phrase, enabled: phrases[action].on }));
}
