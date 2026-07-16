const FOCUSABLE = 'button, [href], input, select, textarea, [tabindex]';

function focusableElements(root) {
  return Array.from(root?.querySelectorAll(FOCUSABLE) || []).filter(
    (element) =>
      !element.disabled &&
      !element.closest('[hidden]') &&
      element.getAttribute('tabindex') !== '-1',
  );
}

// bindDialogFocus is the framework-neutral dashboard dialog lifecycle:
// capture the invoker, focus the initial control, contain Tab navigation,
// close on Escape, and restore the invoker on every teardown path.
export function bindDialogFocus({
  dialog,
  initialFocus = null,
  onEscape,
  shouldHandle = () => true,
}) {
  const invoker = document.activeElement;
  let disposed = false;
  queueMicrotask(() => {
    if (disposed || !dialog?.isConnected) return;
    const focusable = focusableElements(dialog);
    const requested = typeof initialFocus === 'function' ? initialFocus() : initialFocus;
    const target =
      (focusable.includes(requested) && requested) ||
      focusable.find((element) => element.hasAttribute('autofocus')) ||
      focusable[0];
    target?.focus();
    const selectOnFocus = target?.getAttribute('data-select-on-focus');
    if (target && selectOnFocus !== null && selectOnFocus !== 'false') target.select?.();
  });
  const onKeyDown = (event) => {
    if (!shouldHandle()) return;
    if (event.key === 'Escape') {
      event.preventDefault();
      onEscape?.();
      return;
    }
    if (event.key !== 'Tab') return;
    const focusable = focusableElements(dialog);
    if (!focusable.length) {
      event.preventDefault();
      return;
    }
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (!dialog?.contains(document.activeElement)) {
      event.preventDefault();
      (event.shiftKey ? last : first).focus();
    } else if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  };
  document.addEventListener('keydown', onKeyDown);
  return () => {
    if (disposed) return;
    disposed = true;
    document.removeEventListener('keydown', onKeyDown);
    if (invoker?.isConnected) invoker.focus();
  };
}
