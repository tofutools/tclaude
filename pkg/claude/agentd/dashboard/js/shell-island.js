import { h, render } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { syncBotAnimations } from './helpers.js';
import { trustedHTMLToVNodes } from './html-vnodes.js';
import {
  footerMetaView,
  globalActivityView,
  messagesBadgeView,
  usageView,
} from './shell-model.js';

const html = htm.bind(h);

function UsageToken({ token }) {
  if (token.kind === 'cost') {
    return html`
      <span class="uw ucost" data-goto-tab="costs">
        <span class="ulabel">${token.label}</span>
        ${token.today ? html`<span class="ucost-amt">${token.today}</span> <span class="urem">(today)</span>` : null}
        <span class="ucost-amt">${token.mtd}</span> <span class="urem">(mtd)</span>
      </span>
    `;
  }
  const blocks = [];
  for (let index = 0; index < 8; index++) {
    blocks.push(html`<span key=${index} class=${index < token.filled ? 'ubar-fill' : 'ubar-empty'} style=${index < token.filled ? `color:${token.color}` : ''}>█</span>`);
  }
  return html`
    <span class=${`uw${token.hidden ? ' umissing' : ''}`} aria-hidden=${token.hidden ? 'true' : null}>
      <span class="ulabel">${token.label}</span>
      <span class="ubar">${blocks}</span>
      <span class="upct">${token.pct}%</span>
      <span class="urem">${token.remaining}</span>
    </span>
  `;
}

function Usage({ state }) {
  const view = usageView(state.snapshot.value?.usage);
  if (view.na) return html`<span id="usage" class="meta na" title=${view.title}>${view.text}</span>`;
  return html`
    <span id="usage" class=${`meta${view.multiline ? ' multiline' : ''}`} title=${view.title}>
      ${view.lines.map((line) => view.multiline ? html`
        <span key=${line.key} class="uline">
          <span class="usrc">${line.label}</span>
          ${line.tokens.map((token) => html`<${UsageToken} key=${token.key} token=${token} />`)}
        </span>
      ` : line.tokens.map((token) => html`<${UsageToken} key=${token.key} token=${token} />`))}
    </span>
  `;
}

function GlobalActivity({ state }) {
  const snapshot = state.snapshot.value;
  const [wizard, setWizard] = useState(() => document.body.classList.contains('wizard'));
  const view = globalActivityView(snapshot, wizard);
  useEffect(() => {
    const update = () => setWizard(document.body.classList.contains('wizard'));
    document.addEventListener('tclaude:wizard', update);
    return () => document.removeEventListener('tclaude:wizard', update);
  }, []);
  useLayoutEffect(() => syncBotAnimations(), [view.markup]);
  return html`
    <span class="global-activity" id="global-activity" aria-label="Activity across all groups" title=${view.title || null}>
      ${trustedHTMLToVNodes(view.markup)}
    </span>
  `;
}

function Status({ feedback }) {
  const current = feedback.status.value;
  const classes = ['meta'];
  if (current.error) classes.push('error');
  else if (current.text) classes.push('live');
  return html`<span class=${classes.join(' ')} id="status">${current.text}</span>`;
}

function MessagesBadge({ state }) {
  const view = messagesBadgeView(state.snapshot.value);
  return html`
    <span id="messages-badge" class=${`tab-badge${view.blink ? ' blink' : ''}`} hidden=${view.hidden}>${view.text}</span>
  `;
}

function FooterMeta({ state }) {
  const view = footerMetaView(state.snapshot.value);
  if (!view) return html`<span class="meta" id="meta">loading…</span>`;
  return html`
    <span class="meta" id="meta">
      <span class="meta-version">tclaude version ${view.version}</span>
      <span class="meta-sep"> · </span><span class="meta-base">${view.base}</span>
      <span class="meta-sep"> · </span>refreshed <span class="meta-time">${new Date(view.generatedAt).toLocaleTimeString()}</span>
    </span>
  `;
}

function Disconnect({ state }) {
  const disconnected = state.connection.value.status === 'disconnected';
  return html`
    <div class=${`disconnect-overlay${disconnected ? ' show' : ''}`} id="disconnect-overlay">
      <div class="disconnect-card" role="alert" aria-live="assertive">
        <div class="disconnect-icon" aria-hidden="true">⚠️</div>
        <h2 class="disconnect-title" id="disconnect-title">Disconnected from agentd</h2>
        <p class="disconnect-body">The dashboard can’t reach the tclaude agentd daemon. Everything below may be stale, and the music has been stopped.</p>
        <p class="disconnect-status" id="disconnect-status">${disconnected ? 'Reconnecting…' : ''}</p>
      </div>
    </div>
  `;
}

function Toast({ feedback }) {
  const current = feedback.toast.value;
  return html`<div class=${`toast${current.error ? ' error' : ''}${current.visible ? ' show' : ''}`} id="toast">${current.message}</div>`;
}

function Confirm({ feedback }) {
  const model = feedback.confirmation.value;
  const okRef = useRef(null);
  useLayoutEffect(() => {
    if (model) okRef.current?.focus();
  }, [model]);
  useEffect(() => {
    if (!model) return undefined;
    const onKey = (event) => {
      if (event.key !== 'Escape') return;
      event.preventDefault();
      event.stopImmediatePropagation();
      feedback.resolveConfirmation(false);
    };
    document.addEventListener('keydown', onKey, true);
    return () => document.removeEventListener('keydown', onKey, true);
  }, [model, feedback]);
  const dismissBackdrop = (event) => {
    if (event.currentTarget === event.target) feedback.resolveConfirmation(false);
  };
  return html`
    <div class=${`modal-overlay${model ? ' show' : ''}`} id="confirm-modal" onClick=${dismissBackdrop}>
      <div class="modal" role="dialog" aria-modal="true" aria-labelledby="confirm-title">
        <h3 id="confirm-title">${model?.title || ''}</h3>
        <p id="confirm-body">${model?.body || ''}</p>
        <div class="modal-meta" id="confirm-meta" style=${`display:${model?.meta ? 'block' : 'none'}`}>${model?.meta || ''}</div>
        <div class="modal-buttons">
          <button id="confirm-cancel" onClick=${() => feedback.resolveConfirmation(false)}>${model?.cancelLabel || 'Cancel'}</button>
          <button ref=${okRef} id="confirm-ok" class="confirm-danger" onClick=${() => feedback.resolveConfirmation(true)}>${model?.okLabel || 'Confirm'}</button>
        </div>
      </div>
    </div>
  `;
}

export function mountShellIsland({ hosts, state, feedback, registerCleanup }) {
  const roots = [
    [hosts.activityHost, html`<${GlobalActivity} state=${state} />`],
    [hosts.usageHost, html`<${Usage} state=${state} />`],
    [hosts.statusHost, html`<${Status} feedback=${feedback} />`],
    [hosts.messagesBadgeHost, html`<${MessagesBadge} state=${state} />`],
    [hosts.metaHost, html`<${FooterMeta} state=${state} />`],
    [hosts.disconnectHost, html`<${Disconnect} state=${state} />`],
    [hosts.toastHost, html`<${Toast} feedback=${feedback} />`],
    [hosts.confirmHost, html`<${Confirm} feedback=${feedback} />`],
  ];
  const mounted = [];
  registerCleanup(() => {
    feedback.dispose();
    for (const host of mounted.slice().reverse()) render(null, host);
  });
  for (const [host, vnode] of roots) {
    mounted.push(host);
    render(vnode, host);
  }
}

export { Confirm, Disconnect, FooterMeta, GlobalActivity, MessagesBadge, Status, Toast, Usage };
