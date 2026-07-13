import { signal } from '@preact/signals';

export function createShellState({ setTimer = setTimeout, clearTimer = clearTimeout } = {}) {
  const status = signal({ text: '', error: false });
  const toast = signal({ id: 0, message: '', error: false, visible: false });
  const confirmation = signal(null);

  let toastTimer = null;
  let confirmationResolve = null;

  function showStatus(text, error = false) {
    status.value = { text: String(text || ''), error: !!error };
  }

  function notify(message, error = false) {
    if (toastTimer !== null) clearTimer(toastTimer);
    const next = {
      id: toast.value.id + 1,
      message: String(message || ''),
      error: !!error,
      visible: true,
    };
    toast.value = next;
    toastTimer = setTimer(() => {
      toastTimer = null;
      if (toast.value.id === next.id) toast.value = { ...toast.value, visible: false };
    }, 3000);
  }

  function confirm(options = {}) {
    if (confirmationResolve) confirmationResolve(false);
    return new Promise((resolve) => {
      confirmationResolve = resolve;
      confirmation.value = {
        title: String(options.title || ''),
        body: String(options.body || ''),
        meta: String(options.meta || ''),
        okLabel: String(options.okLabel || 'Confirm'),
        cancelLabel: String(options.cancelLabel || 'Cancel'),
      };
    });
  }

  function resolveConfirmation(result) {
    if (!confirmationResolve) return;
    const resolve = confirmationResolve;
    confirmationResolve = null;
    confirmation.value = null;
    resolve(!!result);
  }

  function dispose() {
    if (toastTimer !== null) clearTimer(toastTimer);
    toastTimer = null;
    toast.value = { ...toast.value, visible: false };
    resolveConfirmation(false);
  }

  return Object.freeze({
    status,
    toast,
    confirmation,
    showStatus,
    notify,
    confirm,
    resolveConfirmation,
    dispose,
  });
}

export const shellState = createShellState();

export function showShellStatus(text, error) {
  shellState.showStatus(text, error);
}

export function shellToast(message, error) {
  shellState.notify(message, error);
}

export function shellConfirm(options) {
  return shellState.confirm(options);
}

export function shellConfirmDiscard() {
  return shellConfirm({
    title: 'Discard input?',
    body: 'Closing the form will discard any unsaved input. Continue?',
    okLabel: 'Discard',
  });
}
