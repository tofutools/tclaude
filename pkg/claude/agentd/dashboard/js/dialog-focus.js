import { useLayoutEffect, useRef } from 'preact/hooks';
import { bindDialogFocus } from './dialog-focus-core.js';

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
  escapeRef.current = onEscape;
  shouldHandleRef.current = shouldHandle;

  useLayoutEffect(() => {
    if (!open) return undefined;
    return bindDialogFocus({
      dialog: dialogRef.current,
      initialFocus: () => initialFocusRef?.current,
      onEscape: () => escapeRef.current?.(),
      shouldHandle: () => shouldHandleRef.current(),
    });
  }, [open]);

  return { dialogRef };
}
