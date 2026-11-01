// The source download for the Steward's knowledge base. Nothing downloads until
// the operator says so: a card puts the question once, says what it is, how big
// and from where, and then shows the download as it runs and any failure. The
// answer lands in Config (steward.source_download), where it can be changed.
import { h, render } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';

const html = htm.bind(h);
const POLL_MS = 500;
const DISMISSED_PREF = 'tclaude.knowledgeSourceDismissed';

// SOURCE_CHOICE_EVENT says the choice changed on disk, so an open Config form can
// follow it without losing the operator's other edits.
export const SOURCE_CHOICE_EVENT = 'tclaude:knowledge-source-choice';

// A dismissed failure stays dismissed across reloads until a different one comes.
function readDismissed() {
  try { return globalThis.localStorage?.getItem(DISMISSED_PREF) || ''; } catch { return ''; }
}
function writeDismissed(detail) {
  try { globalThis.localStorage?.setItem(DISMISSED_PREF, detail); } catch { /* Lasts until reload, then. */ }
}

export const mib = (bytes) => `${Math.round((bytes || 0) / (1 << 20))} MiB`;

export async function answerSource(action, fetchImpl = globalThis.fetch) {
  const response = await fetchImpl('/api/knowledge/source', {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ action }),
  });
  // Even a failed Remove has saved its choice.
  globalThis.document?.dispatchEvent(new CustomEvent(SOURCE_CHOICE_EVENT));
  if (!response.ok) throw new Error((await response.text?.()) || `${response.status}`);
  return response.json();
}

// useLiveSource follows a running download faster than snapshots arrive, and
// falls back to the snapshot the moment it ends.
function useLiveSource(fromSnapshot, fetchImpl, setTimer = globalThis.setInterval, clearTimer = globalThis.clearInterval) {
  const [live, setLive] = useState(null);
  const view = live || fromSnapshot;
  const fetching = view?.state === 'fetching';
  useEffect(() => {
    if (!fetching) return undefined;
    const timer = setTimer(async () => {
      try {
        const response = await fetchImpl('/api/knowledge/source', { credentials: 'same-origin' });
        if (response.ok) setLive(await response.json());
      } catch { /* The next tick or snapshot catches up. */ }
    }, POLL_MS);
    return () => clearTimer(timer);
  }, [fetching]);
  // A snapshot taken after the live view moved on replaces it.
  useEffect(() => { if (live && live.state !== 'fetching') setLive(null); }, [fromSnapshot]);
  return [view, setLive];
}

export function KnowledgeSourceCard({ snapshot, fetchImpl = globalThis.fetch, notify = () => {}, setTimer, clearTimer }) {
  const [view, setLive] = useLiveSource(snapshot.value?.knowledge_source, fetchImpl, setTimer, clearTimer);
  const [busy, setBusy] = useState(false);
  const [dismissed, setDismissed] = useState(readDismissed);
  if (!view) return null;

  async function answer(action) {
    setBusy(true);
    try {
      setLive(await answerSource(action, fetchImpl));
    } catch (error) {
      notify(`That answer could not be saved: ${error?.message || error}`, true);
    } finally {
      setBusy(false);
    }
  }

  if (view.state === 'fetching') {
    const received = view.received || 0;
    return html`<div class="knowledge-source-card" role="status" aria-label="Downloading source">
      <b>Downloading tclaude's source for the Steward</b>
      <progress max=${view.download_bytes} value=${Math.min(received, view.download_bytes)}></progress>
      <p>${mib(received)} of about ${mib(view.download_bytes)} from ${view.origin}. Docs work meanwhile.</p>
    </div>`;
  }
  // After a No, the Config row is where a failure shows.
  if (view.state === 'failed' && view.policy !== 'never' && dismissed !== view.detail) {
    return html`<div class="knowledge-source-card failed" role="alert">
      <b>The source download failed</b>
      <p>${view.detail}</p>
      <div class="knowledge-source-actions">
        <button type="button" class="companion-primary" disabled=${busy} onClick=${() => answer('once')}>Try again</button>
        <button type="button" disabled=${busy} onClick=${() => { writeDismissed(view.detail); setDismissed(view.detail); }}>Dismiss</button>
      </div>
    </div>`;
  }
  if (!view.ask) return null;
  return html`<div class="knowledge-source-card" role="group" aria-label="Download tclaude's source?">
    <b>Download tclaude's source for the Steward?</b>
    <p>The Steward has the docs for ${view.version}. With the source as well, it can check how this version really behaves.</p>
    <p>About ${mib(view.download_bytes)} from ${view.origin}, about ${mib(view.stored_bytes)} on disk. Only one version is kept.</p>
    <div class="knowledge-source-actions">
      <button type="button" class="companion-primary" disabled=${busy} onClick=${() => answer('once')}>Yes, this version</button>
      <button type="button" disabled=${busy} onClick=${() => answer('each_version')}>Yes, every new version</button>
      <button type="button" disabled=${busy} onClick=${() => answer('never')}>No</button>
    </div>
    <p class="knowledge-source-note">You can change this in Config, under Steward.</p>
  </div>`;
}

// KnowledgeSourceRow is the Config tab's view of the same state.
export function KnowledgeSourceRow({ snapshot, fetchImpl = globalThis.fetch }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const view = snapshot.value?.knowledge_source;
  if (!view) return null;
  async function act(action) {
    setBusy(true);
    setError('');
    try {
      await answerSource(action, fetchImpl);
    } catch (failure) {
      setError(`That did not work: ${failure?.message || failure}`);
    } finally {
      setBusy(false);
    }
  }
  const ready = view.state === 'ready';
  const label = ready ? `${view.files} files, ${mib(view.bytes)} on disk for ${view.version}`
    : view.state === 'fetching' ? 'Downloading now'
      : view.state === 'failed' ? `Download failed: ${view.detail}`
        : `Not downloaded. About ${mib(view.download_bytes)} from ${view.origin}, about ${mib(view.stored_bytes)} on disk`;
  return html`<div class="knowledge-source-row">
    <span id="cfg-knowledge-source-state">${label}</span>
    ${ready && html`<button type="button" class="cfg-inline" disabled=${busy} title="Delete the stored copy and set this to Never"
      onClick=${() => act('remove')}>Remove</button>`}
    ${!ready && view.state !== 'fetching' && !/no commit/.test(view.detail || '') && html`<button type="button" class="cfg-inline" disabled=${busy}
      onClick=${() => act('once')}>Download now</button>`}
    ${error && html`<span class="knowledge-source-error" role="alert">${error}</span>`}
  </div>`;
}

export function KnowledgeSourceGate(props) {
  return props.snapshot.value?.collab_enabled === true ? html`<${KnowledgeSourceCard} ...${props} />` : null;
}

export function mountKnowledgeSourceIsland({ host, registerCleanup, ...props }) {
  render(html`<${KnowledgeSourceGate} ...${props} />`, host);
  registerCleanup(() => render(null, host));
}
