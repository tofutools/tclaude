// The close timing and the close rule of the dashboard's popovers, free of
// Preact so imperative widgets (process-node-chooser, the toolbar cog menu)
// load it too. hover-popover.js re-exports all of it next to the hooks.

// Every hover popover outlives the pointer leaving it by HOVER_CLOSE_MS, so a
// cursor that slips off the trigger or the panel for a moment does not drop
// it. CSS-only popovers mirror the value in --popover-close-grace
// (dashboard.css); keep the two in step.
export const HOVER_CLOSE_MS = 200;

// A popover opened by a click is one the operator asked for, so it waits longer
// for a slipped pointer to come back (watchPopover).
export const CLICK_CLOSE_MS = 600;

// isTextField is a control that takes typing. A checkbox or a range slider
// holds nothing that hover-away could lose.
function isTextField(el) {
  if (el?.isContentEditable) return true;
  const tag = el?.tagName;
  if (tag === 'TEXTAREA' || tag === 'SELECT') return true;
  return tag === 'INPUT' && !/^(button|checkbox|color|file|hidden|image|radio|range|reset|submit)$/.test(el.type || '');
}

// holdsText says a popover has text in it that closing would throw away.
export function holdsText(root) {
  const fields = root?.querySelectorAll?.('input, textarea') || [];
  return [...fields].some((field) => isTextField(field) && field.value);
}

// Open watched popovers, oldest first: Escape takes the newest only, so it
// closes one layer of a popover opened from inside another.
const watched = [];

const OVERLAY = '.modal-overlay, .manage-overlay';
const DIALOG = '.modal-overlay.show, .manage-overlay.show';

// onDialogShown calls listener(dialog) for each modal dialog shown in doc from
// now on, until the returned function stops it. One observer serves every
// listener, and runs only while one is listening.
const dialogWatch = new Map();
function onDialogShown(doc, listener) {
  const MutationObserverImpl = doc?.defaultView?.MutationObserver || globalThis.MutationObserver;
  if (!doc?.body || !MutationObserverImpl) return () => {};
  let watch = dialogWatch.get(doc);
  if (!watch) {
    watch = { listeners: new Set(), shown: new Set(doc.querySelectorAll(DIALOG)) };
    // Most batches are table refreshes and terminal output: re-read the shown
    // dialogs only for one that could show or hide one.
    const touchesDialog = (node) => node?.nodeType === 1
      && (node.matches?.(OVERLAY) || !!node.querySelector?.(OVERLAY));
    watch.observer = new MutationObserverImpl((records) => {
      if (!records.some((record) => (record.type === 'attributes' ? record.target.matches?.(OVERLAY)
        : [...record.addedNodes, ...record.removedNodes].some(touchesDialog)))) return;
      const now = new Set(doc.querySelectorAll(DIALOG));
      const fresh = [...now].filter((dialog) => !watch.shown.has(dialog));
      watch.shown = now;
      for (const dialog of fresh) for (const notify of [...watch.listeners]) notify(dialog);
    });
    watch.observer.observe(doc.body, { subtree: true, childList: true, attributes: true, attributeFilter: ['class'] });
    dialogWatch.set(doc, watch);
  }
  watch.listeners.add(listener);
  return () => {
    watch.listeners.delete(listener);
    if (watch.listeners.size) return;
    watch.observer.disconnect();
    dialogWatch.delete(doc);
  };
}

