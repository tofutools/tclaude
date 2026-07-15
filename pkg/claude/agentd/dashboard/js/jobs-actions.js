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

  return Object.freeze({
    refresh,
    openCronCreate: state.openCronCreate,
    openCronEdit: state.openCronEdit,
    openCronDuplicate: state.openCronDuplicate,
    closeCronDialog: state.closeCronDialog,
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
    downloadExport: (job) => download(job.id),
    dismissExport: async (job) => {
      const yes = await confirm({
        title: 'Dismiss this export?',
        body: 'Removes the export job from the Jobs list and deletes its file from the server (if one was delivered). A still-running job is discarded when it lands.',
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
  });
}
