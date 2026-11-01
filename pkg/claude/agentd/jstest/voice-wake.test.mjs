import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function fakeWin(harness, { local = 'unsupported', install = null } = {}) {
  const made = [];
  class Recognition {
    constructor() { made.push(this); }
    start() {}
    abort() { this.aborted = true; this.onend?.(); }
    stop() { this.onend?.(); }
    hear(transcript) {
      const result = [{ transcript }];
      result.isFinal = true;
      this.onresult({ resultIndex: 0, results: [result] });
    }
    die(error) { this.onerror?.({ error }); this.onend?.(); }
  }
  if (local !== 'unsupported') Recognition.available = async () => local;
  if (install) Recognition.install = install;
  const win = harness.window;
  win.SpeechRecognition = Recognition;
  win.isSecureContext = true;
  const stored = new Map();
  win.localStorage = { getItem: (k) => stored.get(k) ?? null, setItem: (k, v) => stored.set(k, v), removeItem: (k) => stored.delete(k) };
  return { win, made, last: () => made[made.length - 1], Recognition };
}

function clock() {
  let t = 0;
  const timers = [];
  return {
    now: () => t,
    setTimeoutImpl: (fn, ms) => { const timer = { fn, at: t + ms }; timers.push(timer); return timer; },
    clearTimeoutImpl: (timer) => { const i = timers.indexOf(timer); if (i >= 0) timers.splice(i, 1); },
    advance(ms) {
      t += ms;
      for (const timer of timers.filter((item) => item.at <= t)) {
        timers.splice(timers.indexOf(timer), 1);
        timer.fn();
      }
    },
    pending: () => timers.length,
  };
}

async function setup(t, options = {}) {
  const harness = await createPreactHarness(t);
  const voice = await harness.importDashboardModule('js/companion-voice.js');
  const wakeModule = await harness.importDashboardModule('js/voice-wake.js');
  const fake = fakeWin(harness, options);
  const time = clock();
  const views = [];
  const woken = [];
  const listener = wakeModule.createWakeListener({
    win: fake.win, phrase: () => 'hey tofu', onChange: (view) => views.push(view), onWake: (words) => woken.push(words),
    locks: options.locks ?? null, ChannelImpl: options.ChannelImpl ?? null, pausedElsewhere: options.pausedElsewhere, now: time.now, setTimeoutImpl: time.setTimeoutImpl, clearTimeoutImpl: time.clearTimeoutImpl,
  });
  t.after(() => listener.dispose());
  return { harness, voice, wakeModule, ...fake, time, views, woken, listener, state: () => views[views.length - 1]?.state };
}

test('the wake phrase is found anywhere in a phrase, and only whole words count', async (t) => {
  const { wakeModule: { wakePhraseRest } } = await setup(t);
  assert.equal(wakePhraseRest('hey tofu', 'hey tofu'), '');
  assert.equal(wakePhraseRest('Hey, Tofu. Fix the bug', 'hey tofu'), 'Fix the bug');
  assert.equal(wakePhraseRest('so hey tofu fix it', 'hey tofu'), 'fix it');
  assert.equal(wakePhraseRest('they tofu', 'hey tofu'), null);
  assert.equal(wakePhraseRest('hey tofus', 'hey tofu'), null);
  assert.equal(wakePhraseRest('anything', ''), null);
});

test('it listens for finished phrases only, and hands over what followed the phrase', async (t) => {
  const s = await setup(t);
  await s.listener.setEnabled(true);
  assert.equal(s.state(), 'listening');
  assert.equal(s.last().interimResults, false);
  assert.equal(s.last().continuous, true);
  s.last().hear('nothing to see');
  assert.deepEqual(s.woken, []);
  s.last().hear('hey tofu fix the bug');
  assert.deepEqual(s.woken, ['fix the bug']);
});

test('it restarts after a quiet end, backs off after errors, then stops and says so', async (t) => {
  const s = await setup(t);
  await s.listener.setEnabled(true);
  s.time.advance(60000);
  s.last().die('no-speech');
  s.time.advance(300);
  assert.equal(s.made.length, 2, 'a quiet end restarts soon');
  for (let i = 0; i < 4; i++) {
    s.last().die('network');
    s.time.advance(1000 * 2 ** i - 1);
    const before = s.made.length;
    s.time.advance(1);
    assert.equal(s.made.length, before + 1, `attempt ${i} waits its backoff`);
  }
  s.last().die('network');
  assert.equal(s.state(), 'failed');
  assert.match(s.views.at(-1).detail, /repeated errors \(network\)/);
  assert.equal(s.time.pending(), 0, 'no more restarts');
  await s.listener.retry();
  assert.equal(s.state(), 'listening');
});

