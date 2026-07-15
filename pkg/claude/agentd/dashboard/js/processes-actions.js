import { buildWorklistAction, isDestructiveAction, mintUUID, retainedActionKey } from './process-worklist-core.js';
import { templateHeadSignature } from './process-external-change.js';
import { dashboardState } from './snapshot-store.js';
import {
  PROCESS_SCRIBE_NAME, PROCESS_SCRIBE_SLUGS, processScribeBrief, processScribeHandoff,
  processScribeScopeLabel, processScribeSessions, processScribeTaskRef,
} from './process-scribe.js';

export function processActorPresentation(snapshot, actor) {
  const ref = String(actor || '');
  if (ref === 'human:operator') return { label: 'the operator', live: false, agentId: '' };
  if (!/^agent:agt_[A-Za-z0-9]+$/.test(ref)) return null;
  const agentId = ref.slice('agent:'.length);
  const stableAgentId = /^agt_[0-9a-f]{32}$/.test(agentId) ? agentId : '';
  const candidates = [
    ...(snapshot?.agents || []),
    ...(snapshot?.groups || []).flatMap((group) => group.members || []),
  ];
  const row = stableAgentId ? candidates.find((candidate) => candidate.agent_id === stableAgentId) : null;
  const short = `${agentId.slice(0, 12)}…`;
  return {
    label: row?.title ? `agent ${row.title}` : `agent ${short}`,
    live: !!row?.online,
    agentId: stableAgentId,
  };
}

export async function processJSON(path, fetchImpl = fetch) {
  const response = await fetchImpl(path, { credentials: 'same-origin' });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.message || body.error || `${response.status} ${response.statusText}`);
  return body;
}

