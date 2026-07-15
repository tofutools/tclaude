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
  confirm = async () => false,
}) {
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

    async finishBulkRetire(result) {
      // Successful bulk responses deliberately remain painted as a stable,
      // read-only outcome table. Only Done/Escape/backdrop reaches this seam:
      // unpaint first, then refresh the roster, then resolve the launcher.
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
