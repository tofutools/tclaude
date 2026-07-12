import { useLayoutEffect, useRef } from 'preact/hooks';

const FOCUSABLE = 'button, [href], input, select, textarea, [tabindex]';

function focusableElements(root) {
  return Array.from(root?.querySelectorAll(FOCUSABLE) || [])
    .filter(element => !element.disabled && !element.hidden && element.getAttribute('tabindex') !== '-1');
}

// useDialogFocus gives Preact-owned dialogs one keyboard lifecycle: capture
// the invoker, focus the initial control after mount, contain Tab navigation,
// close on Escape, and restore the invoker on every close/unmount path.
export function useDialogFocus({ open, initialFocusRef, onEscape }) {
  const dialogRef = useRef(null);
  const escapeRef = useRef(onEscape);
  const keyDownRef = useRef(null);
  escapeRef.current = onEscape;

  useLayoutEffect(() => {
    if (!open) return undefined;
    const invoker = document.activeElement;
    queueMicrotask(() => {
      if (!dialogRef.current?.isConnected) return;
      (initialFocusRef?.current || focusableElements(dialogRef.current)[0])?.focus();
    });
    const onDocumentKeyDown = event => keyDownRef.current?.(event);
    document.addEventListener('keydown', onDocumentKeyDown);
    return () => {
      document.removeEventListener('keydown', onDocumentKeyDown);
      if (invoker?.isConnected) invoker.focus();
    };
  }, [open]);

  keyDownRef.current = (event) => {
    if (event.key === 'Escape') {
      event.preventDefault();
      escapeRef.current?.();
      return;
    }
    if (event.key !== 'Tab') return;
    const focusable = focusableElements(dialogRef.current);
    if (!focusable.length) { event.preventDefault(); return; }
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
