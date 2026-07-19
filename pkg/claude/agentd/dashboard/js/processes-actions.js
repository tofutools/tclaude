import { buildWorklistAction, isDestructiveAction, mintUUID, retainedActionKey } from './process-worklist-core.js';
import { templateHeadSignature } from './process-external-change.js';
import { idempotentRequestHeaders } from './request-idempotency.js';
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
  // Announces a location change to the history router (nav-history.js). The
  // location is passed EXPLICITLY rather than left to the router's DOM read:
  // this island is signal-driven, so the event fires before Preact commits the
  // new active class, and an open editor's template id lives in state that the
  // DOM does not spell out at all.
  // `correction: true` marks an announcement that is not a user navigation but
  // this tab telling the router its URL is wrong, which the router must apply
  // by REPLACING the current entry rather than pushing a new one.
  dispatchNavigated = (location, { correction = false } = {}) => document.dispatchEvent(
    new CustomEvent('tclaude:navigated', location ? { detail: { location, correction } } : undefined),
  ),
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
  // announceLocation tells the router where this tab ended up, reading the
  // authoritative location off the state (see `location` in processes-state.js).
  // Used after any change the URL should follow.
  function announceLocation() { dispatchNavigated(state.location.value); }
  // correctLocation reports the same thing, but as a fix to a URL the router
  // already committed — so it replaces the current history entry instead of
  // pushing a new one. See applyLocation for the two cases that need it.
  function correctLocation() { dispatchNavigated(state.location.value, { correction: true }); }

  async function activateSubtab(name, { navigate = true } = {}) {
    if (!(await canLeaveEditor())) return false;
    state.setEditor(null); state.setCanvas(null); state.setSubtab(name);
    await load(name);
    if (navigate) announceLocation();
    return true;
  }
  async function openEditor(id, blank = false, name = '', { navigate = true, view = null } = {}) {
    state.setCanvas({ kind: 'editor', id, blank, name, view, key: `${id}:${blank}:${Date.now()}` });
    state.setNotice(blank ? `Blank template “${name}” ready.` : `Opening ${name || id}.`);
    // `navigate: false` is the router restoring this location FROM the URL —
    // pushing it back would forge a duplicate history entry.
    if (navigate) announceLocation();
  }
  // Creation collects a NAME, never an id. Submitting the dialog persists the
  // named scaffold through the collection endpoint, which mints the permanent
  // id; only then do we open the normal stored-template editor. That keeps the
  // editor's identity, name, and CAS base in one backend-confirmed generation.
  function openCreate() {
    state.setCreate({ key: `create:${Date.now()}`, name: '' });
    return true;
  }
  function closeCreate() {
    if (state.mutation.value.busy) return false;
    state.setCreate(null);
    return true;
  }
  async function submitCreate(name) {
    const next = String(name ?? '').trim();
    if (!next) return false;
    const spec = state.create.value;
    if (!spec) return false;
    if (spec.attempt?.name === next && spec.attempt.blocked) {
      const message = 'The earlier create may already have succeeded. Refresh Templates to reconcile it before starting another creation.';
      state.setCreate({ ...spec, name: next, error: message });
      state.setNotice(`Template creation paused: ${message}`);
      return false;
    }
    if (!state.beginMutation()) return false;
    const attempt = spec.attempt?.name === next
      ? spec.attempt : { name: next, key: mintAttemptID() };
    state.setCreate({ ...spec, name: next, error: '', attempt });
    try {
      const { blankEditView } = await import('./process-edit-model.js');
      const scaffold = blankEditView(next);
      const template = { ...scaffold.template };
      delete template.id;
      const path = '/v1/process/templates';
      const requestBody = JSON.stringify({ template, edges: scaffold.edges, layout: scaffold.layout });
      const response = await fetchImpl(path, {
        method: 'POST', headers: idempotentRequestHeaders('POST', path, requestBody, attempt.key),
        credentials: 'same-origin', body: requestBody,
      });
      const body = await response.json().catch(() => null);
      if (!response.ok) {
        const outcomeUnknown = body?.code === 'idempotency_unknown';
        const detail = body?.message || body?.error || `${response.status} ${response.statusText}`;
        const error = new Error(outcomeUnknown
          ? `${detail} Refresh Templates to reconcile the earlier create before starting another creation.`
          : detail);
        error.definitiveResponse = !outcomeUnknown;
        error.outcomeUnknown = outcomeUnknown;
        throw error;
      }
      if (!body) throw new Error('template creation returned an unreadable response');
      const id = String(body.id || '').trim();
      if (!id) throw new Error('template creation returned no id');
      const currentRef = String(body.ref || '').trim();
      const sourceHash = String(body.sourceHash || '').trim();
      const semanticHash = String(body.semanticHash || '').trim();
      if (!currentRef || !sourceHash || !semanticHash) throw new Error('template creation returned incomplete version metadata');
      const createdView = {
        ...scaffold,
        template: { ...template, id },
        currentRef, sourceHash, semanticHash,
        diagnostics: body.diagnostics || [],
        ...(body.actor ? { actor: body.actor } : {}),
        ...(body.authoredAt ? { authoredAt: body.authoredAt } : {}),
      };
      if (state.create.value?.key === spec.key) state.setCreate(null);
      await openEditor(id, false, next, { view: createdView });
      void load('templates', { quiet: true });
      return true;
    } catch (error) {
      if (state.create.value?.key === spec.key) {
        state.setCreate({
          ...spec, name: next, error: error.message,
          attempt: error.definitiveResponse ? null : { ...attempt, blocked: !!error.outcomeUnknown },
        });
      }
      state.setNotice(`Template creation failed: ${error.message}`);
      notify(`process template creation failed: ${error.message}`, true);
      return false;
    } finally {
      state.endMutation();
    }
  }
  // applyLocation drives the tab from a location the ROUTER resolved (a deep
  // link, a reload, or a browser Back/Forward). It reuses the ordinary open
  // paths — so the unsaved-changes guard still runs — but suppresses the
  // outgoing navigation event, because the URL is already where it wants to be.
  //
  // Returns false when it did NOT end up where it was asked to go, which
  // happens two ways:
  //   - the operator refused to discard an unsaved editor, so we stayed put;
  //   - the URL named something this tab cannot show — today a run selection,
  //     /processes/runs/<id>, which is a modelled but unwired detail view.
  // Either way the caller corrects the URL, so a bookmarked or hand-typed path
  // can never leave the address bar permanently describing a view that is not
  // on screen.
  //
  // A template id that no longer exists is deliberately NOT treated this way:
  // the editor itself reports it, and evicting the id on a transient
  // template-list failure would break a perfectly good deep link on reload.
  async function applyLocation({ subtab, selection } = {}) {
    const name = ['templates', 'runs', 'worklist'].includes(subtab) ? subtab : 'templates';
    const requested = String(selection || '');
    const target = name === 'templates' ? requested : '';
    // Already showing exactly this? Nothing to do — and nothing to prompt about.
    const showing = state.location.value;
    if (showing.subtab === name && (showing.selection || '') === target) return target === requested;
    if (!(await activateSubtab(name, { navigate: false }))) return false;
    if (target) await openEditor(target, false, '', { navigate: false });
    return target === requested;
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
        const { openTermModal } = await import('./terminals-tab.js');
        openTermModal({ wsPath: result.focus_ws, label: name, hideConv: result.conv_id || null });
      }
      state.setNotice(`${result.reused ? 'Reopened' : 'Summoned'} process scribe ${name}.`);
      notify(`${result.reused ? 'reopened' : 'summoned'} process scribe ${name}`);
      return result;
    } catch (error) {
      const message = error?.message || String(error);
      state.setNotice(`Process scribe unavailable: ${message}`);
      notify(`Could not open a process scribe: ${message}. Check the agent daemon and Ask & scribe defaults, then retry.`, true);
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
        const { openTermModal } = await import('./terminals-tab.js');
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
  // Renaming edits only the display name. The immutable id stays the store key,
  // so every pinned ref and running instance is unaffected -- but the name is
  // part of the semantic hash, so this still commits a normal CAS version.
  // sourceHash is captured when the dialog opens, not when it is submitted, so
  // the CAS check covers the whole time the operator sat in the dialog. Saving
  // against the head read at submit time would silently clobber a concurrent
  // edit by an agent or another tab.
  async function openRename({ id, name = '', sourceHash = '' } = {}) {
    if (!id) return false;
    const key = `${id}:${Date.now()}`;
    state.setRename({ key, id, name: String(name || ''), sourceHash: String(sourceHash || ''), error: '' });
    return true;
  }
  function closeRename() {
    if (state.mutation.value.busy) return false;
    state.setRename(null);
    return true;
  }
  // renameTemplate is the shared commit. It takes its target explicitly rather
  // than reading dialog state, so the inline list editor can rename WITHOUT
  // opening (and instantly closing) the dialog.
  async function renameTemplate({ id, name = '', sourceHash = '' } = {}, value) {
    if (!id) return false;
    const next = String(value ?? '').trim();
    if (next === String(name || '').trim()) return true;
    if (!state.beginMutation()) { state.setNotice('Another process action is still running; retry the rename once it settles.'); return false; }
    const path = `/v1/process/templates/${encodeURIComponent(id)}`;
    try {
      // Round-trip the head's full edit view so layout and edges survive the
      // rename untouched; only the display name differs from what we read. The
      // save is expressed against the hash observed when the edit STARTED, so a
      // head that moved in the meantime is rejected rather than overwritten.
      const head = await processJSON(path, fetchImpl);
      if (!head.template) throw new Error('template head returned no editable model');
      const response = await fetchImpl(path, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin',
        // Only these four keys are decodable: the save handler rejects unknown
        // fields, so read-only view fields must not be forwarded.
        body: JSON.stringify({
          template: { ...head.template, name: next },
          edges: head.edges, layout: head.layout, sourceHash: sourceHash || head.sourceHash,
        }),
      });
      const body = await response.json().catch(() => ({}));
      if (response.status === 409 || body.code === 'process_template_conflict') {
        throw new Error('this template changed while you were renaming it; reload and try again');
      }
      if (body.code === 'process_template_invalid') {
        throw new Error('this template no longer passes validation, so it cannot be saved under a new name until the graph is fixed in the editor');
      }
      if (!response.ok) throw new Error(body.message || body.error || `${response.status} ${response.statusText}`);
      state.setNotice(next ? `Renamed ${id} to ${next}.` : `Cleared the display name for ${id}.`);
      void load('templates', { quiet: true });
      return true;
    } catch (error) {
      state.setNotice(`Rename failed: ${error.message}`);
      notify(`process template rename failed: ${error.message}`, true);
      throw error;
    } finally {
      state.endMutation();
    }
  }
  // submitRename is the DIALOG wrapper: it owns dialog lifecycle (close on
  // success, keep open and show the error on failure) around the shared commit.
  async function submitRename(name) {
    const spec = state.rename.value;
    if (!spec?.id) return false;
    try {
      const ok = await renameTemplate(spec, name);
      if (!ok) return false;
      if (state.rename.value?.key === spec.key) state.setRename(null);
      return true;
    } catch (error) {
      if (state.rename.value?.key === spec.key) state.setRename({ ...spec, error: error.message });
      return false;
    }
  }
  // deleteTemplate is the shared commit for BOTH delete affordances (the row
  // button and the drag-to-bin drop), so the confirm copy, the in-use handling,
  // and the list refresh cannot drift between them.
  //
  // Deleting drops the whole version history for the id. The daemon refuses
  // outright while any run that still needs the stored template references it.
  //
  // The copy deliberately does NOT promise that finished runs stay fully
  // readable. A finished run keeps the snapshot pinned into its own record, but
  // the execution-view and verification surfaces resolve the template body from
  // the library and report the run as inconsistent once it is gone.
  async function deleteTemplate({ id, name = '', versionCount = 0 } = {}) {
    if (!id) return false;
    const label = String(name || '').trim() || id;
    const versions = Number(versionCount) || 0;
    const wizard = document.body?.classList?.contains('wizard');
    const approved = await confirm({
      title: wizard ? 'Unmake this rite?' : 'Delete this process template?',
      body: wizard
        ? `This unmakes ${label} and every one of its ${versions || 'stored'} inscribed version${versions === 1 ? '' : 's'}, along with its authorship trail. Quests already ended keep their own bound copy, but their scrying and attestation views will read as broken once the rite is gone. A rite still underway cannot be unmade.`
        : `This permanently deletes ${label} and all ${versions || 'stored'} version${versions === 1 ? '' : 's'} of it, including its authorship history. Runs that already finished keep their own pinned copy, but their execution and verification views will report as inconsistent once the template is gone. This cannot be undone.`,
      meta: id,
      okLabel: wizard ? 'Unmake rite' : 'Delete template',
    });
    if (!approved) return false;
    if (!state.beginMutation()) {
      state.setNotice('Another process action is still running; retry the delete once it settles.');
      return false;
    }
    try {
      const response = await fetchImpl(`/v1/process/templates/${encodeURIComponent(id)}`, {
        method: 'DELETE', credentials: 'same-origin',
      });
      const body = await response.json().catch(() => ({}));
      if (body.code === 'process_template_in_use') {
        const runs = Array.isArray(body.runIds) ? body.runIds : [];
        const unreadable = Array.isArray(body.unreadableRunIds) ? body.unreadableRunIds : [];
        // Bound the list: a store with many blocked runs must not push a wall of
        // ids into the notice line.
        const nameRuns = (ids) => {
          const shown = ids.slice(0, 3).join(', ');
          return `${shown}${ids.length > 3 ? ` and ${ids.length - 3} more` : ''}`;
        };
        // Unreadable runs need repair, not completion, so they get their own
        // sentence rather than being folded into the "finish or cancel" advice.
        if (runs.length) {
          throw new Error(
            `${runs.length} run${runs.length === 1 ? '' : 's'} still need${runs.length === 1 ? 's' : ''} it (${nameRuns(runs)}). `
            + 'Finish or cancel them first.'
            + (unreadable.length ? ` ${unreadable.length} run${unreadable.length === 1 ? '' : 's'} could not be read (${nameRuns(unreadable)}) and must be repaired or removed.` : ''),
          );
        }
        throw new Error(
          `${unreadable.length} run${unreadable.length === 1 ? '' : 's'} could not be read (${nameRuns(unreadable)}), `
          + 'so it is not safe to say whether this template is still in use. Repair or remove them first.',
        );
      }
      if (response.status === 404) throw new Error('this template no longer exists; refresh Processes');
      if (!response.ok) throw new Error(body.message || body.error || `${response.status} ${response.statusText}`);
      // An editor still open on the deleted id would keep accepting edits and
      // then fail confusingly on save, so close it. Deliberately unguarded by
      // canLeaveEditor: the operator just confirmed the destruction, and the
      // template those edits target no longer exists to save them against.
      if (state.currentEditor()?.model?.template?.id === id) {
        state.setEditor(null);
        state.setCanvas(null);
        // The editor owns a URL of its own (/processes/templates/<id>), which
        // now names a template that does not exist. CORRECT rather than
        // announce: pushing would leave a Back entry pointing at the deleted
        // editor, which can only fail to restore.
        correctLocation();
      }
      state.setNotice(`Deleted ${label}.`);
      notify(`deleted process template ${label}`);
      void load('templates', { quiet: true });
      return true;
    } catch (error) {
      state.setNotice(`Delete failed: ${error.message}`);
      notify(`process template delete failed: ${error.message}`, true);
      return false;
    } finally {
      state.endMutation();
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
      announceLocation();
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
  function loadRunView(id, offset = 0, limit = 25) {
    const query = new URLSearchParams({ detailOffset: String(offset), detailLimit: String(limit) });
    return processJSON(`/v1/process/runs/${encodeURIComponent(id)}/view?${query}`, fetchImpl);
  }
  async function closeCanvas() {
    if (!(await canLeaveEditor())) return false;
    state.setEditor(null); state.setCanvas(null); await load(state.subtab.value);
    // Back to the list: drop the /<template-id> segment from the URL so Back
    // returns to the editor rather than re-entering an already-closed view.
    announceLocation();
    return true;
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
    load, observeTemplateHeads, activateSubtab, openEditor, applyLocation, announceLocation, correctLocation,
    summonScribe, describeActor, openActor,
    openScribe, stopScribe, retireScribe, openInstantiation, closeInstantiation,
    openRename, closeRename, submitRename, renameTemplate, deleteTemplate,
    openCreate, closeCreate, submitCreate,
    submitInstantiation, openViewer, loadRunView, closeCanvas, openRunInList, submitWorklistAction, refreshActive,
  });
}
