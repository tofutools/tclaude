// group-triggers.js — the installed trigger rules with each one's latest
// firing, as the Groups view projects them (read-only), through fetchImpl:
// the page's node, or another node's (node-api.js nodeFetch).
async function responseError(response) {
  return (await response.text()) || `HTTP ${response.status}`;
}

export async function loadTriggerRules(fetchImpl) {
  const response = await fetchImpl('/api/triggers', { credentials: 'same-origin' });
  if (!response.ok) {
    throw new Error(`load triggers failed: ${await responseError(response)}`);
  }
  const body = await response.json();
  const rules = Array.isArray(body?.triggers) ? body.triggers : [];
  return Promise.all(rules.map(async (rule) => {
    const history = await fetchImpl(
      `/api/triggers/${encodeURIComponent(rule.id)}/firings?limit=1`,
      { credentials: 'same-origin' },
    );
    if (!history.ok) throw new Error(`load trigger state failed: ${await responseError(history)}`);
    const ledger = await history.json();
    return { ...rule, firings: Array.isArray(ledger?.firings) ? ledger.firings : [] };
  }));
}
