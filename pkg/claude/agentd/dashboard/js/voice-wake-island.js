// voice-wake-island.js -- runs the wake listener where the operator opted in,
// and always shows that it is on: a mic that hears the room must not be silent
// about it. Separate from the red listening state of a dictation.

import { h, render } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { dictationBlock } from './insecure-origin.js';
import { speechRecognitionCtor } from './companion-voice.js';
import { voicePhrase, voiceWakeOn } from './voice-config.js';
import { createWakeListener, pickWakeTarget } from './voice-wake.js';

const html = htm.bind(h);
const PAUSE_PREF = 'tclaude.dash.voice.wake-paused';
const NOTICE_MS = 6000;

function readPaused(win) {
  try { return win?.localStorage?.getItem(PAUSE_PREF) === '1'; } catch { return false; }
}

function writePaused(win, paused) {
  try {
    if (paused) win?.localStorage?.setItem(PAUSE_PREF, '1');
    else win?.localStorage?.removeItem(PAUSE_PREF);
  } catch { /* the pause just won't outlive this page */ }
}

export function VoiceWake({ snapshot, win = globalThis.window, listenerImpl = createWakeListener }) {
  const value = snapshot.value;
  const phrase = voicePhrase(value, 'wake');
  const wanted = voiceWakeOn(value) && !!phrase;
  const block = dictationBlock(!!speechRecognitionCtor(win), win);
  const usable = wanted && !block;
  const [view, setView] = useState({ state: 'off', detail: '', local: false });
  const [notice, setNotice] = useState('');
  const phraseRef = useRef(phrase);
  phraseRef.current = phrase;
  const listener = useRef(null);
  useEffect(() => {
    if (!usable) return undefined;
    let noticeTimer = null;
    const say = (text) => {
      setNotice(text);
      clearTimeout(noticeTimer);
      noticeTimer = setTimeout(() => setNotice(''), NOTICE_MS);
    };
    const wake = listenerImpl({
      win,
      phrase: () => phraseRef.current,
      onChange: setView,
      onWake: (words) => {
        const target = pickWakeTarget(win.document);
        if (target) target.start(words);
        else say(`Heard “${phraseRef.current}”, but no terminal has focus. Click the one you mean and say it again.`);
      },
    });
    listener.current = wake;
    if (readPaused(win)) wake.pause();
    void wake.setEnabled(true);
    return () => {
      clearTimeout(noticeTimer);
      wake.dispose();
      listener.current = null;
      setView({ state: 'off', detail: '', local: false });
    };
  }, [usable, listenerImpl, win]);

  const setPaused = (paused) => {
    writePaused(win, paused);
    if (paused) listener.current?.pause();
    else listener.current?.resume();
  };

  if (wanted && block) {
    return html`<div class="voice-wake blocked" role="status" data-capture-skip title=${block.why}>👂 Wake phrase is off: ${block.note}</div>`;
  }
  if (!usable || view.state === 'off') return null;
  const text = {
    listening: `Listening for “${phrase}”: ${view.route}`,
    held: 'Wake phrase waits while the mic or a reply is busy',
    elsewhere: 'Wake phrase is listening in another tab',
    paused: 'Wake phrase paused',
    failed: view.detail,
  }[view.state];
  return html`<div class=${`voice-wake ${view.state}`} role="status" data-capture-skip>
    <span aria-hidden="true">👂</span>
    <span>${notice || text}</span>
    ${view.state === 'paused' ? html`<button type="button" onClick=${() => setPaused(false)}>Resume</button>`
      : view.state === 'failed' ? html`<button type="button" onClick=${() => listener.current?.retry()}>Retry</button>`
      : view.state === 'elsewhere' ? null
      : html`<button type="button" onClick=${() => setPaused(true)}>Pause</button>`}
  </div>`;
}

export function mountVoiceWakeIsland({ host, snapshot }) {
  render(html`<${VoiceWake} snapshot=${snapshot} />`, host);
  return () => render(null, host);
}