// watchPopover is the one close rule for popovers opened by a click: a press
// outside closes at once, Escape closes, and the pointer staying away for
// CLICK_CLOSE_MS closes. Text in the popover, a field the operator is using, or
// hold() keeps it through hover-away, and the check repeats while the pointer
// is away, so it closes once the reason ends. A press or Escape still closes.
// A popover that hover can open too passes hoverOpened(), and takes the hover
// delay for as long as that says so. fadeMs is for a popover that fades out as
// hover-away closes it: fading(true) starts the fade, close follows fadeMs
// later, and the pointer coming back first calls fading(false) and keeps it.
//
// A cog menu focuses its filter as it opens, which is why focus alone does not
// count: the field counts once the operator pressed it or used the keyboard.
//
// roots() gives the elements that are the popover, usually one holding both the
// trigger and the panel; a pointer moving between separate ones is not away.
// close(why) gets 'outside', 'escape' or 'away'. The thing opened last wins
// (tcl-s7zf2k): a modal dialog shown while the popover is open closes it, pinned
// or not, unless the popover is inside that dialog or the dialog inside the
// popover. That close is an 'outside' one, so the caller leaves focus where the
// dialog put it. A dialog that was already up when the popover
// opened owns the page: its presses and Escape are its own, and the popover
// waits behind it. A popover inside that dialog carries on as usual. escape:
// false is for a menu whose Escape does something first, like clearing its
// filter. The returned function stops watching.
export function watchPopover({ roots, close, hold = null, hoverOpened = null, escape = true, fadeMs = 0, fading = null, doc = null }) {
  const parts = () => [roots()].flat().filter(Boolean);
  const within = (target) => parts().some((root) => root.contains?.(target));
  const owner = doc || parts()[0]?.ownerDocument || globalThis.document;
  const entry = {};
  let timer = 0;
  let away = false;
  let pressed = false;
  let engaged = false;
  let fadingOut = false;
  const cancel = () => {
    if (timer) globalThis.clearTimeout(timer);
    timer = 0;
  };
  const dialogAbove = () => [...(owner?.querySelectorAll?.(DIALOG) || [])]
    .some((overlay) => !parts().some((root) => overlay.contains(root)));
  const held = () => {
    if (pressed || hold?.() || dialogAbove()) return true;
    const active = owner?.activeElement;
    return (engaged && isTextField(active) && within(active)) || parts().some(holdsText);
  };
  const fade = () => {
    fadingOut = true;
    fading?.(true);
    timer = globalThis.setTimeout(() => {
      timer = 0;
      close('away');
    }, fadeMs);
  };
  const schedule = () => {
    cancel();
    timer = globalThis.setTimeout(() => {
      timer = 0;
      if (!away) return;
      if (held()) schedule();
      else if (fadeMs) fade();
      else close('away');
    }, hoverOpened?.() ? HOVER_CLOSE_MS : CLICK_CLOSE_MS);
  };
  const onLeave = () => { away = true; schedule(); };
  const onEnter = () => {
    away = false;
    cancel();
    if (fadingOut) fading?.(false);
    fadingOut = false;
  };
  const onPointerDown = (event) => {
    if (dialogAbove()) return;
    if (within(event.target)) {
      pressed = true;
      if (isTextField(event.target)) engaged = true;
    } else {
      close('outside');
    }
  };
  // A drag that starts inside can end anywhere, a slider's or a selection's.
  const onPointerUp = () => { pressed = false; };
  const onKeyDown = (event) => {
    if (within(event.target)) engaged = true;
    if (!escape || event.key !== 'Escape' || watched.at(-1) !== entry || dialogAbove()) return;
    event.preventDefault();
    event.stopPropagation();
    close('escape');
  };
  const listening = parts();
  for (const root of listening) {
    root.addEventListener?.('mouseleave', onLeave);
    root.addEventListener?.('mouseenter', onEnter);
  }
  owner?.addEventListener?.('pointerdown', onPointerDown, true);
  owner?.addEventListener?.('pointerup', onPointerUp, true);
  owner?.addEventListener?.('keydown', onKeyDown, true);
  watched.push(entry);
  const stopDialogs = onDialogShown(owner, (dialog) => {
    if (parts().some((root) => dialog.contains(root) || root.contains?.(dialog))) return;
    close('outside');
  });
  return () => {
    cancel();
    stopDialogs();
    watched.splice(watched.indexOf(entry), 1);
    for (const root of listening) {
      root.removeEventListener?.('mouseleave', onLeave);
      root.removeEventListener?.('mouseenter', onEnter);
    }
    owner?.removeEventListener?.('pointerdown', onPointerDown, true);
    owner?.removeEventListener?.('pointerup', onPointerUp, true);
    owner?.removeEventListener?.('keydown', onKeyDown, true);
  };
}
