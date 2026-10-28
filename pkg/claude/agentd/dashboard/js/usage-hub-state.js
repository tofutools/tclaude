import { computed, signal } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';
import { featureState } from './feature-state-registry.js';

// The Usage tab hosts three views: an Overview summarising both halves, the
// subscription Quota forecasts (usage-history-*), and Costs (costs-*). The two
// halves keep their own server-side gates — usage_tab_visible (retained quota
// history) and cost_tab_visible (real spend, or costs enabled for subscription
// accounts) — so the hub only decides which of them is on screen. By default
// the tab, once either half has something, offers all three views: an empty
// half shows its own empty state. Config dashboard.usage_collapse_subviews
// restores the collapse to the only populated half.
export const USAGE_SUBVIEWS = Object.freeze(['overview', 'quota', 'costs']);

// effectiveSubview resolves the requested view against what is visible. The
// Overview only makes sense with both halves; with one (only reachable under
// dashboard.usage_collapse_subviews), that half IS the tab and the subtab bar
// collapses. Before the first snapshot nothing is known yet,
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

// withCostsPeriod adds the Costs view's period (/usage/costs?period=), which
// the costs island owns, to a Usage location.
export function withCostsPeriod(loc) {
  const period = loc?.subtab === 'costs' ? featureState('costs')?.period?.value : '';
  return period ? { ...loc, params: { ...(loc.params || {}), period } } : loc;
}

export function createUsageHubState({
  snapshot = dashboardState.snapshot,
  activeTab = dashboardState.activeTab,
} = {}) {
  const requested = signal('overview');
  const halves = computed(() => {
    const snap = snapshot.value;
    const quota = !!snap?.usage_tab_visible;
    const costs = !!snap?.cost_tab_visible;
    if (!snap?.usage_collapse_subviews && (quota || costs)) return { loaded: snap !== null, quota: true, costs: true };
    return { loaded: snap !== null, quota, costs };
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

  // open brings a view up from outside the tab (the header's cost link, the
  // command palette) as ONE history step: with the view chosen first, the
  // nav click records the hub's location; already on the tab, the switch is
  // announced like a subnav click.
  function open(name, doc = globalThis.document) {
    if (!USAGE_SUBVIEWS.includes(name)) return false;
    const changed = select(name);
    const nav = doc?.querySelector('nav [data-tab="usage"]');
    if (!nav) return false;
    if (!nav.classList.contains('active')) nav.click();
    else if (changed) doc.dispatchEvent(new CustomEvent('tclaude:navigated', { detail: { location: withCostsPeriod(location.value) } }));
    return true;
  }

  return Object.freeze({ requested, halves, subview, location, view, select, applyLocation, shown, polling, open });
}

export const usageHubState = createUsageHubState();
