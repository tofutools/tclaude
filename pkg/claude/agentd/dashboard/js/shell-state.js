import { signal } from '@preact/signals';

export function createShellState({ setTimer = setTimeout, clearTimer = clearTimeout } = {}) {
  const status = signal({ text: '', error: false });
  const toast = signal({ id: 0, message: '', error: false, visible: false });
  const confirmation = signal(null);

  let toastTimer = null;
  let confirmationResolve = null;
  let confirmationReject = null;
  let confirmationAction = null;
  let busySeq = 0;
  let activeBusy = null;

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

  // confirm is the shared confirmation modal, and it is BLOCKING when given an
  // `action`: the dialog stays up, its buttons disable, and its primary button
  // swaps to `busyLabel` with the standard .btn-spinner until the action
  // settles. Same busy vocabulary the transaction dialogs already use, so the
  // fleet ops and the retire previews behave identically.
  //
  // Without it a confirm answers "may I?" and immediately gets out of the way,
  // which is wrong for the operations that then take real time: the dashboard's
  // power buttons ran a request that legitimately waits for agents to stop or
  // come up, with nothing on screen while it did. An instant dismissal followed
  // by silence reads as "it did nothing".
  //
  // With an action the returned promise resolves to the action's value; without
  // one it resolves true/false exactly as before, so every existing caller is
  // unaffected.
  function confirm(options = {}) {
    if (confirmationResolve) resolveConfirmation(false);
    return new Promise((resolve, reject) => {
      confirmationResolve = resolve;
      confirmationReject = reject;
      confirmationAction = typeof options.action === 'function' ? options.action : null;
      confirmation.value = {
        title: String(options.title || ''),
        body: String(options.body || ''),
        meta: String(options.meta || ''),
        okLabel: String(options.okLabel || 'Confirm'),
        cancelLabel: String(options.cancelLabel || 'Cancel'),
        busyLabel: String(options.busyLabel || 'Working…'),
        informational: !!options.informational,
        preformatted: !!options.preformatted,
        busy: false,
      };
    });
  }

  function resolveConfirmation(result) {
    if (!confirmationResolve) return;
    const resolve = confirmationResolve;
    const reject = confirmationReject;
    const action = confirmationAction;
    // A confirmed blocking action owns the modal from here: it is detached from
    // the pending-confirmation slot (so a second confirm can never resolve it
    // twice) but the dialog stays on screen, busy, until the work settles.
    if (result && action) {
      confirmationResolve = null;
      confirmationReject = null;
      confirmationAction = null;
      // The token IDENTIFIES this dialog. "Is the current modal busy?" is not
      // the same question — with two blocking confirms in flight, the first to
      // settle would tear down the second's still-running dialog and put the
      // operator right back at "the button did nothing".
      const token = ++busySeq;
      confirmation.value = { ...confirmation.value, busy: true, token };
      const settle = (finish, value) => {
        if (activeBusy?.token === token) activeBusy = null;
        if (confirmation.value?.token === token) confirmation.value = null;
        finish(value);
      };
      activeBusy = { token, resolve };
      Promise.resolve()
        .then(action)
        .then((value) => settle(resolve, value), (err) => settle(reject, err));
      return;
    }
    confirmationResolve = null;
    confirmationReject = null;
    confirmationAction = null;
    confirmation.value = null;
    resolve(!!result);
  }

  function dispose() {
    if (toastTimer !== null) clearTimer(toastTimer);
    toastTimer = null;
    toast.value = { ...toast.value, visible: false };
    resolveConfirmation(false);
    // A blocking action detached itself from the pending slot, so the call
    // above cannot reach it. Unmounting still has to answer its promise —
    // otherwise the caller awaiting the confirm is stranded forever. The
    // request itself is already in flight and keeps running; only the UI
    // contract is being closed out.
    if (activeBusy) {
      const { resolve } = activeBusy;
      activeBusy = null;
      confirmation.value = null;
      resolve(false);
    }
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
