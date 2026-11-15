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
//     actions: [{ key, icon, label, onClick, disabled?, danger?, muted? }], top bar buttons
//     (muted: looks off but still takes a tap, which says why)
//     hideNav: boolean, hide the bottom nav while the page is up
//     routed:  boolean, the page is a router location of its own; otherwise
//              it is a history step of its own (phone-history.js)
//     view:    the shell's own view the page is pushed over ('board': a
//              task's page), or absent for a tab's page; the shell shows a
//              page only over the view it belongs to
//     covered: the tab's page this one took the slot from, put back when
//              it goes; an unrouted one still counts as a history layer
//   } | null
//
// A page never outlives its tab: usePhonePage takes it down when its view goes
// inactive, and the shell clears it when the operator picks another tab. An
// own view (Now, Board) covers it without clearing it, so going back to the
// tab finds the page still up.

import { signal } from '@preact/signals';
import { useLayoutEffect, useRef } from 'preact/hooks';

export const phonePage = signal(null);

// pageKey is what identifies a page's look: its title, whether it hides the
// nav, and each action's key, label and disabled state. Handlers are not part
// of it.
export function pageKey(page) {
  if (!page) return '';
  const acts = (page.actions || []).map((act) => [act.key || act.label, act.icon || '', act.label, !!act.disabled, !!act.danger, !!act.muted]);
  return JSON.stringify([page.title || '', !!page.hideNav, acts, page.view || '']);
}

// usePhonePage shows `page` in the shell while `active` holds and the
// component is mounted, and takes it down again after. The shell's copy is
// replaced only when the page's look changes (pageKey), never on a mere
// re-render, and its handlers always call the latest ones passed here.
// Clearing only removes the page this component set, never another view's.
// It is a layout effect, so the bar, and the history step the page is
// (phone-history.js), land in the same commit as the page itself: a passive
// effect can run a frame late, and a Back in between left the page up with
// the root's bar and no entry to step off.
export function usePhonePage(page, active = true) {
  const latest = useRef(page);
  latest.current = page;
  const key = active && page ? pageKey(page) : '';
  useLayoutEffect(() => {
    if (!key) return undefined;
    const now = latest.current;
    const shown = {
      title: now.title || '',
      hideNav: !!now.hideNav,
      routed: !!now.routed,
      ...(now.view ? { view: now.view } : {}),
      ...(now.covered ? { covered: now.covered } : {}),
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
//     actions: [{ key, icon, label, onClick, disabled?, danger?, badge? }];
//              an action with text (the mock's "+ Grant") is a pill with
//              that text in place of a round icon
//   } | null
export const phoneBar = signal(null);

function barKey(bar) {
  if (!bar) return '';
  const acts = (bar.actions || []).map((act) => [act.key || act.label, act.icon || '', act.text || '', act.label, !!act.disabled, !!act.danger, Number(act.badge) || 0]);
  return JSON.stringify([bar.tab || '', bar.menu ? [bar.menu.sub || '', bar.menu.label || ''] : null, acts]);
}

// barFor is the bar of `tab` in phoneBar's stack (the latest set first,
// each covering the one before), or null when that tab set none.
export function barFor(bar, tab) {
  for (let at = bar; at; at = at.covered || null) if (at.tab === tab) return at;
  return null;
}

// withoutBar is the stack with `owner`'s bar taken out, wherever it is: a
// view that stays mounted on a tab not on screen (Agents) may have set its
// bar after this one.
function withoutBar(bar, owner) {
  if (!bar) return null;
  const rest = withoutBar(bar.covered || null, owner);
  if (bar.owner === owner) return rest;
  if (rest === (bar.covered || null)) return bar;
  const kept = { ...bar };
  if (rest) kept.covered = rest; else delete kept.covered;
  return kept;
}

// usePhoneBar shows `bar` in the shell's top bar while `active` holds and the
// component is mounted, the way usePhonePage does for a page: replaced only
// when its look changes, its handlers always the latest. Bars stack: each
// mounted view's stays under the later ones, and the shell shows the one of
// the tab on screen (barFor), so a view on another tab setting its bar later
// does not hide this one. Clearing takes out only this component's.
export function usePhoneBar(bar, active = true) {
  const latest = useRef(bar);
  latest.current = bar;
  const key = active && bar ? barKey(bar) : '';
  useLayoutEffect(() => {
    if (!key) return undefined;
    const now = latest.current;
    const owner = {};
    const shown = {
      owner,
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
    if (prev) shown.covered = prev;
    phoneBar.value = shown;
    return () => { phoneBar.value = withoutBar(phoneBar.peek(), owner); };
  }, [key]);
}