test('a blocked microphone stops it at once', async (t) => {
  const s = await setup(t);
  await s.listener.setEnabled(true);
  s.last().die('not-allowed');
  assert.equal(s.state(), 'failed');
  assert.equal(s.time.pending(), 0);
});

test('it stops while hidden and comes back when visible', async (t) => {
  const s = await setup(t);
  await s.listener.setEnabled(true);
  const first = s.last();
  Object.defineProperty(s.win.document, 'hidden', { configurable: true, get: () => true });
  s.win.document.dispatchEvent(new s.win.Event('visibilitychange'));
  assert.equal(first.aborted, true);
  assert.equal(s.state(), 'off');
  Object.defineProperty(s.win.document, 'hidden', { configurable: true, get: () => false });
  s.win.document.dispatchEvent(new s.win.Event('visibilitychange'));
  assert.equal(s.made.length, 2);
  assert.equal(s.state(), 'listening');
});

test('it never hears a reply read aloud', async (t) => {
  const s = await setup(t);
  const utterances = [];
  s.win.SpeechSynthesisUtterance = class { constructor(text) { this.text = text; utterances.push(this); } };
  s.win.speechSynthesis = { speak() {}, cancel() {}, getVoices: () => [] };
  const speaker = s.voice.createSpeaker(s.win);
  await s.listener.setEnabled(true);
  const listening = s.last();
  speaker.speak('hey tofu is what you said');
  assert.equal(listening.aborted, true);
  assert.equal(s.state(), 'held');
  utterances[0].onend();
  assert.equal(s.state(), 'listening');
  assert.equal(s.made.length, 2);
});

test('a dictation takes the mic from it, and it comes back afterwards', async (t) => {
  const s = await setup(t);
  await s.listener.setEnabled(true);
  const wake = s.last();
  const dictation = s.voice.createDictation(s.win, {});
  dictation.start();
  assert.equal(wake.aborted, true);
  assert.equal(s.made.length, 2, 'the dictation opens once the mic is free');
  assert.equal(s.state(), 'held');
  dictation.abort();
  assert.equal(s.state(), 'listening');
  assert.equal(s.made.length, 3);
});

test('only the tab with the lock listens', async (t) => {
  let grant;
  const locks = { request: (_name, _options, callback) => new Promise((resolve) => { grant = () => callback().then(resolve); }) };
  const s = await setup(t, { locks });
  await s.listener.setEnabled(true);
  assert.equal(s.state(), 'elsewhere');
  assert.equal(s.made.length, 0);
  grant();
  assert.equal(s.state(), 'listening');
});

test('paused means no recognizer at all', async (t) => {
  const s = await setup(t);
  await s.listener.setEnabled(true);
  s.listener.pause();
  assert.equal(s.last().aborted, true);
  assert.equal(s.state(), 'paused');
  s.time.advance(60000);
  assert.equal(s.made.length, 1);
  s.listener.resume();
  assert.equal(s.made.length, 2);
});

test('it uses on-device speech when installed, and never downloads it', async (t) => {
  let installs = 0;
  const s = await setup(t, { local: 'available', install: async () => { installs += 1; return true; } });
  await s.listener.setEnabled(true);
  assert.equal(s.last().processLocally, true);
  assert.equal(s.views.at(-1).route, 'on this device');
  assert.equal(installs, 0);
});

test('a downloadable pack is left alone', async (t) => {
  let installs = 0;
  const s = await setup(t, { local: 'downloadable', install: async () => { installs += 1; return true; } });
  await s.listener.setEnabled(true);
  assert.equal(installs, 0);
  assert.notEqual(s.last().processLocally, true);
  assert.notEqual(s.views.at(-1).route, 'on this device');
});

test('on-device failing stops it rather than sending the room to a service', async (t) => {
  const s = await setup(t, { local: 'available' });
  await s.listener.setEnabled(true);
  s.last().die('network');
  assert.equal(s.state(), 'failed');
  assert.match(s.views.at(-1).detail, /rather than send room audio/);
  assert.equal(s.made.length, 1);
  assert.equal(s.time.pending(), 0);
});

test('on-device gone since last time stops it too, until the operator chooses', async (t) => {
  const s = await setup(t, { local: 'downloadable' });
  const local = await s.harness.importDashboardModule('js/voice-local.js');
  s.win.localStorage.setItem('tclaude.dash.voice.on-device', '1');
  await s.listener.setEnabled(true);
  assert.equal(s.state(), 'failed');
  assert.equal(s.made.length, 0, 'no recognizer ran');
  local.forgetOnDevice(s.win);
  await s.listener.retry();
  assert.equal(s.state(), 'listening');
});

