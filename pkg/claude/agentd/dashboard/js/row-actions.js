// row-actions.js — stateless delegated routing for live data-act producers.
//
// The document listener is only an integration adapter: it validates that an
// event came from the current document, freezes that producer, and delegates
// the operation. It owns no presentation or operation state.

import { handleRowAction } from './row-action-handler.js';

const BACKGROUND_CONTEXT_ACTIONS = new Set(['jump', 'web-open-window']);

export function liveActionSource(event, selector = '[data-act]') {
  const source = event?.target?.closest?.(selector);
  if (!source || source.ownerDocument !== document || !source.isConnected) return null;
  return document.documentElement.contains(source) ? source : null;
}

export function actionDescriptor(source, event = null) {
  return Object.freeze({
    producerId: source.id || '',
    openInBackground: Boolean(event?.ctrlKey || event?.metaKey),
    data: Object.freeze({ ...source.dataset }),
  });
}

let rowActionsCleanup = null;
export function bindRowActions() {
  if (rowActionsCleanup) return rowActionsCleanup;

  let contextActivatedSource = null;
  const clearContextActivation = () => {
    contextActivatedSource = null;
  };

  // A fresh mouse gesture makes an unmatched context-only activation stale.
  // The original gesture's mousedown precedes contextmenu, so this does not
  // clear the source before WebKit's follow-up click can consume it.
  document.addEventListener('mousedown', clearContextActivation);

  const onClick = (event) => {
    const source = liveActionSource(event);
    if (!source) return;
    // WebKit follows macOS Control-click's contextmenu with a click for the
    // same producer. The context handler already dispatched it; consume this
    // one event-sequence duplicate before it can open a second terminal.
    if (source === contextActivatedSource) {
      clearContextActivation();
      event.preventDefault();
      return;
    }
    // data-act controls can live inside <summary>; never also toggle it.
    event.preventDefault();
    void handleRowAction(actionDescriptor(source, event));
  };
  document.addEventListener('click', onClick);

  // macOS turns Control-primary-click into a context-menu gesture instead of
  // the click above. Route that native event only for the two Groups terminal
  // actions that advertise background opening; ordinary context menus and
  // every unrelated data-act remain untouched.
  const onContextMenu = (event) => {
    if (!event.ctrlKey && !event.metaKey) return;
    const source = liveActionSource(event);
    if (!source) return;
    const action = actionDescriptor(source, event);
    if (!BACKGROUND_CONTEXT_ACTIONS.has(action.data.act)) return;
    event.preventDefault();
    contextActivatedSource = source;
    void handleRowAction(action);
  };
  document.addEventListener('contextmenu', onContextMenu);

  // Focusable span chips need explicit Enter/Space activation. Native buttons
  // already synthesize clicks, and all clicks share the live-source guard above.
  const onChipKeyDown = (event) => {
    if (event.key !== 'Enter' && event.key !== ' ') return;
    if (event.repeat || event.ctrlKey || event.altKey || event.metaKey) return;
    const chip = liveActionSource(event, 'span[data-act][role="button"]');
    if (!chip) return;
    event.preventDefault();
    chip.click();
  };
  document.addEventListener('keydown', onChipKeyDown);

  const cleanup = () => {
    document.removeEventListener('click', onClick);
    document.removeEventListener('contextmenu', onContextMenu);
    document.removeEventListener('mousedown', clearContextActivation);
    document.removeEventListener('keydown', onChipKeyDown);
    clearContextActivation();
    if (rowActionsCleanup === cleanup) rowActionsCleanup = null;
  };
  rowActionsCleanup = cleanup;
  return cleanup;
}
