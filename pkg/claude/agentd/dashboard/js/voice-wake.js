// voice-wake.js -- the opt-in wake listener (tcl-jk3kal): an always-on
// recognizer that starts dictation when it hears the wake phrase. It runs only
// in a visible tab, in one tab at a time, and gives the mic up to a dictation
// or to speech playing, so it never hears a reply read aloud. Chrome's
// recognizer ends by itself, so it restarts, backing off after errors and
// stopping with a message rather than looping.

import { localSpeechState, onDeviceChosen, onLocalSpeechChange, speechRouteLabel } from './voice-local.js';
import { dictationOpen, holdMicForWake, onMicChange, phrasePattern, speechPlaying, speechRecognitionCtor } from './companion-voice.js';

export const WAKE_LOCK = 'tclaude.voice.wake';
const MIC_CHANNEL = 'tclaude.voice.mic';
// A window busy with the mic says so this often, and silence means it's gone.
const BUSY_REPEAT_MS = 15000;
const BUSY_EXPIRY_MS = 40000;
const RESTART_MS = 300;
const BACKOFF_MAX_MS = 30000;
const MAX_FAILURES = 5;
// A run this short got nowhere, whatever it reported.
const FUTILE_RUN_MS = 1000;
const RELEASE_TIMEOUT_MS = 500;
const NO_DOWNGRADE = 'On-device speech stopped working, so the wake listener stopped rather than send room audio to a speech service.';
const FATAL_ERRORS = new Set(['not-allowed', 'service-not-allowed', 'language-not-supported']);

// wakePhraseRest finds the wake phrase in one finished phrase and returns the
// words after it, or null when it isn't there.
export function wakePhraseRest(heard, phrase) {
  const pattern = phrasePattern(phrase);
  if (!pattern) return null;
  const text = String(heard || '');
  const match = new RegExp(`(?:^|[\\s\\p{P}])${pattern}(?=$|[\\s\\p{P}])`, 'iu').exec(text);
  return match ? text.slice(match.index + match[0].length).replace(/^[\s,;:.!?]+/, '') : null;
}

// ---- the shared target rule ----

const targets = new Set();

// registerWakeTarget offers a place to dictate into: host() is its element
// while it can take dictation, else null, and start(words) opens its mic.
export function registerWakeTarget(target) {
  targets.add(target);
  return () => targets.delete(target);
}

// pickWakeTarget is the terminal or chat window holding the keyboard, or the
// only one showing. Anything else would be a guess, so it answers null.
export function pickWakeTarget(doc) {
  const showing = [...targets].filter((target) => target.host());
  const focused = showing.find((target) => target.host().contains(doc?.activeElement));
  return focused || (showing.length === 1 ? showing[0] : null);
}

// The Steward's window, where this page has one: refusal() says why it can't
// be switched to, '' when it can, reserve() holds the mic for it, and
// open(words, carry) switches and dictates there, carrying continuous mode
// along when carry is set.
let stewardDoor = null;

export function registerStewardDoor(door) {
  stewardDoor = door;
  return () => { if (stewardDoor === door) stewardDoor = null; };
}

export function stewardRefusal() {
  return stewardDoor ? stewardDoor.refusal() : 'There’s no Steward window on this page.';
}

export function reserveStewardDoor() {
  stewardDoor?.reserve();
}

export function openStewardDoor(words, carry = false) {
  stewardDoor?.open(words, carry);
}

