import { useLayoutEffect, useRef } from 'preact/hooks';

const FOCUSABLE = 'button, [href], input, select, textarea, [tabindex]';

function focusableElements(root) {
  return Array.from(root?.querySelectorAll(FOCUSABLE) || []).filter(
    (element) =>
      !element.disabled &&
      !element.closest('[hidden]') &&
      element.getAttribute('tabindex') !== '-1',
  );
}

// useDialogFocus gives Preact-owned dialogs one keyboard lifecycle: capture
// the invoker, focus the initial control after mount, contain Tab navigation,
// close on Escape, and restore the invoker on every close/unmount path.
export function useDialogFocus({
  open,
  initialFocusRef,
  onEscape,
  shouldHandle = () => true,
}) {
  const dialogRef = useRef(null);
  const escapeRef = useRef(onEscape);
  const shouldHandleRef = useRef(shouldHandle);
  const keyDownRef = useRef(null);
  escapeRef.current = onEscape;
  shouldHandleRef.current = shouldHandle;

  useLayoutEffect(() => {
    if (!open) return undefined;
    const invoker = document.activeElement;
    queueMicrotask(() => {
      if (!dialogRef.current?.isConnected) return;
      const focusable = focusableElements(dialogRef.current);
      const requested = initialFocusRef?.current;
      const target =
        (focusable.includes(requested) && requested) ||
        focusable.find((element) => element.hasAttribute('autofocus')) ||
        focusable[0];
      target?.focus();
      const selectOnFocus = target?.getAttribute('data-select-on-focus');
      if (selectOnFocus !== null && selectOnFocus !== 'false')
        target.select?.();
    });
    const onDocumentKeyDown = (event) => keyDownRef.current?.(event);
    document.addEventListener('keydown', onDocumentKeyDown);
    return () => {
      document.removeEventListener('keydown', onDocumentKeyDown);
      if (invoker?.isConnected) invoker.focus();
    };
  }, [open]);

  keyDownRef.current = (event) => {
    if (!shouldHandleRef.current()) return;
    if (event.key === 'Escape') {
      event.preventDefault();
      escapeRef.current?.();
      return;
    }
    if (event.key !== 'Tab') return;
    const focusable = focusableElements(dialogRef.current);
    if (!focusable.length) {
      event.preventDefault();
      return;
    }
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (!dialogRef.current?.contains(document.activeElement)) {
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

  return { dialogRef };
}
