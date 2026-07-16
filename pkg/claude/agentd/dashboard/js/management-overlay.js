import { h } from 'preact';
import { useCallback, useLayoutEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { makeModalResizable } from './helpers.js';
import { useDialogFocus } from './dialog-focus.js';
import { isTopmostOverlay } from './overlay-stack.js';

const html = htm.bind(h);

export function ManagementOverlay({
  id,
  manage = false,
  dialogClass = '',
  overlayClass = '',
  labelledby,
  describedby,
  ariaLabel,
  onClose,
  beforeClose = null,
  onCloseError = null,
  onSubmitHotkey = null,
  onSubmitEnter = null,
  dirty = false,
  blocked = false,
  confirmDiscard,
  resizeKey = '',
  fitContent = true,
  guardBackdropDrag = false,
  onDragEnter = null,
  onDragOver = null,
  onDragLeave = null,
  onDrop = null,
  onPaste = null,
  initialFocusRef = null,
  registerClose = null,
  children,
}) {
  const overlayRef = useRef(null);
  const pressedOnBackdrop = useRef(false);
  const confirming = useRef(false);
  const closeState = useRef(null);
  closeState.current = { beforeClose, blocked, confirmDiscard, dirty, onClose, onCloseError };
  const close = useCallback(async () => {
    const state = closeState.current;
    if (state.blocked || confirming.current) return false;
    confirming.current = true;
    const overlay = overlayRef.current;
    const dialog = dialogRef.current;
    let suspended = false;
    try {
      const prepared = state.beforeClose ? await state.beforeClose() : true;
      if (prepared === false) return false;
      const isDirty = typeof state.dirty === 'function' ? state.dirty() : state.dirty;
      if (!isDirty) {
        state.onClose();
        return true;
      }
      if (overlay) {
        overlay.inert = true;
        overlay.setAttribute('inert', '');
        overlay.setAttribute('aria-hidden', 'true');
      }
      dialog?.setAttribute('aria-modal', 'false');
      suspended = true;
      const discard = await state.confirmDiscard();
      if (discard) state.onClose();
      return !!discard;
    } catch (error) {
      state.onCloseError?.(error);
      return false;
    } finally {
      confirming.current = false;
      if (suspended && overlay?.isConnected) {
        overlay.inert = false;
        overlay.removeAttribute('inert');
        overlay.removeAttribute('aria-hidden');
      }
      if (suspended && dialog?.isConnected) dialog.setAttribute('aria-modal', 'true');
    }
  }, []);
  const { dialogRef } = useDialogFocus({
    open: true,
    initialFocusRef,
    onEscape: () => {
      void close();
    },
    shouldHandle: () => isTopmostOverlay(overlayRef.current),
  });
  useLayoutEffect(() => {
    if (!resizeKey) return undefined;
    return fitContent
      ? makeModalResizable(dialogRef.current, resizeKey)
      : makeModalResizable(dialogRef.current, resizeKey, { fitContent: false });
  }, [resizeKey, fitContent]);
  useLayoutEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return undefined;
    const handleKeyDown = (event) => {
      if (blocked || confirming.current) return;
      if (
        onSubmitEnter &&
        event.key === 'Enter' &&
        !event.isComposing &&
        event.keyCode !== 229 &&
        !event.ctrlKey &&
        !event.metaKey
      ) {
        event.preventDefault();
        onSubmitEnter();
        return;
      }
      if (
        onSubmitHotkey &&
        event.key === 'Enter' &&
        !event.isComposing &&
        event.keyCode !== 229 &&
        (event.ctrlKey || event.metaKey)
      ) {
        event.preventDefault();
        onSubmitHotkey();
      }
    };
    dialog.addEventListener('keydown', handleKeyDown);
    return () => dialog.removeEventListener('keydown', handleKeyDown);
  }, [blocked, onSubmitEnter, onSubmitHotkey]);
  useLayoutEffect(() => {
    const cleanup = registerClose?.(close);
    return () => {
      if (typeof cleanup === 'function') cleanup();
      else registerClose?.(null);
    };
  }, [close, registerClose]);
  const wideEditor = [
    'template-editor-modal',
    'profile-editor-modal',
    'role-editor-modal',
  ].includes(id);
  const modalClass =
    dialogClass ||
    (wideEditor
      ? 'cron-create-modal template-editor-modal'
      : 'cron-create-modal');
  return html`<div
    ref=${overlayRef}
    class=${`${manage ? 'manage-overlay show' : 'modal-overlay show'}${overlayClass ? ` ${overlayClass}` : ''}`}
    id=${id}
    onMouseDown=${(event) => {
      if (guardBackdropDrag) {
        pressedOnBackdrop.current = event.target === event.currentTarget;
      } else if (event.target === event.currentTarget) {
        void close();
      }
    }}
    onClick=${guardBackdropDrag ? (event) => {
      const dismiss = event.target === event.currentTarget && pressedOnBackdrop.current;
      pressedOnBackdrop.current = false;
      if (dismiss) void close();
    } : undefined}
    onDragEnter=${onDragEnter}
    onDragOver=${onDragOver}
    onDragLeave=${onDragLeave}
    onDrop=${onDrop}
    onPaste=${onPaste}
  >
    <div
      ref=${dialogRef}
      class=${manage ? 'manage-modal' : modalClass}
      role="dialog"
      aria-modal="true"
      aria-labelledby=${labelledby}
      aria-describedby=${describedby}
      aria-label=${ariaLabel}
    >
      ${children}
    </div>
  </div>`;
}
