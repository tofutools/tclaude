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
//     routed:  boolean, the page is a router location of its own; otherwise
//              it is a history step of its own (phone-history.js)
//     view:    the shell's own view the page is pushed over ('board': a
//              task's page), or absent for a tab's page; the shell shows a
//              page only over the view it belongs to
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
  return JSON.stringify([page.title || '', !!page.hideNav, acts, page.view || '']);
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
      routed: !!now.routed,
      ...(now.view ? { view: now.view } : {}),
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

// phoneBar is what a tab's root view (no page pushed) adds to the top bar:
// icon actions before the shell's own (🔍, the node chip, the live dot), and
// a menu on the title (the mock's "Notifications ▾", "Board webshop ▾").
//
//   phoneBar.value = {
//     tab:     string, the tab it belongs to; the shell shows it only while
//              that tab is the one on screen
//     menu:    { sub, label, onClick } | null, a ▾ after the title: sub is the
//              small text beside it, label what it does for a screen reader
//     actions: [{ key, icon, label, onClick, disabled?, danger?, badge? }]
//   } | null
export const phoneBar = signal(null);

function barKey(bar) {
  if (!bar) return '';
  const acts = (bar.actions || []).map((act) => [act.key || act.label, act.icon || '', act.label, !!act.disabled, !!act.danger, Number(act.badge) || 0]);
  return JSON.stringify([bar.tab || '', bar.menu ? [bar.menu.sub || '', bar.menu.label || ''] : null, acts]);
}

// usePhoneBar shows `bar` in the shell's top bar while `active` holds and the
// component is mounted, the way usePhonePage does for a page: replaced only
// when its look changes, its handlers always the latest, and cleared only if
// it is still this component's. Clearing it puts back the bar of another tab
// it covered.
export function usePhoneBar(bar, active = true) {
  const latest = useRef(bar);
  latest.current = bar;
  const key = active && bar ? barKey(bar) : '';
  useEffect(() => {
    if (!key) return undefined;
    const now = latest.current;
    const shown = {
      tab: now.tab || '',
      menu: now.menu ? { sub: now.menu.sub || '', label: now.menu.label || '', onClick: () => latest.current?.menu?.onClick?.() } : null,
      actions: (now.actions || []).map((act, index) => ({
        ...act,
        onClick: () => latest.current?.actions?.[index]?.onClick?.(),
      })),
    };
    // An own view's bar (Board's) covers the bar of the tab under it
    // (Mail's): that tab's view stays mounted and sets its bar only when its
    // look changes, so taking this one down puts the covered one back.
    const prev = phoneBar.peek();
    const covered = prev && prev.tab !== shown.tab ? prev : (prev?.covered || null);
    if (covered) shown.covered = covered;
    phoneBar.value = shown;
    return () => {
      if (phoneBar.peek() === shown) phoneBar.value = shown.covered || null;
    };
  }, [key]);
}
