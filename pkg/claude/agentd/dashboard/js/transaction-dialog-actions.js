async function responsePayload(response) {
  const text = await response.text();
  if (!text) return {};
  try { return JSON.parse(text); } catch (_) { return { error: text }; }
}

function responseError(response, payload) {
  const message = payload?.error || payload?.message || `HTTP ${response.status}`;
  const error = new Error(String(message));
  error.status = response.status;
  error.payload = payload;
  return error;
}

function retireNotice(label, choice, response) {
  let message = choice.shutdown
    ? `retired + session stopped: ${label}`
    : `retired: ${label}`;
  const detail = choice.deleteWorktree && response?.worktree?.detail;
  if (detail) message += ` · ${detail}`;
  return message;
}

function lifecycleError(response, payload) {
  const message = payload?.detail || payload?.error || payload?.message
    || `shutdown failed: ${payload?.action || `HTTP ${response.status}`}`;
  const error = new Error(String(message));
  error.status = response.status;
  error.payload = payload;
  return error;
}

export function createTransactionDialogActions({
  state,
  fetchImpl = fetch,
  refresh = async () => {},
  notify = () => {},
  words = (plain) => plain,
  confirm = async () => false,
  openWebWindowPane = () => {},
  closeTerminalsForWindowOp = () => {},
  openWorktreeCleanup = async () => {},
}) {
  // A request object is the immutable identity of one visible delete-group
  // plan. Phase progress lives outside Preact render state so retries cannot
  // accidentally replay a completed retire/detach phase after a final DELETE
  // failure. Weak keys release the bookkeeping with the finished dialog.
  const deleteGroupProgress = new WeakMap();

  const report = (message, isError = false) => {
    // Feedback is advisory. A broken injected sink must never strand the
    // promise-backed transaction after its visual owner has handed off.
    try {
      const pending = isError ? notify(message, true) : notify(message);
      pending?.catch?.(() => {});
    } catch (_) {}
  };

  const finishSuccess = async (result, message) => {
    // The mutation is authoritative once the daemon accepts it. Unpaint the
    // dialog before snapshot reconciliation, then resolve the compatibility
    // promise even if the advisory refresh itself fails. Request failures never
    // reach this seam, so their frozen descriptor remains mounted for an
    // explicit renderer-owned retry.
    state.handoff();
    report(message);
    try { await refresh(); } finally { state.finish(result); }
    return result;
  };

  return Object.freeze({
    close: state.close,

    async selectAgentWindows(request) {
      // Web focus never reaches the native window endpoint: the terminal shell
      // adapter is the lifecycle authority for browser panes. Relinquish the
      // dialog first, matching the legacy close-before-open ordering.
      if (request.direction === 'focus' && request.webTerminal) {
        state.handoff();
        const result = {
          direction: 'focus', scope: request.scope,
          targeted: request.targets?.length || 0,
          focused: request.targets?.length || 0,
          terminal: 'web',
        };
        try {
          for (const target of request.targets || []) {
            openWebWindowPane(target.selector, target.label);
          }
          report(`focus web terminals: ${result.focused} focused`);
          return result;
        } catch (cause) {
          report(`focus web terminals failed: ${cause?.message || cause}`, true);
          throw cause;
        } finally {
          // Even an unexpected terminal-shell adapter failure cannot orphan the
          // promise-backed launcher after visual ownership was handed off.
          state.finish(result);
        }
      }

      // Only the daemon's exact native wire fields cross the HTTP boundary.
      const payload = {
        direction: request.direction,
        scope: request.scope,
        convs: request.convs,
      };
      if (request.scope === 'group') payload.group = request.group;
      let response;
      try {
        response = await fetchImpl('/api/agent-windows', {
          method: 'POST',
          credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload),
        });
      } catch (cause) {
        throw new Error(`request failed: ${cause?.message || cause}`);
      }
      if (!response.ok) {
        const message = await response.text();
        throw new Error(message || `HTTP ${response.status}`);
      }
      // The daemon has already accepted the operation. An unreadable/empty
      // success body is the legacy generic-success path, never grounds to retry
      // a possibly non-idempotent window operation.
      let text = '';
      try { text = await response.text(); } catch (_) {}
      let result = null;
      if (text) {
        try { result = JSON.parse(text); } catch (_) {}
      }

      state.handoff();
      if (!result) {
        report('agent windows: done');
      } else if (request.direction === 'focus') {
        const extra = result.failed ? `, ${result.failed} failed` : '';
        report(
          `focus windows (${result.targeted} targeted): ${result.focused} focused${extra}`,
          result.failed > 0,
        );
      } else {
        // Pane cleanup is keyed to the daemon's returned identities/outcomes,
        // never the optimistic submitted list. The terminal shell decides which
        // exact detached panes to close and leaves no-window/failed rows alone.
        try {
          closeTerminalsForWindowOp(result.agents);
        } catch (cause) {
          report(`unfocus terminal cleanup failed: ${cause?.message || cause}`, true);
        }
        const parts = [`${result.detached} detached`];
        if (result.no_window) parts.push(`${result.no_window} had no window`);
        if (result.failed) parts.push(`${result.failed} failed`);
        report(
          `unfocus windows (${result.targeted} targeted): ${parts.join(', ')}`,
          result.failed > 0,
        );
      }
      state.finish(result || { ok: true });
      return result;
    },

    async loadAgentWorktree(conv, { signal } = {}) {
      const response = await fetchImpl(
        `/api/agents/${encodeURIComponent(conv)}/worktree`,
        { credentials: 'same-origin', signal },
      );
      const payload = await responsePayload(response);
      if (!response.ok) throw responseError(response, payload);
      return payload;
    },

    async shutdownAgent({ agent, label, force }) {
      const choice = Object.freeze({ force: !!force });
      const response = await fetchImpl(
        `/api/agents/${encodeURIComponent(agent)}/stop`,
        {
          method: 'POST',
          credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(choice),
        },
      );
      const payload = await responsePayload(response);
      if (!response.ok) throw responseError(response, payload);
      // stopOneConv reports lifecycle failures inside an HTTP-200 envelope.
      // Treat that result as a failed transaction so the mounted dialog can
      // surface it inline and retry the same frozen target and force choice.
      if (payload?.action === 'error') throw lifecycleError(response, payload);
      const result = {
        ok: true,
        action: String(payload?.action || 'ok'),
        response: payload,
      };
      return finishSuccess(
        result,
        `shutdown ${label || agent}: ${result.action}`,
      );
    },

    async deleteAgent({ agent, label, deleteWorktree, expectedWorktree }) {
      // Only the exact frozen opt-in boolean adds the destructive worktree
      // query. Freeze the exact probed path into a server-authoritative
      // precondition so a moved agent cannot redirect deletion to another
      // worktree between probe and confirmation. Permanent delete has no body.
      const choice = Object.freeze({
        deleteWorktree: deleteWorktree === true,
        expectedWorktree: deleteWorktree === true && typeof expectedWorktree === 'string'
          ? expectedWorktree : '',
      });
      if (choice.deleteWorktree && !choice.expectedWorktree) {
        throw new Error('delete worktree requires a freshly probed worktree path');
      }
      const params = new URLSearchParams();
      if (choice.deleteWorktree) {
        params.set('delete_worktree', '1');
        params.set('expected_worktree', choice.expectedWorktree);
      }
      const query = params.size ? `?${params}` : '';
      const response = await fetchImpl(
        `/api/agents/${encodeURIComponent(agent)}${query}`,
        { method: 'DELETE', credentials: 'same-origin' },
      );
      const payload = await responsePayload(response);
      if (!response.ok) throw responseError(response, payload);
      const result = { ok: true, response: payload };
      const worktree = payload?.worktree;
      const notice = worktree
        ? `deleted ${label || agent} · ${worktree}`
        : `deleted ${label || agent}`;
      return finishSuccess(result, notice);
    },

    async retireGroupPreview({ group, convs, shutdown, deleteWorktree }) {
      const response = await fetchImpl(
        `/api/groups/${encodeURIComponent(group)}/retire`,
        {
          method: 'POST',
          credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ convs, shutdown: !!shutdown, delete_worktree: !!deleteWorktree }),
        },
      );
      const payload = await responsePayload(response);
      if (!response.ok) throw responseError(response, payload);
      return payload;
    },

    async retireUngroupedPreview({ agents, shutdown, deleteWorktrees }) {
      const response = await fetchImpl(
        '/api/cleanup/agents',
        {
          method: 'POST',
          credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            agents, mode: 'retire', include_online: true,
            shutdown: !!shutdown, delete_worktrees: !!deleteWorktrees,
          }),
        },
      );
      const payload = await responsePayload(response);
      if (!response.ok) throw responseError(response, payload);
      return payload;
    },

    async deleteRetiredPreview({ agents, deleteWorktrees }) {
      const response = await fetchImpl(
        '/api/cleanup/agents',
        {
          method: 'POST',
          credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            agents, mode: 'delete', delete_worktrees: !!deleteWorktrees,
          }),
        },
      );
      const payload = await responsePayload(response);
      if (!response.ok) throw responseError(response, payload);
      return payload;
    },

    async cleanup(request) {
      const groupMode = request.mode === 'group';
      const url = groupMode ? '/api/cleanup/group' : '/api/cleanup/agents';
      const payload = groupMode ? {
        group: request.group,
        members: request.targets,
        include_owners: !!request.includeOwners,
      } : {
        agents: request.targets,
        mode: request.tier,
        include_owners: !!request.includeOwners,
        include_online: !!request.includeOnline,
        delete_worktrees: !!request.deleteWorktrees,
        shutdown: !!request.shutdown,
      };
      let response;
      try {
        response = await fetchImpl(url, {
          method: 'POST',
          credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload),
        });
      } catch (cause) {
        throw new Error(`Request failed: ${cause?.message || cause}`);
      }
      const result = await responsePayload(response);
      if (!response.ok) throw responseError(response, result);
      return result;
    },

    async finishCleanup(response) {
      // Accepted cleanup outcomes are a stable, read-only phase. Reconcile the
      // dashboard only after Done/Escape/backdrop dismisses that exact result.
      const result = { kind: 'cleanup', response };
      state.handoff();
      try { await refresh(); } finally { state.finish(result); }
      return result;
    },

    async handoffCleanupWorktrees(descriptor = {}) {
      // The dedicated worktree island is the next visual owner. Unpaint the
      // keyed cleanup transaction first, let opener focus restore, then launch
      // the exact follow-up descriptor without admitting a competing dialog.
      const followup = Object.freeze({ group: String(descriptor.group || '') });
      const result = { kind: 'cleanup-worktrees', descriptor: followup };
      state.handoff();
      await Promise.resolve();
      try {
        await openWorktreeCleanup(followup.group);
      } finally {
        state.finish(result);
      }
      return result;
    },

    async deleteGroupPlan(request) {
      let progress = deleteGroupProgress.get(request);
      if (!progress) {
        progress = {
          pendingRetire: [...(request.retireMembers || [])],
          retiredSelectors: new Set(),
          retireComplete: (request.retireMembers || []).length === 0,
          deleteComplete: false,
          result: null,
        };
        deleteGroupProgress.set(request, progress);
      }
      if (progress.result) return progress.result;

      if (!progress.retireComplete) {
        let response;
        try {
          response = await fetchImpl(
            `/api/groups/${encodeURIComponent(request.group)}/retire`,
            {
              method: 'POST',
              credentials: 'same-origin',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({
                convs: progress.pendingRetire.map((member) => member.selector),
                shutdown: true,
                delete_worktree: false,
              }),
            },
          );
        } catch (cause) {
          const error = new Error(cause?.message || String(cause));
          error.phase = 'retire';
          error.network = true;
          throw error;
        }
        const payload = await responsePayload(response);
        if (!response.ok) {
          const error = responseError(response, payload);
          error.phase = 'retire';
          throw error;
        }

        const hasTopLevelError = payload !== null && typeof payload === 'object'
          && !Array.isArray(payload)
          && Object.prototype.hasOwnProperty.call(payload, 'error');
        if (hasTopLevelError || !Array.isArray(payload?.members)) {
          const detail = hasTopLevelError
            ? (payload.error || payload.message || 'endpoint returned an error')
            : 'invalid response (expected a "members" array)';
          const error = new Error(`retire failed: ${String(detail)}`);
          error.phase = 'retire';
          error.status = response.status;
          error.payload = payload;
          throw error;
        }

        const rows = payload.members;
        const aliases = new Map();
        for (const member of progress.pendingRetire) {
          for (const alias of [member.selector, member.agent_id, member.conv_id]) {
            if (alias) aliases.set(String(alias), member);
          }
        }
        const failed = new Set();
        let unmappedErrors = 0;
        for (const row of rows) {
          const member = aliases.get(String(row?.agent_id || ''))
            || aliases.get(String(row?.conv_id || ''));
          if (row?.action === 'error') {
            if (member) failed.add(member);
            else unmappedErrors += 1;
            continue;
          }
          if (row?.action === 'retired' && member) {
            progress.retiredSelectors.add(member.selector);
          }
        }
        if (failed.size || unmappedErrors) {
          // Only retry exact frozen selectors whose own result failed. If an
          // impossible malformed response reports an unidentifiable error,
          // retain the whole pending cohort and keep the delete barrier shut.
          progress.pendingRetire = unmappedErrors
            ? progress.pendingRetire : [...failed];
          const error = new Error(`retire failed for ${failed.size + unmappedErrors} members`);
          error.phase = 'retire';
          error.memberErrors = failed.size + unmappedErrors;
          throw error;
        }
        // A successful explicit response is authoritative. Missing rows mean a
        // frozen selector is no longer a current member; there is no member
        // error to replay, and the final group delete will detach what remains.
        progress.pendingRetire = [];
        progress.retireComplete = true;
      }

      if (!progress.deleteComplete) {
        let response;
        try {
          response = await fetchImpl(
            `/api/groups/${encodeURIComponent(request.group)}`,
            { method: 'DELETE', credentials: 'same-origin' },
          );
        } catch (cause) {
          const error = new Error(cause?.message || String(cause));
          error.phase = 'delete';
          error.network = true;
          throw error;
        }
        const payload = await responsePayload(response);
        if (!response.ok) {
          const error = responseError(response, payload);
          error.phase = 'delete';
          throw error;
        }
        progress.deleteComplete = true;
      }

      const retired = progress.retiredSelectors.size;
      progress.result = Object.freeze({
        ok: true,
        group: request.group,
        retired,
        detached: Math.max(0, Number(request.memberCount || 0) - retired),
      });
      return progress.result;
    },

    async finishDeleteGroup(result, notices) {
      return finishSuccess(result, words(notices.plain, notices.wizard));
    },

    async finishBulkRetire(result) {
      // Successful bulk responses deliberately remain painted as a stable,
      // read-only outcome table. Only Done/Escape/backdrop reaches this seam:
      // unpaint first, then refresh the roster, then resolve the launcher.
      state.handoff();
      try { await refresh(); } finally { state.finish(result); }
      return result;
    },

    async finishDeleteRetired(result) {
      // The accepted cleanup response stays mounted as the authoritative
      // per-item result. Only Done/Escape/backdrop reconciles the dashboard,
      // after the result has relinquished visual ownership.
      state.handoff();
      try { await refresh(); } finally { state.finish(result); }
      return result;
    },

    async retireAgent({ conv, label, shutdown, deleteWorktree }) {
      // Keep the daemon's deliberately asymmetric defaults explicit at this
      // destructive boundary: shutdown is always named; worktree deletion is
      // sent only when the frozen choice opted in.
      const choice = Object.freeze({
        shutdown: !!shutdown,
        deleteWorktree: !!deleteWorktree,
      });
      const query = `shutdown=${choice.shutdown ? 1 : 0}`
        + (choice.deleteWorktree ? '&delete_worktree=1' : '');
      const response = await fetchImpl(
        `/api/agents/${encodeURIComponent(conv)}/retire?${query}`,
        { method: 'POST', credentials: 'same-origin' },
      );
      const payload = await responsePayload(response);
      if (!response.ok) {
        if (response.status === 409 && payload?.dangling) {
          return {
            dangling: true,
            convID: String(payload.conv_id || conv),
          };
        }
        throw responseError(response, payload);
      }
      const result = { ok: true, response: payload };
      state.handoff();
      report(retireNotice(label || conv, choice, payload));
      try { await refresh(); } finally { state.finish(result); }
      return { response: payload };
    },

    async handoffDangling({ convID, conv, label }) {
      // The shell confirmation becomes the sole painted owner. Closing the
      // transaction first also restores its opener before the follow-up focus
      // boundary mounts.
      state.handoff();
      // Give the transaction root one microtask to unmount and restore its
      // opener before the shell confirmation captures the next focus owner.
      await Promise.resolve();
      const target = convID || conv;
      const finish = (removed, reason) => {
        const result = { dangling: true, removed, convID: target, reason };
        state.finish(result);
        return result;
      };
      const finishWithNotice = (removed, reason, message, isError = false) => {
        const result = finish(removed, reason);
        report(message, isError);
        return result;
      };
      let approved;
      try {
        approved = await confirm({
          title: 'Remove dangling agent entry?',
          body: 'No conversation data was found for this agent — its conversation is '
            + 'gone, so it can’t be retired (there’s nothing to demote). Remove the '
            + 'dangling entry instead? This purges its leftover enrollment, group '
            + 'and permission rows. It cannot be undone.',
          meta: label || conv,
          okLabel: 'Remove dangling entry',
        });
      } catch (error) {
        return finishWithNotice(
          false, 'confirm_failed', `Remove failed: ${error?.message || error}`, true,
        );
      }
      if (!approved) {
        return finishWithNotice(false, 'declined', 'dangling entry kept');
      }
      let response;
      try {
        response = await fetchImpl(`/api/agents/${encodeURIComponent(target)}`, {
          method: 'DELETE', credentials: 'same-origin',
        });
      } catch (error) {
        return finishWithNotice(
          false, 'transport_failed', `Remove failed: ${error?.message || error}`, true,
        );
      }
      if (!response.ok) {
        let payload;
        try {
          payload = await responsePayload(response);
        } catch (_) {
          return finishWithNotice(
            false, 'http_failed',
            `Remove failed: HTTP ${response.status} (response body unreadable)`, true,
          );
        }
        return finishWithNotice(
          false, 'http_failed',
          `Remove failed: ${payload?.error || payload?.message || `HTTP ${response.status}`}`, true,
        );
      }
      const result = { dangling: true, removed: true, convID: target, reason: 'removed' };
      report(`removed dangling entry: ${label || conv}`);
      try { await refresh(); } finally { state.finish(result); }
      return result;
    },
  });
}