export function createProcessesActions({
  state,
  fetchImpl = fetch,
  confirm = async () => true,
  confirmDiscard = async () => true,
  notify = () => {},
  dashboardOrigin = globalThis.location?.origin || '',
  dispatchNavigated = () => document.dispatchEvent(new CustomEvent('tclaude:navigated')),
  mintAttemptID = mintUUID,
} = {}) {
  const actionKeys = new Map();
  let listedHeadsSignature = null;
  let headObservationPending = false;

  const requestBusy = (lifecycle) => ['loading', 'refreshing'].includes(lifecycle.request.value.phase);
  const editorGeneration = () => {
    const editor = state.currentEditor();
    return {
      editor, model: editor?.model, ref: editor?.model?.currentRef || '', sourceHash: editor?.model?.sourceHash || '',
    };
  };
  const publishMatchingHead = (generation, heads) => {
    if (!generation.editor || !generation.model || state.currentEditor() !== generation.editor
        || generation.editor.model !== generation.model
        || generation.model.currentRef !== generation.ref
        || generation.model.sourceHash !== generation.sourceHash) return false;
    const id = generation.model?.template?.id;
    const head = (heads || []).find((candidate) => candidate.id === id);
    if (!head?.ref || !head?.sourceHash) return false;
    generation.editor.observeExternalHead?.(head);
    return true;
  };

  async function load(name, { quiet = false } = {}) {
    const lifecycle = ({ templates: state.templatesRequest, runs: state.runsRequest, worklist: state.worklistRequest })[name];
    const path = ({ templates: '/v1/process/templates', runs: '/v1/process/runs', worklist: '/v1/process/worklist' })[name];
    if (!lifecycle || !path) return false;
    if (name === 'templates' && (requestBusy(lifecycle) || headObservationPending)) return false;
    const generation = name === 'templates' ? editorGeneration() : null;
    const token = lifecycle.beginRequest();
    try {
      const body = await processJSON(path, fetchImpl);
      if (!lifecycle.commitRequest(token, body)) return false;
      if (name === 'worklist') {
        const items = body.items || [];
        state.pruneWorklistState(items);
        const live = new Set(items.map((item) => item.id));
        for (const payload of actionKeys.keys()) {
          if (!live.has(payload.slice(0, payload.indexOf('\x00')))) actionKeys.delete(payload);
        }
        if (!quiet) state.setNotice(`${state.view.value.actionable} actionable item${state.view.value.actionable === 1 ? '' : 's'}`);
        if (!items.length && state.runs.value === null) void load('runs', { quiet: true });
      } else if (name === 'templates') {
        const rows = body.templates || [];
        const heads = rows.map((template) => ({
          id: template.id, ref: template.latestVersion?.ref || '', sourceHash: template.latestVersion?.sourceHash || '',
          ...(template.latestVersion?.actor ? { actor: template.latestVersion.actor } : {}),
          ...(template.latestVersion?.authoredAt ? { authoredAt: template.latestVersion.authoredAt } : {}),
        }));
        listedHeadsSignature = templateHeadSignature(heads);
        publishMatchingHead(generation, heads);
        if (!quiet) {
          state.setNotice(`${rows.length} template${rows.length === 1 ? '' : 's'}`);
        }
      } else if (!quiet) {
        const rows = body[name] || [];
        state.setNotice(`${rows.length} run${rows.length === 1 ? '' : 's'}`);
      }
      return true;
    } catch (error) {
      if (!lifecycle.failRequest(token, error)) return false;
      if (!quiet) state.setNotice(`${name} failed: ${error.message}`);
      return false;
    }
  }

  async function observeTemplateHeads() {
    if (headObservationPending || requestBusy(state.templatesRequest)) return false;
    const generation = editorGeneration();
    headObservationPending = true;
    let shouldRefresh = false;
    try {
      const body = await processJSON('/v1/process/template-heads', fetchImpl);
      const heads = body.heads || [];
      publishMatchingHead(generation, heads);
      shouldRefresh = templateHeadSignature(heads) !== listedHeadsSignature;
    } catch {
      return false;
    } finally {
      headObservationPending = false;
    }
    return shouldRefresh ? load('templates', { quiet: true }) : true;
  }

  async function canLeaveEditor() {
    const editor = state.currentEditor();
    const dirty = editor?.dirty ?? editor?.model?.dirty;
    return !dirty || confirmDiscard();
  }
  async function activateSubtab(name, { navigate = true } = {}) {
    if (!(await canLeaveEditor())) return false;
    state.setEditor(null); state.setCanvas(null); state.setSubtab(name);
    await load(name);
    if (navigate) dispatchNavigated();
    return true;
  }
  async function openEditor(id, blank = false) {
    state.setCanvas({ kind: 'editor', id, blank, key: `${id}:${blank}:${Date.now()}` });
    state.setNotice(blank ? 'Blank template scaffold ready.' : `Opening ${id}.`);
  }
  async function summonScribe(anchor = { kind: 'library' }, handoffOptions = {}) {
    try {
      const { freshnessGuard, ...briefOptions } = handoffOptions;
      const handoff = processScribeHandoff(anchor);
      const task = processScribeTaskRef(handoff, dashboardOrigin);
      const scribes = processScribeSessions(dashboardState.snapshot.value);
      const exactLive = scribes.find((scribe) => scribe.online
        && scribe.scope.id === handoff.scope.id);
      const incompatible = scribes.filter((scribe) => scribe.online
        && scribe.scope.id !== handoff.scope.id);
      const target = processScribeScopeLabel(handoff.scope);
      const transition = incompatible.length
        ? ` ${incompatible.length === 1 ? `The live scribe scoped to ${incompatible[0].scopeLabel}` : `${incompatible.length} live scribes with other scopes`} will not be reused or have its permissions changed; this starts a separate, explicitly scoped conversation.`
        : '';
      const approved = await confirm({
        title: exactLive ? 'Reuse or replace process scribe?' : 'Grant process-scribe access?',
        body: `Scope: ${target}. The daemon reuses a live same-scope scribe only when its exact permissions match and it has no active temporary elevation; otherwise this approval creates a fresh scribe receiving only process.templates.read and process.templates.manage while leaving the existing scribe unchanged. Every other registered capability is explicitly denied. Manage allows validated CAS saves, but summoning never instantiates or runs a process.${transition}`,
        meta: 'process.templates.read + process.templates.manage',
        okLabel: exactLive ? 'Reuse or summon' : incompatible.length ? 'Start separate scribe' : 'Grant & summon',
      });
      if (!approved) {
        state.setNotice('Process scribe cancelled; no permissions or sessions changed.');
        return null;
      }
      let fresh = true;
      try {
        if (typeof freshnessGuard === 'function') fresh = freshnessGuard() === true;
      } catch {
        fresh = false;
      }
      if (!fresh) {
        state.setNotice('Process scribe cancelled because the editor context changed during approval. Review the current context and try again.');
        return null;
      }
      const response = await fetchImpl('/api/scribe', {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: PROCESS_SCRIBE_NAME, slugs: PROCESS_SCRIBE_SLUGS,
          exclusive: true, scope: handoff.scope, brief: processScribeBrief(handoff, briefOptions),
          task_ref_url: task.url, task_ref_label: task.label,
        }),
      });
      const result = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(result.message || result.error || `${response.status} ${response.statusText}`);
      const name = result.name || PROCESS_SCRIBE_NAME;
      if (result.focus_mode === 'browser' && result.focus_ws) {
        const { openTermModal } = await import('./modal-term.js');
        openTermModal({ wsPath: result.focus_ws, label: name, hideConv: result.conv_id || null });
      }
      state.setNotice(`${result.reused ? 'Reopened' : 'Summoned'} process scribe ${name}.`);
      notify(`${result.reused ? 'reopened' : 'summoned'} process scribe ${name}`);
      return result;
    } catch (error) {
      const message = error?.message || String(error);
      state.setNotice(`Process scribe unavailable: ${message}`);
      notify(`Could not open a process scribe: ${message}. Check the agent daemon and Scribe defaults, then retry.`, true);
      return null;
    }
  }
  function describeActor(actor) {
    return processActorPresentation(dashboardState.snapshot.value, actor);
  }
  async function openAgent(agentId, label) {
    if (!agentId) return false;
    try {
      const response = await fetchImpl(`/api/open-window/${encodeURIComponent(agentId)}`, {
        method: 'POST', credentials: 'same-origin',
      });
      const result = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(result.message || result.error || `${response.status} ${response.statusText}`);
      if (result.mode === 'browser' && result.ws) {
        const { openTermModal } = await import('./modal-term.js');
        openTermModal({ wsPath: result.ws, label, hideConv: agentId });
      }
      state.setNotice(`Opened ${label}.`);
      return true;
    } catch (error) {
      state.setNotice(`Could not open ${label}: ${error.message}. Refresh Processes and summon a new scribe if this session is stale.`);
      return false;
    }
  }
  async function openActor(actor) {
    const presentation = describeActor(actor);
    if (!presentation?.live || !presentation.agentId) return false;
    return openAgent(presentation.agentId, presentation.label);
  }
  async function openScribe(scribe) {
    if (!scribe?.online) {
      state.setNotice(`${scribe?.name || 'Process scribe'} is stopped. Retire it or summon a fresh scribe for this scope.`);
      return false;
    }
    return openAgent(scribe.agentId, scribe.name);
  }
  async function stopScribe(scribe) {
    if (!scribe?.online) {
      state.setNotice(`${scribe?.name || 'Process scribe'} is already stopped. Its conversation and permissions remain; retire it or summon a fresh scribe.`);
      return false;
    }
    const approved = await confirm({
      title: 'Stop process scribe?',
      body: `This soft-stops ${scribe.name}. It does not delete process templates, versions, the conversation, task reference, permissions, or local editor work. Summon again to continue in a fresh live session.`,
      meta: scribe.scopeLabel, okLabel: 'Stop scribe',
    });
    if (!approved) return false;
    try {
      const response = await fetchImpl(`/api/agents/${encodeURIComponent(scribe.agentId)}/stop`, {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ force: false }),
      });
      const result = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(result.message || result.error || `${response.status} ${response.statusText}`);
      if (result.action === 'skipped:already_offline') {
        state.setNotice(`${scribe.name} was already stopped; templates, permissions, and editor work are unchanged. Retire it or summon a fresh scribe.`);
        return true;
      }
      if (result.action === 'soft_stopped') {
        state.setNotice(`Asked ${scribe.name} to stop; refresh Processes to confirm it is offline. If it remains active, force-stop it from Agents. Templates, permissions, and editor work are unchanged.`);
        notify(`requested stop for process scribe ${scribe.name}`);
        return true;
      }
      if (result.action !== 'killed_no_soft_exit') {
        throw new Error(result.detail || `the daemon returned ${result.action || 'no lifecycle result'}; the session may still be running`);
      }
      state.setNotice(`Stopped ${scribe.name}; templates and editor work are unchanged. Summon a new scribe to continue.`);
      notify(`stopped process scribe ${scribe.name}`);
      return true;
    } catch (error) {
      state.setNotice(`Could not stop ${scribe.name}: ${error.message}. Refresh Processes; it may already be stopped or retired.`);
      return false;
    }
  }
  async function retireScribe(scribe) {
    if (!scribe?.agentId) return false;
    const approved = await confirm({
      title: 'Retire process scribe?',
      body: `This stops ${scribe.name}, revokes its permissions, and removes its agent-group memberships. The conversation remains reinstatable; process templates, versions, and local editor work are not deleted.`,
      meta: scribe.scopeLabel, okLabel: 'Retire scribe',
    });
    if (!approved) return false;
    try {
      const query = new URLSearchParams({ shutdown: '1', delete_worktree: '0', reason: `process scribe retired from ${scribe.scopeLabel}` });
      const response = await fetchImpl(`/api/agents/${encodeURIComponent(scribe.agentId)}/retire?${query}`, {
        method: 'POST', credentials: 'same-origin',
      });
      const result = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(result.message || result.error || `${response.status} ${response.statusText}`);
      if (result.outcome?.retired !== true) {
        throw new Error('the daemon did not confirm retirement; refresh Processes before retrying');
      }
      const shutdownAction = result.shutdown?.action;
      if (!['soft_stopped', 'skipped:already_offline', 'killed_no_soft_exit'].includes(shutdownAction)) {
        const detail = result.shutdown?.detail || `the daemon returned ${shutdownAction || 'no shutdown result'}`;
        state.setNotice(`Retired ${scribe.name} and revoked its access, but its session may still be running: ${detail}. Stop the conversation from Agents; process templates and editor work are unchanged.`);
        notify(`retired process scribe ${scribe.name}, but its session may still be running`, true);
        return true;
      }
      if (shutdownAction === 'soft_stopped') {
        state.setNotice(`Retired ${scribe.name} and revoked its access; its stop was requested but is not yet confirmed. Refresh Processes to confirm it is offline, then force-stop the conversation from Agents if needed. Process templates remain unchanged.`);
        notify(`retired process scribe ${scribe.name}; stop requested`);
        return true;
      }
      const stopped = shutdownAction === 'skipped:already_offline' ? 'its session was already stopped' : 'its session was stopped';
      state.setNotice(`Retired ${scribe.name}; access was revoked, ${stopped}, and process templates remain unchanged. Summon a new scribe when needed.`);
      notify(`retired process scribe ${scribe.name}`);
      return true;
    } catch (error) {
      state.setNotice(`Could not retire ${scribe.name}: ${error.message}. Refresh Processes; if it was already retired, summon a new scribe.`);
      return false;
    }
  }
  async function openInstantiation({ id, ref, template = null } = {}) {
    if (!id || !ref) return false;
    const key = `${ref}:${Date.now()}`;
    const runId = `${id}-${mintAttemptID()}`;
    state.setInstantiation({ key, id, ref, runId, template, phase: template ? 'ready' : 'loading', error: '' });
    if (template) return true;
    try {
      const body = await processJSON(`/v1/process/templates/${encodeURIComponent(id)}?version=${encodeURIComponent(ref)}`, fetchImpl);
      if (state.instantiation.value?.key !== key) return false;
      if (body.currentRef !== ref) throw new Error('the requested exact template version was not returned');
      state.setInstantiation({ key, id, ref, runId, template: body.template, phase: 'ready', error: '' });
      return true;
    } catch (error) {
      if (state.instantiation.value?.key === key) state.setInstantiation({ key, id, ref, runId, template: null, phase: 'error', error: error.message });
      return false;
    }
  }
  function closeInstantiation() {
    if (state.mutation.value.busy) return false;
    state.setInstantiation(null);
    return true;
  }
  async function submitInstantiation(params) {
    const spec = state.instantiation.value;
    if (!spec?.ref || !spec.runId || spec.phase !== 'ready' || !state.beginMutation()) return false;
    try {
      const response = await fetchImpl('/v1/process/runs', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin',
        body: JSON.stringify({ templateRef: spec.ref, runId: spec.runId, params }),
      });
      const body = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(body.message || body.error || `${response.status} ${response.statusText}`);
      if (!body.run?.id || body.run.templateRef !== spec.ref) throw new Error('run creation returned an invalid response');
      state.setInstantiation(null);
      state.setSubtab('runs');
      openViewer(body.run.id);
      state.setNotice(`Created run ${body.run.id}.`);
      notify(`Created process run ${body.run.id}`);
      void load('runs', { quiet: true });
      dispatchNavigated();
      return true;
    } catch (error) {
      state.setNotice(`Run creation failed: ${error.message}`);
      notify(`process run creation failed: ${error.message}`, true);
      return false;
    } finally {
      state.endMutation();
    }
  }
  function openViewer(id) { state.setCanvas({ kind: 'viewer', id, key: id }); state.setNotice(`Opening run ${id}.`); }
  async function closeCanvas() {
    if (!(await canLeaveEditor())) return false;
    state.setEditor(null); state.setCanvas(null); await load(state.subtab.value); return true;
  }
  async function openRunInList(id) {
    if (!(await activateSubtab('runs'))) return;
    state.setHighlightedRun(id);
  }

  async function submitWorklistAction(itemID, action) {
    const item = state.worklist.value?.items?.find((candidate) => candidate.id === itemID);
    const comment = (state.drafts.value[itemID] || '').trim();
    if (!item) return false;
    if (!comment) { state.requireComment(itemID); state.setNotice('A comment is required for every worklist action.'); return false; }
    if (!state.beginMutation()) return false;
    try {
      if (isDestructiveAction(action)) {
        const ok = await confirm({
          title: `${action} — are you sure?`,
          body: `“${action}” on ${item.node} (run ${item.run}) is recorded durably in the run's audit log and drives the run forward.`,
          meta: item.summary || '', okLabel: action,
        });
        if (!ok) return false;
      }
      const { payload, key } = retainedActionKey(actionKeys, item, action, comment);
      const request = buildWorklistAction(item, action, comment, key);
      if (!request) { state.setNotice(`“${action}” is no longer available for this item.`); return false; }
      const response = await fetchImpl(request.path, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin',
        body: JSON.stringify(request.body),
      });
      const body = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(body.message || body.error || `${response.status} ${response.statusText}`);
      actionKeys.delete(payload); state.setDraft(itemID, ''); notify(`${request.body.action} recorded for ${item.node}`);
      return true;
    } catch (error) {
      notify(`worklist action failed: ${error.message}`, true); return false;
    } finally {
      state.endMutation();
      void load('worklist');
    }
  }

  function refreshActive() { return load(state.subtab.value); }
  return Object.freeze({
    load, observeTemplateHeads, activateSubtab, openEditor, summonScribe, describeActor, openActor,
    openScribe, stopScribe, retireScribe, openInstantiation, closeInstantiation,
    submitInstantiation, openViewer, closeCanvas, openRunInList, submitWorklistAction, refreshActive,
  });
}
