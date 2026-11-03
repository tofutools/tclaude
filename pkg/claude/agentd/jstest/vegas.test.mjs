// The Vegas radio HUD (vegas.js) is a Preact island over an imperative
// audio engine: the theme/sound events build one <audio>, and the card on
// #vegas-player renders its station pickers, now-playing readout and the
// play/pause transport from the engine's state.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const wait = () => new Promise((resolve) => setTimeout(resolve, 0));

async function setup(t, { nowPlaying = {}, gestureless = false } = {}) {
  const harness = await createPreactHarness(t);
  globalThis.__vegasTest = { active: true, sound: true };
  t.after(() => { delete globalThis.__vegasTest; });
  await harness.replaceDashboardModule('js/slop.js', 'export const isVegasActive = () => globalThis.__vegasTest.active;\nexport const wizWord = (plain) => plain;\n');
  await harness.replaceDashboardModule('js/slop-audio.js', 'export const isSlopSoundEnabled = () => globalThis.__vegasTest.sound;\n');

  // The engine's <audio>: a stub that records play/pause and fires the
  // media events the HUD follows.
  const audios = [];
  const realCreate = harness.document.createElement.bind(harness.document);
  harness.document.createElement = (tag, ...rest) => {
    const el = realCreate(tag, ...rest);
    if (String(tag).toLowerCase() !== 'audio') return el;
    let paused = true;
    let muted = false;
    Object.defineProperties(el, {
      paused: { get: () => paused },
      muted: { get: () => muted, set: (v) => { muted = !!v; el.dispatchEvent(new harness.window.Event('volumechange')); } },
    });
    el.volume = 1;
    // A gestureless load: the browser refuses autoplay with sound, but
    // allows it muted.
    el.play = () => {
      if (gestureless && !muted) return Promise.reject(new Error('NotAllowedError'));
      paused = false; el.dispatchEvent(new harness.window.Event('play')); return Promise.resolve();
    };
    el.pause = () => { paused = true; el.dispatchEvent(new harness.window.Event('pause')); };
    el.load = () => {};
    audios.push(el);
    return el;
  };
  t.after(() => { harness.document.createElement = realCreate; });

  const calls = [];
  const previous = globalThis.fetch;
  globalThis.fetch = async (url, init = {}) => {
    calls.push({ url: String(url), method: init.method || 'GET', body: init.body || null });
    if (String(url).startsWith('/api/slop/nowplaying')) return { ok: true, json: async () => nowPlaying };
    return { ok: true, json: async () => ({ channel: '', persisted: false }) };
  };
  t.after(() => { globalThis.fetch = previous; });

  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  host.id = 'vegas-player';
  const vegas = await harness.importDashboardModule('js/vegas.js');
  t.after(() => harness.act(() => vegas.setConnectionLost(true)));
  await harness.act(async () => { vegas.bindVegasMusic(); await wait(); });
  return { harness, host, audios, calls, vegas };
}

test('the Vegas HUD renders the station pickers and plays on activation', async (t) => {
  const { harness, host, audios } = await setup(t);
  assert.equal(audios.length, 1, 'one engine element');
  assert.equal(audios[0].paused, false, 'the stream autoplays');
  assert.match(audios[0].src, /ice1\.somafm\.com\/illstreet-128-mp3$/);
  assert.equal(host.querySelector('audio'), null, 'the engine stays out of the DOM');
  assert.deepEqual([...host.querySelectorAll('#vegas-group option')].map((o) => o.value), ['vegas', 'wizard']);
  const stations = [...host.querySelectorAll('#vegas-channel option')].map((o) => o.value);
  assert.ok(stations.includes('illstreet') && !stations.includes('thistle'), 'the channel picker lists the active group only');
  const play = host.querySelector('.vegas-play');
  assert.equal(play.getAttribute('aria-label'), 'Pause');

  await harness.act(() => harness.fireEvent(play, 'click'));
  assert.equal(audios[0].paused, true, 'pause pauses the engine');
  assert.equal(host.querySelector('.vegas-play').getAttribute('aria-label'), 'Play');
  await harness.act(() => harness.fireEvent(host.querySelector('.vegas-play'), 'click'));
  assert.equal(audios[0].paused, false);
});

test('the HUD shows the on-air song and its time on air', async (t) => {
  const startedAt = Math.floor(Date.now() / 1000) - 83;
  const { host } = await setup(t, { nowPlaying: { artist: 'Dean', title: 'Sway', search_url: 'https://www.youtube.com/results?search_query=x', started_at: startedAt } });
  assert.equal(host.querySelector('.vegas-artist').textContent, '♪ Dean');
  const link = host.querySelector('a.vegas-title');
  assert.equal(link.textContent, 'Sway');
  assert.equal(link.getAttribute('rel'), 'noopener');
  assert.match(host.querySelector('#vegas-elapsed').textContent, /^1:2\d$/);
});

test('switching channel persists the pick and re-tunes the engine', async (t) => {
  const { harness, host, audios, calls } = await setup(t);
  const channel = host.querySelector('#vegas-channel');
  const other = [...channel.querySelectorAll('option')].map((o) => o.value).find((v) => v !== 'illstreet');
  Object.defineProperty(channel, 'value', { configurable: true, writable: true, value: other }); // the test DOM's <select> has no settable value
  await harness.act(async () => { harness.fireEvent(channel, 'change'); await wait(); });
  assert.ok(calls.some((c) => c.url === '/api/slop/channel' && c.method === 'POST' && JSON.parse(c.body).channel === other));
  assert.equal(audios.length, 2, 'a fresh engine element for the new station');
  assert.equal(audios[0].getAttribute('src'), null, 'the old stream is released');
  assert.match(audios[1].src, new RegExp(`/${other}-128-mp3$`));
});

test('a dead stream shows the error card, and leaving the mode clears the HUD', async (t) => {
  const { harness, host, audios, vegas } = await setup(t);
  await harness.act(() => audios[0].dispatchEvent(new harness.window.Event('error')));
  assert.match(host.querySelector('.vegas-error').textContent, /Couldn’t reach the stream/);
  assert.equal(host.querySelector('.vegas-error a').getAttribute('href'), 'https://somafm.com/illstreet/');
  assert.equal(audios[0].getAttribute('src'), null, 'the failed stream is released');

  globalThis.__vegasTest.active = false;
  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:vegas')));
  assert.equal(host.innerHTML, '', 'no player once the Vegas features are off');
  vegas.setMusicVolume(30); // no player: a no-op that must not throw
});

test('a gestureless load plays muted and unmutes on the first interaction', async (t) => {
  const { harness, host, audios } = await setup(t, { gestureless: true });
  assert.equal(audios[0].muted, true, 'muted autoplay connects the stream');
  assert.equal(audios[0].paused, false);
  assert.equal(host.querySelector('.vegas-play').getAttribute('aria-label'), 'Play', 'not audible yet, so the button invites the click');
  await harness.act(() => harness.document.dispatchEvent(new harness.window.Event('pointerdown')));
  assert.equal(audios[0].muted, false, 'the first gesture brings the sound up');
  assert.equal(host.querySelector('.vegas-play').getAttribute('aria-label'), 'Pause');
});
