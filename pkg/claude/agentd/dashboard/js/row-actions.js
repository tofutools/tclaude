// row-actions.js — stateless delegated routing for live data-act producers.
//
// The document listener is only an integration adapter: it validates that an
// event came from the current document, freezes that producer, and delegates
// the operation. It owns no presentation or operation state.

import { handleRowAction } from './row-action-handler.js';

export function liveActionSource(event, selector = '[data-act]') {
  const source = event?.target?.closest?.(selector);
  if (!source || source.ownerDocument !== document || !source.isConnected) return null;
  return document.documentElement.contains(source) ? source : null;
}

export function actionDescriptor(source) {
  return Object.freeze({
    producerId: source.id || '',
    data: Object.freeze({ ...source.dataset }),
  });
}

let rowActionsCleanup = null;
export function bindRowActions() {
  if (rowActionsCleanup) return rowActionsCleanup;

  const onClick = (event) => {
    const source = liveActionSource(event);
    if (!source) return;
    // data-act controls can live inside <summary>; never also toggle it.
    event.preventDefault();
    void handleRowAction(actionDescriptor(source));
  };
  document.addEventListener('click', onClick);

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
    document.removeEventListener('keydown', onChipKeyDown);
    if (rowActionsCleanup === cleanup) rowActionsCleanup = null;
  };
  rowActionsCleanup = cleanup;
  return cleanup;
}
