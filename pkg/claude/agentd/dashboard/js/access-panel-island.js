// The quick permissions panel: every access request waiting on the operator, in
// a right-hand drawer over whatever tab is showing, so answering one does not
// mean leaving the work for the Messages tab. Like the human-notification reader
// it is its own island on a body-level host, and surfaces raise it by event
// (openAccessPanel). It closes by itself once nothing is left to answer.
import { h, render } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ApprovalCard, OPEN_ACCESS_PANEL_EVENT } from './companion-approval.js';
import { approvalsFor, companionAgents, pendingApprovals } from './companion-model.js';
import { applyChromeTopInset } from './chrome-inset.js';

const html = htm.bind(h);

export function AccessPanel({ snapshot, fetchImpl, notify, documentRef = globalThis.document }) {
  const [opened, setOpened] = useState(null); // {agentId, launcher}
  const panel = useRef(null);
  const hadSome = useRef(false);
  useEffect(() => {
    const open = (event) => {
      applyChromeTopInset();
      hadSome.current = false;
      setOpened({ agentId: event.detail?.agentId || '', launcher: event.detail?.launcher || null });
    };
    documentRef.addEventListener(OPEN_ACCESS_PANEL_EVENT, open);
    return () => documentRef.removeEventListener(OPEN_ACCESS_PANEL_EVENT, open);
  }, [documentRef]);

  const close = () => {
    const held = panel.current?.contains?.(documentRef.activeElement);
    const launcher = opened?.launcher;
    setOpened(null);
    if (held && launcher?.isConnected) launcher.focus({ preventScroll: true });
  };
  const requests = opened ? pendingApprovals(snapshot.value) : [];
  useEffect(() => {
    if (!opened) return;
    if (requests.length) hadSome.current = true;
    else if (hadSome.current) close();
  }, [opened, requests.length]);
  useEffect(() => { if (opened) panel.current?.focus?.({ preventScroll: true }); }, [opened]);
  if (!opened) return null;

  const agents = companionAgents(snapshot.value);
  const asker = opened.agentId ? agents.get(opened.agentId) : null;
  const mine = new Set(approvalsFor(snapshot.value, asker));
  const ordered = [...requests.filter((r) => mine.has(r)), ...requests.filter((r) => !mine.has(r))];
  const nameOf = (request) => agents.get(request.agent_id)?.name || request.conv_title || 'An agent';
  return html`<aside ref=${panel} class="human-notification-drawer access-panel" role="dialog" aria-modal="false"
      aria-label="Permission requests" tabindex="-1"
      onKeyDown=${(event) => { if (event.key === 'Escape') { event.stopPropagation(); close(); } }}>
    <header class="access-panel-header">
      <b><span aria-hidden="true">🔑</span> Waiting for your OK</b>
      <span class="access-panel-count">${requests.length || ''}</span>
      <button type="button" aria-label="Close" title="Close (Esc). Requests keep waiting" onClick=${close}>×</button>
    </header>
    <div class="access-panel-list">
      ${ordered.map((request) => html`<${ApprovalCard} key=${request.id} request=${request} fetchImpl=${fetchImpl} notify=${notify} from=${nameOf(request)} />`)}
      ${!ordered.length && html`<p class="access-panel-empty">Nothing is waiting for your OK.</p>`}
    </div>
  </aside>`;
}

// AccessPanelGate leaves the panel out while features.collab is off.
export function AccessPanelGate(props) {
  return props.snapshot.value?.collab_enabled === true ? html`<${AccessPanel} ...${props} />` : null;
}

export function mountAccessPanelIsland({ host, registerCleanup, ...props }) {
  render(html`<${AccessPanelGate} ...${props} />`, host);
  registerCleanup(() => render(null, host));
}