test('a dictation falls back from on-device with a visible note', async (t) => {
  const s = await setup(t, { local: 'available' });
  const local = await s.harness.importDashboardModule('js/voice-local.js');
  await local.localSpeechState(s.win);
  const routes = [];
  const states = [];
  const dictation = s.voice.createDictation(s.win, { onRoute: (...args) => routes.push(args), onState: (on) => states.push(on) });
  dictation.start();
  assert.equal(s.last().processLocally, true);
  s.last().die('language-not-supported');
  assert.notEqual(s.last().processLocally, true);
  assert.deepEqual(routes, [[true, false], [false, true]]);
  assert.ok(dictation.active, 'it keeps listening');
  dictation.abort();
  s.voice.setLocalSpeech(false);
});

test('the shared target rule: focus first, then the only one showing, else nobody', async (t) => {
  const s = await setup(t);
  const { registerWakeTarget, pickWakeTarget } = s.wakeModule;
  const doc = s.win.document;
  const make = () => {
    const host = doc.body.appendChild(doc.createElement('div'));
    const input = host.appendChild(doc.createElement('input'));
    return { host, input, target: { host: () => host, start() {} } };
  };
  assert.equal(pickWakeTarget(doc), null);
  const a = make();
  const b = make();
  const stopA = registerWakeTarget(a.target);
  assert.ok(pickWakeTarget(doc) === a.target, 'the only one showing');
  const stopB = registerWakeTarget(b.target);
  assert.equal(pickWakeTarget(doc), null, 'two and no focus is a guess');
  b.input.focus();
  assert.ok(pickWakeTarget(doc) === b.target);
  stopA();
  stopB();
});

test('nothing listens before the route is settled', async (t) => {
  const s = await setup(t, { local: 'available' });
  let answer;
  s.Recognition.available = () => new Promise((resolve) => { answer = resolve; });
  const enabling = s.listener.setEnabled(true);
  s.win.document.dispatchEvent(new s.win.Event('visibilitychange'));
  assert.equal(s.made.length, 0);
  answer('available');
  await enabling;
  assert.equal(s.last().processLocally, true);
});

test('installing on-device speech moves a running listener over', async (t) => {
  const s = await setup(t, { local: 'downloadable', install: async () => { s.Recognition.available = async () => 'available'; return true; } });
  const local = await s.harness.importDashboardModule('js/voice-local.js');
  await s.listener.setEnabled(true);
  const google = s.last();
  assert.notEqual(google.processLocally, true);
  await local.installLocalSpeech(s.win);
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(google.aborted, true);
  assert.equal(s.last().processLocally, true);
  assert.equal(s.views.at(-1).route, 'on this device');
});

test('a dictation nobody clicked never falls back', async (t) => {
  const s = await setup(t, { local: 'available' });
  const local = await s.harness.importDashboardModule('js/voice-local.js');
  await local.localSpeechState(s.win);
  const made = s.made.length;
  const states = [];
  const dictation = s.voice.createDictation(s.win, { onState: (...args) => states.push(args) });
  dictation.start({ strictRoute: true });
  s.last().die('network');
  assert.equal(s.made.length, made + 1, 'no second recognizer');
  assert.ok(states.some(([on, error]) => !on && error === 'network'));
  s.voice.setLocalSpeech(false);
});

test('a mic busy in another window holds the listener', async (t) => {
  const channels = [];
  class Channel {
    constructor() { channels.push(this); this.sent = []; }
    postMessage(data) { this.sent.push(data); }
    close() {}
  }
  const s = await setup(t, { ChannelImpl: Channel });
  await s.listener.setEnabled(true);
  const listening = s.last();
  channels[0].onmessage({ data: { id: 'other', busy: true } });
  assert.equal(listening.aborted, true);
  assert.equal(s.state(), 'held');
  channels[0].onmessage({ data: { id: 'other', busy: false } });
  assert.equal(s.state(), 'listening');
  channels[0].onmessage({ data: { id: 'other', busy: true } });
  s.time.advance(41000);
  assert.equal(s.state(), 'listening', 'a window that went away stops counting');
  const dictation = s.voice.createDictation(s.win, {});
  dictation.start();
  assert.deepEqual(channels[0].sent.at(-1).busy, true, 'its own dictation is announced');
  dictation.abort();
  assert.deepEqual(channels[0].sent.at(-1).busy, false);
});

