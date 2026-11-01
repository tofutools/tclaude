// voice-local.js -- recognition on this device. Chrome can run speech
// recognition locally once it has a language pack, so no audio leaves the
// machine. tclaude ships no model: the browser downloads and manages the pack,
// and only ever on the operator's click (Config). Once they have it, it is
// used, and it stays their choice: the wake listener stops rather than quietly
// sending room audio to a speech service if it goes away.

import { setLocalSpeech, speechRecognitionCtor } from './companion-voice.js';

const CHOSEN_PREF = 'tclaude.dash.voice.on-device';

function readChosen(win) {
  try { return win?.localStorage?.getItem(CHOSEN_PREF) === '1'; } catch { return false; }
}

function writeChosen(win, chosen) {
  try {
    if (chosen) win?.localStorage?.setItem(CHOSEN_PREF, '1');
    else win?.localStorage?.removeItem(CHOSEN_PREF);
  } catch { /* the choice just won't outlive this page */ }
}

const changes = new Set();
const routeChanged = () => { for (const listener of changes) listener(); };

// onLocalSpeechChange calls listener when on-device speech is installed or the
// operator gives it up, and returns how to stop.
export function onLocalSpeechChange(listener) {
  changes.add(listener);
  return () => changes.delete(listener);
}

const options = (win) => ({ langs: [win?.navigator?.language || 'en-US'], processLocally: true });

// localSpeechState is the browser's answer for this page's language:
// 'available', 'downloadable', 'downloading', 'unavailable', or 'unsupported'
// for a browser without on-device speech. It also tells dictation what to use.
export async function localSpeechState(win = globalThis.window) {
  const Ctor = speechRecognitionCtor(win);
  let state = 'unsupported';
  if (typeof Ctor?.available === 'function') {
    try {
      state = await Ctor.available(options(win));
    } catch {
      state = 'unavailable';
    }
  }
  setLocalSpeech(state === 'available');
  if (state === 'available') writeChosen(win, true);
  return state;
}

// onDeviceChosen reports whether this browser has had on-device speech, so its
// loss is a failure rather than a reason to use a speech service instead.
export function onDeviceChosen(win = globalThis.window) {
  return readChosen(win);
}

// forgetOnDevice accepts the speech service again, on the operator's word.
export function forgetOnDevice(win = globalThis.window) {
  writeChosen(win, false);
  routeChanged();
}

// installLocalSpeech asks the browser for the language pack. Call it only from
// the operator's click. It throws when the browser declines.
export async function installLocalSpeech(win = globalThis.window) {
  const Ctor = speechRecognitionCtor(win);
  if (typeof Ctor?.install !== 'function') throw new Error('this browser has no on-device speech');
  const installed = await Ctor.install(options(win));
  if (installed === false) throw new Error('the browser could not install it');
  const state = await localSpeechState(win);
  routeChanged();
  return state;
}

// speechServiceName names where the audio goes without on-device speech.
export function speechServiceName(win = globalThis.window) {
  const brands = win?.navigator?.userAgentData?.brands || [];
  return brands.some((item) => item.brand === 'Google Chrome') || /Chrome\//.test(win?.navigator?.userAgent || '') && !/Edg\//.test(win?.navigator?.userAgent || '')
    ? 'Google’s speech service' : 'your browser’s speech service';
}

// speechRouteLabel says where recognition runs, for the listening states.
export function speechRouteLabel(local, win = globalThis.window) {
  return local ? 'on this device' : speechServiceName(win);
}
