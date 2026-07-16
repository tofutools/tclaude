// The static Groups-toolbar cog survives snapshot reconciliation and may be
// re-homed into the dock. Bind directly to those persistent nodes so its open
// state is not part of the cross-feature data-act router.

import { $ } from './helpers.js';

let toolbarMenuCleanup = null;

function bindToolbarActionsMenu() {
  if (toolbarMenuCleanup) return toolbarMenuCleanup;
  const host = $('.filter-bar-cog');
  const cog = host?.querySelector('.cog-btn');
  const menu = host?.querySelector('.action-menu');
  if (!host || !cog || !menu) return () => {};

  const close = (restoreFocus = false) => {
    menu.classList.remove('open');
    cog.setAttribute('aria-expanded', 'false');
    if (restoreFocus) cog.focus();
  };
  const onCogClick = (event) => {
    event.preventDefault();
    const open = !menu.classList.contains('open');
    menu.classList.remove('opens-up');
    menu.classList.toggle('open', open);
    cog.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (open) {
      const menuRect = menu.getBoundingClientRect();
      if (menuRect.bottom > window.innerHeight
          && menuRect.height < cog.getBoundingClientRect().top) {
        menu.classList.add('opens-up');
      }
    }
  };
  const onMenuClick = (event) => {
    if (event.target.closest('button')) close();
  };
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
  const cleanup = () => {
    cog.removeEventListener('click', onCogClick);
    menu.removeEventListener('click', onMenuClick);
    document.removeEventListener('click', onDocumentClick);
    document.removeEventListener('keydown', onDocumentKeyDown);
    if (toolbarMenuCleanup === cleanup) toolbarMenuCleanup = null;
  };
  toolbarMenuCleanup = cleanup;
  return cleanup;
}

export { bindToolbarActionsMenu };
