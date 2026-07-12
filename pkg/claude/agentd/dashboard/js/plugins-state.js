import { batch, computed, signal } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';
import { dashPrefs } from './prefs.js';

const FILTER_KEY = 'tclaude.dash.filter.plugins';

function matches(plugin, needle) {
  if (!needle) return true;
  if ((plugin.name || '').toLowerCase().includes(needle)) return true;
  if ((plugin.descr || '').toLowerCase().includes(needle)) return true;
  return (plugin.steps || []).some((step) =>
    [step.name, step.check, step.run, step.stop]
      .some((value) => (value || '').toLowerCase().includes(needle)));
}

export const pluginBusyKey = (action, name = '', index = '') =>
  `${action}:${name}:${index}`;

export function createPluginsState({
  snapshot = dashboardState.snapshot,
  poll = dashboardState.poll,
  activeTab = dashboardState.activeTab,
  prefs = dashPrefs,
} = {}) {
  const query = signal('');
  const busy = signal(new Set());
  const modal = signal(null);
  let initialized = false;
  let nextStepKey = 1;
  const emptyStep = () => ({ _key: nextStepKey++, name: '', check: '', run: '', stop: '' });

  const view = computed(() => {
    const value = snapshot.value;
    const all = value?.plugins || [];
    const catalog = value?.plugins_catalog || [];
    const needle = query.value.trim().toLowerCase();
    const installedNames = new Set(all.map((plugin) => plugin.name));
    return {
      all,
      installed: all.filter((plugin) => matches(plugin, needle)),
      catalog: catalog.filter((plugin) => !installedNames.has(plugin.name) && matches(plugin, needle)),
      query: query.value,
      warningCount: value?.plugins_warn || 0,
      registryError: value?.plugins_error || null,
      visible: !!value?.plugins_tab_visible,
      activeTab: activeTab.value,
      request: {
        phase: poll.value.phase,
        requestId: poll.value.requestId,
        hasLoaded: value !== null,
        error: poll.value.error,
      },
      busy: busy.value,
      modal: modal.value,
    };
  });

  function initialize() {
    if (initialized) return false;
    initialized = true;
    query.value = prefs.getItem(FILTER_KEY) || '';
    return true;
  }

  function setQuery(value) {
    const next = String(value ?? '');
    query.value = next;
    if (next) prefs.setItem(FILTER_KEY, next);
    else prefs.removeItem(FILTER_KEY);
  }

  function beginBusy(key) {
    if (busy.value.has(key)) return false;
    const next = new Set(busy.value);
    next.add(key);
    busy.value = next;
    return true;
  }

  function endBusy(key) {
    if (!busy.value.has(key)) return false;
    const next = new Set(busy.value);
    next.delete(key);
    busy.value = next;
    return true;
  }

  function openModal(plugin = null) {
    modal.value = {
      mode: plugin ? 'edit' : 'create',
      originalName: plugin?.name || null,
      name: plugin?.name || '',
      descr: plugin?.descr || '',
      steps: plugin?.steps?.length ? plugin.steps.map((step) => ({ ...emptyStep(), ...step })) : [emptyStep()],
      submitting: false,
      error: null,
    };
  }

  function closeModal() {
    modal.value = null;
  }

  function updateModal(patch) {
    if (!modal.value) return false;
    modal.value = { ...modal.value, ...patch, error: null };
    return true;
  }

  function updateStep(index, patch) {
    if (!modal.value?.steps[index]) return false;
    const steps = modal.value.steps.map((step, current) =>
      current === index ? { ...step, ...patch } : step);
    updateModal({ steps });
    return true;
  }

  function addStep() {
    if (!modal.value) return false;
    updateModal({ steps: [...modal.value.steps, emptyStep()] });
    return true;
  }

  function removeStep(index) {
    if (!modal.value?.steps[index]) return false;
    updateModal({ steps: modal.value.steps.filter((_, current) => current !== index) });
    return true;
  }

  function beginSubmit() {
    if (!modal.value || modal.value.submitting) return false;
    modal.value = { ...modal.value, submitting: true, error: null };
    return true;
  }

  function failSubmit(error) {
    if (!modal.value) return false;
    modal.value = { ...modal.value, submitting: false, error: String(error?.message || error) };
    return true;
  }

  return Object.freeze({
    query, busy, modal, view, initialize, setQuery, beginBusy, endBusy,
    openModal, closeModal, updateModal, updateStep, addStep, removeStep,
    beginSubmit, failSubmit,
  });
}

export const pluginsState = createPluginsState();
