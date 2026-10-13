import { computed, signal } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';

// The Usage tab hosts three views: an Overview summarising both halves, the
// subscription Quota forecasts (usage-history-*), and Costs (costs-*). The two
// halves keep their own server-side gates — usage_tab_visible (retained quota
// history) and cost_tab_visible (real spend, or costs enabled for subscription
// accounts) — so the hub only decides which of them is on screen.
export const USAGE_SUBVIEWS = Object.freeze(['overview', 'quota', 'costs']);

// effectiveSubview resolves the requested view against what is visible. The
// Overview only makes sense with both halves; with one, that half IS the tab
// (the subtab bar collapses). Before the first snapshot nothing is known yet,
// so a deep link keeps the view it asked for instead of flickering away.
export function effectiveSubview(requested, halves) {
  if (!halves.loaded) return requested;
  if (halves.quota && halves.costs) return requested;
  if (halves.quota) return 'quota';
  if (halves.costs) return 'costs';
  return requested;
}

export function usageLocation(subview) {
  return subview === 'overview' ? { tab: 'usage' } : { tab: 'usage', subtab: subview };
}

export function createUsageHubState({
  snapshot = dashboardState.snapshot,
  activeTab = dashboardState.activeTab,
} = {}) {
  const requested = signal('overview');
  const halves = computed(() => {
    const snap = snapshot.value;
    return { loaded: snap !== null, quota: !!snap?.usage_tab_visible, costs: !!snap?.cost_tab_visible };
  });
  const subview = computed(() => effectiveSubview(requested.value, halves.value));
  const location = computed(() => usageLocation(subview.value));
  // shown(name) is true while that view is on screen; polling(name) while its
  // data should stay fresh — the Overview reads both halves' payloads.
  const view = computed(() => {
    const onTab = activeTab.value === 'usage';
    const sub = subview.value;
    const h = halves.value;
    return {
      onTab,
      activeTab: activeTab.value,
      subview: sub,
      halves: h,
      visible: h.quota || h.costs,
      showSubnav: h.quota && h.costs,
    };
  });

  function select(name) {
    if (!USAGE_SUBVIEWS.includes(name)) return false;
    if (requested.value === name) return false;
    requested.value = name;
    return true;
  }

  function applyLocation(loc) {
    if (loc?.tab !== 'usage') return false;
    return select(USAGE_SUBVIEWS.includes(loc.subtab) ? loc.subtab : 'overview');
  }

  function shown(name, tab = activeTab.value) {
    return tab === 'usage' && subview.value === name;
  }

  function polling(name, tab = activeTab.value) {
    return tab === 'usage' && (subview.value === name || subview.value === 'overview');
  }

  return Object.freeze({ requested, halves, subview, location, view, select, applyLocation, shown, polling });
}

export const usageHubState = createUsageHubState();
