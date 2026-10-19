// usage-share-model.js — usage sharing in the spawn dialog: which peers'
// model gateways a new agent may bill its model calls to, and what a spawn
// that uses one sends. A share is one entry of
// /api/federation/models/shares/received (model_shares.go).

// shareKey is a share's spawn reference, also its select value.
export function shareKey(share) {
  return `${share.gateway}@${share.peer}`;
}

// shareLeftUSD is what a capped share has left (null: no cap).
export function shareLeftUSD(share) {
  if (!(share?.max_usd > 0)) return null;
  return Math.max(0, share.max_usd - (share.spent_usd || 0));
}

function dollars(n) {
  return `$${(Math.floor(n * 100) / 100).toFixed(2)}`;
}

function endText(iso, now) {
  const at = Date.parse(iso || '');
  if (!Number.isFinite(at)) return '';
  const days = Math.round((at - now) / 86400000);
  if (days >= 2) return `${days} days`;
  const hours = Math.max(1, Math.round((at - now) / 3600000));
  return `${hours} h`;
}

// modelAllowed checks a model against a share's allowlist (exact IDs and
// trailing-* prefixes). A blank model is the harness default and an alias
// such as "sonnet" or "opus[1m]" resolves inside the harness, so only the
// gateway can judge those.
export function modelAllowed(share, model) {
  const m = String(model || '').trim().replace(/\[[^\]]*\]$/, '');
  if (!m || !m.includes('-')) return true;
  return (share.models || []).some((p) => (p.endsWith('*') ? m.startsWith(p.slice(0, -1)) : p === m));
}

// shareOptions turns the received shares into the dialog's choices for a
// harness and model, each with why it cannot be used, if it cannot.
export function shareOptions(shares, harness, model, now = Date.now()) {
  const dialect = harness?.model_proxy_dialect || '';
  return (Array.isArray(shares) ? shares : []).map((share) => {
    const left = shareLeftUSD(share);
    const ends = share.expires_at ? endText(share.expires_at, now) : '';
    let reason = '';
    if (!dialect) reason = `${harness?.display_name || 'this harness'} cannot use a model gateway`;
    else if (share.dialect !== dialect) reason = `${share.dialect === 'openai' ? 'OpenAI' : 'Anthropic'} models only: not for ${harness?.display_name || 'this harness'}`;
    else if (share.expires_at && Date.parse(share.expires_at) <= now) reason = 'ended';
    else if (left !== null && left <= 0) reason = `used up (${dollars(share.spent_usd || 0)} of ${dollars(share.max_usd)})`;
    else if (!modelAllowed(share, model)) reason = `not for ${model}`;
    const detail = [left === null ? 'no cap' : `${dollars(left)} of ${dollars(share.max_usd)} left`, ends && `ends in ${ends}`].filter(Boolean).join(' · ');
    return {
      value: shareKey(share),
      label: `${share.peer_name || share.peer} · ${share.gateway}`,
      detail,
      reason,
      share,
    };
  });
}

// shareHint explains a chosen share under the row.
export function shareHint(share) {
  if (!share) return '';
  const who = share.peer_name || share.peer;
  const models = (share.models || []).join(', ');
  return `${who} pays for this agent's model calls through ${share.gateway}${models ? `. Models: ${models}` : ''}.`;
}

// shareSpawnFields are what a spawn billed to a share adds to its request:
// the gateway reference and the new agent's own models.proxy permission,
// scoped to that peer and gateway, which the launch checks before binding.
export function shareSpawnFields(share, overrides = {}) {
  if (!share) return {};
  return {
    model_proxy: shareKey(share),
    permission_overrides: {
      ...overrides,
      'models.proxy': { effect: 'grant', scope: { peer: [share.peer], http_proxy: [share.gateway] } },
    },
  };
}
