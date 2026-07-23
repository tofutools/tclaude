// Actions for the Preact-owned global notification bell and settings popover.
// State lives in notify-state.js and markup/listener ownership lives in
// notify-island.js; this module is the only layer that knows the API route or
// how the Config-tab shortcut is reached.

import { requestBrowserNotifyPermission } from './browser-notify.js';

function detail(error) {
  return error?.message || String(error);
}

export function createNotifyActions({
  state,
  notify,
  fetchImpl = globalThis.fetch,
  documentRef = globalThis.document,
} = {}) {
  if (!state || typeof state.setOpen !== 'function' ||
      typeof state.beginRequest !== 'function' || typeof state.commitRequest !== 'function') {
    throw new TypeError('notify actions require state');
  }
  if (typeof notify !== 'function') throw new TypeError('notify actions require notify');
  if (typeof fetchImpl !== 'function') throw new TypeError('notify actions require fetch');

  async function load() {
    const requestId = state.beginRequest();
    try {
      const response = await fetchImpl('/api/notifications', { credentials: 'same-origin' });
      if (!response.ok) throw new Error('HTTP ' + response.status);
      return state.commitRequest(requestId, await response.json());
    } catch (error) {
      if (!state.failRequest(requestId, error)) return false;
      notify('Could not load notification settings: ' + detail(error), true);
      return false;
    }
  }

  // A successful POST response is the authoritative complete settings block.
  // On failure, reload from disk so an optimistic checkbox event can never
  // leave the controlled inputs showing a value that did not persist.
  async function post(body, okMessage) {
    const requestId = state.beginRequest();
    try {
      const response = await fetchImpl('/api/notifications', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!response.ok) {
        throw new Error((await response.text()) || ('HTTP ' + response.status));
      }
      const committed = state.commitRequest(requestId, await response.json());
      if (committed && okMessage) notify(okMessage);
      return committed;
    } catch (error) {
      if (!state.failRequest(requestId, error)) return false;
      notify('Notification update failed: ' + detail(error), true);
      await load();
      return false;
    }
  }

  function close() {
    state.setOpen(false);
  }

  function open() {
    state.setOpen(true);
    // Fresh state on every open: Config-tab edits, CLI writes and another
    // browser are all reflected before the operator changes a setting.
    return load();
  }

  function toggle() {
    if (state.open.value) {
      close();
      return Promise.resolve(false);
    }
    return open();
  }

  function openConfig() {
    close();
    documentRef?.querySelector?.('nav [data-tab="config"]')?.click();
  }

  return Object.freeze({
    load,
    post,
    open,
    close,
    toggle,
    openConfig,
    // Deliberately channel-agnostic wording: the master switch gates every
    // delivery channel (see notifications.delivery), so a toast that said
    // "OS notifications" would be wrong whenever delivery is browser/both.
    setEnabled: (enabled) => post(
      { enabled: !!enabled },
      enabled ? 'Notifications ON' : 'Notifications OFF (everything muted)',
    ),
    setType: (type, enabled) => post({ types: { [type]: !!enabled } }),
    setHumanMessages: (enabled) => post({ human_messages: !!enabled }),
    setAccessRequests: (enabled) => post({ access_requests: !!enabled }),
    // Choosing a channel that includes the browser also asks the browser
    // for permission (from this real click) so the very next notification
    // can actually be raised — the daemon persists the routing, but the
    // browser is the one that must consent. Best-effort: the persisted
    // choice stands even if the human dismisses the prompt.
    setDelivery: async (delivery) => {
      const committed = await post({ delivery });
      if (committed && (delivery === 'browser' || delivery === 'both')) {
        await requestBrowserNotifyPermission(globalThis);
      }
      return committed;
    },
  });
}
