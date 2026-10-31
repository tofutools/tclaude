import { batch, computed, effect, signal } from '@preact/signals';

function messageOf(error) {
  if (!error) return null;
  return String(error.message || error);
}

// createDashboardState builds the server-backed state boundary shared by
// future Preact islands. It deliberately contains no fetch, DOM, selector, or
// rendering knowledge: refresh.js remains the one authoritative poll during
// the mixed legacy/Preact migration and publishes only accepted snapshots.
export function createDashboardState({ now = () => Date.now() } = {}) {
  const snapshot = signal(null);
  const activeTab = signal('groups');
  const lastRefreshAt = signal(null);
  const poll = signal({
    phase: 'idle',
    requestId: 0,
    startedAt: null,
    completedAt: null,
    responded: false,
    error: null,
  });
  const connection = signal({
    status: 'connecting',
    consecutiveFailures: 0,
    changedAt: now(),
    error: null,
  });

  let latestRequestId = 0;
  let pendingRequestId = null;

  const generatedAt = computed(() => snapshot.value?.generated_at ?? null);
  const activeTabView = computed(() => {
    const tab = activeTab.value;
    const value = snapshot.value;
    return {
      tab,
      data: value?.[tab] ?? null,
      paging: value?.paging?.[tab] ?? null,
    };
  });

  function beginRequest(startedAt = now()) {
    const requestId = ++latestRequestId;
    pendingRequestId = requestId;
    poll.value = {
      ...poll.value,
      phase: 'loading',
      requestId,
      startedAt,
      responded: false,
      error: null,
    };
    return requestId;
  }

  function isCurrentRequest(requestId) {
    return requestId === latestRequestId && requestId === pendingRequestId;
  }

  function commitRequest(requestId, value, completedAt) {
    if (!isCurrentRequest(requestId)) return false;
    if (completedAt === undefined) completedAt = now();
    batch(() => {
      snapshot.value = value;
      lastRefreshAt.value = completedAt;
      poll.value = {
        ...poll.value,
        phase: 'ready',
        requestId,
        completedAt,
        responded: true,
        error: null,
      };
    });
    pendingRequestId = null;
    return true;
  }

  function failRequest(requestId, error, { responded = false, completedAt } = {}) {
    if (!isCurrentRequest(requestId)) return false;
    if (completedAt === undefined) completedAt = now();
    pendingRequestId = null;
    poll.value = {
      ...poll.value,
      phase: 'error',
      requestId,
      completedAt,
      responded,
      error: messageOf(error),
    };
    return true;
  }

  function discardRequest(requestId, { responded = false, completedAt } = {}) {
    if (!isCurrentRequest(requestId)) return false;
    if (completedAt === undefined) completedAt = now();
    pendingRequestId = null;
    poll.value = {
      ...poll.value,
      phase: snapshot.value ? 'ready' : 'idle',
      requestId,
      completedAt,
      responded,
      error: null,
    };
    return true;
  }

  function setActiveTab(tab) {
    if (typeof tab !== 'string' || !tab) return false;
    if (activeTab.value === tab) return false;
    activeTab.value = tab;
    return true;
  }

  function setConnection(status, options = {}) {
    const {
      consecutiveFailures = 0,
      error = null,
    } = options;
    const nextError = messageOf(error);
    const previous = connection.value;
    if (previous.status === status &&
        previous.consecutiveFailures === consecutiveFailures &&
        previous.error === nextError) {
      return false;
    }
    const changedAt = options.changedAt ?? now();
    connection.value = {
      status,
      consecutiveFailures,
      changedAt,
      error: nextError,
    };
    return true;
  }

  // publishLocalEdit applies an OPTIMISTIC local change to the published
  // snapshot — marking a human notification read, say — outside the request
  // lifecycle, so every island reading this store sees it before the next poll
  // reconciles to server truth. It deliberately touches no request bookkeeping:
  // the poll in flight still owns commit/fail/discard.
  function publishLocalEdit(value) {
    snapshot.value = value ? { ...value } : null;
  }

  // Feature modules derive views instead of copying snapshot slices into
  // additional mutable stores. The selector reruns only when its Signal
  // dependencies change and stays independent of legacy rendering modules.
  function select(selector) {
    return computed(() => selector(snapshot.value));
  }

  return Object.freeze({
    snapshot,
    activeTab,
    activeTabView,
    lastRefreshAt,
    generatedAt,
    poll,
    connection,
    beginRequest,
    isCurrentRequest,
    commitRequest,
    failRequest,
    discardRequest,
    setActiveTab,
    setConnection,
    publishLocalEdit,
    select,
  });
}

export const dashboardState = createDashboardState();

// SNAPSHOT_STATIC_KEYS are the registries a snapshot leaves out when the
// request's static_version still matches (dashboard.go snapshotStaticVersion).
const SNAPSHOT_STATIC_KEYS = ['slugs', 'templates', 'profiles', 'roles', 'saved_prompts', 'plugins_catalog', 'harnesses', 'sandbox_impl'];

// restoreUnchangedStatics fills next's omitted registries from prev, the
// snapshot whose static_version the request sent. It changes next in place.
export function restoreUnchangedStatics(next, prev) {
  if (!next?.static_unchanged || !prev || prev.static_version !== next.static_version) return next;
  // Only what this reply left out: a peer on an older build omits fewer keys.
  for (const key of SNAPSHOT_STATIC_KEYS) next[key] = next[key] ?? prev[key];
  if (next.permissions) next.permissions.scope_dim_options ??= prev.permissions?.scope_dim_options;
  return next;
}

// The Fleet map renders from its own per-node summary polls, not this node's
// snapshot, so the snapshot poll slows while it is shown. (Groups · all nodes
// renders this node's groups from the snapshot, so it keeps the full cadence.)
const OVERVIEW_TABS = new Set(['map']);
export function fleetOverviewShown(tab = dashboardState.activeTab) {
  return OVERVIEW_TABS.has(tab.value);
}
// onFleetOverviewChange calls cb (outside the tracking effect) whenever an
// overview is entered or left; returns the unsubscribe.
export function onFleetOverviewChange(cb, tab = dashboardState.activeTab) {
  let prev = fleetOverviewShown(tab);
  return effect(() => {
    const shown = OVERVIEW_TABS.has(tab.value);
    if (shown !== prev) { prev = shown; queueMicrotask(cb); }
  });
}
