import { groupCreateRequest } from './group-create-model.js';

async function responseText(response) {
  try { return await response.text(); } catch (_) { return ''; }
}

export function createGroupCreateActions({
  fetchImpl = fetch,
  pickDirectory,
  openTemplateManager,
  notify = () => {},
  refresh = () => {},
  setExpanded = () => {},
  recordInteraction = () => {},
} = {}) {
  return Object.freeze({
    async submit(draft, template, parentGroup = '') {
      const request = groupCreateRequest(draft, template, parentGroup);
      const response = await fetchImpl(request.url, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(request.body),
      });
      const raw = await responseText(response);
      if (!response.ok) throw new Error(raw || `HTTP ${response.status}`);
      let payload = {};
      if (request.kind === 'template') {
        try { payload = JSON.parse(raw); } catch (_) {}
      }
      return Object.freeze({ ...request, response: payload });
    },

    async loadTemplates() {
      const response = await fetchImpl('/api/templates', {
        credentials: 'same-origin',
      });
      if (!response.ok) throw new Error((await responseText(response)) || `HTTP ${response.status}`);
      const payload = await response.json();
      return Array.isArray(payload) ? payload : [];
    },

    pickDirectory(options) {
      return pickDirectory(options);
    },

    openTemplateManager(onClose) {
      return openTemplateManager({ onClose });
    },

    complete(result, parentGroup = '') {
      const { name } = result;
      if (result.kind === 'blank') {
        notify(parentGroup
          ? `subgroup created: ${name} under ${parentGroup}`
          : `group created: ${name}`);
      } else {
        const response = result.response || {};
        const failed = response.failed || 0;
        notify(failed
          ? `group ${name}: spawned ${response.spawned || 0}, ${failed} failed — check the group`
          : `group ${name}: spawned ${response.spawned || 0} agent${response.spawned === 1 ? '' : 's'}`,
        failed > 0);
        const patternErrors = response.pattern_errors || [];
        if (patternErrors.length) {
          notify(`⚠ work pattern: ${patternErrors.length} step${patternErrors.length === 1 ? '' : 's'} not sent — ${patternErrors[0]}`, true);
        } else if (response.pattern_delivered) {
          notify(`work pattern: ${response.pattern_delivered} briefing${response.pattern_delivered === 1 ? '' : 's'} sent`);
        }
      }
      try { setExpanded(name); } catch (_) {}
      recordInteraction(name);
      refresh();
    },
  });
}
