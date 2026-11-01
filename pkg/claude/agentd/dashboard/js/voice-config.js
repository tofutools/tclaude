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
    action: 'wake', phrase: 'hey tofu', on: false, title: 'Wake phrase',
    summary: 'say this to start dictating without a click',
    more: 'Words after the phrase start the draft. Pick something you would not say to an agent. It must differ from the send phrase while both are on. It only does anything with the wake listener on, below.',
  }),
]);

export const SILENCE_SEND_DEFAULT = 3;
export const SILENCE_SEND_MIN = 2;
export const SILENCE_SEND_MAX = 30;

const defaultPhrase = (action) => VOICE_ACTIONS.find((item) => item.action === action)?.phrase ?? '';

// voicePhrase is an action's phrase, '' when it is off. A snapshot from before
// the setting, or none yet, means the default.
export function voicePhrase(snapshot, action) {
  const stored = snapshot?.voice?.phrases?.find((item) => item.action === action);
  return stored ? stored.phrase : defaultPhrase(action);
}

export function voiceWakeOn(snapshot) {
  return snapshot?.voice?.wake_enabled === true;
}

export function voiceSilenceSeconds(snapshot) {
  return snapshot?.voice?.silence_send_seconds || 0;
}

const sameWords = (a, b) => a.trim().split(/\s+/).join(' ').toLowerCase() === b.trim().split(/\s+/).join(' ').toLowerCase();

// voicePhrasesClash reports whether two phrases would answer to the same words.
export function voicePhrasesClash(a, b) {
  return !!a.trim() && sameWords(a, b);
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
