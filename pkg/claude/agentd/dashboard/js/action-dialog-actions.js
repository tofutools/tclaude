import { clonePayload } from './clone-payload.js';
import { findSpawnHarness, sandboxImplOptionsFor } from './agent-spawn-model.js';
import { pickDirectory as defaultPickDirectory } from './helpers.js';
import { migrateGroupRenamePrefs } from './group-rename-prefs.js';
import { announceNodeChanged, nodeFetch, nodeSnapshot } from './node-api.js';

const CLONE_TIMEOUT_MS = 35_000;
const EXPORT_POLL_INTERVAL_MS = 2_000;
const EXPORT_SLOW_AFTER_MS = 90_000;

async function responseError(response) {
  const text = await response.text();
  try {
    const payload = JSON.parse(text);
    if (payload?.error) return String(payload.error);
  } catch (_) { /* retain plain-text response below */ }
  return text || `HTTP ${response.status}`;
}

async function requestJSON(fetchImpl, url, options = {}) {
  const response = await fetchImpl(url, { credentials: 'same-origin', ...options });
  if (!response.ok) {
    const error = new Error(await responseError(response));
    error.status = response.status;
    throw error;
  }
  try { return await response.json(); } catch (_) { return {}; }
}

export function normaliseFollowUp(value) {
  return String(value || '').replace(/[\r\n\t]+/g, ' ').replace(/\s+/g, ' ').trim();
}

export function descendantsOf(name, groups) {
  const children = new Map();
  for (const group of groups || []) {
    if (!group.parent) continue;
    const rows = children.get(group.parent) || [];
    rows.push(group.name);
    children.set(group.parent, rows);
  }
  const found = new Set();
  const pending = [name];
  while (pending.length) {
    const current = pending.pop();
    if (found.has(current)) continue;
    found.add(current);
    pending.push(...(children.get(current) || []));
  }
  return found;
}

// groupSettingsPatch is the PATCH body for the settings dialog's values.
function groupSettingsPatch(values) {
	return {
		descr: values.descr.trim(), default_cwd: values.defaultCwd.trim(),
		default_context: values.defaultContext, default_profile: values.defaultProfile,
		default_prompts: values.defaultPrompts || [],
		environment: values.environment.map((entry) => ({ name: entry.name.trim(), value: entry.value })).filter((entry) => entry.name),
		max_members: Math.max(0, Number(values.maxMembers) || 0),
		notify_enabled: !!values.notifyEnabled,
		remote_control_policy: values.remoteControlPolicy,
		reinject_after_compact: values.reinjectAfterCompact || 'contexts',
	};
}

