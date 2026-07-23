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
import { MAX_TERMINAL_GROUP_NAME_LENGTH } from './terminal-shell-state.js';

const html = htm.bind(h);
const INTERACTION_HINT = 'Select: Option-drag (macOS) / Shift-drag (Linux/Windows) · Copy: Ctrl/Cmd+Shift+C';
const TERMINAL_TAB_DRAG_MIME = 'application/x-tclaude-terminal-tab';
const TERMINAL_GROUP_DRAG_MIME = 'application/x-tclaude-terminal-group';
// A pointer click on a group pill waits this long before collapsing so a
// double-click (rename) can cancel it. Roughly the platform double-click
// threshold; keyboard clicks bypass it entirely.
export const GROUP_COLLAPSE_CLICK_DELAY_MS = 200;

export function terminalTabReorderOffset(event) {
  if (event?.type !== 'keydown' || event.target?.closest?.('button')) return null;
  if (!event.shiftKey || !event.altKey) return null;
  if (event.key === 'ArrowLeft') return -1;
  if (event.key === 'ArrowRight') return 1;
  return null;
}

// The whole tab is the drag handle, so a drag whose gesture started on the
// close button has to be declined — otherwise pressing × and moving would drag
// the tab (a browser drags the nearest draggable ancestor even from a
// draggable=false child). This predicate is the guard; it is exported so it can
// be unit-tested against real rendered nodes, because the test DOM cannot
// deliver a bubbled/target-spoofed dragstart to the tab's own listener.
export function tabDragStartedOnClose(target) {
  return Boolean(target?.closest?.('.mux-tab-close'));
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
//
// The listener also accepts the drag across the rest of the page. A browser
// draws the "no drop" cursor over anything that has not called preventDefault
// on dragover, so without this the gesture spent its whole life under a
// prohibition sign while being perfectly willing to fire on release. Accepting
// is scoped to a live terminal drag and never overrides a target that already
// claimed the event, so the tab strip's own reorder edges keep their meaning.
function useDragOutArmed(active, regionRef) {
  const [armed, setArmed] = useState(false);
  useEffect(() => {
    if (!active) {
      setArmed(false);
      return undefined;
    }
    const onDragOver = (event) => {
      // Arming first is deliberate: it answers "what happens if you release
      // here", which is pure geometry. Whether some other target claimed the
      // event has no bearing on that — the gesture fires from dragend either
      // way — so skipping the hint for a claimed dragover would hide a detach
      // that is still going to happen. The guard below is about a different
      // question: whose dropEffect wins.
      setArmed(dragLeftRegion(event, regionRef.current));
      if (event.defaultPrevented) return;
      event.preventDefault();
      if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
    };
    // Accepting the drag means a drop now lands here on release, so consume it:
    // the gesture is decided on dragend, and an unclaimed drop would otherwise
    // be offered to whatever else is listening.
    const onDrop = (event) => {
      if (!event.defaultPrevented) event.preventDefault();
    };
    document.addEventListener('dragover', onDragOver);
    document.addEventListener('drop', onDrop);
    return () => {
      document.removeEventListener('dragover', onDragOver);
      document.removeEventListener('drop', onDrop);
    };
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
          ${reattachArmed ? html`<span class="mux-drag-out-hint">Release anywhere — even outside the browser — to send this terminal back to the dashboard</span>` : null}
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
  pane, active, menuOpen, groupId = null, actions, openMenu, dragging, dropSide,
  onDragStart, onDragEnd, onDragOver, onDragLeave, onDrop, onReordered,
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
      kind: 'pane',
      key: pane.key,
      groupId,
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
  // The whole tab is the drag handle, not just the label, so the grab target is
  // the full tab. A drag that starts on the close button is cancelled, so the ×
  // stays a click target and never begins a tab drag.
  const startDrag = (event) => {
    if (tabDragStartedOnClose(event.target)) {
      event.preventDefault();
      return;
    }
    onDragStart(event, pane.key);
  };
  return html`
    <div
      class=${`mux-tab${active ? ' active' : ''}${dragging ? ' dragging' : ''}${dropSide ? ` drop-${dropSide}` : ''}`}
      role="tab"
      data-pane-key=${pane.key}
      tabIndex="0"
      draggable="true"
      aria-selected=${active ? 'true' : 'false'}
      aria-controls=${pane.id}
      aria-keyshortcuts="Alt+Shift+ArrowLeft Alt+Shift+ArrowRight"
      aria-describedby="terminal-tab-reorder-help"
      aria-haspopup="menu"
      aria-expanded=${menuOpen ? 'true' : 'false'}
      title="Drag to reorder · drop onto another tab to group them · drag off the strip to detach · Alt+Shift+Left/Right to move · Right-click or Shift+F10 for actions"
      onClick=${activate}
      onKeyDown=${onKeyDown}
      onContextMenu=${openContextMenu}
      onDragStart=${startDrag}
      onDragEnd=${onDragEnd}
      onDragOver=${(event) => onDragOver(event, pane.key)}
      onDragLeave=${(event) => onDragLeave(event, pane.key)}
      onDrop=${(event) => onDrop(event, pane.key)}
    >
      <span class="mux-tab-label">${pane.label}</span>
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

// GroupStack renders one tab stack: a pill that names and collapses the stack,
// followed by its member tabs. The pill is the stack's drop target — releasing
// a tab on it joins the stack — and its own context-menu anchor.
//
// The wrapper carries role="group" inside the strip's tablist so assistive tech
// announces "N tabs, <name>" around the members instead of a flat run. The pill
// itself is a button, not a tab: it selects nothing, it only opens and closes.
function GroupStack({
  group, panes, activeKey, actions, renaming, onRenameStart, onRenameCommit, onRenameCancel,
  openMenu, menuOpen, dropActive, dragging, onPillDragOver, onPillDragLeave, onPillDrop,
  onPillDragStart, onPillDragEnd, onKeyboardMove, renderTab,
}) {
  const inputRef = useRef(null);
  // A single click collapses, a double click renames — the two share the pill.
  // Collapsing is NOT a pure toggle: collapsing a group that owns the active
  // terminal deliberately moves activation outside the group, and expanding
  // does not move it back. So the collapse cannot fire on the first click of a
  // double-click and be "undone" — that would silently switch the active
  // terminal. Instead a pointer click ARMS the collapse on a short timer that a
  // second click (the double-click) cancels, so renaming never collapses at
  // all. A keyboard-synthesised click (Enter/Space, detail 0) has no
  // double-click to wait for, so it collapses immediately. F2 renames.
  const collapseTimer = useRef(null);
  const clearCollapseTimer = () => {
    if (collapseTimer.current) {
      clearTimeout(collapseTimer.current);
      collapseTimer.current = null;
    }
  };
  useEffect(() => clearCollapseTimer, []);
  const onPillClick = (event) => {
    if (event.detail === 0) {
      actions.toggleGroupCollapsed(group.id);
      return;
    }
    clearCollapseTimer();
    collapseTimer.current = setTimeout(() => {
      collapseTimer.current = null;
      actions.toggleGroupCollapsed(group.id);
    }, GROUP_COLLAPSE_CLICK_DELAY_MS);
  };
  const startRename = () => {
    clearCollapseTimer();
    onRenameStart(group.id);
  };
  const renameOnDblClick = (event) => {
    event.preventDefault();
    event.stopPropagation();
    startRename();
  };
  // onBlur commits, so a cancel must first mark itself: browsers do not fire
  // blur when a focused node is removed today, but any future close that moves
  // focus before unmount would otherwise turn Escape into a commit. The ref is
  // the guard that keeps Escape meaning "discard" regardless of blur timing.
  const cancelledRef = useRef(false);
  useLayoutEffect(() => {
    if (!renaming) return undefined;
    cancelledRef.current = false;
    inputRef.current?.focus();
    // Selecting the existing name makes retyping it the default gesture.
    // Guarded because the test DOM implements focus() but not select().
    if (typeof inputRef.current?.select === 'function') inputRef.current.select();
    return undefined;
  }, [renaming]);
  const cancelRename = () => {
    cancelledRef.current = true;
    onRenameCancel();
  };
  const commitRename = (value) => {
    if (cancelledRef.current) return;
    onRenameCommit(group.id, value);
  };
  // A collapsed stack still shows the terminal the operator is looking at when
  // there is nowhere outside the stack for activation to move to — collapsing
  // must never leave the strip with no visible active tab.
  const visible = group.collapsed ? panes.filter((pane) => pane.key === activeKey) : panes;
  const openMenuAt = (event) => {
    const keyboard = event.type === 'keydown';
    if (keyboard && event.key !== 'ContextMenu' && !(event.key === 'F10' && event.shiftKey)) return false;
    event.preventDefault();
    event.stopPropagation();
    const rect = event.currentTarget.getBoundingClientRect();
    openMenu({
      kind: 'group',
      id: group.id,
      label: group.name,
      collapsed: group.collapsed,
      trigger: event.currentTarget,
      x: keyboard ? rect.left : event.clientX,
      y: keyboard ? rect.bottom : event.clientY,
    });
    return true;
  };
  const onPillKeyDown = (event) => {
    if (event.key === 'F2') {
      event.preventDefault();
      event.stopPropagation();
      startRename();
      return;
    }
    // Alt+Shift+Left/Right moves the whole stack among its neighbours — the
    // keyboard mirror of dragging the pill.
    if (event.altKey && event.shiftKey && (event.key === 'ArrowLeft' || event.key === 'ArrowRight')) {
      event.preventDefault();
      event.stopPropagation();
      onKeyboardMove(group.id, event.key === 'ArrowRight' ? 1 : -1);
      return;
    }
    openMenuAt(event);
  };
  return html`
    <div
      class=${`mux-tab-group mux-group-${group.color}${group.collapsed ? ' collapsed' : ''}${dropActive ? ' drop-into' : ''}${dragging ? ' dragging' : ''}`}
      role="group"
      aria-label=${`${group.name} — ${panes.length} terminal${panes.length === 1 ? '' : 's'}`}
      onDragOver=${(event) => onPillDragOver(event, group.id)}
      onDragLeave=${(event) => onPillDragLeave(event, group.id)}
      onDrop=${(event) => onPillDrop(event, group.id)}
    >
      ${renaming ? html`
        <input
          ref=${inputRef}
          class="mux-group-rename"
          type="text"
          maxLength=${MAX_TERMINAL_GROUP_NAME_LENGTH}
          aria-label=${`Rename terminal group ${group.name}`}
          value=${group.name}
          onKeyDown=${(event) => {
            event.stopPropagation();
            if (event.key === 'Enter') commitRename(event.currentTarget.value);
            else if (event.key === 'Escape') cancelRename();
          }}
          onBlur=${(event) => commitRename(event.currentTarget.value)}
        />
      ` : html`
        <button
          type="button"
          class="mux-group-pill"
          draggable="true"
          data-group-id=${group.id}
          aria-expanded=${group.collapsed ? 'false' : 'true'}
          aria-haspopup="menu"
          aria-describedby="terminal-group-pill-help"
          aria-keyshortcuts="Alt+Shift+ArrowLeft Alt+Shift+ArrowRight"
          title=${`Drag to move the whole ${group.name} group · click to ${group.collapsed ? 'expand' : 'collapse'} · double-click or F2 to rename · right-click or Shift+F10 for group actions`}
          onClick=${onPillClick}
          onDblClick=${renameOnDblClick}
          onContextMenu=${openMenuAt}
          onKeyDown=${onPillKeyDown}
          onDragStart=${(event) => onPillDragStart(event, group.id)}
          onDragEnd=${onPillDragEnd}
          aria-label=${`${group.name}, ${panes.length} terminal${panes.length === 1 ? '' : 's'}`}
          data-menu-open=${menuOpen ? 'true' : 'false'}
        >
          <span class="mux-group-caret" aria-hidden="true">${group.collapsed ? '▸' : '▾'}</span>
          <span class="mux-group-name">${group.name}</span>
          <span class="mux-group-count" aria-hidden="true">${panes.length}</span>
        </button>
      `}
      ${visible.map((pane) => renderTab(pane))}
    </div>
  `;
}

// StripGap is a thin drop target at a group boundary — the one place a plain
// tab drop cannot land, because the only tab to drop onto there belongs to the
// stack and dropping on it would join.
//
// It is ALWAYS rendered, even at rest, and only reveals itself (via the
// `active` class → width) while a drag is in flight. Rendering it conditionally
// would insert a node next to the group the moment a drag begins; that
// insertion makes Preact reconcile — and recreate — the neighbouring group
// subtree, and recreating the DOM node the browser just picked up as a drag
// source silently aborts the native drag. Grabbing a grouped tab then "did
// nothing" most of the time. Keeping the node present and toggling a class
// never disturbs the drag source.
function StripGap({ active, armed, grow = false, onDragOver, onDragLeave, onDrop }) {
  return html`
    <div
      class=${`mux-strip-gap${grow ? ' grow' : ''}${active ? ' active' : ''}${armed ? ' armed' : ''}`}
      aria-hidden="true"
      onDragOver=${onDragOver}
      onDragLeave=${onDragLeave}
      onDrop=${onDrop}
    ></div>
  `;
}

function PaneContextMenu({
  menu, actions, groups, closeMenu, focusAfterAction, focusAfterDismiss, onRenameGroup,
}) {
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
  // Most commands hand focus back to the strip. The rename commands must not:
  // they hand focus to the rename input the stack is about to render, and the
  // parent's refocus would blur it — committing the edit before it is typed.
  const run = (action, { refocus = true } = {}) => {
    closeMenu(false);
    void action();
    if (refocus) focusAfterAction();
  };

  const item = (label, action, { className = 'mux-tab-menu-item', refocus = true } = {}) => html`
    <button type="button" role="menuitem" tabIndex="-1" class=${className} onClick=${() => run(action, { refocus })}>${label}</button>
  `;
  const separator = html`<div class="mux-tab-menu-separator" role="separator"></div>`;
  // The grouping commands live in this menu rather than only on the drag
  // gesture so every one of them is reachable from the keyboard (Shift+F10 on a
  // focused tab), which drag-and-drop alone can never be.
  const groupItems = menu.kind === 'group' ? [
    item(menu.collapsed ? 'Expand group' : 'Collapse group', () => actions.toggleGroupCollapsed(menu.id)),
    item('Rename group…', () => onRenameGroup(menu.id), { refocus: false }),
    item('Ungroup tabs', () => actions.dissolveGroup(menu.id)),
    separator,
    item('Close tabs in group', () => actions.closeGroupPanes(menu.id), { className: 'mux-tab-menu-item danger' }),
  ] : [
    item('New group from this tab', () => {
      const group = actions.createGroup({ keys: [menu.key] });
      if (group) onRenameGroup(group.id);
    }, { refocus: false }),
    ...groups
      .filter((group) => group.id !== menu.groupId)
      .map((group) => item(`Add to “${group.name}”`, () => actions.assignPaneToGroup(menu.key, group.id))),
    ...(menu.groupId ? [item('Remove from group', () => actions.assignPaneToGroup(menu.key, null))] : []),
    separator,
    item('Detach tab', () => actions.popOutPane(menu.key)),
    separator,
    item('Close tab', () => actions.closePane(menu.key)),
    item('Close other tabs', () => actions.closeOtherPanes(menu.key)),
    item('Close all tabs', () => actions.closeAllPanes(), { className: 'mux-tab-menu-item danger' }),
  ];

  return html`
    <div
      ref=${menuRef}
      class="mux-tab-menu"
      role="menu"
      aria-label=${`Actions for ${menu.label}`}
      style=${{ left: `${menu.x}px`, top: `${menu.y}px` }}
      onKeyDown=${onMenuKeyDown}
    >${groupItems}</div>
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
  const dragGroupIdRef = useRef(null);
  const droppedRef = useRef(false);
  const [dragKey, setDragKey] = useState(null);
  const [dragGroupId, setDragGroupId] = useState(null);
  const [dropTarget, setDropTarget] = useState(null);
  const [reorderAnnouncement, setReorderAnnouncement] = useState('');
  const [tabMenu, setTabMenu] = useState(null);
  const [menuFocusRequest, setMenuFocusRequest] = useState(0);
  const [renamingGroup, setRenamingGroup] = useState(null);
  const [groupDropTarget, setGroupDropTarget] = useState(null);
  const [stripGap, setStripGap] = useState(null);

  const detachArmed = useDragOutArmed(Boolean(dragKey), tabsRef);
  const clearDrag = () => {
    dragKeyRef.current = null;
    dragGroupIdRef.current = null;
    droppedRef.current = false;
    setDragKey(null);
    setDragGroupId(null);
    setDropTarget(null);
    setGroupDropTarget(null);
    setStripGap(null);
  };
  // The live region has to say where a tab landed AND which stack it landed in,
  // because a keyboard move at a stack edge changes both at once.
  const announceReorder = ({ pane, index, count, group, leftGroup }) => {
    const where = group ? ` in group ${group.name}`
      : leftGroup ? `, out of group ${leftGroup.name}` : '';
    setReorderAnnouncement(`Moved ${pane.label} to position ${index + 1} of ${count}${where}.`);
  };
  const groupIdOfKey = (key) => {
    const segment = current.segments.find((candidate) => candidate.panes.some((pane) => pane.key === key));
    return segment?.type === 'group' ? segment.group.id : null;
  };
  const labelOfKey = (key) => current.panes.find((pane) => pane.key === key)?.label || 'terminal';
  // A tab has three drop zones: the outer quarter on each side reorders the
  // dragged tab before/after it, and the centre half combines the two into a
  // group. Dropping a tab on another tab that is not yet in a group is the
  // discoverable way to CREATE a group — the same gesture Chrome uses — so it
  // no longer requires finding the context menu first. When the two tabs are
  // already in the same group the centre would do nothing, so it degrades to a
  // plain before/after reorder instead of highlighting a no-op.
  const tabDropSide = (event, element, sourceKey, targetKey) => {
    const bounds = element.getBoundingClientRect();
    const fraction = bounds.width ? (event.clientX - bounds.left) / bounds.width : 0.5;
    const half = fraction < 0.5 ? 'before' : 'after';
    if (fraction < 0.25 || fraction > 0.75) return half;
    const targetGroup = groupIdOfKey(targetKey);
    return targetGroup && targetGroup === groupIdOfKey(sourceKey) ? half : 'group';
  };
  const focusPaneTab = (key) => {
    for (const tab of shellRef.current?.querySelectorAll('.mux-tab[data-pane-key]') || []) {
      if (tab.getAttribute('data-pane-key') === key) { tab.focus(); return; }
    }
  };
  // A keyboard move can carry a tab across a group boundary, which changes its
  // DOM parent (group wrapper ⇄ strip), so Preact recreates the node and focus
  // falls to <body>. Put focus back on the tab the operator is still moving,
  // after the render that reparented it — otherwise the next arrow keypress is
  // swallowed. Drops keep the pointer and need no such repair.
  const announceAndRefocus = (moved) => {
    announceReorder(moved);
    queueMicrotask(() => focusPaneTab(moved.pane.key));
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
  // A stack moves as one block. Dragging its pill is a distinct gesture from
  // dragging a member tab (which reorders that one tab), so it carries its own
  // drag id; the drop handlers below route to moveGroup instead of reorderPane
  // when a group drag is in flight. A group is never detached off the strip.
  const announceGroupMove = ({ group, index, count }) => {
    setReorderAnnouncement(
      `Moved group ${group.name} (${count} terminal${count === 1 ? '' : 's'}) to position ${index + 1}.`,
    );
  };
  const focusGroupPill = (groupID) => {
    for (const pill of shellRef.current?.querySelectorAll('.mux-group-pill[data-group-id]') || []) {
      if (pill.getAttribute('data-group-id') === groupID) { pill.focus(); return; }
    }
  };
  const announceGroupAndRefocus = (moved) => {
    announceGroupMove(moved);
    queueMicrotask(() => focusGroupPill(moved.group.id));
  };
  const moveGroupKeyboard = (groupID, offset) => {
    const moved = actions.moveGroupByOffset(groupID, offset);
    if (moved) announceGroupAndRefocus(moved);
  };
  const startGroupDrag = (event, groupID) => {
    event.stopPropagation();
    event.dataTransfer.setData(TERMINAL_GROUP_DRAG_MIME, groupID);
    event.dataTransfer.effectAllowed = 'move';
    dragGroupIdRef.current = groupID;
    dragKeyRef.current = null;
    droppedRef.current = false;
    setDragGroupId(groupID);
    setDragKey(null);
    setDropTarget(null);
  };
  const endGroupDrag = () => clearDrag();
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
  // A before/after split, no centre group zone — used while dragging a whole
  // stack, which drops next to a tab rather than combining with it.
  const beforeAfter = (event, element) => {
    const bounds = element.getBoundingClientRect();
    return event.clientX < bounds.left + bounds.width / 2 ? 'before' : 'after';
  };
  const tabDragOver = (event, targetKey) => {
    const groupDrag = dragGroupIdRef.current;
    if (groupDrag) {
      // A stack cannot land on one of its own tabs.
      if (groupIdOfKey(targetKey) === groupDrag) { setDropTarget(null); return; }
      event.preventDefault();
      event.stopPropagation();
      if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
      const side = beforeAfter(event, event.currentTarget);
      if (dropTarget?.key !== targetKey || dropTarget.side !== side) setDropTarget({ key: targetKey, side });
      return;
    }
    const sourceKey = dragKeyRef.current;
    if (!sourceKey || sourceKey === targetKey) {
      setDropTarget(null);
      return;
    }
    event.preventDefault();
    event.stopPropagation();
    if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
    const side = tabDropSide(event, event.currentTarget, sourceKey, targetKey);
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
    const groupDrag = dragGroupIdRef.current;
    if (groupDrag) {
      const moved = groupIdOfKey(targetKey) !== groupDrag
        && actions.moveGroup(groupDrag, targetKey, { after: beforeAfter(event, event.currentTarget) === 'after' });
      clearDrag();
      if (moved) announceGroupMove(moved);
      return;
    }
    const key = event.dataTransfer?.getData(TERMINAL_TAB_DRAG_MIME) || dragKeyRef.current;
    // A tab dropped on itself is a no-op. tabDragOver already declines to accept
    // a self-hover, so a real browser never fires this drop; the guard keeps a
    // future unconditional-accept change from building a singleton group from
    // keys:[X, X].
    if (!key || key === targetKey) { clearDrag(); return; }
    const side = tabDropSide(event, event.currentTarget, key, targetKey);
    if (side === 'group') {
      // Centre drop: combine the two tabs. If the target already has a group,
      // the dragged tab joins it; otherwise a new group is born around both.
      const existing = groupIdOfKey(targetKey);
      if (existing) {
        const joined = actions.assignPaneToGroup(key, existing);
        const group = joined && current.groups.find((candidate) => candidate.id === existing);
        if (group) setReorderAnnouncement(`Grouped ${labelOfKey(key)} into ${group.name}.`);
      } else {
        const group = actions.createGroup({ keys: [targetKey, key] });
        if (group) {
          setReorderAnnouncement(
            `Grouped ${labelOfKey(key)} with ${labelOfKey(targetKey)} as ${group.name}.`,
          );
        }
      }
      clearDrag();
      return;
    }
    const moved = actions.reorderPane(key, targetKey, { after: side === 'after' });
    clearDrag();
    if (moved) announceReorder(moved);
  };
  // Dropping ON a stack (its pill or its padding) joins the stack at the end.
  // Dropping on a member TAB is handled by that tab and adopts its membership,
  // so the same gesture covers "join here" and "join at this exact position".
  const groupMembers = (groupID) =>
    current.segments.find((segment) => segment.type === 'group' && segment.group.id === groupID)?.panes || [];
  const groupDragOver = (event, groupID) => {
    const groupDrag = dragGroupIdRef.current;
    if (groupDrag) {
      // Dropping one stack on another places it beside that stack, never inside.
      if (groupDrag === groupID || event.defaultPrevented) return;
      event.preventDefault();
      if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
      if (groupDropTarget !== groupID) setGroupDropTarget(groupID);
      return;
    }
    const sourceKey = dragKeyRef.current;
    if (!sourceKey || event.defaultPrevented) return;
    event.preventDefault();
    if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
    if (groupDropTarget !== groupID) setGroupDropTarget(groupID);
  };
  const groupDragLeave = (event, groupID) => {
    if (groupDropTarget !== groupID || event.currentTarget.contains(event.relatedTarget)) return;
    setGroupDropTarget(null);
  };
  const dropOnGroup = (event, groupID) => {
    if (event.defaultPrevented) return;
    event.preventDefault();
    event.stopPropagation();
    droppedRef.current = true;
    const groupDrag = dragGroupIdRef.current;
    if (groupDrag) {
      const anchor = groupMembers(groupID)[0]?.key;
      const moved = groupDrag !== groupID && anchor
        && actions.moveGroup(groupDrag, anchor, { after: beforeAfter(event, event.currentTarget) === 'after' });
      clearDrag();
      if (moved) announceGroupMove(moved);
      return;
    }
    const key = event.dataTransfer?.getData(TERMINAL_TAB_DRAG_MIME) || dragKeyRef.current;
    const pane = key && current.panes.find((candidate) => candidate.key === key);
    const joined = key && actions.assignPaneToGroup(key, groupID);
    clearDrag();
    const group = joined && current.groups.find((candidate) => candidate.id === groupID);
    if (group && pane) setReorderAnnouncement(`Moved ${pane.label} into group ${group.name}.`);
  };
  // Inter-stack drop gaps. A drop adopts the target's membership, and at a
  // group boundary the only thing to drop onto is a member — which would join.
  // So the position just before a leading stack, or between two adjacent
  // stacks, is unreachable by drag (the keyboard hop covers it). These thin
  // gaps at the group boundaries close that hole: a drop parks the tab there,
  // ungrouped, beside the stack. `gap` is `${groupID}:before|after`.
  const gapDragOver = (event, gap) => {
    if ((!dragKeyRef.current && !dragGroupIdRef.current) || event.defaultPrevented) return;
    event.preventDefault();
    event.stopPropagation();
    if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
    if (stripGap !== gap) setStripGap(gap);
  };
  const gapDragLeave = (event, gap) => {
    if (stripGap !== gap || event.currentTarget.contains(event.relatedTarget)) return;
    setStripGap(null);
  };
  const dropOnGap = (event, groupID, after) => {
    event.preventDefault();
    event.stopPropagation();
    droppedRef.current = true;
    const groupDrag = dragGroupIdRef.current;
    if (groupDrag) {
      // A group boundary gap places the dragged stack just before/after the
      // gap's stack.
      const members = groupMembers(groupID);
      const anchor = after ? members.at(-1)?.key : members[0]?.key;
      const moved = groupDrag !== groupID && anchor && actions.moveGroup(groupDrag, anchor, { after });
      clearDrag();
      if (moved) announceGroupMove(moved);
      return;
    }
    const key = event.dataTransfer?.getData(TERMINAL_TAB_DRAG_MIME) || dragKeyRef.current;
    const moved = key && actions.movePaneOutsideGroup(key, groupID, { after });
    clearDrag();
    if (moved) announceReorder(moved);
  };
  // The strip's empty right margin is one large drop zone. Releasing anywhere
  // past the last tab or group lands the drag at the very end — the whole
  // far-right area counts as the right edge of the furthest-right segment, not
  // just that segment's own right quarter. The zone grows to fill the free
  // space during a drag (see .mux-strip-gap.grow) and resolves to the last
  // segment here, so it covers a trailing plain tab and a trailing group alike.
  const lastSegment = () => current.segments.at(-1);
  // Dropping the furthest-right segment onto the end is a no-op — a tab already
  // last, or the group being dragged. Skip arming so the marker never promises
  // a move that will not happen.
  const endZoneActive = () => {
    const segment = lastSegment();
    if (!segment) return false;
    const groupDrag = dragGroupIdRef.current;
    if (groupDrag) return !(segment.type === 'group' && segment.group.id === groupDrag);
    const key = dragKeyRef.current;
    return Boolean(key) && !(segment.type === 'pane' && segment.pane.key === key);
  };
  const endZoneDragOver = (event) => {
    if ((!dragKeyRef.current && !dragGroupIdRef.current) || event.defaultPrevented) return;
    if (!endZoneActive()) { if (stripGap === 'end') setStripGap(null); return; }
    event.preventDefault();
    event.stopPropagation();
    if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
    if (stripGap !== 'end') setStripGap('end');
  };
  const endZoneDragLeave = (event) => {
    if (stripGap !== 'end' || event.currentTarget.contains(event.relatedTarget)) return;
    setStripGap(null);
  };
  const dropOnEnd = (event) => {
    event.preventDefault();
    event.stopPropagation();
    droppedRef.current = true;
    const segment = lastSegment();
    const groupDrag = dragGroupIdRef.current;
    if (groupDrag) {
      const anchor = segment?.panes.at(-1)?.key;
      const moved = anchor && !(segment.type === 'group' && segment.group.id === groupDrag)
        && actions.moveGroup(groupDrag, anchor, { after: true });
      clearDrag();
      if (moved) announceGroupMove(moved);
      return;
    }
    const key = event.dataTransfer?.getData(TERMINAL_TAB_DRAG_MIME) || dragKeyRef.current;
    if (!key || !segment) { clearDrag(); return; }
    const moved = segment.type === 'group'
      ? actions.movePaneOutsideGroup(key, segment.group.id, { after: true })
      : actions.reorderPane(key, segment.pane.key, { after: true });
    clearDrag();
    if (moved) announceReorder(moved);
  };
  const commitGroupRename = (groupID, name) => {
    setRenamingGroup(null);
    actions.renameGroup(groupID, name);
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

  // A menu anchored to something that is gone — a closed tab, or a stack whose
  // last member closed — has nothing left to act on.
  useEffect(() => {
    if (!tabMenu) return;
    const alive = tabMenu.kind === 'group'
      ? current.segments.some((segment) => segment.type === 'group' && segment.group.id === tabMenu.id)
      : current.panes.some((pane) => pane.key === tabMenu.key);
    if (!alive) setTabMenu(null);
  }, [current.panes, current.segments, tabMenu]);

  useEffect(() => {
    if (renamingGroup && !current.groups.some((group) => group.id === renamingGroup)) setRenamingGroup(null);
  }, [current.groups, renamingGroup]);

  // Collapse and expand happen from the pill, the group menu, and implicitly
  // when a collapsed stack's member is activated — three paths, one place to
  // announce them. Diffing the collapsed state across renders covers all of
  // them without each caller having to remember to speak. Focus is often off
  // the pill (menu just closed) when it changes, so aria-expanded alone is not
  // enough for a screen reader.
  const collapsedRef = useRef(new Map());
  useEffect(() => {
    const previous = collapsedRef.current;
    const next = new Map();
    for (const group of current.groups) {
      next.set(group.id, group.collapsed);
      if (previous.has(group.id) && previous.get(group.id) !== group.collapsed) {
        setReorderAnnouncement(`${group.name} group ${group.collapsed ? 'collapsed' : 'expanded'}.`);
      }
    }
    collapsedRef.current = next;
  }, [current.groups]);

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
          Tabs can be collected into named groups: drop one tab onto the middle of another to group them, use a tab's context menu to start
          or join a group, drop a tab on a group to join it, and press Alt+Shift+Left Arrow or Alt+Shift+Right Arrow at the edge of a group
          to move the tab out of it.
          A group's pill collapses and expands the group; double-click it or press F2 to rename, and it carries its own context menu for
          renaming, ungrouping, and closing its tabs.
        </span>
        <span id="terminal-group-pill-help" class="mux-tab-a11y">
          Enter or Space collapses or expands this group. Press F2 to rename it, or Shift+F10 for its actions menu.
          Drag this pill, or press Alt+Shift+Left Arrow or Alt+Shift+Right Arrow, to move the whole group among the other tabs.
        </span>
        <span class="mux-tab-a11y" role="status" aria-live="polite" aria-atomic="true">${reorderAnnouncement}</span>
        <div ref=${tabsRef} class=${`mux-tabs${detachArmed ? ' drag-out-armed' : ''}`} role="tablist" aria-label="Open terminals">
          ${current.segments.map((segment) => {
            const renderTab = (pane) => html`
              <${PaneTab}
                key=${pane.key}
                pane=${pane}
                active=${current.activeKey === pane.key}
                menuOpen=${tabMenu?.kind !== 'group' && tabMenu?.key === pane.key}
                groupId=${segment.type === 'group' ? segment.group.id : null}
                actions=${actions}
                openMenu=${setTabMenu}
                dragging=${dragKey === pane.key}
                dropSide=${dropTarget?.key === pane.key ? dropTarget.side : ''}
                onDragStart=${startTabDrag}
                onDragEnd=${endTabDrag}
                onDragOver=${tabDragOver}
                onDragLeave=${tabDragLeave}
                onDrop=${dropTab}
                onReordered=${announceAndRefocus}
              />
            `;
            if (segment.type !== 'group') return renderTab(segment.pane);
            const gid = segment.group.id;
            const dragging = Boolean(dragKey) || Boolean(dragGroupId);
            // A leading gap is only reachable — and only needed — when this
            // stack is not the very first segment, or when it is: the position
            // before a leading stack is exactly the unreachable one. The
            // position after the last segment is covered by the strip's trailing
            // end-zone (rendered after this map), so no per-group trailing gap is
            // needed. Redundant gaps elsewhere are harmless but omitted to keep
            // the strip quiet.
            const leadingGap = html`
              <${StripGap}
                key=${`${gid}:gap-before`}
                active=${dragging}
                armed=${stripGap === `${gid}:before`}
                onDragOver=${(event) => gapDragOver(event, `${gid}:before`)}
                onDragLeave=${(event) => gapDragLeave(event, `${gid}:before`)}
                onDrop=${(event) => dropOnGap(event, gid, false)}
              />`;
            return html`
              ${leadingGap}
              <${GroupStack}
                key=${segment.key}
                group=${segment.group}
                panes=${segment.panes}
                activeKey=${current.activeKey}
                actions=${actions}
                renaming=${renamingGroup === gid}
                onRenameStart=${setRenamingGroup}
                onRenameCommit=${commitGroupRename}
                onRenameCancel=${() => setRenamingGroup(null)}
                openMenu=${setTabMenu}
                menuOpen=${tabMenu?.kind === 'group' && tabMenu.id === gid}
                dropActive=${groupDropTarget === gid}
                dragging=${dragGroupId === gid}
                onPillDragOver=${groupDragOver}
                onPillDragLeave=${groupDragLeave}
                onPillDrop=${dropOnGroup}
                onPillDragStart=${startGroupDrag}
                onPillDragEnd=${endGroupDrag}
                onKeyboardMove=${moveGroupKeyboard}
                renderTab=${renderTab}
              />
            `;
          })}
          ${hasPanes ? html`
            <${StripGap}
              key="strip-end"
              grow=${true}
              active=${Boolean(dragKey) || Boolean(dragGroupId)}
              armed=${stripGap === 'end'}
              onDragOver=${endZoneDragOver}
              onDragLeave=${endZoneDragLeave}
              onDrop=${dropOnEnd}
            />` : null}
        </div>
        ${detachArmed ? html`<div class="mux-drag-out-hint">Release anywhere — even outside the browser — to detach this terminal into its own window</div>` : null}
      ` : null}
      ${tabMenu ? html`<${PaneContextMenu} menu=${tabMenu} actions=${actions} groups=${current.groups}
        closeMenu=${closeTabMenu} focusAfterAction=${focusAfterTabMenuAction}
        focusAfterDismiss=${focusAfterTabMenuDismiss} onRenameGroup=${setRenamingGroup} />` : null}
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

export {
  GroupStack, OpaqueTerminalHost, PaneContextMenu, PaneTab, TerminalBadge, TerminalModal,
  TerminalModalSession, TerminalPane, TerminalTabs,
};