// createWakeListener runs the listener. onChange gets {state, detail, local}
// (where the audio goes) and route, with state 'off', 'listening', 'paused', 'held' (a dictation or speech has
// the mic), 'elsewhere' (another tab listens) or 'failed'. One recognizer hears
// both phrases, and onWake(words, action) says which it was: 'wake' or 'steward'.
export function createWakeListener({
  win = globalThis.window,
  phrase = () => '',
  stewardPhrase = () => '',
  onWake = () => {},
  onChange = () => {},
  pausedElsewhere = () => false,
  locks = win?.navigator?.locks,
  ChannelImpl = win?.BroadcastChannel,
  now = () => Date.now(),
  setTimeoutImpl = (...args) => globalThis.setTimeout(...args),
  clearTimeoutImpl = (...args) => globalThis.clearTimeout(...args),
} = {}) {
  const Ctor = speechRecognitionCtor(win);
  const doc = win?.document;
  const lang = win?.navigator?.language || 'en-US';
  let enabled = false;
  let paused = false;
  let failure = '';
  let failures = 0;
  let recognition = null;
  let restartTimer = null;
  let startedAt = 0;
  let heardAnything = false;
  let lastError = '';
  let waking = null;
  let lockState = 'none';
  let releaseLock = null;
  let lockAbort = null;
  let local = false;
  let lastView = '';
  let afterRelease = null;
  let stopping = false;
  let routeKnown = false;
  let busyTimer = null;
  let wasBusy = false;
  const others = new Map();
  const self = `${now()}-${Math.random()}`;
  const channel = typeof ChannelImpl === 'function' ? new ChannelImpl(MIC_CHANNEL) : null;

  // Nothing listens until the route is settled: an early start would send the
  // room to a speech service before on-device speech was even looked for.
  const wantsLock = () => enabled && routeKnown && !paused && !failure && !doc?.hidden;
  const busyHere = () => dictationOpen() || speechPlaying();
  // The mic and the speakers belong to the whole browser, not this page.
  const busyElsewhere = () => {
    for (const [id, until] of others) {
      if (until <= now()) others.delete(id);
    }
    return others.size > 0;
  };
  const held = () => busyHere() || busyElsewhere();

  function announceBusy() {
    const busy = busyHere();
    if (busy || wasBusy) channel?.postMessage({ id: self, busy });
    wasBusy = busy;
    if (busyTimer !== null) clearTimeoutImpl(busyTimer);
    busyTimer = busy ? setTimeoutImpl(() => { busyTimer = null; announceBusy(); }, BUSY_REPEAT_MS) : null;
  }

  function hearOther({ data } = {}) {
    if (!data || data.id === self) return;
    if (data.busy) {
      others.set(data.id, now() + BUSY_EXPIRY_MS);
      setTimeoutImpl(sync, BUSY_EXPIRY_MS + 100);
    } else {
      others.delete(data.id);
    }
    sync();
  }
  if (channel) channel.onmessage = hearOther;

  function report() {
    let state = 'listening';
    if (!enabled) state = 'off';
    else if (failure) state = 'failed';
    else if (paused) state = 'paused';
    else if (doc?.hidden) state = 'off';
    else if (lockState !== 'held') state = 'elsewhere';
    else if (held()) state = 'held';
    const view = { state, detail: failure, local, route: speechRouteLabel(local, win) };
    const key = JSON.stringify(view);
    if (key !== lastView) {
      lastView = key;
      onChange(view);
    }
  }

  function acquireLock() {
    if (lockState !== 'none') return;
    if (typeof locks?.request !== 'function') {
      lockState = 'held';
      return;
    }
    lockState = 'waiting';
    const abort = typeof win?.AbortController === 'function' ? new win.AbortController() : null;
    lockAbort = abort;
    locks.request(WAKE_LOCK, abort ? { signal: abort.signal } : {}, () => {
      lockState = 'held';
      // Paused in the tab that held it, the lock must not start this one.
      if (pausedElsewhere()) paused = true;
      sync();
      return new Promise((resolve) => { releaseLock = resolve; });
    }).catch(() => {});
  }

  function dropLock() {
    lockAbort?.abort();
    lockAbort = null;
    releaseLock?.();
    releaseLock = null;
    lockState = 'none';
  }

  function clearRestart() {
    if (restartTimer !== null) clearTimeoutImpl(restartTimer);
    restartTimer = null;
  }

  function fail(message) {
    failure = message;
    sync();
  }

  function begin() {
    const r = new Ctor();
    r.continuous = true;
    r.interimResults = false;
    r.lang = lang;
    if (local) r.processLocally = true;
    startedAt = now();
    heardAnything = false;
    lastError = '';
    r.onresult = (event) => {
      if (recognition !== r) return;
      for (let i = event.resultIndex; i < event.results.length; i++) {
        if (!event.results[i].isFinal) continue;
        heardAnything = true;
        const heard = event.results[i][0].transcript;
        const steward = wakePhraseRest(heard, stewardPhrase());
        const rest = steward ?? wakePhraseRest(heard, phrase());
        // A reply read aloud must not wake it, however the timing fell.
        if (rest === null || speechPlaying()) continue;
        waking = { words: rest, action: steward === null ? 'wake' : 'steward' };
        r.abort?.();
        return;
      }
    };
    r.onerror = (event) => { lastError = event?.error || 'error'; };
    r.onend = () => ended(r);
    recognition = r;
    try {
      r.start();
    } catch (error) {
      recognition = null;
      fail(`The wake listener could not start: ${error?.message || error}`);
    }
  }

  function ended(r) {
    if (recognition !== r) return;
    recognition = null;
    const free = afterRelease;
    afterRelease = null;
    const woken = waking;
    waking = null;
    const meant = stopping;
    stopping = false;
    if (free) {
      free();
    } else if (meant) {
      // Stopped on purpose, so it says nothing about the recognizer.
    } else if (woken !== null) {
      onWake(woken.words, woken.action);
    } else if (local && lastError && lastError !== 'no-speech' && lastError !== 'aborted') {
      fail(`${NO_DOWNGRADE} (${lastError})`);
      return;
    } else if (FATAL_ERRORS.has(lastError)) {
      fail(lastError === 'language-not-supported'
        ? 'The wake listener stopped: this browser can’t recognise your language.'
        : 'The wake listener stopped: the browser blocked the microphone.');
      return;
    } else {
      const ran = now() - startedAt;
      const quiet = !lastError || lastError === 'no-speech' || lastError === 'aborted';
      if (heardAnything || (quiet && ran >= FUTILE_RUN_MS)) failures = 0;
      else failures += 1;
      if (failures >= MAX_FAILURES) {
        fail(`The wake listener stopped after repeated errors${lastError ? ` (${lastError})` : ''}.`);
        return;
      }
      if (failures) {
        restartTimer = setTimeoutImpl(() => { restartTimer = null; sync(); }, Math.min(1000 * 2 ** (failures - 1), BACKOFF_MAX_MS));
        report();
        return;
      }
      restartTimer = setTimeoutImpl(() => { restartTimer = null; sync(); }, RESTART_MS);
      report();
      return;
    }
    sync();
  }

  function sync() {
    if (wantsLock()) acquireLock();
    else dropLock();
    const run = wantsLock() && lockState === 'held' && !held();
    if (!run) {
      clearRestart();
      if (recognition) stopping = true;
      recognition?.abort?.();
    } else if (!recognition && restartTimer === null) {
      begin();
    }
    report();
  }

  // Once this browser has had on-device speech, losing it is a failure: the
  // operator chose that their room stays on this machine.
  async function chooseRoute() {
    routeKnown = false;
    sync();
    local = await localSpeechState(win) === 'available';
    if (!local && onDeviceChosen(win)) failure = NO_DOWNGRADE;
    routeKnown = true;
  }

  // Installed from Config: move the running listener over at once.
  const stopRoute = onLocalSpeechChange(async () => {
    if (!enabled) return;
    failure = '';
    await chooseRoute();
    sync();
  });

  const stopHold = holdMicForWake({
    release(then) {
      clearRestart();
      if (!recognition) {
        then();
        return;
      }
      let done = false;
      const once = () => {
        if (done) return;
        done = true;
        then();
      };
      afterRelease = once;
      recognition.abort?.();
      setTimeoutImpl(once, RELEASE_TIMEOUT_MS);
    },
  });
  const stopMic = onMicChange(() => {
    announceBusy();
    sync();
  });
  doc?.addEventListener?.('visibilitychange', sync);

  return {
    supported: !!Ctor,
    // setEnabled turns it on or off. It uses recognition on this device when
    // the browser has it, and never asks for a download.
    async setEnabled(next) {
      enabled = !!next && !!Ctor;
      failure = '';
      if (enabled) await chooseRoute();
      sync();
    },
    pause() {
      paused = true;
      sync();
    },
    resume() {
      paused = false;
      sync();
    },
    async retry() {
      failure = '';
      failures = 0;
      await chooseRoute();
      sync();
    },
    dispose() {
      enabled = false;
      stopHold();
      stopMic();
      stopRoute();
      if (busyTimer !== null) clearTimeoutImpl(busyTimer);
      if (wasBusy) channel?.postMessage({ id: self, busy: false });
      channel?.close?.();
      doc?.removeEventListener?.('visibilitychange', sync);
      clearRestart();
      recognition?.abort?.();
      recognition = null;
      dropLock();
    },
  };
}