test('a lock handed over while paused elsewhere stays paused', async (t) => {
  let grant;
  const locks = { request: (_name, _options, callback) => new Promise((resolve) => { grant = () => callback().then(resolve); }) };
  const s = await setup(t, { locks, pausedElsewhere: () => true });
  await s.listener.setEnabled(true);
  grant();
  assert.equal(s.state(), 'paused');
  assert.equal(s.made.length, 0);
});

test('off by default: no listener, no indicator', async (t) => {
  const harness = await createPreactHarness(t);
  const { VoiceWake } = await harness.importDashboardModule('js/voice-wake-island.js');
  fakeWin(harness);
  let built = 0;
  const snapshot = harness.signals.signal({ voice: { wake_enabled: false, phrases: [{ action: 'wake', phrase: 'hey tofu' }] } });
  const view = await harness.mount(harness.html`<${VoiceWake} snapshot=${snapshot} win=${harness.window} listenerImpl=${() => { built += 1; }} />`);
  assert.equal(built, 0);
  assert.ok(!view.container.querySelector('.voice-wake'));
});

test('on a non-secure page it never starts, and says why', async (t) => {
  const harness = await createPreactHarness(t);
  const { VoiceWake } = await harness.importDashboardModule('js/voice-wake-island.js');
  const { win } = fakeWin(harness);
  win.isSecureContext = false;
  let built = 0;
  const snapshot = harness.signals.signal({ voice: { wake_enabled: true, phrases: [{ action: 'wake', phrase: 'hey tofu' }] } });
  const view = await harness.mount(harness.html`<${VoiceWake} snapshot=${snapshot} win=${win} listenerImpl=${() => { built += 1; }} />`);
  assert.equal(built, 0);
  assert.match(view.container.querySelector('.voice-wake').textContent, /HTTPS or localhost/);
});

test('pausing in one tab pauses them all, and a wake in an unused window stays idle', async (t) => {
  const harness = await createPreactHarness(t);
  const { VoiceWake } = await harness.importDashboardModule('js/voice-wake-island.js');
  const { registerWakeTarget } = await harness.importDashboardModule('js/voice-wake.js');
  const { win } = fakeWin(harness);
  const calls = [];
  let woke;
  let change;
  const impl = ({ onChange, onWake }) => {
    change = onChange;
    woke = onWake;
    return { pause: () => calls.push('pause'), resume: () => calls.push('resume'), setEnabled: async () => {}, dispose() {} };
  };
  const snapshot = harness.signals.signal({ voice: { wake_enabled: true, phrases: [{ action: 'wake', phrase: 'hey tofu' }] } });
  const view = await harness.mount(harness.html`<${VoiceWake} snapshot=${snapshot} win=${win} listenerImpl=${impl} />`);
  await harness.act(() => change({ state: 'listening', detail: '', local: false, route: 'x' }));
  win.dispatchEvent(Object.assign(new win.Event('storage'), { key: 'tclaude.dash.voice.wake-paused', newValue: '1' }));
  win.dispatchEvent(Object.assign(new win.Event('storage'), { key: 'tclaude.dash.voice.wake-paused', newValue: null }));
  assert.deepEqual(calls, ['pause', 'resume']);
  const started = [];
  const host = win.document.body.appendChild(win.document.createElement('div'));
  const stop = registerWakeTarget({ host: () => host, start: (words) => started.push(words) });
  win.document.hasFocus = () => false;
  win.localStorage.setItem('tclaude.dash.voice.last-window', 'another');
  await harness.act(() => woke('fix it'));
  assert.deepEqual(started, []);
  assert.match(view.container.querySelector('.voice-wake').textContent, /no terminal here has focus/);
  stop();
});

test('the indicator shows the route and pauses in one click', async (t) => {
  const harness = await createPreactHarness(t);
  const { VoiceWake } = await harness.importDashboardModule('js/voice-wake-island.js');
  const { win } = fakeWin(harness);
  win.document.body.dataset.x = '1';
  const calls = [];
  let change;
  const impl = ({ onChange }) => {
    change = onChange;
    return { pause: () => calls.push('pause'), resume: () => calls.push('resume'), setEnabled: async () => {}, dispose() {} };
  };
  const snapshot = harness.signals.signal({ voice: { wake_enabled: true, phrases: [{ action: 'wake', phrase: 'hey tofu' }] } });
  const view = await harness.mount(harness.html`<${VoiceWake} snapshot=${snapshot} win=${win} listenerImpl=${impl} />`);
  await harness.act(() => change({ state: 'listening', detail: '', local: true, route: 'on this device' }));
  const pill = view.container.querySelector('.voice-wake');
  assert.match(pill.textContent, /Listening for “hey tofu”: on this device/);
  await harness.act(() => pill.querySelector('button').click());
  assert.deepEqual(calls, ['pause']);
});
