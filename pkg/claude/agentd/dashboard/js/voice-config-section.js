// voice-config-section.js -- the parts of Config's Voice section that aren't
// plain fields: what the phrase checkboxes and hands-free switches do to each
// other's display, and the on-device speech row, which lives in this browser rather than in config.json.

import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { VOICE_ACTIONS } from './voice-config.js';
import { forgetOnDevice, installLocalSpeech, localSpeechState, onDeviceChosen, speechServiceName } from './voice-local.js';

const html = htm.bind(h);

// syncVoiceSection disables the text of a phrase that is off and shows the
// notes for combinations. It only displays: no setting changes another.
export function syncVoiceSection(doc = globalThis.document) {
  const checked = (id) => !!doc.getElementById(id)?.checked;
  for (const { action } of VOICE_ACTIONS) {
    const field = doc.getElementById(`cfg-voice-phrase-${action}`);
    if (field) field.disabled = !checked(`cfg-voice-on-${action}`);
  }
  const phrase = checked('cfg-voice-on-wake');
  const listener = checked('cfg-voice-wake');
  const show = (id, shown) => { const note = doc.getElementById(id); if (note) note.hidden = !shown; };
  show('cfg-voice-wake-no-listener', phrase && !listener);
  show('cfg-voice-wake-no-phrase', listener && !phrase);
  show('cfg-voice-hands-free-warning', phrase && listener && checked('cfg-voice-silence'));
}

function usingLine(state, win) {
  return state === 'available' ? 'In use: on this device. Audio stays on this machine.' : `In use: ${speechServiceName(win)}.`;
}

// LocalSpeechRow shows what this browser has and downloads only on the click.
export function LocalSpeechRow({ win = globalThis.window }) {
  const [state, setState] = useState('checking');
  const [error, setError] = useState('');
  const [chosen, setChosen] = useState(() => onDeviceChosen(win));
  useEffect(() => {
    let live = true;
    void localSpeechState(win).then((next) => {
      if (!live) return;
      setState(next);
      setChosen(onDeviceChosen(win));
    });
    return () => { live = false; };
  }, [win]);
  async function download() {
    setError('');
    setState('downloading');
    try {
      setState(await installLocalSpeech(win));
      setChosen(onDeviceChosen(win));
    } catch (failure) {
      setError(`The download failed: ${failure?.message || failure}`);
      setState(await localSpeechState(win));
    }
  }
  if (state === 'checking') return html`<span class="cfg-inline">Checking this browser…</span>`;
  const lost = chosen && state !== 'available';
  const status = {
    available: '✓ Installed.',
    downloading: 'Downloading… Chrome doesn’t report progress.',
    downloadable: 'Not installed.',
    unsupported: 'This browser has no on-device speech.',
    unavailable: 'On-device speech isn’t available for your language here.',
  }[state];
  return html`<div class="cfg-voice-local">
    <span class="cfg-inline" data-voice-local=${state}>${status} ${usingLine(state, win)}</span>
    ${state === 'downloadable' && html`<button type="button" class="cfg-inline" onClick=${download}>Download on-device speech</button>`}
    ${lost && html`<span class="cfg-hint" style="padding-left:0">On-device speech was here and is gone, so the wake listener stays stopped. Download it again if you can, or
      <button type="button" class="cfg-inline" onClick=${() => { forgetOnDevice(win); setChosen(false); }}>use ${speechServiceName(win)}</button></span>`}
    ${error && html`<span class="cfg-hint" role="alert" style="padding-left:0">${error}</span>`}
  </div>`;
}
