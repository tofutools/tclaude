// The static Groups-toolbar cog survives snapshot reconciliation and may be
// re-homed into the dock. Bind directly to those persistent nodes so its open
// state is not part of the cross-feature data-act router.

import { $ } from './helpers.js';
import {
  applyMenuFilter, bindMenuHover, handleMenuFilterKeyDown,
  MENU_FILTER_PLACEHOLDER, MENU_FILTER_WIZARD_PLACEHOLDER,
} from './menu-filter.js';
import { isWizardActive } from './slop.js';

let toolbarMenuCleanup = null;

function bindToolbarActionsMenu() {
  if (toolbarMenuCleanup) return toolbarMenuCleanup;
  const host = $('.filter-bar-cog');
  const cog = host?.querySelector('.cog-btn');
  const menu = host?.querySelector('.action-menu');
  if (!host || !cog || !menu) return () => {};
  // This cog's items are static markup, so the filter box is authored in
  // dashboard.html rather than rendered. The behaviour is the shared core.
  const filter = menu.querySelector('.action-menu-filter');

  const resetFilter = () => {
    if (!filter) return;
    filter.value = '';
    applyMenuFilter(menu, '', { input: filter });
  };
  const close = (restoreFocus = false) => {
    const focusInside = menu.contains(document.activeElement);
    menu.classList.remove('open');
    cog.setAttribute('aria-expanded', 'false');
    resetFilter();
    if (restoreFocus || focusInside) cog.focus();
  };
  const onCogClick = (event) => {
    event.preventDefault();
    const open = !menu.classList.contains('open');
    menu.classList.remove('opens-up');
    menu.classList.toggle('open', open);
    cog.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (open) {
      // Reset before measuring: the box has to be empty (and the full list
      // shown) for the flip-up decision to use the menu's real height.
      resetFilter();
      // A placeholder is an attribute, so it cannot swap via the CSS theme-copy
      // pattern the menu items use — set it per open instead.
      if (filter) {
        filter.placeholder = isWizardActive()
          ? MENU_FILTER_WIZARD_PLACEHOLDER : MENU_FILTER_PLACEHOLDER;
      }
      const menuRect = menu.getBoundingClientRect();
      if (menuRect.bottom > window.innerHeight
          && menuRect.height < cog.getBoundingClientRect().top) {
        menu.classList.add('opens-up');
      }
      filter?.focus();
    } else {
      resetFilter();
    }
  };
  const onMenuClick = (event) => {
    if (event.target.closest('button')) close();
  };
  const onFilterInput = () => applyMenuFilter(menu, filter.value, { input: filter });
  const onFilterKeyDown = (event) => handleMenuFilterKeyDown(menu, event, {
    hasQuery: !!filter.value,
    clearQuery: () => {
      filter.value = '';
      applyMenuFilter(menu, '', { input: filter });
    },
  });
  const onDocumentClick = (event) => {
    if (!host.contains(event.target)) close();
  };
  const onDocumentKeyDown = (event) => {
    if (event.key !== 'Escape' || !menu.classList.contains('open')) return;
    event.preventDefault();
    close(menu.contains(document.activeElement));
  };

  cog.addEventListener('click', onCogClick);
  menu.addEventListener('click', onMenuClick);
  document.addEventListener('click', onDocumentClick);
  document.addEventListener('keydown', onDocumentKeyDown);
  filter?.addEventListener('input', onFilterInput);
  filter?.addEventListener('keydown', onFilterKeyDown);
  const unbindHover = bindMenuHover(menu, { resolveInput: () => filter });
  const cleanup = () => {
    cog.removeEventListener('click', onCogClick);
    menu.removeEventListener('click', onMenuClick);
    document.removeEventListener('click', onDocumentClick);
    document.removeEventListener('keydown', onDocumentKeyDown);
    filter?.removeEventListener('input', onFilterInput);
    filter?.removeEventListener('keydown', onFilterKeyDown);
    unbindHover();
    if (toolbarMenuCleanup === cleanup) toolbarMenuCleanup = null;
  };
  toolbarMenuCleanup = cleanup;
  return cleanup;
}

export { bindToolbarActionsMenu };
