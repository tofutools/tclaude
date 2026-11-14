// phone-page.js — the seam between a tab's phone view and the phone shell's
// top bar (js/phone-shell-island.js).
//
// A tab view that pushes a page over its list (Mail's reader, later a task or
// an agent) describes that page here; the shell then shows a back button, the
// page's title and its actions in the top bar instead of the logo, title and
// live dot, and can hide the bottom nav so the page owns the bottom edge (a
// reply bar). The tab never sees the shell's own state: this module-level
// signal is the whole contract.
//
//   phonePage.value = {
//     title:   string, may be empty
//     onBack:  () => void, what the back button does
//     actions: [{ key, icon, label, onClick, disabled?, danger? }], top bar buttons
//     hideNav: boolean, hide the bottom nav while the page is up
//   } | null
//
// A page never outlives its tab: usePhonePage takes it down when its view goes
// inactive, and the shell clears it when the operator picks another tab. An
// own view (Now, Board) covers it without clearing it, so going back to the
// tab finds the page still up.

import { signal } from '@preact/signals';
import { useEffect, useRef } from 'preact/hooks';

export const phonePage = signal(null);

// pageKey is what identifies a page's look: its title, whether it hides the
// nav, and each action's key, label and disabled state. Handlers are not part
// of it.
export function pageKey(page) {
  if (!page) return '';
  const acts = (page.actions || []).map((act) => [act.key || act.label, act.icon || '', act.label, !!act.disabled, !!act.danger]);
  return JSON.stringify([page.title || '', !!page.hideNav, acts]);
}

// usePhonePage shows `page` in the shell while `active` holds and the
// component is mounted, and takes it down again after. The shell's copy is
// replaced only when the page's look changes (pageKey), never on a mere
// re-render, and its handlers always call the latest ones passed here.
// Clearing only removes the page this component set, never another view's.
export function usePhonePage(page, active = true) {
  const latest = useRef(page);
  latest.current = page;
  const key = active && page ? pageKey(page) : '';
  useEffect(() => {
    if (!key) return undefined;
    const now = latest.current;
    const shown = {
      title: now.title || '',
      hideNav: !!now.hideNav,
      onBack: () => latest.current?.onBack?.(),
      actions: (now.actions || []).map((act, index) => ({
        ...act,
        onClick: () => latest.current?.actions?.[index]?.onClick?.(),
      })),
    };
    phonePage.value = shown;
    return () => {
      if (phonePage.peek() === shown) phonePage.value = null;
    };
  }, [key]);
}
