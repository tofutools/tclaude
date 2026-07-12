function detail(error) {
  let message = error?.message || String(error);
  if (error?.body != null) {
    const body = typeof error.body === 'string'
      ? error.body
      : (error.body.error || error.body.message || JSON.stringify(error.body));
    if (body) message += `: ${body}`;
  }
  return message;
}

export function createPluginsActions({ requestMutation, refresh, confirm, notify, state } = {}) {
  for (const [name, dependency] of Object.entries({ requestMutation, refresh, confirm, notify })) {
    if (typeof dependency !== 'function') throw new TypeError(`plugins actions require ${name}`);
  }
  if (!state?.beginBusy || !state?.endBusy) throw new TypeError('plugins actions require state');

  async function run(key, operation, success) {
    if (!state.beginBusy(key)) return false;
    try {
      const result = await operation();
      const message = success?.(result);
      if (message) notify(message.text || message, !!message.error);
      return true;
    } catch (error) {
      notify(`Request failed: ${detail(error)}`, true);
      return false;
    } finally {
      state.endBusy(key);
    }
  }

  return Object.freeze({
    refresh,
    checkAll: (key) => run(key,
      () => requestMutation('/api/plugins/check', { method: 'POST' }),
      (result) => result?.warn
        ? `checks done — ${result.warn} plugin${result.warn === 1 ? '' : 's'} not active`
        : 'checks done — all plugins active'),
    checkPlugin: (plugin, key) => run(key,
      () => requestMutation(`/api/plugins/${encodeURIComponent(plugin.name)}/check`, { method: 'POST' }),
      (result) => ({
        text: `${plugin.name}: ${result?.status === 'ok' ? 'all checks pass' : result?.status === 'warn' ? 'some checks fail' : 'status unknown'}`,
        error: result?.status === 'warn',
      })),
    toggleStep: (plugin, index, verb, key) => run(key,
      () => requestMutation(`/api/plugins/${encodeURIComponent(plugin.name)}/steps/${encodeURIComponent(index)}/${encodeURIComponent(verb)}`, { method: 'POST' }),
      (result) => {
        const first = (result?.output || '').split('\n')[0].slice(0, 120);
        return { text: `step ${verb} ${result?.ok ? 'OK' : 'FAILED'}${first ? ': ' + first : ''}`, error: !result?.ok };
      }),
    togglePlugin: (plugin, verb, key) => run(key,
      () => requestMutation(`/api/plugins/${encodeURIComponent(plugin.name)}/${encodeURIComponent(verb)}`, { method: 'POST' }),
      (result) => {
        const first = (result?.output || '').split('\n')[0].slice(0, 120);
        if (!result?.ok) return { text: `${plugin.name} ${verb} had failures${first ? ': ' + first : ''} — see step outputs`, error: true };
        if (!result?.ran) return `${plugin.name} ${verb}d (nothing to ${verb === 'activate' ? 'run — all steps already satisfied' : 'stop — nothing was active'})`;
        return `${plugin.name} ${verb}d`;
      }),
    install: (plugin, key) => run(key,
      () => requestMutation('/api/plugins', { method: 'POST', body: plugin }),
      () => `plugin ${plugin.name} installed — click its lamp to bring it up`),
    deletePlugin: async (plugin, key) => {
      const yes = await confirm({
        title: 'Delete plugin?',
        body: `Remove the plugin definition "${plugin.name}" from plugins.json. This does NOT stop containers or unregister MCPs its steps set up — it only forgets the definition.`,
        okLabel: 'Delete',
      });
      if (!yes) return false;
      return run(key,
        () => requestMutation(`/api/plugins/${encodeURIComponent(plugin.name)}`, { method: 'DELETE' }),
        () => `plugin ${plugin.name} deleted`);
    },
    save: async (draft) => {
      if (!state.beginSubmit()) return false;
      const body = {
        name: draft.name.trim(), descr: draft.descr.trim(),
        steps: draft.steps.map((step) => ({
          name: step.name.trim(), check: step.check.trim(), run: step.run.trim(), stop: step.stop.trim(),
        })),
      };
      try {
        const editing = draft.mode === 'edit';
        await requestMutation(editing
          ? `/api/plugins/${encodeURIComponent(draft.originalName)}`
          : '/api/plugins', { method: editing ? 'PUT' : 'POST', body, refreshAfter: false });
        state.closeModal();
        notify(editing ? `plugin ${body.name} saved` : `plugin ${body.name} created`);
        // Preact removes the modal overlay after this synchronous state write.
        // Bypass only the modal suspension so the canonical post-save snapshot
        // is not dropped while the closing overlay finishes committing.
        await refresh({ force: true });
        return true;
      } catch (error) {
        state.failSubmit(detail(error));
        return false;
      }
    },
  });
}
