import { groupCreateRequest } from './group-create-model.js';
import { insertGroupBeside, setGroupOrderPref, sortGroupsByPref } from './group-order.js';

const CLONE_TRANSPORT_PREF = 'tclaude.dash.group-create.clone-transport';

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
  getSnapshot = () => null,
  prefs,
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
      if (!response.ok) {
        let message = raw;
        try {
          const parsed = JSON.parse(raw);
          message = parsed?.message || parsed?.error || raw;
        } catch (_) {}
        throw new Error(message || `HTTP ${response.status}`);
      }
      let payload = {};
      if (request.kind === 'template' || request.kind === 'clone') {
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

    cloneTransport() {
      try { return prefs?.getItem(CLONE_TRANSPORT_PREF) === 'https' ? 'https' : 'ssh'; } catch (_) { return 'ssh'; }
    },

    rememberCloneTransport(value) {
      const transport = value === 'https' ? 'https' : 'ssh';
      try { prefs?.setItem(CLONE_TRANSPORT_PREF, transport); } catch (_) {}
    },

    complete(result, parentGroup = '') {
      let { name } = result;
      if (result.kind === 'blank') {
        notify(parentGroup
          ? `subgroup created: ${name} under ${parentGroup}`
          : `group created: ${name}`);
      } else if (result.kind === 'template') {
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
      } else {
        const response = result.response || {};
        name = response.group || name;
        if (response.group && result.placement?.anchor) {
          const snapshotGroups = getSnapshot()?.groups || [];
          const names = sortGroupsByPref(snapshotGroups.slice()).map((group) => group.name);
          setGroupOrderPref(insertGroupBeside(
            names, response.group, result.placement.anchor, !!result.placement.before,
          ));
        }
        const failed = (response.members || []).filter((member) => member?.error).length;
        const created = response.group ? `"${response.group}"` : 'new group';
        const bits = [];
        if (!result.withAgents) bits.push('no member agents');
        if (!result.copyOwners) bits.push('no owners');
        notify(
          result.withAgents
            ? failed
              ? `Cloned ${result.source} → ${created} (${failed} member(s) skipped — see CLI for detail${bits.length ? `; ${bits.join(', ')}` : ''})`
              : `Cloned ${result.source} → ${created}${bits.length ? ` (${bits.join(', ')})` : ''}`
            : `Cloned ${result.source} → ${created} (settings only${result.copyOwners ? ' + owners' : ''})`,
          failed > 0,
        );
      }
      try { setExpanded(name); } catch (_) {}
      recordInteraction(name);
      refresh();
    },
  });
}
