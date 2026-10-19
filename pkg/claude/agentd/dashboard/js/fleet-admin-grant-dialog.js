import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { GRANT_CATEGORIES, GRANT_LIFETIMES, LAUNCH_FIELDS, PEER_SLUGS, REQUESTER_PAYS, extraPolicy, grantConsequence, grantExpiryText, grantRows, planGrants, slugCategory, slugInfo } from './fleet-admin-model.js';

const html = htm.bind(h);

function errText(error) { return error?.message || String(error); }

function where(g) {
  return g.gateway ? `on gateway ${g.gateway}` : g.group ? `in group ${g.group}` : slugInfo(g.slug).kind === 'node' ? 'node-wide' : slugInfo(g.slug).gateway ? 'on every gateway' : 'in every group';
}

// heldText says where the target already holds slug directly.
function heldText(rows, slug) {
  const held = rows.filter((r) => !r.pool && r.slug === slug);
  if (!held.length) return '';
  if (held.some((r) => !r.scope)) return slugInfo(slug).kind === 'node' && !slugInfo(slug).gateway ? 'already granted' : 'held: everywhere';
  return `held: ${held.map((r) => r.gateway || r.group || 'deleted group').join(', ')}`;
}

// AddGrantDialog picks permissions from a searchable, categorised list and
// grants each to one peer or pool, per chosen group. It plans every grant
// up front (planGrants), shows that plan, and sends it through one confirm.
export function AddGrantDialog({ targets, initialTarget, groups, actions, confirm, toast, onClose, onDone }) {
  const [to, setTo] = useState(initialTarget || '');
  // rows are the target's current grants (null while loading): a plan made
  // without them could re-grant a spawn/job grant and drop its settings.
  const [rows, setRows] = useState(null);
  const [rowsError, setRowsError] = useState('');
  const [reload, setReload] = useState(0);
  const [q, setQ] = useState('');
  const [cat, setCat] = useState('');
  const [hideSensitive, setHideSensitive] = useState(false);
  const [picked, setPicked] = useState([]);
  const [allGroups, setAllGroups] = useState(false);
  const [chosen, setChosen] = useState([]);
  const [gateway, setGateway] = useState('');
  const [maxLive, setMaxLive] = useState(2);
  const [launch, setLaunch] = useState({});
  const [showLaunch, setShowLaunch] = useState(false);
  const [lifetime, setLifetime] = useState('');
  const [busy, setBusy] = useState(false);
  const target = targets.find((t) => t.id === to) || null;

  useEffect(() => {
    setRows(null); setRowsError('');
    if (!target) return undefined;
    let off = false;
    actions.grants(target.id).then((g) => { if (!off) setRows(grantRows(g, { ownPool: target.pool ? target.id.slice(6) : '', groups })); }).catch((e) => { if (!off) setRowsError(errText(e)); });
    return () => { off = true; };
  }, [to, reload]);

  const needle = q.trim().toLowerCase();
  const visible = PEER_SLUGS
    .filter((s) => (!cat || slugCategory(s.slug) === cat) && (!hideSensitive || !s.sensitive || picked.includes(s.slug)))
    .filter((s) => !needle || [s.slug, s.what, s.warning, slugCategory(s.slug)].some((x) => String(x || '').toLowerCase().includes(needle)));
  const counts = Object.fromEntries(GRANT_CATEGORIES.map((c) => [c, PEER_SLUGS.filter((s) => slugCategory(s.slug) === c).length]));
  const infos = picked.map(slugInfo);
  const groupKinds = infos.some((i) => i.kind === 'group' || i.kind === 'scoped');
  const scopedOnly = infos.some((i) => i.kind === 'scoped');
  const gatewayKinds = infos.some((i) => i.gateway);
  const policySlugs = picked.filter((s) => slugInfo(s).policy);
  const fields = LAUNCH_FIELDS.filter((f) => policySlugs.some((s) => !f.slugs || f.slugs.includes(s)));
  const gw = gateway.trim();
  const gwBad = gatewayKinds && gw && !/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(gw);
  const plan = target && rows ? planGrants({ target: target.id, slugs: picked, groups: chosen, allGroups, gateway: gw, maxLive, launch, rows, lifetime: target.pool ? '' : lifetime }) : { grants: [], missing: [] };
  const todo = plan.grants.filter((g) => !g.skip);
  const canGrant = !!target && !!rows && todo.length > 0 && !plan.missing.length && !gwBad && !busy;

  const toggle = (slug) => setPicked((p) => (p.includes(slug) ? p.filter((x) => x !== slug) : [...p, slug]));
  const toggleGroup = (g) => setChosen((c) => (c.includes(g) ? c.filter((x) => x !== g) : [...c, g]));

  const grant = () => {
    if (!canGrant) return;
    const n = todo.length;
    const notes = todo.map((g) => {
      const c = grantConsequence({ target: target.label, slug: g.slug, group: g.group, maxLive: g.body.spawn_policy?.max_live, gateway: g.gateway, policy: g.body.spawn_policy });
      const kept = g.existing && g.body.spawn_policy ? extraPolicy(g.existing.policy).filter((k) => !(k in g.body.spawn_policy) || g.existing.policy[k] === g.body.spawn_policy[k]) : [];
      const replaces = !g.existing ? '' : g.body.spawn_policy ? ` This updates the existing grant (cap ${g.existing.maxLive || 2} → ${g.body.spawn_policy.max_live})${kept.length ? `; its other launch settings (${kept.join(', ')}) are kept` : ''}.` : ' This changes when the existing grant ends.';
      const until = g.body.expires_at ? ` It ends ${new Date(g.body.expires_at).toLocaleString()}.` : '';
      return `${c}${replaces}${until}`;
    });
    const sensitive = todo.some((g) => slugInfo(g.slug).sensitive);
    const warnings = [...new Set(todo.map((g) => slugInfo(g.slug).warning).filter(Boolean))];
    const single = n === 1 ? todo[0] : null;
    setBusy(true);
    return confirm({
      title: single ? `${single.existing ? 'Update' : 'Grant'} ${single.slug} ${single.existing ? 'for' : 'to'} ${target.label}?` : `Grant ${n} permissions to ${target.label}?`,
      body: `${notes.join(' ')}${sensitive ? ` This lets it act on this node, not only read.` : ''}${warnings.length ? ` ${warnings.join(' ')}` : ''}${target.pool ? ' The grants apply to every node in the pool, including nodes added later.' : ''}`,
      okLabel: n === 1 ? 'Grant' : `Grant ${n}`,
      busyLabel: 'Granting…',
      action: async () => {
        const warned = [];
        for (const [i, g] of todo.entries()) {
          try {
            const res = g.extend ? await actions.extend({ ...g.body, scope: g.scope }) : await actions.grant(g.body);
            warned.push(...(res?.warnings || []));
          } catch (e) {
            for (const w of warned) toast(w, true);
            throw new Error(`granted ${i} of ${n}; ${g.slug} ${where(g)} and the rest were not: ${errText(e)}`);
          }
        }
        return { warnings: warned };
      },
    }).then((res) => {
      if (!res) return;
      for (const w of res.warnings || []) toast(w, true);
      toast(single ? `Granted ${single.slug} to ${target.label}` : `Granted ${n} permissions to ${target.label}`, false);
      onDone();
    }).catch((e) => { toast(`Grant failed: ${errText(e)}`, true); setReload((n) => n + 1); onDone(false); })
      .finally(() => setBusy(false));
  };

  let lastCat = '';
  return html`<${Overlay} id="fleet-grant-dialog" labelledby="fleet-grant-dialog-title" dialogClass="cron-create-modal fa-grant-dialog" onClose=${onClose} blocked=${busy}>
    <h3 id="fleet-grant-dialog-title">Add grants</h3>
    <div class="fa-gd">
      <div class="fa-gd-pick">
        <input id="fleet-grant-search" type="text" placeholder="Search permissions: name or what it allows…" value=${q} autocomplete="off" spellcheck="false" onInput=${(e) => setQ(e.currentTarget.value)} />
        <div class="fa-gd-chips">
          <button type="button" class=${`fa-gd-chip${cat ? '' : ' on'}`} data-cat="" onClick=${() => setCat('')}>All <span class="muted">${PEER_SLUGS.length}</span></button>
          ${GRANT_CATEGORIES.map((c) => html`<button key=${c} type="button" class=${`fa-gd-chip${cat === c ? ' on' : ''}`} data-cat=${c} onClick=${() => setCat(cat === c ? '' : c)}>${c} <span class="muted">${counts[c]}</span></button>`)}
          <label class="muted fa-gd-hide"><input id="fleet-grant-hide-sensitive" type="checkbox" checked=${hideSensitive} onChange=${(e) => setHideSensitive(e.currentTarget.checked)} /> hide sensitive</label>
        </div>
        <div class="fa-gd-list" id="fleet-grant-list">
          ${visible.length === 0 && html`<div class="empty">No permission matches.</div>`}
          ${[...visible].sort((a, b) => GRANT_CATEGORIES.indexOf(slugCategory(a.slug)) - GRANT_CATEGORIES.indexOf(slugCategory(b.slug))).map((s) => {
            const c = slugCategory(s.slug);
            const head = c !== lastCat ? html`<div class="fa-gd-cat" key=${`c|${c}`}>${c}</div>` : null;
            lastCat = c;
            const held = heldText(rows || [], s.slug);
            const on = picked.includes(s.slug);
            return html`${head}<label key=${s.slug} class=${`fa-gd-item${on ? ' on' : ''}`} data-grant-slug=${s.slug}>
              <input type="checkbox" checked=${on} onChange=${() => toggle(s.slug)} />
              <code class=${s.sensitive ? 'fa-sensitive' : ''}>${s.slug}</code>
              <span class="fa-gd-badges">${held && html`<span class="fa-gd-badge held">${held}</span>`}${s.sensitive && html`<span class="fa-gd-badge sens">sensitive</span>`}<span class="fa-gd-badge">${s.gateway ? 'gateway' : s.kind === 'node' ? 'node-wide' : 'group'}</span></span>
              <span class="fa-gd-what muted">${s.what}</span>
              ${s.warning && html`<span class="fa-gd-what fa-warn">${s.warning}</span>`}
            </label>`;
          })}
        </div>
      </div>
      <div class="fa-gd-side">
        <label class="fa-gd-row"><span class="fa-k">To</span>
          <select id="fleet-grant-to" value=${to} onChange=${(e) => setTo(e.currentTarget.value)}>
            <option value="">pick a peer or pool…</option>
            ${targets.map((t) => html`<option key=${t.id} value=${t.id}>${t.label}${t.level === 'unrestricted' ? ' (unrestricted)' : ''}</option>`)}
          </select></label>
        ${target?.level === 'unrestricted' && html`<div class="fa-warn fa-gd-note">Unrestricted: ${target.label} already holds every group permission on all groups; these matter once it is restricted.</div>`}
        ${groupKinds && html`<div class="fa-gd-row fa-gd-top"><span class="fa-k">Where</span><div>
          ${!scopedOnly || infos.some((i) => i.kind === 'group') ? html`<label class="fa-gd-radio"><input type="radio" name="fleet-grant-where" id="fleet-grant-all-groups" checked=${allGroups} onChange=${() => setAllGroups(true)} /> <span class="fa-warn">all groups, incl. future</span></label>` : ''}
          <label class="fa-gd-radio"><input type="radio" name="fleet-grant-where" id="fleet-grant-some-groups" checked=${!allGroups} onChange=${() => setAllGroups(false)} /> these groups:</label>
          <div class="fa-gd-groups">${groups.length === 0 ? html`<span class="muted">no groups on this node</span>` : groups.map((g) => html`<label key=${g} class=${`fa-gd-group${chosen.includes(g) ? ' on' : ''}`} data-grant-group=${g}><input type="checkbox" checked=${chosen.includes(g)} onChange=${() => toggleGroup(g)} />${g}</label>`)}</div>
          ${allGroups && scopedOnly && html`<div class="muted fa-gd-note">${picked.filter((s) => slugInfo(s).kind === 'scoped').join(', ')} always need${picked.filter((s) => slugInfo(s).kind === 'scoped').length === 1 ? 's' : ''} the groups picked here.</div>`}
        </div></div>`}
        ${gatewayKinds && html`<label class="fa-gd-row"><span class="fa-k">Gateway</span><input id="fleet-grant-gateway" type="text" placeholder="all gateways (or a name)" value=${gateway} autocomplete="off" spellcheck="false" onInput=${(e) => setGateway(e.currentTarget.value)} /></label>`}
        ${gwBad && html`<div class="fa-danger fa-gd-note">A gateway name is letters, digits, . _ - (max 64).</div>`}
        ${policySlugs.length > 0 && html`<div class="fa-gd-row"><span class="fa-k">Cap</span><span><input id="fleet-grant-cap" type="number" min="1" value=${maxLive} style="width:4em" onInput=${(e) => setMaxLive(e.currentTarget.value)} />
          <span class="muted"> live workers</span> <button id="fleet-grant-launch-toggle" type="button" class="fa-link" aria-expanded=${showLaunch ? 'true' : 'false'} onClick=${() => setShowLaunch(!showLaunch)}>launch settings${Object.keys(launch).length ? ' •' : ''}</button></span></div>`}
        ${policySlugs.length > 0 && showLaunch && html`<div class="fa-grant-launch" id="fleet-grant-launch">
          <span class="muted">How this node starts workers for the peer. Empty uses the group's or profile's default; on an existing grant, untouched settings are kept.</span>
          ${fields.map((f) => html`<label key=${f.key}><span class="fa-k">${f.label}</span><input data-launch=${f.key} type="text" placeholder=${f.hint} value=${launch[f.key] ?? ''} autocomplete="off" spellcheck="false" onInput=${(e) => setLaunch({ ...launch, [f.key]: e.currentTarget.value })} /></label>`)}
          ${policySlugs.includes('groups.members.spawn') && html`<label><span class="fa-k">requester pays</span><select data-launch="requester_pays" value=${launch.requester_pays ?? ''} onChange=${(e) => setLaunch({ ...launch, requester_pays: e.currentTarget.value })}>
            ${REQUESTER_PAYS.map((o) => html`<option key=${o.value} value=${o.value}>${o.label}</option>`)}</select></label>`}
          ${policySlugs.includes('jobs.run') && html`<label><span class="fa-k">job approval</span><select data-launch="job_approval" value=${launch.job_approval ?? ''} onChange=${(e) => setLaunch({ ...launch, job_approval: e.currentTarget.value })}>
            <option value="">auto (default)</option><option value="manual">manual: each job waits for you</option></select></label>`}
        </div>`}
        ${!target?.pool && html`<label class="fa-gd-row"><span class="fa-k">For</span><select id="fleet-grant-lifetime" value=${lifetime} onChange=${(e) => setLifetime(e.currentTarget.value)}>
          ${GRANT_LIFETIMES.map((o) => html`<option key=${o.value} value=${o.value}>${o.label}</option>`)}</select></label>`}
        ${!target?.pool && !lifetime && rows?.some((r) => !r.pool && r.expiresAt && picked.includes(r.slug)) && html`<div class="muted fa-gd-note">A timed grant you update keeps its time limit.</div>`}
        <div class="fa-gd-plan" id="fleet-grant-plan">
          ${rowsError ? html`<span class="fa-danger" role="alert">Could not read ${target?.label}'s grants: ${rowsError}</span>` : !picked.length ? html`<span class="muted">Tick permissions on the left.</span>` : !target ? html`<span class="muted">Pick who gets them.</span>` : !rows ? html`<span class="muted">Loading ${target.label}'s grants…</span>` : html`
            <b>${todo.length ? `${target.label} gets ${todo.length} grant${todo.length === 1 ? '' : 's'}:` : 'Nothing new to grant.'}</b>
            <ul>${plan.grants.map((g) => html`<li key=${g.key} data-plan=${g.key} class=${g.skip ? 'muted' : ''}><code class=${slugInfo(g.slug).sensitive ? 'fa-sensitive' : ''}>${g.slug}</code> ${where(g)}${g.body.spawn_policy ? ` (cap ${g.body.spawn_policy.max_live})` : ''}${g.body.expires_at ? html` <span class="muted">${grantExpiryText(g.body.expires_at)}</span>` : ''}${g.skip ? ' — already granted' : g.existing ? ' — updates the existing grant' : ''}</li>`)}</ul>
            ${plan.missing.length > 0 && html`<div class="fa-warn">Pick a group for ${plan.missing.join(', ')}.</div>`}`}
        </div>
        <div class="modal-buttons"><span class="spacer"></span>
          <button type="button" onClick=${onClose}>Cancel</button>
          <button id="fleet-grant-submit" type="button" class="primary" disabled=${!canGrant} onClick=${grant}>${todo.length > 1 ? `Grant ${todo.length}…` : 'Grant…'}</button>
        </div>
      </div>
    </div>
    <div class="muted fa-cli-note">CLI: <code>tclaude federation grant PEER SLUG [--scope …] [--max-live N] [--profile …] [--harness …] [--model …] [--cwd …] [--allow-profile …] [--requester-pays …] [--job-approval …] [--for 7d]</code> · <code>tclaude federation extend PEER SLUG --for 7d</code></div>
  </${Overlay}>`;
}
