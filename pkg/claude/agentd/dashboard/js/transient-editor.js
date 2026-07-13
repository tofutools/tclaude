// Mount an imperative legacy editor beside (never in place of) an island-owned
// host node. The host stays connected so Preact retains its real DOM identity;
// callers must restore before publishing the next island render.
export function mountTransientSiblingEditor(host, editor) {
  if (!host?.parentNode || !editor) {
    throw new TypeError('transient editor requires a connected host and editor');
  }
  const wasHidden = host.hidden;
  host.hidden = true;
  host.after(editor);
  let restored = false;
  return () => {
    if (restored) return;
    restored = true;
    editor.remove();
    host.hidden = wasHidden;
  };
}
