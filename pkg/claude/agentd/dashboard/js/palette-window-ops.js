// Window-operation adapter for the command palette. Native focus and every
// unfocus operation belong to agentd; web focus belongs to the browser terminal
// shell because opening its websocket is what attaches the terminal client.

export function webWindowTargets(candidates) {
  const seen = new Set();
  const targets = [];
  for (const candidate of candidates || []) {
    if (!candidate?.online) continue;
    const conv = String(candidate.conv_id || '').trim();
    if (!conv || seen.has(conv)) continue;
    seen.add(conv);
    targets.push({
      selector: String(candidate.agent_id || '').trim() || conv,
      label: String(candidate.title || '').trim() || conv.slice(0, 8),
    });
  }
  return targets;
}

export function createPaletteWindowOperator({
  fetchImpl,
  notify,
  openWebWindowPane,
  closeTerminalsForWindowOp,
}) {
  return async function runPaletteWindowOp(
    payload, what, { webTerminal = false, targets = [] } = {},
  ) {
    if (payload.direction === 'focus' && webTerminal) {
      try {
        for (const target of targets) {
          openWebWindowPane(target.selector, target.label);
        }
        const result = {
          direction: 'focus',
          scope: payload.scope,
          targeted: targets.length,
          focused: targets.length,
          terminal: 'web',
        };
        notify(`${what}: ${result.focused} focused`);
        return result;
      } catch (cause) {
        notify(`${what}: ${(cause && cause.message) || cause}`, true);
        return null;
      }
    }

    let response;
    try {
      response = await fetchImpl('/api/agent-windows', {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
    } catch (cause) {
      notify(`${what}: request failed: ${(cause && cause.message) || cause}`, true);
      return null;
    }
    if (!response.ok) {
      notify(`${what}: ${await response.text()}`, true);
      return null;
    }
    const result = await response.json().catch(() => null);
    if (!result) {
      notify(`${what}: done`);
      return null;
    }
    if (payload.direction === 'focus') {
      const extra = result.failed ? `, ${result.failed} failed` : '';
      notify(`${what}: ${result.focused} focused${extra}`, result.failed > 0);
    } else {
      // Pane cleanup is driven by daemon outcomes, not by the optimistic input:
      // no-window and failed agents must keep any existing browser panes.
      closeTerminalsForWindowOp(result.agents);
      const extra = result.failed ? `, ${result.failed} failed` : '';
      notify(`${what}: ${result.detached} detached${extra}`, result.failed > 0);
    }
    return result;
  };
}
