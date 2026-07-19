import { h, render } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { mountTerminalWidget } from './terminals-core.js';
import { arcanePaletteEnabled } from './terminal-theme.js';
import { terminalComposeShortcutAction } from './terminal-compose-route.js';
import { registerTerminalShellController } from './terminals-tab.js';
import { hasShownOverlay } from './overlay-stack.js';
import { loadXtermRuntime } from './xterm-loader.js';
import { bindTerminalHandoffReceiver } from './terminal-handoff.js';

const html = htm.bind(h);
const INTERACTION_HINT = 'Select: Option-drag (macOS) / Shift-drag (Linux/Windows) · Copy: Ctrl/Cmd+Shift+C';

function composeTarget(pane, actions) {
  const { initialRetry: _initialRetry, ...seed } = pane.seed;
  return Object.freeze({
    ...seed,
    // activatePane refits and focuses the opaque xterm widget. If this exact
    // pane closed while the composer was open, activation safely returns false.
    restoreFocus: () => actions.activatePane(pane.key),
  });
}

function useTerminalThemeState() {
  const read = () => ({
    wizard: document.body.classList.contains('wizard'),
    palette: arcanePaletteEnabled(),
  });
  const [theme, setTheme] = useState(read);
  useEffect(() => {
    const sync = () => setTheme(read());
    document.addEventListener('tclaude:wizard', sync);
    document.addEventListener('tclaude:terminal-palette', sync);
    return () => {
      document.removeEventListener('tclaude:wizard', sync);
      document.removeEventListener('tclaude:terminal-palette', sync);
    };
  }, []);
  return theme;
}

// OpaqueTerminalHost is the one component/xterm boundary. Preact owns this div
// but never renders a child into it; the adapter alone owns its descendants.
function OpaqueTerminalHost({
  descriptor,
  runtimeID,
  active,
  activationToken,
  authenticate,
  className,
  fitClassName,
  actions,
  widgetFactory,
  onStatus,
  onReconnectChange,
  onSelectionChange,
  onComposeMessage,
  onDisconnect,
}) {
  const hostRef = useRef(null);
  const widgetRef = useRef(null);
  useLayoutEffect(() => {
    const widget = widgetFactory({
      host: hostRef.current,
      wsPath: descriptor.seed.ws,
      authenticate,
      active,
      onStatus,
      onReconnectChange,
      onSelectionChange,
      onComposeMessage,
      onDisconnect,
      initialRetry: descriptor.seed.initialRetry === true,
    });
    widgetRef.current = widget;
    const unregister = actions.registerWidget(runtimeID, widget);
    widget.setActive(active);
    void widget.connect();
    return () => {
      widget.dispose();
      unregister();
      if (widgetRef.current === widget) widgetRef.current = null;
    };
  }, [descriptor.id]);
  useLayoutEffect(() => widgetRef.current?.setActive(active), [active, activationToken]);
  return html`<div class=${className}><div ref=${hostRef} class=${fitClassName}></div></div>`;
}

function CopyButton({ className, id, hasSelection, actions, runtimeID }) {
  const label = hasSelection
    ? 'Copy selected terminal text (Ctrl/Cmd+Shift+C)'
    : `Copy terminal text. ${INTERACTION_HINT}`;
  return html`
    <button
      type="button"
      class=${className}
      id=${id || null}
      data-has-selection=${hasSelection ? 'true' : 'false'}
      aria-label=${label}
      onClick=${() => void actions.widgetFor(runtimeID)?.copy()}
    >Copy</button>
  `;
}

