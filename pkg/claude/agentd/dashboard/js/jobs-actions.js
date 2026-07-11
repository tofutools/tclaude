export function createJobsActions({
  requestMutation,
  refresh,
  confirm,
  notify,
  download,
  createCron,
  editCron,
} = {}) {
  for (const [name, dependency] of Object.entries({
    requestMutation, refresh, confirm, notify, download, createCron, editCron,
  })) {
    if (typeof dependency !== 'function') throw new TypeError(`jobs actions require ${name}`);
  }
  async function run(label, operation) {
    try {
      await operation();
      if (label) notify(label);
      return true;
    } catch (error) {
      let detail = error?.message || String(error);
      if (error?.body != null) {
        const body = typeof error.body === 'string'
          ? error.body
          : (error.body.error || error.body.message || JSON.stringify(error.body));
        if (body) detail += `: ${body}`;
      }
      notify(`Request failed: ${detail}`, true);
      return false;
    }
  }

  return Object.freeze({
    refresh,
    createCron,
    editCron,
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