export function createActionDialogActions({
  state,
  fetchImpl = fetch,
  refresh,
  notify,
  downloadExport = () => {},
  getSnapshot = () => null,
  pickDirectory = defaultPickDirectory,
  setTimer = setTimeout,
  clearTimer = clearTimeout,
}) {
  async function exportRequest(url, options = {}) {
    return requestJSON(fetchImpl, url, options);
  }

  return Object.freeze({
    openClone: state.openClone,
    openReincarnate: state.openReincarnate,
    openNest({ group, node = '' }) {
      if (!group) { notify('no group', true); return; }
      state.openNest({ group, node });
    },
    openTaskLink: state.openTaskLink,
    openGroupAttachment: state.openGroupAttachment,
		openGroupSettings: state.openGroupSettings,
    pickDirectory,
    openPresetClone(options) {
      if (!options?.source?.name || typeof options.create !== 'function') {
        notify('nothing to clone', true);
        return false;
      }
      return state.openPresetClone(options);
    },
    openExport: state.openExport,
    openSandboxImpl: state.openSandboxImpl,
    openLaunchTarget: state.openLaunchTarget,
    openTerminalDirectory: state.openTerminalDirectory,
    openTerminalChooser: state.openTerminalChooser,
    finishChoice: state.finishChoice,
    close: state.close,
    // node names another node's group (the fused Groups view): its parent
    // candidates are that node's groups.
    nestModel(group, node = '') {
      const groups = (nodeSnapshot(node) || getSnapshot())?.groups || [];
      const current = groups.find((item) => item.name === group);
      const blocked = descendantsOf(group, groups);
      return {
        currentParent: current?.parent || '',
        candidates: groups.map((item) => item.name)
          .filter((name) => !blocked.has(name))
          .sort((a, b) => a.localeCompare(b)),
      };
    },
    async loadWorktrees(repo, { signal } = {}) {
      if (!repo?.trim()) return { is_repo: false, empty: true };
      return requestJSON(fetchImpl, `/api/worktrees?repo=${encodeURIComponent(repo.trim())}`, { signal });
    },
    async createWorktree({ repo, branch, fromBranch }) {
      return requestJSON(fetchImpl, '/api/worktrees', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ repo, branch, from_branch: fromBranch || '' }),
      });
    },
    // node names another node's agent (blank is the page's), whose route
    // takes the follow-up and the history choice only.
    async cloneAgent({ conv, node = '', label, followUp, copyConversation, copyOwnership = false, cwd }, owner) {
      const controller = new AbortController();
      const timer = setTimeout(() => controller.abort(), CLONE_TIMEOUT_MS);
      try {
        const body = { follow_up: normaliseFollowUp(followUp), no_copy_conv: !copyConversation };
        if (!node && !globalThis.__tclaudeRemoteNode?.id) Object.assign(body, { copy_ownership: !!copyOwnership, cwd: cwd || '' });
        const payload = await requestJSON(nodeFetch(node, { fetchImpl }), `/api/agents/${encodeURIComponent(conv)}/clone`, {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
          signal: controller.signal,
        });
        state.close(owner);
        announceNodeChanged(node);
        const target = payload.new_conv ? ` → ${String(payload.new_conv).slice(0, 8)}` : '';
        notify(payload.warning
          ? `cloned ${label}${target} (warning: ${payload.warning})`
          : `cloned ${label}${target}`, !!payload.warning);
        await refresh();
        return payload;
      } catch (error) {
        if (error?.name === 'AbortError') {
          throw new Error(`clone timed out after ${CLONE_TIMEOUT_MS / 1000}s — the new agent may still come online; check ~/.tclaude/output.log and refresh in a moment.`);
        }
        throw error;
      } finally { clearTimeout(timer); }
    },
    async reincarnateAgent({ conv, node = '', label, mode, focusHint, followUp }, owner) {
      const body = mode === 'force'
        ? { mode: 'force', follow_up: normaliseFollowUp(followUp) }
        : { mode: 'self', ...(focusHint.trim() ? { focus_hint: focusHint.trim() } : {}) };
      if (mode === 'force' && !body.follow_up) throw new Error('follow-up is required for force reincarnate');
      // node names another node's agent (the fused view); blank is the page's.
      const payload = await requestJSON(nodeFetch(node, { fetchImpl }), `/api/agents/${encodeURIComponent(conv)}/reincarnate`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
      });
      state.close(owner);
      announceNodeChanged(node);
      if (mode === 'force') {
        const suffix = payload.new_title ? ` → ${payload.new_title}`
          : payload.new_conv ? ` → ${String(payload.new_conv).slice(0, 8)}` : '';
        notify(`reincarnated ${label}${suffix}`);
      } else notify(`asked ${label} to reincarnate itself`);
      await refresh();
      return payload;
    },
    async nestGroup({ group, parent, node = '' }, owner) {
      await requestJSON(nodeFetch(node, { fetchImpl }), `/api/groups/${encodeURIComponent(group)}/parent`, {
        method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ parent }),
      });
      state.close(owner);
      notify(parent ? `${group}: nested under ${parent}` : `${group}: moved to top level`);
      announceNodeChanged(node);
      await refresh();
    },
    // Persist the two independent pieces of an agent's task reference — the full
    // http(s) URL and its optional display label. A blank label is sent blank so
    // the daemon stays the single source of truth for Linear/GitHub/hostname
    // derivation; a blank URL clears the reference. `changed` is false when the
    // submit is a no-op, in which case nothing is POSTed.
    async setTaskLink({ conv, node = '', label, url, taskLabel, changed }, owner) {
      if (!changed) {
        state.close(owner);
        notify('no changes');
        return;
      }
      // node names another node's agent (the fused view); blank is the page's.
      await requestJSON(nodeFetch(node, { fetchImpl }), `/api/agents/${encodeURIComponent(conv)}/task`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(url ? { url, label: taskLabel } : { clear: true }),
      });
      state.close(owner);
      notify(url ? `task link updated: ${label}` : `task link cleared: ${label}`);
      announceNodeChanged(node);
      await refresh();
    },
    async setGroupAttachment({ group, node = '', url, attachmentLabel, changed }, owner) {
      if (!changed) {
        state.close(owner);
        notify('no changes');
        return;
      }
      // node names another node's group (the fused view); blank is the page's.
      await requestJSON(nodeFetch(node, { fetchImpl }), `/api/groups/${encodeURIComponent(group)}/attachment`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(url ? { url, label: attachmentLabel } : { clear: true }),
      });
      state.close(owner);
      notify(url ? `${group}: attachment updated` : `${group}: attachment cleared`);
      announceNodeChanged(node);
      await refresh();
    },
		// Only the parts that changed are sent (all of them without an initial
		// baseline): each has its own permission gate, and a caller acting on
		// another node may hold some of them only. node names that node's group
		// (the fused Groups view); blank is the page's.
		async saveGroupSettings({ group, values, initial = null, node = '' }, owner) {
			const newName = String(values.name || '').trim();
			if (!newName) throw new Error('name is required');
			const api = nodeFetch(node, { fetchImpl });
			const patch = groupSettingsPatch(values);
			const before = initial ? groupSettingsPatch(initial) : null;
			const changedFields = Object.fromEntries(Object.entries(patch)
				.filter(([key, value]) => !before || JSON.stringify(value) !== JSON.stringify(before[key])));
			const attachment = (v) => (v.attachmentURL.trim() ? { url: v.attachmentURL.trim(), label: v.attachmentLabel.trim() } : { clear: true });
			if (Object.keys(changedFields).length) {
				await requestJSON(api, `/api/groups/${encodeURIComponent(group)}`, {
					method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(changedFields),
				});
			}
			if (!initial || values.sandboxProfile !== initial.sandboxProfile) {
				await requestJSON(api, `/api/groups/${encodeURIComponent(group)}/sandbox-profile`, {
					method: values.sandboxProfile ? 'PUT' : 'DELETE', headers: { 'Content-Type': 'application/json' },
					...(values.sandboxProfile ? { body: JSON.stringify({ name: values.sandboxProfile }) } : {}),
				});
			}
			if (!initial || JSON.stringify(attachment(values)) !== JSON.stringify(attachment(initial))) {
				await requestJSON(api, `/api/groups/${encodeURIComponent(group)}/attachment`, {
					method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(attachment(values)),
				});
			}
			if (newName !== group) {
				await requestJSON(api, `/api/groups/${encodeURIComponent(group)}/rename`, {
					method: 'POST', headers: { 'Content-Type': 'application/json' },
					body: JSON.stringify({ new_name: newName }),
				});
				// Rename prefs are this browser's view of the page node's groups.
				if (!node) migrateGroupRenamePrefs(group, newName);
			}
			state.close(owner);
			notify(`${newName}: settings saved`);
			announceNodeChanged(node);
			await refresh();
		},
    async clonePreset({ source, create, name }, owner) {
      const cleanName = String(name || '').trim();
      if (!cleanName) throw new Error('name is required');
      if (cleanName === source.name) throw new Error('pick a different name for the copy');
      await create(clonePayload(source, cleanName));
      state.close(owner);
      notify(`cloned: ${cleanName}`);
      await refresh();
    },
    async startExport({ conv, preset, title, instructions, sameGroup, signal }) {
      return exportRequest(`/api/agents/${encodeURIComponent(conv)}/export`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          preset,
          title: String(title || '').trim(),
          instructions: String(instructions || '').trim(),
          same_group: !!sameGroup,
        }),
        signal,
      });
    },
    async loadExportHistory(conv, { signal } = {}) {
      const payload = await exportRequest(
        `/api/agents/${encodeURIComponent(conv)}/exports`, { signal },
      );
      return payload?.exports || [];
    },
    async deleteExport(jobID) {
      await exportRequest(`/api/export-jobs/${encodeURIComponent(jobID)}`, { method: 'DELETE' });
    },
    // The implementation catalog the spawn dialog renders, named after the
    // agent's own harness. Shared rather than re-derived so the assignment dialog
    // and the spawn dialog cannot describe the same implementation differently.
    // catalog is another node's launch catalog (loadSandboxImpl); blank reads
    // the page's snapshot.
    sandboxImplOptions(harnessName, catalog) {
      const snapshot = catalog || getSnapshot() || {};
      const harness = findSpawnHarness(snapshot.harnesses, harnessName);
      const label = harness?.display_name || harness?.name || harnessName;
      // canBuiltinOSSandbox is deliberately NOT passed through here. It hides
      // harness-builtin in the spawn dialog for a harness that owns no OS
      // sandbox, which is right when CHOOSING a posture — but harness-builtin is
      // the recorded value such an agent already has, so hiding it would leave
      // the operator unable to put one back.
      return sandboxImplOptionsFor(snapshot.sandbox_impl?.options, label);
    },
    // Host/harness availability for ONE selected implementation, as the spawn
    // dialog and the profile editor disclose it. The picker deliberately offers
    // every implementation — the launch-time refusal is the authority, and a
    // dialog that hid an option would replace it — so this is what keeps the
    // operator from choosing one this box cannot run and learning it from a POST.
    sandboxImplAvailability(harnessName, implementation, nodeCatalog) {
      const snapshot = nodeCatalog || getSnapshot() || {};
      const catalog = snapshot.sandbox_impl || {};
      const harness = findSpawnHarness(snapshot.harnesses, harnessName);
      if (implementation === 'stacked') {
        if (harness && !harness.can_stacked) {
          return `${harness.display_name || harnessName} has no nested sandbox to stack under tclaude’s sandbox.`;
        }
        const stacked = catalog.stacked?.[harnessName];
        if (stacked && !stacked.available) return stacked.unavailable_reason || 'the stacked engine is unavailable on this host.';
        if (catalog.stacked_apparmor_nested_bwrap_likely) {
          return 'this host most likely denies the nested bubblewrap that stacked needs (AppArmor bwrap-userns-restrict).';
        }
        return '';
      }
      if (implementation === 'tclaude-layer') {
        const serverBoundary = !!harness?.tclaude_layer_server_boundary;
        const available = serverBoundary ? catalog.server_host_available : catalog.host_available;
        const reason = serverBoundary
          ? catalog.server_host_unavailable_reason
          : catalog.host_unavailable_reason;
        if (available === false) return reason || 'tclaude’s sandbox is unavailable on this host.';
      }
      return '';
    },
    // The harness's own sandbox modes, for the optional pin. Empty for a harness
    // whose sandbox is configured out of band, which is what hides the row.
    sandboxModes(harnessName, catalog) {
      const harness = findSpawnHarness((catalog || getSnapshot() || {}).harnesses, harnessName);
      if (!harness?.can_sandbox) return [];
      return Array.isArray(harness.sandbox_modes) ? harness.sandbox_modes : [];
    },
    // The durable sandbox posture the NEXT launch will use. Read from the server
    // rather than the row snapshot, whose sandbox fields describe the last launch
    // — the two diverge as soon as an assignment is recorded.
    // node names another node's agent (the fused view; blank is the page's).
    // Another node's harness and implementation catalogs come with the
    // posture, as catalog, since its snapshot (and a peer page's) carries
    // neither.
    async loadSandboxImpl(conv, { signal, node = '' } = {}) {
      const api = nodeFetch(node, { fetchImpl });
      const posture = requestJSON(api, `/api/agents/${encodeURIComponent(conv)}/sandbox-impl`, { signal });
      if (!node && !globalThis.__tclaudeRemoteNode?.id) return posture;
      const [data, catalog] = await Promise.all([posture,
        // An older node without the catalog route still shows its posture.
        requestJSON(api, '/api/spawn-launch-catalog', { signal }).catch((cause) => { if (cause?.name === 'AbortError') throw cause; return null; })]);
      return { ...data, catalog: { harnesses: catalog?.harnesses || [], sandbox_impl: catalog?.sandbox_impl || {} } };
    },
    async assignSandboxImpl({ conv, node = '', label, implementation, sandbox }, owner) {
      const payload = await requestJSON(
        nodeFetch(node, { fetchImpl }), `/api/agents/${encodeURIComponent(conv)}/sandbox-impl`, {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ implementation, sandbox: sandbox || '' }),
        },
      );
      state.close(owner);
      announceNodeChanged(node);
      const name = label || String(conv).slice(0, 8);
      // Name the wake explicitly. The assignment changed nothing about the
      // agent as it stands — it is recorded intent, and an operator who reads
      // "assigned" as "applied" would believe a stopped agent is now bounded.
      notify(`${name}: sandbox implementation → ${payload.sandbox_implementation} (applied when you wake it)`);
      await refresh();
      return payload;
    },
    async clearExports(conv) {
      await exportRequest(`/api/agents/${encodeURIComponent(conv)}/exports`, { method: 'DELETE' });
    },
    downloadExport,
    exportReady(label) { notify(`Export ready for ${label}`); },
    exportStillRunning() { notify('Export still running — follow it on the Automations tab'); },
    // Polling stays outside the component. The returned cleanup owns both the
    // timer and any in-flight request, so close/unmount cannot publish stale
    // status into a later dialog or trigger a foreign download.
    watchExport(jobID, { onStatus, onReady, onFailed, onSlow }) {
      let active = true;
      let timer = null;
      let controller = null;
      const startedAt = Date.now();
      let slowNotified = false;
      const schedule = () => {
        if (active) timer = setTimer(tick, EXPORT_POLL_INTERVAL_MS);
      };
      const tick = async () => {
        if (!active) return;
        controller = new AbortController();
        try {
          const job = await exportRequest(`/api/export-jobs/${encodeURIComponent(jobID)}`, {
            signal: controller.signal,
          });
          if (!active) return;
          if (job.status === 'ready') { onReady(job); return; }
          if (job.status === 'failed') { onFailed(job); return; }
          onStatus(job.status);
          if (!slowNotified && Date.now() - startedAt > EXPORT_SLOW_AFTER_MS) {
            slowNotified = true;
            onSlow();
          }
        } catch (error) {
          if (!active || error?.name === 'AbortError') return;
          if (error?.status === 404) {
            onFailed({ error: 'the export job is no longer available' });
            return;
          }
          // Other network/server failures are transient; retain legacy retry.
        } finally {
          controller = null;
        }
        schedule();
      };
      schedule();
      return () => {
        active = false;
        if (timer !== null) clearTimer(timer);
        controller?.abort();
      };
    },
  });
}
