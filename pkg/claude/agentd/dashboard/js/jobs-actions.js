export function createJobsActions({
  state,
  requestMutation,
  refresh,
  confirm,
  notify,
  download,
} = {}) {
  if (!state || typeof state.upsertCron !== 'function') {
    throw new TypeError('jobs actions require state');
  }
  for (const [name, dependency] of Object.entries({
    requestMutation, refresh, confirm, notify, download,
  })) {
    if (typeof dependency !== 'function') throw new TypeError(`jobs actions require ${name}`);
  }

  function detail(error) {
    let value = error?.message || String(error);
    if (error?.body != null) {
      const body = typeof error.body === 'string'
        ? error.body
        : (error.body.error || error.body.message || JSON.stringify(error.body));
      if (body) value += `: ${body}`;
    }
    return value;
  }

  async function run(label, operation) {
    try {
      await operation();
      if (label) notify(label);
      return true;
    } catch (error) {
      notify(`Request failed: ${detail(error)}`, true);
      return false;
    }
  }

  function standingOrderCAS(order) {
    return `row_version=${encodeURIComponent(order.row_version)}`;
  }

  return Object.freeze({
    refresh,
    openCronCreate: state.openCronCreate,
    openCronEdit: state.openCronEdit,
    openCronDuplicate: state.openCronDuplicate,
    closeCronDialog: state.closeCronDialog,
    openStandingOrderCreate: state.openStandingOrderCreate,
    openStandingOrderEdit: state.openStandingOrderEdit,
    closeStandingOrderDialog: state.closeStandingOrderDialog,
    openTriggerCreate: state.openTriggerCreate,
    openTriggerEdit: state.openTriggerEdit,
    closeTriggerDialog: state.closeTriggerDialog,
    loadTriggers: async () => {
      const response = await requestMutation('/api/triggers', { method: 'GET', refreshAfter: false });
      const rules = Array.isArray(response?.triggers) ? response.triggers : [];
      return Promise.all(rules.map(async (rule) => {
        const history = await requestMutation(`/api/triggers/${encodeURIComponent(rule.id)}/firings?limit=1`, {
          method: 'GET', refreshAfter: false,
        });
        return { ...rule, firings: Array.isArray(history?.firings) ? history.firings : [] };
      }));
    },
    loadTriggerFirings: async (id) => {
      const response = await requestMutation(`/api/triggers/${encodeURIComponent(id)}/firings?limit=20`, {
        method: 'GET', refreshAfter: false,
      });
      return Array.isArray(response?.firings) ? response.firings : [];
    },
    saveTrigger: async ({ editing, id, payload }) => {
      try {
        const trigger = await requestMutation(editing
          ? `/api/triggers/${encodeURIComponent(id)}` : '/api/triggers', {
          method: editing ? 'PATCH' : 'POST', body: payload, refreshAfter: false,
        });
        state.invalidateTriggers();
        notify(`trigger ${editing ? 'saved' : 'created'}: ${trigger?.name || ('#' + (trigger?.id || ''))}`);
        return trigger;
      } catch (error) {
        throw new Error(detail(error), { cause: error });
      }
    },
    toggleTrigger: (rule) => {
      const verb = rule.enabled ? 'disable' : 'enable';
      return run(`trigger ${verb}: ${rule.name}`, () => requestMutation(
        `/api/triggers/${encodeURIComponent(rule.id)}/${verb}?row_version=${encodeURIComponent(rule.row_version)}`,
        { method: 'POST', refreshAfter: false },
      ));
    },
    deleteTrigger: async (rule) => {
      const yes = await confirm({
        title: 'Delete trigger?',
        body: 'Removes the trigger and its firing history. Spawned workers and messages are unaffected.',
        meta: rule.name,
        okLabel: 'Delete trigger',
      });
      if (!yes) return false;
      return run(`trigger delete: ${rule.name}`, () => requestMutation(
        `/api/triggers/${encodeURIComponent(rule.id)}?row_version=${encodeURIComponent(rule.row_version)}`,
        { method: 'DELETE', refreshAfter: false },
      ));
    },
    explainCron: (expr) => requestMutation('/api/cron/explain', {
      body: { expr }, refreshAfter: false,
    }),
    saveCron: async ({ path, method, payload }) => {
      try {
        const cron = await requestMutation(path, {
          method, body: payload, refreshAfter: false,
        });
        state.upsertCron(cron);
        notify(`cron ${method === 'PATCH' ? 'saved' : 'created'}: ${cron?.name || ('#' + (cron?.id || ''))}`);
        void refresh();
        return cron;
      } catch (error) {
        throw new Error(detail(error), { cause: error });
      }
    },
    saveStandingOrder: async ({ path, method, payload }) => {
      try {
        const order = await requestMutation(path, {
          method, body: payload, refreshAfter: false,
        });
        notify(`standing order ${method === 'PATCH' ? 'saved' : 'created'}: ${order?.name || ('#' + (order?.id || ''))}`);
        if (order?.hook_setup_warning) {
          notify(`Standing order saved, but hook setup needs attention: ${order.hook_setup_warning}`, true);
        }
        void refresh();
        return order;
      } catch (error) {
        throw new Error(detail(error), { cause: error });
      }
    },
    downloadExport: (job) => download(job.id),
    dismissExport: async (job) => {
      const yes = await confirm({
        title: 'Dismiss this export?',
        body: 'Removes the export job from the Automations list and deletes its file from the server (if one was delivered). A still-running job is discarded when it lands.',
        meta: job.title || job.conv_label || ('#' + job.id),
        okLabel: 'Dismiss',
      });
      if (!yes) return false;
      return run(`export job dismiss: ${job.title || job.conv_label || ('#' + job.id)}`, () =>
        requestMutation(`/api/export-jobs/${encodeURIComponent(job.id)}`, { method: 'DELETE' }));
    },
    toggleCron: (job) => {
      const verb = job.enabled ? 'disable' : 'enable';
      return run(`cron ${verb}: ${job.name}`, () =>
        requestMutation(`/api/cron/${encodeURIComponent(job.id)}/${verb}`, { method: 'POST' }));
    },
    runCron: async (job) => {
      const yes = await confirm({
        title: 'Fire this cron job now?',
        body: "Sends the job's message to its target immediately. Stamps last_run_at so the regular cadence resumes from now.",
        meta: job.name,
        okLabel: 'Fire now',
      });
      if (!yes) return false;
      return run(`cron run now: ${job.name}`, () =>
        requestMutation(`/api/cron/${encodeURIComponent(job.id)}/run-now`, { method: 'POST' }));
    },
    deleteCron: async (job) => {
      const yes = await confirm({
        title: 'Delete cron job?',
        body: 'Removes the job and its run history. The target itself is unaffected; you can re-create the job with `tclaude agent cron add`.',
        meta: job.name,
        okLabel: 'Delete job',
      });
      if (!yes) return false;
      return run(`cron delete: ${job.name}`, () =>
        requestMutation(`/api/cron/${encodeURIComponent(job.id)}`, { method: 'DELETE' }));
    },
    toggleStandingOrder: async (order) => {
      const verb = order.enabled ? 'disable' : 'enable';
      if (!order.enabled && order.disabled_reason) {
        const yes = await confirm({
          title: 'Re-enable automatically disabled order?',
          body: `This order was disabled automatically (${order.disabled_reason}). Re-enabling is an explicit override and clears that retirement marker.`,
          meta: order.name,
          okLabel: 'Re-enable order',
        });
        if (!yes) return false;
      }
      return run(`standing order ${verb}: ${order.name}`, () =>
        requestMutation(`/api/standing-orders/${encodeURIComponent(order.id)}/${verb}?${standingOrderCAS(order)}`,
          { method: 'POST' }));
    },
    deleteStandingOrder: async (order) => {
      const yes = await confirm({
        title: 'Delete standing order?',
        body: 'Removes the order and its evaluation history. The target itself is unaffected.',
        meta: order.name,
        okLabel: 'Delete order',
      });
      if (!yes) return false;
      return run(`standing order delete: ${order.name}`, () =>
        requestMutation(`/api/standing-orders/${encodeURIComponent(order.id)}?${standingOrderCAS(order)}`,
          { method: 'DELETE' }));
    },
  });
}