function TerminalPane({
  pane, active, activationToken, solo, manageTitle, actions, widgetFactory, onComposeMessage,
}) {
  const [status, setStatus] = useState('disconnected');
  const [reconnect, setReconnect] = useState(false);
  const [hasSelection, setHasSelection] = useState(false);
  const theme = useTerminalThemeState();
  const composeMessage = pane.seed.agent && onComposeMessage
    ? () => onComposeMessage(composeTarget(pane, actions))
    : null;
  useEffect(() => {
    if (active && manageTitle) {
      document.title = `${pane.label ? `${pane.label} — ` : ''}tclaude terminals`;
    }
  }, [active, manageTitle, pane.label]);
  return html`
    <div
      class=${`mux-pane${active ? ' active' : ''}${theme.wizard && theme.palette ? ' arcane-palette' : ''}`}
      id=${pane.id}
      role=${solo ? null : 'tabpanel'}
    >
      <div class="mux-pane-header">
        <span class="mux-pane-title">${pane.label}</span>
        <span class="mux-pane-status" role="status" aria-live="polite" aria-atomic="true">${status}</span>
        <span class="terminal-interaction-hint">${INTERACTION_HINT}</span>
        ${reconnect ? html`<button type="button" class="mux-btn" onClick=${() => void actions.widgetFor(pane.id)?.connect()}>Reconnect</button>` : null}
        <${CopyButton} className="mux-btn" hasSelection=${hasSelection} actions=${actions} runtimeID=${pane.id} />
        ${composeMessage ? html`
          <button type="button" class="mux-btn" title="Send a queued message to this agent (Ctrl/Cmd+M)" onClick=${composeMessage}>✉ Message</button>
        ` : null}
        <label
          class="mux-palette-toggle"
          hidden=${!theme.wizard}
          title="Recolour terminal defaults with the persisted wizard palette; explicit application RGB colours are unchanged"
        >
          <input
            type="checkbox"
            checked=${theme.palette}
            aria-label="Arcane terminal palette"
            onChange=${(event) => actions.setArcanePaletteEnabled(event.currentTarget.checked)}
          />
          <span>Arcane palette</span>
        </label>
        ${solo ? html`
          <button type="button" class="mux-btn" title="Move this terminal back to its dashboard tab" onClick=${() => void actions.reattachPane(pane.key)}>↩ dashboard</button>
        ` : html`
          <button type="button" class="mux-btn" title="Move this terminal to its own browser tab" onClick=${() => void actions.popOutPane(pane.key)}>⧉ tab</button>
        `}
      </div>
      <${OpaqueTerminalHost}
        descriptor=${pane}
        runtimeID=${pane.id}
        active=${active}
        activationToken=${activationToken}
        authenticate=${true}
        className="mux-pane-xterm"
        fitClassName="mux-pane-xterm-fit"
        actions=${actions}
        widgetFactory=${widgetFactory}
        onStatus=${setStatus}
        onReconnectChange=${setReconnect}
        onSelectionChange=${setHasSelection}
        onComposeMessage=${composeMessage}
      />
    </div>
  `;
}

function PaneTab({ pane, active, actions }) {
  const activate = (event) => {
    if (event.type === 'keydown' && event.key !== 'Enter' && event.key !== ' ' && event.key !== 'Spacebar') return;
    if (event.type === 'keydown') event.preventDefault();
    actions.activatePane(pane.key);
  };
  return html`
    <div
      class=${`mux-tab${active ? ' active' : ''}`}
      role="tab"
      tabIndex="0"
      aria-selected=${active ? 'true' : 'false'}
      aria-controls=${pane.id}
      onClick=${activate}
      onKeyDown=${activate}
    >
      <span class="mux-tab-label">${pane.label}</span>
      <button
        type="button"
        class="mux-tab-close"
        title="Close this terminal"
        aria-label=${`Close ${pane.label}`}
        onClick=${(event) => { event.stopPropagation(); void actions.closePane(pane.key); }}
      >×</button>
    </div>
  `;
}

