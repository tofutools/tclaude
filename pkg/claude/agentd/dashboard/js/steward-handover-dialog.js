// The confirmation for making an agent the Steward: what it gains, what it brings
// with it, and what becomes of the Steward it replaces.
import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay, useGuardedOverlayClose } from './management-overlay.js';
import { holdsMoreThanSteward, loadStewardHandover } from './steward-roster.js';

const html = htm.bind(h);

const THEN_CHOICES = [
  { value: 'roster', label: 'Keep on the roster', hint: 'A normal agent, one click from coming back.' },
  { value: 'keep', label: 'Keep as a normal agent', hint: 'Off the roster. You can still make it the Steward again.' },
  { value: 'retire', label: 'Retire it', hint: 'Ends its conversation. It stays in history.' },
];

// Folded is a long list of slugs, as a count until asked for.
function Folded({ label, slugs }) {
  if (!slugs?.length) return null;
  return html`<details class="steward-handover-line">
    <summary><b>${label} ${slugs.length} ${slugs.length === 1 ? 'permission' : 'permissions'}</b></summary>
    ${slugs.map((slug, index) => html`${index ? ', ' : ''}<code>${slug}</code>`)}
  </details>`;
}

function Slugs({ label, slugs, warn = false }) {
  if (!slugs?.length) return null;
  return html`<p class=${`steward-handover-line${warn ? ' warn' : ''}`}><b>${label}</b> ${slugs.map((slug, index) => html`${index ? ', ' : ''}<code>${slug}</code>`)}</p>`;
}

export function StewardHandoverDialog({ descriptor, actions }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const [preview, setPreview] = useState(null);
  const [then, setThen] = useState('roster');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const fetchImpl = (...args) => globalThis.fetch(...args);

  useEffect(() => {
    let live = true;
    loadStewardHandover(fetchImpl, descriptor.agent)
      .then((result) => { if (live) setPreview(result); })
      .catch((cause) => { if (live) setError(cause?.message || String(cause)); });
    return () => { live = false; };
  }, [descriptor.agent]);

  const blocked = preview?.refused || (preview?.already_active && 'It is already the active Steward.') || '';
  const submit = async () => {
    if (!preview || blocked || busy) return;
    setBusy(true);
    setError('');
    try {
      await actions.handOverSteward({ agent: descriptor.agent, then }, descriptor);
    } catch (cause) {
      setError(cause?.message || String(cause));
    } finally {
      setBusy(false);
    }
  };
  const name = preview?.name || descriptor.label || 'this agent';
  const previous = preview?.previous;
  return html`<${Overlay} id="steward-handover-modal" labelledby="steward-handover-title"
    onClose=${() => actions.close(descriptor)} onSubmitHotkey=${submit} blocked=${busy} registerClose=${registerClose}
    resizeKey="tclaude.dash.modalSize.steward-handover">
    <h3 id="steward-handover-title">Make ${name} the Steward</h3>
    <div class="modal-meta">It keeps its conversation, working directory and groups${preview?.harness ? `, and stays on ${preview.harness}` : ''}.</div>
    ${!preview && !error && html`<p class="modal-hint">Reading its access…</p>`}
    ${blocked && html`<p class="steward-handover-line warn" role="alert">${blocked}</p>`}
    ${preview && !blocked && html`
      <${Folded} label="Gains" slugs=${preview.gains} />
      <${Slugs} label="Already holds, so the Steward can too:" slugs=${[...(preview.extra?.standing || []), ...(preview.extra?.inherited || []), ...(preview.extra_sudo || []).map((slug) => `${slug} (sudo)`)]} warn=${holdsMoreThanSteward(preview)} />
      ${preview.grants_power?.length > 0 && html`<p class="steward-handover-line warn">It can hand out permissions, which a Steward does not normally hold. Take that off first if you don't want it.</p>`}
      <${Slugs} label="Stays denied:" slugs=${preview.denied} />
      <${Slugs} label="Stays off, as you left it:" slugs=${preview.withheld} />
      ${(preview.notes || []).map((note) => html`<p key=${note} class="modal-hint">${note}</p>`)}
      ${!preview.online && html`<p class="modal-hint">It is offline. Its brief waits until you resume it.</p>`}
      ${previous && html`<fieldset class="steward-handover-then">
        <legend>${previous.name} stops being the Steward</legend>
        ${THEN_CHOICES.map((choice) => html`<label key=${choice.value}>
          <input type="radio" name="steward-then" value=${choice.value} checked=${then === choice.value} onChange=${() => setThen(choice.value)} />
          <span>${choice.label}<small>${choice.hint}</small></span>
        </label>`)}
        ${then !== 'retire' && html`<${Folded} label="Loses" slugs=${previous.loses} /><${Folded} label="Keeps" slugs=${previous.keeps} />`}
      </fieldset>`}`}
    ${error && html`<p class="steward-handover-line warn" role="alert">${error}</p>`}
    <div class="modal-buttons">
      <button id="steward-handover-cancel" type="button" disabled=${busy} onClick=${() => { void requestClose(); }}>Cancel</button>
      <span class="spacer"></span>
      <button id="steward-handover-submit" class="primary" type="button" disabled=${!preview || !!blocked || busy} onClick=${submit}>${busy ? 'Handing over…' : 'Make Steward'}</button>
    </div>
  </${Overlay}>`;
}
