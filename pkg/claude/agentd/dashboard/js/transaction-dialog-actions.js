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
