// phone-keyboard.js -- how much of the screen the on-screen keyboard covers,
// as the CSS variable --phone-keyboard on <html>, so what sits at the bottom
// of the screen (a dialog, a sheet) rides above it instead of under it.
//
// iOS Safari does not shrink the page for the keyboard: it shrinks the visual
// viewport and leaves fixed elements where they were, under the keyboard.
// Android Chrome resizes the page itself (the viewport meta asks it to with
// interactive-widget=resizes-content), and then this reads 0.

// keyboardInset is the height the keyboard takes off the bottom of the
// layout viewport, in CSS pixels; 0 with no keyboard, or while the page is
// pinch-zoomed (the visual viewport is smaller then for another reason).
export function keyboardInset(win = globalThis) {
  const view = win?.visualViewport;
  if (!view || Math.abs((view.scale ?? 1) - 1) > 0.01) return 0;
  const covered = win.innerHeight - view.height - view.offsetTop;
  // The toolbar showing and hiding moves a few dozen pixels; a keyboard is
  // more than that.
  return covered > 80 ? Math.round(covered) : 0;
}

// installKeyboardInset keeps --phone-keyboard current. Returns the uninstall
// function.
export function installKeyboardInset(win = globalThis, root = win?.document?.documentElement) {
  const view = win?.visualViewport;
  if (!view || !root) return () => {};
  let last = -1;
  const sync = () => {
    const inset = keyboardInset(win);
    if (inset === last) return;
    last = inset;
    root.style.setProperty('--phone-keyboard', `${inset}px`);
  };
  sync();
  view.addEventListener('resize', sync);
  view.addEventListener('scroll', sync);
  return () => {
    view.removeEventListener('resize', sync);
    view.removeEventListener('scroll', sync);
    root.style.removeProperty('--phone-keyboard');
  };
}