function TerminalTabs({
  state, actions, widgetFactory, onComposeMessage, composeMessageDialogKind = () => '',
  solo = false, manageTitle = false, empty = false,
}) {
  const current = state.view.value;
  const hasPanes = current.panes.length > 0;

  useLayoutEffect(() => {
    if (solo) return undefined;
    document.body.classList.toggle('hide-terminals', !hasPanes);
    if (!hasPanes && document.getElementById('tab-terminals')?.classList.contains('active')) {
      document.querySelector('nav [data-tab="groups"]')?.click();
    }
    return undefined;
  }, [hasPanes, solo]);

  // Preact flushes layout effects child-first, so the active pane's own
  // activation attempt runs while this tab is still display:none, and a real
  // browser drops focus on an unrendered xterm. Refit and refocus the active
  // widget here, once the reveal above has actually made the pane visible.
  useLayoutEffect(() => {
    if (solo || !hasPanes || current.revealRequest === 0) return;
    const terminalSection = document.getElementById('tab-terminals');
    const needsPostRevealFocus = !terminalSection?.classList.contains('active');
    document.body.classList.remove('hide-terminals');
    document.querySelector('nav [data-tab="terminals"]')?.click();
    // Ordinary pane switches already run setActive(true) while this section is
    // visible. Only repeat fit/focus when the request had to reveal the section.
    if (!needsPostRevealFocus || !terminalSection?.classList.contains('active')) return;
    const active = current.panes.find((pane) => pane.key === current.activeKey);
    const widget = active && actions.widgetFor(active.id);
    if (!widget) return;
    widget.fit();
    widget.focus();
  }, [current.revealRequest, hasPanes, solo]);

  useEffect(() => {
    if (!hasPanes) return undefined;
    let armed = true;
    const confirmUnload = (event) => {
      event.preventDefault();
      event.returnValue = true;
    };
    const disarmForAuth = () => {
      if (!armed) return;
      armed = false;
      window.removeEventListener('beforeunload', confirmUnload);
    };
    window.addEventListener('beforeunload', confirmUnload);
    window.addEventListener('tclaude:auth-expired', disarmForAuth);
    return () => {
      window.removeEventListener('tclaude:auth-expired', disarmForAuth);
      window.removeEventListener('beforeunload', confirmUnload);
    };
  }, [hasPanes]);

  useEffect(() => {
    if (!hasPanes && manageTitle) document.title = 'tclaude terminals';
  }, [hasPanes, manageTitle]);

  useEffect(() => {
    if (solo || !onComposeMessage) return undefined;
    const onComposeShortcut = (event) => {
      const pane = current.panes.find((candidate) => candidate.key === current.activeKey);
      const dialogKind = composeMessageDialogKind();
      const action = terminalComposeShortcutAction(event, {
        tabActive: document.getElementById('tab-terminals')?.classList.contains('active'),
        operatorModalOpen: dialogKind === 'operator-message',
        blockingOverlayOpen: hasShownOverlay(),
        eligiblePane: Boolean(pane?.seed?.agent),
      });
      if (action === 'ignore') return;
      event.preventDefault();
      event.stopPropagation();
      if (action === 'open') onComposeMessage(composeTarget(pane, actions));
    };
    // Capture before xterm or terminal-tab chrome sees Ctrl/Cmd+M. This keeps
    // the shortcut available from the pane, tab strip, and header alike.
    document.addEventListener('keydown', onComposeShortcut, true);
    return () => document.removeEventListener('keydown', onComposeShortcut, true);
  }, [actions, composeMessageDialogKind, current.activeKey, current.panes, onComposeMessage, solo]);

  return html`
    <div class="terminal-shell-root">
      ${!solo ? html`
        <div class="mux-tabs" role="tablist" aria-label="Open terminals">
          ${current.panes.map((pane) => html`<${PaneTab} key=${pane.key} pane=${pane} active=${current.activeKey === pane.key} actions=${actions} />`)}
        </div>
      ` : null}
      ${hasPanes || !empty ? html`
        <div class="mux-panes">
          ${current.panes.map((pane) => html`
            <${TerminalPane}
              key=${pane.key}
              pane=${pane}
              active=${current.activeKey === pane.key}
              activationToken=${current.activeKey === pane.key ? current.revealRequest : 0}
              solo=${solo}
              manageTitle=${manageTitle}
              actions=${actions}
              widgetFactory=${widgetFactory}
              onComposeMessage=${onComposeMessage}
            />
          `)}
        </div>
      ` : null}
      ${empty && !hasPanes ? html`
        <div id="mux-empty">
          <div class="mux-empty-title">No terminals open</div>
          <div>Open one from the dashboard with the <code>web term</code> or <code>web window</code> buttons — they open in the dashboard's <code>Terminals</code> tab.</div>
          <div>Each terminal there has a <code>⧉ tab</code> button that pops it out into its own browser window here.</div>
        </div>
      ` : null}
    </div>
  `;
}

function TerminalBadge({ state }) {
  const count = state.view.value.count;
  return html`<span id="terminals-badge" class="tab-badge count" hidden=${count === 0}>${count}</span>`;
}

