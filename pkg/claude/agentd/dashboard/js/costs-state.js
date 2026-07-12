import { batch, computed, signal } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';
import { dashPrefs } from './prefs.js';
import {
  COST_COLUMNS, buildCostChart, costHarnesses, filterCostData, harnessLabel,
  matchesCostAgent, monthLabel, monthProjection, oldestMonthOffset,
  resolveHarnessSelection, sortCostAgents,
} from './costs-model.js';

const FILL_KEY = 'tclaude.dash.costs.fillEmptyWeekdays';
const WEEKENDS_KEY = 'tclaude.dash.costs.includeWeekends';
const HARNESSES_KEY = 'tclaude.dash.costs.harnesses';

function errorMessage(error) {
  return String(error?.message || error);
}

function savedHarnesses(prefs) {
  try {
    const value = JSON.parse(prefs.getItem(HARNESSES_KEY) || '[]');
    return Array.isArray(value) ? value : [];
  } catch { return []; }
}

export function createCostsState({
  snapshot = dashboardState.snapshot,
  activeTab = dashboardState.activeTab,
  prefs = dashPrefs,
  now = () => new Date(),
} = {}) {
  const span = signal('month');
  const monthOffset = signal(0);
  const fillEmpty = signal(false);
  const includeWeekends = signal(false);
  const selectedHarnesses = signal([]);
  const query = signal('');
  const sort = signal({ key: 'activity', dir: 'desc' });
  const payload = signal(null);
  const request = signal({ phase: 'idle', requestId: 0, hasLoaded: false, error: null });
  const factor = signal({ raw: '', status: '', error: false, editVersion: 0, requestId: 0 });
  let initialized = false;

  // Keep cost-only derivations outside the snapshot-dependent view. The
  // dashboard publishes a fresh snapshot every two seconds; rebuilding the
  // chart object for those unrelated updates makes CostsChart tear down its
  // imperative chart effect (and the active body-level hover tooltip).
  const costData = computed(() => {
    const data = payload.value;
    const agents = data?.agents || [];
    const harnesses = costHarnesses(agents);
    const selected = resolveHarnessSelection(harnesses, selectedHarnesses.value);
    const narrowed = data ? filterCostData(data, selected) : null;
    const projection = narrowed && span.value === 'month'
      ? monthProjection(narrowed, fillEmpty.value, includeWeekends.value, now())
      : null;
    return {
      data, agents, harnesses, selected, narrowed, projection,
      chart: narrowed ? buildCostChart(narrowed, projection, agents, selected, harnesses) : null,
    };
  });

  const view = computed(() => {
    const snap = snapshot.value;
    const { data, agents, harnesses, selected, narrowed, projection, chart } = costData.value;
    const visibleRows = sortCostAgents(agents, sort.value)
      .filter((agent) => selected.has(harnessLabel(agent.harness)))
      .filter((agent) => matchesCostAgent(agent, query.value));
    const totalConversations = new Set(agents.map((agent) => agent.conv_id)).size;
    const shownConversations = new Set(visibleRows.map((agent) => agent.conv_id)).size;
    const filtered = visibleRows.length !== agents.length;
    const tableTotal = filtered
      ? visibleRows.reduce((sum, agent) => sum + (agent.cost_usd || 0), 0)
      : (data?.total_usd || 0);
    return {
      span: span.value,
      monthOffset: monthOffset.value,
      monthLabel: monthLabel(monthOffset.value, now()),
      oldestMonthOffset: oldestMonthOffset(data?.first_day, now()),
      fillEmpty: fillEmpty.value,
      includeWeekends: includeWeekends.value,
      selectedHarnesses: selected,
      harnesses,
      query: query.value,
      sort: sort.value,
      payload: data,
      narrowed,
      projection,
      chart,
      rows: visibleRows,
      totalConversations,
      shownConversations,
      filtered,
      tableTotal,
      request: request.value,
      factor: factor.value,
      active: activeTab.value === 'costs',
      activeTab: activeTab.value,
      snapshotLoaded: snap !== null,
      visible: !!snap?.cost_tab_visible,
      whatif: !!snap?.cost_tab_whatif,
    };
  });

  function initialize() {
    if (initialized) return false;
    initialized = true;
    batch(() => {
      fillEmpty.value = prefs.getItem(FILL_KEY) === '1';
      includeWeekends.value = prefs.getItem(WEEKENDS_KEY) === '1';
      selectedHarnesses.value = savedHarnesses(prefs);
    });
    return true;
  }

  function setSpan(key) {
    if (key === 'month') {
      batch(() => { span.value = 'month'; monthOffset.value = 0; });
    } else if (['7d', '30d', '90d'].includes(key)) span.value = key;
  }

  function activateMonth(offset) {
    const next = Math.max(0, Math.min(24, Number(offset) || 0));
    batch(() => { monthOffset.value = next; span.value = next === 0 ? 'month' : 'calmonth'; });
  }

  function setFillEmpty(value) {
    fillEmpty.value = !!value;
    prefs.setItem(FILL_KEY, value ? '1' : '0');
  }

  function setIncludeWeekends(value) {
    includeWeekends.value = !!value;
    prefs.setItem(WEEKENDS_KEY, value ? '1' : '0');
  }

  function toggleHarness(harness) {
    const current = new Set(view.value.selectedHarnesses);
    if (current.has(harness)) current.delete(harness); else current.add(harness);
    if (current.size === 0) return false;
    const all = view.value.harnesses;
    const stored = current.size === all.length && all.every((item) => current.has(item)) ? [] : [...current];
    selectedHarnesses.value = stored;
    if (stored.length) prefs.setItem(HARNESSES_KEY, JSON.stringify(stored));
    else prefs.removeItem(HARNESSES_KEY);
    return true;
  }

  function cycleSort(key) {
    if (!COST_COLUMNS.some((column) => column.sort === key)) return false;
    const current = sort.value;
    if (current.key === key) sort.value = { key, dir: current.dir === 'asc' ? 'desc' : 'asc' };
    else sort.value = { key, dir: COST_COLUMNS.find((column) => column.sort === key)?.text ? 'asc' : 'desc' };
    return true;
  }

  function setQuery(value) { query.value = String(value ?? ''); }

  function beginRequest(requestId) {
    request.value = { ...request.value, phase: 'loading', requestId, error: null };
  }
  function commitRequest(requestId, data) {
    if (request.value.requestId !== requestId) return false;
    batch(() => {
      payload.value = data;
      request.value = { phase: 'ready', requestId, hasLoaded: true, error: null };
    });
    return true;
  }
  function failRequest(requestId, error) {
    if (request.value.requestId !== requestId) return false;
    batch(() => {
      // A range or WHAT-IF request failure must not leave the previous range's
      // chart under newly selected controls. This also matches the legacy tab,
      // which cleared every sibling pane on any endpoint failure.
      payload.value = null;
      request.value = { phase: 'error', requestId, hasLoaded: false, error: errorMessage(error) };
    });
    return true;
  }

  function editFactor(raw) {
    factor.value = { ...factor.value, raw: String(raw ?? ''), status: '', error: false, editVersion: factor.value.editVersion + 1 };
  }
  function beginFactor(status = 'saving…') {
    const requestId = factor.value.requestId + 1;
    factor.value = { ...factor.value, requestId, status, error: false };
    return { requestId, editVersion: factor.value.editVersion };
  }
  function commitFactor(token, patch) {
    if (factor.value.requestId !== token.requestId || factor.value.editVersion !== token.editVersion) return false;
    factor.value = { ...factor.value, ...patch, error: false };
    return true;
  }
  function failFactor(token, error) {
    if (factor.value.requestId !== token.requestId || factor.value.editVersion !== token.editVersion) return false;
    factor.value = { ...factor.value, status: errorMessage(error), error: true };
    return true;
  }

  return Object.freeze({
    span, monthOffset, fillEmpty, includeWeekends, selectedHarnesses, query,
    sort, payload, request, factor, view, initialize, setSpan, activateMonth,
    setFillEmpty, setIncludeWeekends, toggleHarness, cycleSort, setQuery,
    beginRequest, commitRequest, failRequest, editFactor, beginFactor,
    commitFactor, failFactor,
  });
}

export const costsState = createCostsState();
