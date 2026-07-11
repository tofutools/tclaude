import { batch, computed, signal } from '@preact/signals';

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
    select,
  });
}

export const dashboardState = createDashboardState();
