import { batch, computed, signal } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';
import { dashPrefs } from './prefs.js';
import {
  COST_COLUMNS, buildAccumulatedCostChart, buildCostChart, costModelLabel, costModels, costModelStats,
  costProviders, costProviderStats,
  costProviderLabel, filterCostData,
  matchesCostAgent, monthLabel, monthProjection, oldestMonthOffset,
  resolveModelSelection, resolveProviderSelection, sortCostAgents,
} from './costs-model.js';

const FILL_KEY = 'tclaude.dash.costs.fillEmptyWeekdays';
const WEEKENDS_KEY = 'tclaude.dash.costs.includeWeekends';
const PROVIDERS_KEY = 'tclaude.dash.costs.providers';
const LEGACY_HARNESSES_KEY = 'tclaude.dash.costs.harnesses';
const MODELS_KEY = 'tclaude.dash.costs.models';

function errorMessage(error) {
  return String(error?.message || error);
}

function savedSelection(prefs, key) {
  try {
    const value = JSON.parse(prefs.getItem(key) || '[]');
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
  const selectedProviders = signal([]);
  const selectedModels = signal([]);
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
    const providers = costProviders(agents);
    const selected = resolveProviderSelection(providers, selectedProviders.value);
    const models = costModels(agents);
    const modelStats = costModelStats(agents, selected);
    const availableModels = new Set(modelStats.filter((entry) => entry.available).map((entry) => entry.model));
    const compatibleModels = [...resolveModelSelection(models, selectedModels.value)]
      .filter((model) => availableModels.has(model));
    const selectedModelSet = new Set(compatibleModels.length ? compatibleModels : availableModels);
    const narrowed = data ? filterCostData(data, selected, selectedModelSet) : null;
    const projection = narrowed && span.value === 'month'
      ? monthProjection(narrowed, fillEmpty.value, includeWeekends.value, now())
      : null;
    const chart = narrowed ? buildCostChart(narrowed, projection, agents, selected, providers, selectedModelSet) : null;
    const providerStats = costProviderStats(agents);
    const providerScopedTotal = modelStats.reduce((sum, entry) => sum + entry.cost, 0);
    const selectedModelTotal = modelStats.reduce((sum, entry) => sum
      + (selectedModelSet.has(entry.model) ? entry.cost : 0), 0);
    return {
      data, agents, providers, selected, models, selectedModelSet, narrowed, projection,
      providerStats, modelStats, availableModels, providerScopedTotal,
      modelCoverage: providerScopedTotal > 0 ? selectedModelTotal / providerScopedTotal : 0,
      chart,
      accumulatedChart: chart ? buildAccumulatedCostChart(chart) : null,
    };
  });

  const view = computed(() => {
    const snap = snapshot.value;
    const { data, agents, providers, selected, models, selectedModelSet,
      narrowed, projection, chart, accumulatedChart, providerStats, modelStats,
      availableModels, providerScopedTotal, modelCoverage } = costData.value;
    const visibleRows = sortCostAgents(agents, sort.value)
      .filter((agent) => selected.has(costProviderLabel(agent)))
      .filter((agent) => selectedModelSet.has(costModelLabel(agent)))
      .filter((agent) => matchesCostAgent(agent, query.value));
    const totalConversations = new Set(agents.map((agent) => agent.conv_id)).size;
    const shownConversations = new Set(visibleRows.map((agent) => agent.conv_id)).size;
    const filtered = visibleRows.length !== agents.length;
    const tableTotal = filtered
      ? visibleRows.reduce((sum, agent) => sum + (agent.cost_usd || 0), 0)
      : (data?.total_usd || 0);
    const tableWhatIfTotal = filtered
      ? visibleRows.reduce((sum, agent) => sum + (agent.what_if_cost_usd || 0), 0)
      : (data?.what_if_total_usd || 0);
    // The caveat also keys off the row kinds, not the hypothetical subtotal
    // alone: a payload can carry a row's kind without its split fields (the
    // chart walk in costs-model defends against that same legacy shape), and
    // then the banner would hide while the rows still show WHAT-IF markers
    // pointing at it — leaving each marker a dead control. Narrowed by harness
    // like the subtotals, but not by the text query, so the caveat covers the
    // same rows the header totals do.
    const hasWhatIfRows = agents.some((agent) => selected.has(costProviderLabel(agent))
      && selectedModelSet.has(costModelLabel(agent))
      && (agent.cost_kind === 'what_if' || agent.cost_kind === 'mixed'));
    return {
      span: span.value,
      monthOffset: monthOffset.value,
      monthLabel: monthLabel(monthOffset.value, now()),
      oldestMonthOffset: oldestMonthOffset(data?.first_day, now()),
      fillEmpty: fillEmpty.value,
      includeWeekends: includeWeekends.value,
      selectedProviders: selected,
      selectedModels: selectedModelSet,
      availableModels,
      providers,
      models,
      providerStats,
      modelStats,
      providerScopedTotal,
      modelCoverage,
      query: query.value,
      sort: sort.value,
      payload: data,
      narrowed,
      projection,
      chart,
      accumulatedChart,
      rows: visibleRows,
      totalConversations,
      shownConversations,
      filtered,
      tableTotal,
      tableWhatIfTotal,
      hasWhatIf: (narrowed?.what_if_total_usd || 0) > 0 || hasWhatIfRows,
      hasReal: (narrowed?.real_total_usd || 0) > 0,
      request: request.value,
      factor: factor.value,
      active: activeTab.value === 'costs',
      activeTab: activeTab.value,
      snapshotLoaded: snap !== null,
      visible: !!snap?.cost_tab_visible,
      whatIfEnabled: !!snap?.cost_tab_whatif,
    };
  });

  function initialize() {
    if (initialized) return false;
    initialized = true;
    batch(() => {
      fillEmpty.value = prefs.getItem(FILL_KEY) === '1';
      includeWeekends.value = prefs.getItem(WEEKENDS_KEY) === '1';
      selectedProviders.value = savedSelection(prefs, PROVIDERS_KEY);
      if (!selectedProviders.value.length) selectedProviders.value = savedSelection(prefs, LEGACY_HARNESSES_KEY);
      selectedModels.value = savedSelection(prefs, MODELS_KEY);
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

  function toggleProvider(provider) {
    const current = new Set(view.value.selectedProviders);
    if (current.has(provider)) current.delete(provider); else current.add(provider);
    if (current.size === 0) return false;
    const all = view.value.providers;
    const stored = current.size === all.length && all.every((item) => current.has(item)) ? [] : [...current];
    selectedProviders.value = stored;
    if (stored.length) prefs.setItem(PROVIDERS_KEY, JSON.stringify(stored));
    else prefs.removeItem(PROVIDERS_KEY);
    prefs.removeItem(LEGACY_HARNESSES_KEY);
    const available = new Set(costModelStats(view.value.payload?.agents || [], current)
      .filter((entry) => entry.available).map((entry) => entry.model));
    const retained = [...view.value.selectedModels].filter((model) => available.has(model));
    const nextModels = retained.length ? retained : [...available];
    selectedModels.value = nextModels;
    if (nextModels.length) prefs.setItem(MODELS_KEY, JSON.stringify(nextModels));
    else prefs.removeItem(MODELS_KEY);
    return true;
  }

  function toggleModel(model) {
    if (!view.value.availableModels.has(model)) return false;
    const current = new Set(view.value.selectedModels);
    if (current.has(model)) current.delete(model); else current.add(model);
    if (current.size === 0) return false;
    const all = view.value.models;
    const stored = current.size === all.length && all.every((item) => current.has(item)) ? [] : [...current];
    selectedModels.value = stored;
    if (stored.length) prefs.setItem(MODELS_KEY, JSON.stringify(stored));
    else prefs.removeItem(MODELS_KEY);
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
    span, monthOffset, fillEmpty, includeWeekends, selectedProviders, selectedModels, query,
    sort, payload, request, factor, view, initialize, setSpan, activateMonth,
    setFillEmpty, setIncludeWeekends, toggleProvider, toggleModel, cycleSort, setQuery,
    beginRequest, commitRequest, failRequest, editFactor, beginFactor,
    commitFactor, failFactor,
  });
}

export const costsState = createCostsState();
