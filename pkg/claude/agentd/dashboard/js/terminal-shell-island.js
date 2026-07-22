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
import { dragLeftRegion, dragScreenPoint } from './terminal-drag-out.js';

const html = htm.bind(h);
const INTERACTION_HINT = 'Select: Option-drag (macOS) / Shift-drag (Linux/Windows) · Copy: Ctrl/Cmd+Shift+C';
const TERMINAL_TAB_DRAG_MIME = 'application/x-tclaude-terminal-tab';

export function terminalTabReorderOffset(event) {
  if (event?.type !== 'keydown' || event.target?.closest?.('button')) return null;
  if (!event.shiftKey || !event.altKey) return null;
  if (event.key === 'ArrowLeft') return -1;
  if (event.key === 'ArrowRight') return 1;
  return null;
}

function composeTarget(pane, actions) {
  const { initialRetry: _initialRetry, ...seed } = pane.seed;
  return Object.freeze({
    ...seed,
    // activatePane refits and focuses the opaque xterm widget. If this exact
    // pane closed while the composer was open, activation safely returns false.
    restoreFocus: () => actions.activatePane(pane.key),
  });
}

// While a terminal drag is in flight, follow the pointer so the surface can say
// "release here and this terminal leaves" before the user commits. The same
// geometry decides the gesture on dragend, so the hint never lies.
function useDragOutArmed(active, regionRef) {
  const [armed, setArmed] = useState(false);
  useEffect(() => {
    if (!active) {
      setArmed(false);
      return undefined;
    }
    const onDragOver = (event) => setArmed(dragLeftRegion(event, regionRef.current));
    document.addEventListener('dragover', onDragOver);
    return () => document.removeEventListener('dragover', onDragOver);
  }, [active]);
  return armed;
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
  const [dragging, setDragging] = useState(false);
  const headerRef = useRef(null);
  // A solo pop-out has no tab strip to drag out of, so its header is the home
  // region: pull the title off the header and the terminal goes back to the
  // dashboard — the mirror image of dragging a dashboard tab out of the strip.
  const reattachArmed = useDragOutArmed(solo && dragging, headerRef);
  const theme = useTerminalThemeState();
  const composeMessage = pane.seed.agent && onComposeMessage
    ? () => onComposeMessage(composeTarget(pane, actions))
    : null;
  const startTitleDrag = (event) => {
    event.stopPropagation();
    event.dataTransfer?.setData(TERMINAL_TAB_DRAG_MIME, pane.key);
    if (event.dataTransfer) event.dataTransfer.effectAllowed = 'move';
    setDragging(true);
  };
  const endTitleDrag = (event) => {
    setDragging(false);
    if (dragLeftRegion(event, headerRef.current)) void actions.reattachPane(pane.key);
  };
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
      <div ref=${headerRef} class=${`mux-pane-header${reattachArmed ? ' drag-out-armed' : ''}`}>
        ${solo ? html`
          <span
            class=${`mux-pane-title mux-pane-title-drag${dragging ? ' dragging' : ''}`}
            draggable="true"
            title="Drag off this header — or click ↩ dashboard — to move this terminal back to the dashboard's Terminals tab"
            onDragStart=${startTitleDrag}
            onDragEnd=${endTitleDrag}
          >${pane.label}</span>
          ${reattachArmed ? html`<span class="mux-drag-out-hint">Release to send this terminal back to the dashboard</span>` : null}
        ` : html`<span class="mux-pane-title">${pane.label}</span>`}
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

function PaneTab({
  pane, active, menuOpen, actions, openMenu, dragging, dropSide, onDragStart, onDragEnd,
  onDragOver, onDragLeave, onDrop, onReordered,
}) {
  const activate = (event) => {
    if (event.type === 'keydown' && event.target.closest('button')) return;
    const reorderOffset = terminalTabReorderOffset(event);
    if (reorderOffset !== null) {
      event.preventDefault();
      event.stopPropagation();
      const moved = actions.movePaneByOffset(pane.key, reorderOffset);
      if (moved) onReordered(moved);
      return;
    }
    if (event.type === 'keydown' && event.key !== 'Enter' && event.key !== ' ' && event.key !== 'Spacebar') return;
    if (event.type === 'keydown') event.preventDefault();
    actions.activatePane(pane.key);
  };
  const openContextMenu = (event) => {
    const keyboard = event.type === 'keydown';
    if (keyboard && event.key !== 'ContextMenu' && !(event.key === 'F10' && event.shiftKey)) return false;
    event.preventDefault();
    event.stopPropagation();
    const rect = event.currentTarget.getBoundingClientRect();
    openMenu({
      key: pane.key,
      label: pane.label,
      trigger: event.currentTarget,
      x: keyboard ? rect.left : event.clientX,
      y: keyboard ? rect.bottom : event.clientY,
    });
    return true;
  };
  const onKeyDown = (event) => {
    if (!openContextMenu(event)) activate(event);
  };
  return html`
    <div
      class=${`mux-tab${active ? ' active' : ''}${dragging ? ' dragging' : ''}${dropSide ? ` drop-${dropSide}` : ''}`}
      role="tab"
      tabIndex="0"
      aria-selected=${active ? 'true' : 'false'}
      aria-controls=${pane.id}
      aria-keyshortcuts="Alt+Shift+ArrowLeft Alt+Shift+ArrowRight"
      aria-describedby="terminal-tab-reorder-help"
      aria-haspopup="menu"
      aria-expanded=${menuOpen ? 'true' : 'false'}
      title="Right-click or press Shift+F10 for terminal tab actions"
      onClick=${activate}
      onKeyDown=${onKeyDown}
      onContextMenu=${openContextMenu}
      onDragOver=${(event) => onDragOver(event, pane.key)}
      onDragLeave=${(event) => onDragLeave(event, pane.key)}
      onDrop=${(event) => onDrop(event, pane.key)}
    >
      <span
        class="mux-tab-label"
        draggable="true"
        title="Drag to reorder · drag off the strip to detach into its own window · Alt+Shift+Left/Right to move with the keyboard"
        onDragStart=${(event) => onDragStart(event, pane.key)}
        onDragEnd=${onDragEnd}
      >${pane.label}</span>
      <button
        type="button"
        class="mux-tab-close"
        draggable="false"
        title="Close this terminal"
        aria-label=${`Close ${pane.label}`}
        onClick=${(event) => { event.stopPropagation(); void actions.closePane(pane.key); }}
      >×</button>
    </div>
  `;
}

function PaneContextMenu({ menu, actions, closeMenu, focusAfterAction, focusAfterDismiss }) {
  const menuRef = useRef(null);

  useLayoutEffect(() => {
    const node = menuRef.current;
    if (!node) return;
    const rect = node.getBoundingClientRect();
    const viewportWidth = Number.isFinite(window.innerWidth) ? window.innerWidth : rect.right;
    const viewportHeight = Number.isFinite(window.innerHeight) ? window.innerHeight : rect.bottom;
    const left = Math.max(4, Math.min(menu.x, viewportWidth - rect.width - 4));
    const top = Math.max(4, Math.min(menu.y, viewportHeight - rect.height - 4));
    node.style.left = `${left}px`;
    node.style.top = `${top}px`;
    node.querySelector('[role="menuitem"]')?.focus();
  }, [menu]);

  useEffect(() => {
    const onMouseDown = (event) => {
      if (!menuRef.current?.contains(event.target)) closeMenu(false);
    };
    const onKeyDown = (event) => {
      if (event.key !== 'Escape') return;
      event.preventDefault();
      closeMenu(true);
    };
    const onFocusIn = (event) => {
      if (!menuRef.current?.contains(event.target)) closeMenu(false);
    };
    document.addEventListener('mousedown', onMouseDown);
    document.addEventListener('keydown', onKeyDown);
    document.addEventListener('focusin', onFocusIn);
    return () => {
      document.removeEventListener('mousedown', onMouseDown);
      document.removeEventListener('keydown', onKeyDown);
      document.removeEventListener('focusin', onFocusIn);
    };
  }, [closeMenu]);

  const onMenuKeyDown = (event) => {
    if (event.key === 'Tab') {
      event.preventDefault();
      closeMenu(false);
      focusAfterDismiss(event.shiftKey);
      return;
    }
    const items = [...event.currentTarget.querySelectorAll('[role="menuitem"]')];
    const index = items.indexOf(document.activeElement);
    let next = null;
    if (event.key === 'ArrowDown') next = items[(index + 1) % items.length];
    else if (event.key === 'ArrowUp') next = items[(index - 1 + items.length) % items.length];
    else if (event.key === 'Home') next = items[0];
    else if (event.key === 'End') next = items.at(-1);
    if (!next) return;
    event.preventDefault();
    next.focus();
  };
  const run = (action) => {
    closeMenu(false);
    void action();
    focusAfterAction();
  };

  return html`
    <div
      ref=${menuRef}
      class="mux-tab-menu"
      role="menu"
      aria-label=${`Actions for ${menu.label}`}
      style=${{ left: `${menu.x}px`, top: `${menu.y}px` }}
      onKeyDown=${onMenuKeyDown}
    >
      <button type="button" role="menuitem" tabIndex="-1" class="mux-tab-menu-item" onClick=${() => run(() => actions.popOutPane(menu.key))}>Detach tab</button>
      <div class="mux-tab-menu-separator" role="separator"></div>
      <button type="button" role="menuitem" tabIndex="-1" class="mux-tab-menu-item" onClick=${() => run(() => actions.closePane(menu.key))}>Close tab</button>
      <button type="button" role="menuitem" tabIndex="-1" class="mux-tab-menu-item" onClick=${() => run(() => actions.closeOtherPanes(menu.key))}>Close other tabs</button>
      <button type="button" role="menuitem" tabIndex="-1" class="mux-tab-menu-item danger" onClick=${() => run(() => actions.closeAllPanes())}>Close all tabs</button>
    </div>
  `;
}

function TerminalTabs({
  state, actions, widgetFactory, onComposeMessage, composeMessageDialogKind = () => '',
  solo = false, manageTitle = false, empty = false,
}) {
  const current = state.view.value;
  const hasPanes = current.panes.length > 0;
  const shellRef = useRef(null);
  const tabsRef = useRef(null);
  const panesRef = useRef(null);
  const dragKeyRef = useRef(null);
  const droppedRef = useRef(false);
  const [dragKey, setDragKey] = useState(null);
  const [dropTarget, setDropTarget] = useState(null);
  const [reorderAnnouncement, setReorderAnnouncement] = useState('');
  const [tabMenu, setTabMenu] = useState(null);
  const [menuFocusRequest, setMenuFocusRequest] = useState(0);

  const detachArmed = useDragOutArmed(Boolean(dragKey), tabsRef);
  const clearDrag = () => {
    dragKeyRef.current = null;
    droppedRef.current = false;
    setDragKey(null);
    setDropTarget(null);
  };
  const announceReorder = ({ pane, index, count }) => {
    setReorderAnnouncement(`Moved ${pane.label} to position ${index + 1} of ${count}.`);
  };
  const startTabDrag = (event, key) => {
    event.stopPropagation();
    event.dataTransfer.setData(TERMINAL_TAB_DRAG_MIME, key);
    event.dataTransfer.effectAllowed = 'move';
    dragKeyRef.current = key;
    droppedRef.current = false;
    setDragKey(key);
    setDropTarget(null);
  };
  // Nothing in the strip accepted the drag and it ended clear of the strip:
  // treat that as "pull this terminal out of the dashboard". A drop on a
  // sibling tab (reorder) or a cancelled drag never reaches the detach.
  //
  // Dragging a terminal out asks for it out of the way, so this lands in a
  // window of its own — sized to the pane it is leaving and placed where the
  // drag was released — rather than in another tab behind the dashboard. The
  // ⧉ tab button and the tab context menu still open an ordinary tab.
  const endTabDrag = (event) => {
    const key = dragKeyRef.current;
    const detach = !droppedRef.current && key && dragLeftRegion(event, tabsRef.current);
    clearDrag();
    if (!detach) return;
    void actions.popOutPane(key, {
      detachTo: {
        size: panesRef.current?.getBoundingClientRect?.(),
        at: dragScreenPoint(event),
      },
    });
  };
  const tabDragOver = (event, targetKey) => {
    const sourceKey = dragKeyRef.current;
    if (!sourceKey || sourceKey === targetKey) {
      setDropTarget(null);
      return;
    }
    event.preventDefault();
    event.stopPropagation();
    if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
    const bounds = event.currentTarget.getBoundingClientRect();
    const side = event.clientX < bounds.left + bounds.width / 2 ? 'before' : 'after';
    if (dropTarget?.key !== targetKey || dropTarget.side !== side) {
      setDropTarget({ key: targetKey, side });
    }
  };
  const tabDragLeave = (event, targetKey) => {
    if (dropTarget?.key !== targetKey || event.currentTarget.contains(event.relatedTarget)) return;
    setDropTarget(null);
  };
  const dropTab = (event, targetKey) => {
    event.preventDefault();
    event.stopPropagation();
    droppedRef.current = true;
    const key = event.dataTransfer?.getData(TERMINAL_TAB_DRAG_MIME) || dragKeyRef.current;
    const bounds = event.currentTarget.getBoundingClientRect();
    const side = event.clientX < bounds.left + bounds.width / 2 ? 'before' : 'after';
    const moved = key && actions.reorderPane(key, targetKey, { after: side === 'after' });
    clearDrag();
    if (moved) announceReorder(moved);
  };
  const closeTabMenu = (restoreFocus = false) => {
    const trigger = tabMenu?.trigger;
    setTabMenu(null);
    if (restoreFocus) queueMicrotask(() => trigger?.focus());
  };
  const focusAfterTabMenuAction = () => setMenuFocusRequest((value) => value + 1);
  useLayoutEffect(() => {
    if (!menuFocusRequest) return;
    const activeTab = shellRef.current?.querySelector('.mux-tab[aria-selected="true"]');
    if (activeTab) activeTab.focus();
    else document.querySelector('nav [data-tab="groups"]')?.focus();
  }, [menuFocusRequest]);
  const focusAfterTabMenuDismiss = (reverse) => {
    const trigger = tabMenu?.trigger;
    queueMicrotask(() => {
      const paneControl = shellRef.current?.querySelector(
        '.mux-pane.active button:not([disabled]), .mux-pane.active input:not([disabled]), .mux-pane.active [tabindex]:not([tabindex="-1"])',
      );
      (reverse ? trigger : paneControl || trigger)?.focus();
    });
  };

  useEffect(() => {
    if (tabMenu && !current.panes.some((pane) => pane.key === tabMenu.key)) setTabMenu(null);
  }, [current.panes, tabMenu]);

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
    <div ref=${shellRef} class="terminal-shell-root">
      ${!solo ? html`
        <span id="terminal-tab-reorder-help" class="mux-tab-a11y">
          Drag tabs to reorder them, or press Alt+Shift+Left Arrow or Alt+Shift+Right Arrow on a focused tab.
          Drag a tab off the strip to detach it into its own window, or use the tab's context menu to detach it into a browser tab.
        </span>
        <span class="mux-tab-a11y" role="status" aria-live="polite" aria-atomic="true">${reorderAnnouncement}</span>
        <div ref=${tabsRef} class=${`mux-tabs${detachArmed ? ' drag-out-armed' : ''}`} role="tablist" aria-label="Open terminals">
          ${current.panes.map((pane) => html`
            <${PaneTab}
              key=${pane.key}
              pane=${pane}
              active=${current.activeKey === pane.key}
              menuOpen=${tabMenu?.key === pane.key}
              actions=${actions}
              openMenu=${setTabMenu}
              dragging=${dragKey === pane.key}
              dropSide=${dropTarget?.key === pane.key ? dropTarget.side : ''}
              onDragStart=${startTabDrag}
              onDragEnd=${endTabDrag}
              onDragOver=${tabDragOver}
              onDragLeave=${tabDragLeave}
              onDrop=${dropTab}
              onReordered=${announceReorder}
            />
          `)}
        </div>
        ${detachArmed ? html`<div class="mux-drag-out-hint">Release to detach this terminal into its own window</div>` : null}
      ` : null}
      ${tabMenu ? html`<${PaneContextMenu} menu=${tabMenu} actions=${actions} closeMenu=${closeTabMenu} focusAfterAction=${focusAfterTabMenuAction} focusAfterDismiss=${focusAfterTabMenuDismiss} />` : null}
      ${hasPanes || !empty ? html`
        <div ref=${panesRef} class="mux-panes">
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