function TerminalModalSession({ descriptor, actions, widgetFactory }) {
  const [status, setStatus] = useState('disconnected');
  const [hasSelection, setHasSelection] = useState(false);
  const title = descriptor.label ? `Terminal — ${descriptor.label}` : 'Terminal';
  // Escape is NOT a close key here: it is terminal input for vim, less and the
  // agent TUIs. Only the shared confirmation overlay consumes Escape while it
  // is above this shell.
  return html`
    <div
      class="modal-overlay show"
      id="term-session-modal"
      onClick=${(event) => { if (event.currentTarget === event.target) void actions.confirmModalClose(descriptor.id); }}
    >
      <div class="term-session-modal" role="dialog" aria-modal="true" aria-labelledby="term-session-title">
        <div class="term-session-header">
          <h3 id="term-session-title">${title}</h3>
          <span class="term-session-status" id="term-session-status" role="status" aria-live="polite" aria-atomic="true">${status}</span>
          <span class="terminal-interaction-hint">${INTERACTION_HINT}</span>
          <${CopyButton} className="term-session-action" id="term-session-copy" hasSelection=${hasSelection} actions=${actions} runtimeID=${descriptor.id} />
          ${descriptor.seed.hideConv ? html`
            <button type="button" class="term-session-detach" id="term-session-detach" aria-label="Detach" title="Detach — drop this view now; the agent keeps running, reopen to reattach" onClick=${() => void actions.detachModal(descriptor.id)}>Detach</button>
          ` : null}
          <button type="button" class="term-session-pop" id="term-session-pop" title="Move this terminal to the dashboard's Terminals tab, where several can be open at once" onClick=${() => void actions.moveModalToPane(descriptor.id)}>⧉ tab</button>
          <button type="button" class="term-session-close" id="term-session-close" aria-label="Close" title="Close — asks first; the agent keeps running, reopen to reattach" onClick=${() => void actions.confirmModalClose(descriptor.id)}>×</button>
        </div>
        <${OpaqueTerminalHost}
          descriptor=${descriptor}
          runtimeID=${descriptor.id}
          active=${true}
          authenticate=${false}
          className="term-session-xterm"
          fitClassName="term-session-xterm-fit"
          actions=${actions}
          widgetFactory=${widgetFactory}
          onStatus=${setStatus}
          onReconnectChange=${() => {}}
          onSelectionChange=${setHasSelection}
          onDisconnect=${() => actions.onModalDisconnect(descriptor.id)}
        />
      </div>
    </div>
  `;
}

function TerminalModal({ state, actions, widgetFactory }) {
  const descriptor = state.view.value.modal;
  return descriptor
    ? html`<${TerminalModalSession} key=${descriptor.id} descriptor=${descriptor} actions=${actions} widgetFactory=${widgetFactory} />`
    : null;
}

export function mountTerminalShellIsland({
  host,
  badgeHost,
  modalHost,
  state,
  actions,
  registerCleanup,
  widgetFactory = mountTerminalWidget,
  onComposeMessage = null,
  composeMessageDialogKind = () => '',
}) {
  // A custom widget factory (tests or another embedding) owns its own runtime.
  // The production xterm adapter asks the facade to load the classic core
  // before it lets the first pane/modal enter Preact state.
  const runtimeLoader = widgetFactory === mountTerminalWidget ? loadXtermRuntime : null;
  const unregisterController = registerTerminalShellController(actions, runtimeLoader);
  const unbindHandoff = bindTerminalHandoffReceiver({
    openSeed: async (seed) => {
      if (runtimeLoader) await runtimeLoader();
      return actions.receiveHandoffPane(seed);
    },
  });
  render(html`<${TerminalTabs} state=${state} actions=${actions} widgetFactory=${widgetFactory}
    onComposeMessage=${onComposeMessage} composeMessageDialogKind=${composeMessageDialogKind} />`, host);
  render(html`<${TerminalBadge} state=${state} />`, badgeHost);
  render(html`<${TerminalModal} state=${state} actions=${actions} widgetFactory=${widgetFactory} />`, modalHost);
  registerCleanup(() => {
    unbindHandoff();
    unregisterController();
    render(null, modalHost);
    render(null, badgeHost);
    render(null, host);
    actions.dispose();
  });
}

export function mountStandaloneTerminalShell({
  host,
  state,
  actions,
  widgetFactory = mountTerminalWidget,
}) {
  let disposed = false;
  render(html`
    <${TerminalTabs}
      state=${state}
      actions=${actions}
      widgetFactory=${widgetFactory}
      solo=${true}
      manageTitle=${true}
      empty=${true}
    />
  `, host);
  return () => {
    if (disposed) return;
    disposed = true;
    render(null, host);
    actions.dispose();
  };
}

export { OpaqueTerminalHost, PaneTab, TerminalBadge, TerminalModal, TerminalModalSession, TerminalPane, TerminalTabs };
