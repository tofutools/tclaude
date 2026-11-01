// voice-config-section.js -- the parts of Config's Voice section that aren't
// plain fields: the warning for the two hands-free settings together, and the
// on-device speech row, which lives in this browser rather than in config.json.

import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { forgetOnDevice, installLocalSpeech, localSpeechState, onDeviceChosen, speechServiceName } from './voice-local.js';

const html = htm.bind(h);

// syncVoiceWarning shows the warning when wake and silence send are both on.
export function syncVoiceWarning(doc = globalThis.document) {
  const warning = doc.getElementById('cfg-voice-hands-free-warning');
  if (!warning) return;
  warning.hidden = !(doc.getElementById('cfg-voice-wake')?.checked && doc.getElementById('cfg-voice-silence')?.checked);
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
  if (state === 'unsupported' || state === 'unavailable') {
    return html`<span class="cfg-hint" style="padding-left:0">
      ${state === 'unsupported' ? 'This browser has no on-device speech.' : 'On-device speech isn’t available for your language here.'} ${usingLine(state, win)}
    </span>`;
  }
  const lost = chosen && state !== 'available';
  return html`<div class="cfg-voice-local">
    <span class="cfg-inline" data-voice-local=${state}>${state === 'available' ? '✓ Installed.' : state === 'downloading' ? 'Downloading… Chrome doesn’t report progress.' : 'Not installed.'} ${usingLine(state, win)}</span>
    ${state === 'downloadable' && html`<button type="button" class="cfg-inline" onClick=${download}>Download on-device speech</button>`}
    ${lost && html`<span class="cfg-hint" style="padding-left:0">On-device speech was here and is gone, so the wake listener stays stopped. Download it again, or
      <button type="button" class="cfg-inline" onClick=${() => { forgetOnDevice(win); setChosen(false); }}>use ${speechServiceName(win)}</button></span>`}
    ${error && html`<span class="cfg-hint" role="alert" style="padding-left:0">${error}</span>`}
  </div>`;
}
