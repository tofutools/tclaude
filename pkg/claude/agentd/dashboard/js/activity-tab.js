// activity-tab.js — the Activity tab's sub-navigation.
//
// The subnav anchors and the three panels (audit / logs / performance) are
// static markup in dashboard.html; each panel hosts its own bounded Preact
// island. This module mirrors activitySubtab (activity-state.js) onto that DOM
// and reports subtab clicks to the history router.

import { effect } from '@preact/signals';
import { ACTIVITY_TAB, activitySubtab, setActivitySubtab } from './activity-state.js';
import { isModifiedClick } from './helpers.js';
import { peerViewSubtabUsable } from './peer-view-limits.js';

// bindActivityTab wires the static subnav: a plain click selects the view and
// tells the router (tclaude:navigated → /activity/<subtab>); a modified or
// middle click is left to the browser so the href opens in a new tab. Space
// activates like on the Access subnav (Enter already clicks an anchor).
export function bindActivityTab({ doc = document, subtab = activitySubtab } = {}) {
  const root = doc.getElementById('tab-activity');
  if (!root) return () => {};
  const select = (name) => {
    // A peer view shares only its audit log; peer-view-limits greys the other
    // subtabs and refuses their clicks before this runs.
    if (!peerViewSubtabUsable(ACTIVITY_TAB, name)) return;
    setActivitySubtab(name, subtab);
    doc.dispatchEvent(new CustomEvent('tclaude:navigated', {
      detail: { location: { tab: ACTIVITY_TAB, subtab: name } },
    }));
  };
  const onClick = (event) => {
    const link = event.target.closest?.('.activity-subtab');
    if (!link || !root.contains(link)) return;
    if (isModifiedClick(event)) return;
    event.preventDefault();
    select(link.dataset.subtab);
  };
  const onKeyDown = (event) => {
    if (event.key !== ' ' && event.key !== 'Spacebar') return;
    const link = event.target.closest?.('.activity-subtab');
    if (!link || !root.contains(link)) return;
    event.preventDefault();
    select(link.dataset.subtab);
  };
  root.addEventListener('click', onClick);
  root.addEventListener('keydown', onKeyDown);
  const stop = effect(() => {
    const current = subtab.value;
    for (const link of root.querySelectorAll('.activity-subtab')) {
      const on = link.dataset.subtab === current;
      link.classList.toggle('active', on);
      link.setAttribute('aria-selected', on ? 'true' : 'false');
    }
    for (const panel of root.querySelectorAll('.activity-panel')) {
      panel.classList.toggle('active', panel.id === `activity-${current}`);
    }
  });
  return () => {
    stop();
    root.removeEventListener('click', onClick);
    root.removeEventListener('keydown', onKeyDown);
  };
}
