// Model access for a teleported or cloned agent: whose login or which model
// gateway it runs on where it lands. The choice travels as the teleport's
// credentials (local, or proxy:<gateway>@<node>); the receiving node checks
// it against what it accepts from this node (teleport_credentials.go).

// The provider protocol a harness speaks to a gateway; a harness without
// one cannot use a gateway at all.
const HARNESS_DIALECT = Object.freeze({ claude: 'anthropic', codex: 'openai', copilot: 'openai' });

export function harnessDialect(harness) {
  if (!harness) return '';
  return HARNESS_DIALECT[harness] || null;
}

// modelAccessOptions lists the dialog's choices. dest names the receiving
// node; creds is its /api/teleport-credentials answer (null for a pull
// here, where this node decides); own are this node's gateways (push only,
// paid through a lease); shares are gateways the receiving node can use.
// trust is the level this node gives the other node (the receiver of a push,
// the source of a pull), source names a pull's source node, and selfIds are
// the names a landing policy may use for this node. Each option is
// {value, label, reason}; a reason disables it.
export function modelAccessOptions({ dest, harness = '', creds = null, own = [], shares = [], pull = false, trust = '', source = '', sourceName = '', selfIds = [] }) {
  const dialect = harnessDialect(harness);
  const gatewayWhy = dialect === null ? `${harness} cannot use a model gateway` : '';
  const fits = (d) => !dialect || (d || 'anthropic') === dialect;
  // A landing policy names this node's own gateway by whatever alias its
  // operator used for this node.
  const ours = new Set(['self', ...selfIds.filter(Boolean)]);
  const accepts = (mode) => !creds || creds.any || (creds.allowed || []).includes(mode)
    || (mode.endsWith('@self') && (creds.allowed || []).some((m) => {
      const at = m.lastIndexOf('@');
      return m.slice(0, at) === mode.slice(0, -'@self'.length) && ours.has(m.slice(at + 1));
    }));
  // A pull lands here: this node holds the source to its landing policy
  // unless it trusts the source unrestricted.
  const pullWhy = pull && trust !== 'unrestricted' ? `this node holds ${sourceName || 'that node'} to its landing policy; trust it unrestricted to choose here` : '';
  const out = [];
  const def = creds?.default && creds.default !== 'local' ? ` (${creds.default.replace(/^proxy:/, 'gateway ')})` : '';
  out.push({ value: '', label: `${dest}'s default${def}`, reason: '' });
  out.push({ value: 'local', label: `${dest}'s own login`, reason: pull || accepts('local') ? '' : `${dest} does not accept that from this node` });
  if (!pull) {
    for (const g of own) {
      if (!fits(g.dialect)) continue;
      const value = `proxy:${g.name}@self`;
      const reason = gatewayWhy || (!g.enabled ? 'this gateway is disabled'
        : creds?.requester_pays === 'off' ? `${dest} does not take workers you pay for`
          : !accepts(value) ? `${dest}'s landing policy does not list it` : '');
      // The lease needs a models.proxy.leased grant for the receiver, which
      // unrestricted trust implies; an explicit grant is not visible here.
      const grant = trust === 'unrestricted' ? '' : ` (needs a models.proxy.leased grant for ${dest})`;
      out.push({ value, label: `Your gateway ${g.name} (you pay)${grant}`, reason });
    }
  }
  for (const s of shares) {
    // The source's own gateway would become a lease the source pays for, not
    // the share this node holds.
    if (!fits(s.dialect) || (pull && s.peer === source)) continue;
    const value = `proxy:${s.gateway}@${s.peer}`;
    const reason = gatewayWhy || pullWhy || (s.used_up ? 'its cap is spent' : !pull && !accepts(value) ? `${dest}'s landing policy does not list it` : '');
    out.push({ value, label: `${s.peer_name || s.peer}'s gateway ${s.gateway}`, reason });
  }
  return out;
}

// ownGateways turns /api/federation/models/control into picker entries.
export function ownGateways(control) {
  const gws = control?.gateways || {};
  return Object.keys(gws).sort().map((name) => ({ name, dialect: gws[name].dialect || 'anthropic', enabled: !!gws[name].enabled && !control.disabled }));
}
