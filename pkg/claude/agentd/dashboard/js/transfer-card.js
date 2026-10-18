// transfer-card.js — progress cards for the uploads transfers.js tracks. A
// terminal pane shows its own uploads over the terminal (TransferCards with
// the terminal's path); spawn and message uploads show in the page tray.
import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { fileProgress, setTransferMinimized, subscribeTransfers } from './transfers.js';
import { fileSize } from './remote-files-panel.js';

const html = htm.bind(h);

function useTransfers(scope) {
  const [list, setList] = useState([]);
  useEffect(() => subscribeTransfers((items) => setList(items.filter((t) => t.scope === scope && t.visible))), [scope]);
  return list;
}

function percent(transfer) {
  return transfer.total ? Math.floor((transfer.sent / transfer.total) * 100) : 0;
}

function heading(transfer) {
  if (transfer.phase === 'done') return 'Uploaded';
  if (transfer.phase === 'finishing') return transfer.finishing;
  const count = transfer.files.length;
  return transfer.title || `Uploading ${count === 1 ? 'file' : `${count} files`}`;
}

function rowStatus(transfer, file, done) {
  // After the last byte the whole batch still waits (a remote node's hop).
  if (done >= 1 && transfer.phase === 'finishing') return html`<em>sent</em>`;
  if (done >= 1) return html`<em class="transfer-ok">✓ done</em>`;
  if (done <= 0 && transfer.phase === 'uploading') return html`<em>queued</em>`;
  return html`<em>${Math.floor(done * 100)}% · ${fileSize(Math.round(done * file.size))}/${fileSize(file.size)}</em>`;
}

export function TransferCard({ transfer }) {
  const per = fileProgress(transfer.files, transfer.sent);
  if (transfer.minimized) {
    return html`<button type="button" class="transfer-pill" title="Show upload progress"
      onClick=${() => setTransferMinimized(transfer.id, false)}>⇪ ${transfer.phase === 'uploading' ? `${percent(transfer)}%` : heading(transfer)}</button>`;
  }
  return html`<div class=${`transfer-card transfer-${transfer.phase}`} role="status" aria-live="polite">
    <div class="transfer-top">
      <span class="transfer-title">${heading(transfer)}</span>
      ${transfer.cancel ? html`<button type="button" class="mux-btn transfer-cancel" onClick=${transfer.cancel}>
        ${transfer.files.length > 1 ? 'Cancel all' : 'Cancel'}</button>` : null}
      <button type="button" class="transfer-min" title="Minimise" aria-label="Minimise upload progress"
        onClick=${() => setTransferMinimized(transfer.id, true)}>▁</button>
    </div>
    ${transfer.files.map((file, i) => html`<div class="transfer-file" key=${i}>
      <div class="transfer-name"><b title=${file.name}>${file.name}</b>${rowStatus(transfer, file, per[i])}</div>
      <div class=${`transfer-bar${per[i] >= 1 ? ' full' : ''}`}><i style=${`width:${Math.round(per[i] * 100)}%`}></i></div>
    </div>`)}
  </div>`;
}

export function TransferCards({ scope, className = 'transfer-cards' }) {
  const list = useTransfers(scope);
  if (!list.length) return null;
  return html`<div class=${className}>${list.map((t) => html`<${TransferCard} key=${t.id} transfer=${t} />`)}</div>`;
}

// Several islands mount a tray (the spawn dialog's, the message dialog's) so
// one exists on every page that uploads; only the first mounted one renders.
const trayOwners = [];
const trayListeners = new Set();

export function TransferTray() {
  const [token] = useState(() => ({}));
  const [, setTick] = useState(0);
  useEffect(() => {
    const listener = () => setTick((n) => n + 1);
    trayOwners.push(token);
    trayListeners.add(listener);
    listener();
    return () => {
      trayOwners.splice(trayOwners.indexOf(token), 1);
      trayListeners.delete(listener);
      for (const l of [...trayListeners]) l();
    };
  }, []);
  if (trayOwners[0] !== token) return null;
  return html`<${TransferCards} scope="dialog" className="transfer-cards transfer-tray" />`;
}
