import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { confirmHubSave } from './fleet-admin-node-settings.js';
import { grantConsequence, grantRows, shortID } from './fleet-admin-model.js';

const html = htm.bind(h);

// The setup guide (Fleet → Setup guide) walks one node through federation:
// join a hub, trust the other nodes, share groups with them, and let local
// agents use what they share. Every step runs the same daemon routes as the
// rest of Fleet administration and names its CLI, so nothing here is
// dashboard-only. It is the landing page until the first three steps are
// done, and stays one click away after.

function errText(error) { return error?.message || String(error); }

// SHARE_PRESETS are the guide's sets of peer grants on one local group. They
// are plain peer grants: the Peer grants page shows and revokes each one.
export const SHARE_PRESETS = Object.freeze([
  { id: 'mail', label: 'Mail', what: 'mail its members, and see who they are and whether they are online', slugs: ['message.direct', 'groups.roster.read', 'groups.presence.read'] },
  { id: 'status', label: 'Status', what: 'see its agents\' activity and live sessions', slugs: ['agents.status.read', 'sessions.read'] },
  { id: 'watch', label: 'Watch terminals', what: 'watch its agents\' terminals, read-only', slugs: ['sessions.watch'] },
  { id: 'spawn', label: 'Spawn workers', what: 'have workers spawned in it, at most 2 at a time, with the group\'s launch defaults', slugs: ['groups.members.spawn'], sensitive: true },
]);

// SPAWN_CAP is the live worker cap a Spawn workers preset grants.
const SPAWN_CAP = 2;

// CAP_USES maps what a peer's catalog group allows this node to the agent
// permission that uses it (scoped peer=<peer>/<group>), and words both.
const CAP_USES = Object.freeze([
  { cap: 'mail', word: 'mail', slug: 'message.direct' },
  { cap: 'roster', word: 'roster' },
  { cap: 'presence', word: 'presence' },
  { cap: 'agents.status.read', word: 'agent status', slug: 'agents.status.read' },
  { cap: 'sessions', word: 'sessions', slug: 'sessions.read' },
  { cap: 'sessions_watch', word: 'watch terminals', slug: 'sessions.watch' },
  { cap: 'sessions_attach', word: 'attach terminals', slug: 'sessions.attach' },
  { cap: 'spawn', word: 'spawn workers', slug: 'groups.members.spawn' },
  { cap: 'routes', word: 'routes', slug: 'routes.consume' },
  { cap: 'jobs', word: 'jobs', slug: 'jobs.run' },
  { cap: 'agents_receive', word: 'receive agents' },
]);

export function capWords(caps = []) {
  const known = CAP_USES.filter((u) => caps.includes(u.cap)).map((u) => u.word);
  return known.length ? known : caps.slice();
}

// agentSlugs names the agent permissions that use a catalog group's caps.
export function agentSlugs(caps = []) {
  return CAP_USES.filter((u) => u.slug && caps.includes(u.cap)).map((u) => u.slug);
}

// setupProgress reads the admin view as the guide's steps. Joining, trusting
// and sharing are the setup; letting agents use a peer is optional, since a
// peer that only reads or mails this node needs nothing more here.
export function setupProgress(view) {
  const self = view.self;
  const connected = self.enabled && self.hubState === 'connected';
  const trusted = view.trusted.length > 0;
  const shared = view.trusted.some((r) => r.level === 'unrestricted' || r.grants.total > 0);
  const steps = [
    { id: 'hub', title: 'Join a hub', done: connected },
    { id: 'trust', title: 'Trust your peers', done: trusted },
    { id: 'share', title: 'Share groups', done: shared },
    { id: 'agents', title: 'Let agents use peers', optional: true, done: false },
  ];
  const required = steps.filter((s) => !s.optional);
  return {
    steps,
    current: required.find((s) => !s.done)?.id || 'agents',
    done: required.filter((s) => s.done).length,
    total: required.length,
    complete: required.every((s) => s.done),
  };
}

// hubProblem explains a configured hub connection that is not up, in the
// operator's terms, with what usually fixes it.
export function hubProblem(self) {
  if (!self.hubURL) return null;
  if (!self.enabled) return { title: 'Federation is off', text: `This node remembers the hub at ${self.hubURL} but is not connected to it.` };
  if (self.hubState === 'refused') {
    return {
      title: 'The hub refused this node', invite: true,
      text: 'An invite works once and expires. If this node already used its invite, or was never admitted, ask the hub admin for a new invite (or to admit this node by its instance ID) and paste it below.',
    };
  }
  if (self.hubState === 'disconnected') {
    return {
      title: 'The hub is unreachable', settings: true,
      text: 'This node keeps retrying. Check the address and port, that the hub is running, and that this machine can reach it. A hub with a self-signed certificate also needs its CA file on this node (Hub settings).',
    };
  }
  return { title: 'Connecting…', text: `This node is connecting to ${self.hubURL}.` };
}

function Copyable({ value, what, copy, toast }) {
  const onCopy = () => copy(value).then(() => toast(`${what} copied`, false)).catch(() => toast(`${what}: ${value}`, false));
  return html`<span class="fs-copy"><code>${value}</code> <button type="button" class="fa-link" onClick=${onCopy} title=${`Copy the ${what.toLowerCase()}`}>copy</button></span>`;
}

function Cli({ children }) {
  return html`<div class="muted fs-cli">CLI: ${children}</div>`;
}

function IdentityCard({ self, copy, toast }) {
  return html`<div class="fa-dl fs-identity">
    <span class="fa-k">Instance</span><${Copyable} value=${self.id} what="Instance ID" copy=${copy} toast=${toast} />
    <span class="fa-k">Fingerprint</span><${Copyable} value=${self.fingerprint} what="Fingerprint" copy=${copy} toast=${toast} />
  </div>`;
}

function HubStep({ view, actions, confirm, toast, copy, reload, onSettings }) {
  const self = view.self;
  const [form, setForm] = useState({ hubURL: '', name: self.name === 'this node' ? '' : self.name, invite: '', caFile: '', clearCA: false, connect: true });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const set = (k) => (e) => { const value = e.currentTarget.value; setForm((f) => ({ ...f, [k]: value })); };
  const save = (next) => {
    if (busy) return;
    setBusy(true); setError('');
    confirmHubSave({ self, form: next, actions, confirm }).then((r) => {
      if (r.error) setError(r.error);
      else if (r.done) { toast(r.done, false); reload(); }
    }).finally(() => setBusy(false));
  };
  const reconnect = () => actions.setHubEnabled(true)
    .then(() => { toast('Reconnecting to the hub…', false); reload(); })
    .catch((e) => toast(`Reconnect failed: ${errText(e)}`, true));

  if (self.enabled && self.hubState === 'connected') {
    return html`<div class="fs-body" data-setup-step="hub">
      <h3>Joined</h3>
      <p class="fs-ok">✓ Connected to <code>${self.hubURL}</code> as <b>${self.name}</b>${self.spaces.length ? html`, in space${self.spaces.length === 1 ? '' : 's'} <b>${self.spaces.join(', ')}</b>` : ''}.</p>
      <p class="muted">Other nodes in the same space see this node and can ask you to trust them; nothing is shared until you trust a node and grant it something. A node uses one hub at a time: <button type="button" class="fa-link" onClick=${onSettings}>Hub settings…</button> moves it to another hub or renames it.</p>
      <${Cli}><code>tclaude federation status</code><//>
    </div>`;
  }
  const problem = hubProblem(self);
  if (problem) {
    return html`<div class="fs-body" data-setup-step="hub">
      <h3>${problem.title}</h3>
      <p>${problem.text}</p>
      ${self.hubError && html`<div class="fs-error" role="note">${self.hubError}</div>`}
      ${problem.invite && html`<label class="fa-ns-row"><span class="fa-k">new invite</span><input id="fleet-setup-reinvite" type="password" value=${form.invite} placeholder="tchi_…" autocomplete="off" onInput=${set('invite')} /></label>`}
      <div class="fs-actions">
        ${problem.invite && html`<button type="button" class="primary" disabled=${busy || !form.invite.trim()} onClick=${() => save({ ...form, hubURL: self.hubURL, name: self.name, connect: true })}>Retry with this invite…</button>`}
        ${!self.enabled && html`<button type="button" class="primary" onClick=${reconnect}>Reconnect</button>`}
        <button type="button" onClick=${onSettings}>Hub settings…</button>
      </div>
      ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
      <${Cli}><code>tclaude federation status</code>, <code>tclaude federation connect ${self.hubURL} --invite tchi_…</code><//>
    </div>`;
  }
  return html`<div class="fs-body" data-setup-step="hub">
    <h3>Join a hub</h3>
    <p>Nodes link through a hub: each one connects out to it, so no machine needs an open port. Your hub admin gives each node the hub's address and its own single-use invite (<code>tclaude hub invite --space <i>space</i></code>). Nodes see each other when they join the same space.</p>
    <label class="fa-ns-row"><span class="fa-k">hub address</span><input id="fleet-setup-hub" value=${form.hubURL} placeholder="wss://hub.example:8470" autocomplete="off" spellcheck="false" onInput=${set('hubURL')} /></label>
    <label class="fa-ns-row"><span class="fa-k">invite</span><input id="fleet-setup-invite" type="password" value=${form.invite} placeholder="tchi_… (not needed if the admin admitted this node)" autocomplete="off" onInput=${set('invite')} /></label>
    <label class="fa-ns-row"><span class="fa-k">this node's name</span><input id="fleet-setup-name" value=${form.name} placeholder="how other nodes see this one" autocomplete="off" onInput=${set('name')} /></label>
    <details class="fs-more">
      <summary>The hub uses a self-signed certificate</summary>
      <p class="muted">Copy the hub's certificate (or its CA) to this machine and give its absolute path. The daemon reads it on every connect, so leave the file there.</p>
      <label class="fa-ns-row"><span class="fa-k">CA file</span><input id="fleet-setup-ca" value=${form.caFile} placeholder="/home/you/hub.crt" autocomplete="off" spellcheck="false" onInput=${set('caFile')} /></label>
    </details>
    <div class="fs-actions"><button id="fleet-setup-join" type="button" class="primary" disabled=${busy || !form.hubURL.trim()} onClick=${() => save(form)}>Join hub…</button></div>
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="fs-aside">
      <b>No invite?</b> The admin can admit this node by its instance ID and fingerprint instead (<code>tclaude hub admit</code>). Send them these:
      <${IdentityCard} self=${self} copy=${copy} toast=${toast} />
    </div>
    <${Cli}><code>tclaude federation connect wss://… --invite tchi_… [--name …] [--ca-file …]</code><//>
  </div>`;
}

function TrustStep({ view, copy, toast, onTrust, onPeers }) {
  const self = view.self;
  const connected = self.enabled && self.hubState === 'connected';
  return html`<div class="fs-body" data-setup-step="trust">
    <h3>Trust your peers</h3>
    <p>Trust is each operator's own decision, made on both sides. Before you trust a node, compare its fingerprint with its operator over another channel (a call, a chat you trust); they compare yours the same way.</p>
    <div class="fs-aside"><b>Your fingerprint</b> <${Copyable} value=${self.fingerprint} what="Fingerprint" copy=${copy} toast=${toast} /></div>
    ${!connected && html`<p class="fs-warn">Join a hub first: other nodes show up here once this node is connected.</p>`}
    ${connected && (view.waiting.length === 0
      ? html`<div class="empty">No other nodes in your space yet. Each node joins the same space with its own invite; it shows up here once it connects.</div>`
      : html`<table class="fa-table" id="fleet-setup-waiting">
        <thead><tr><th>Node</th><th>Fingerprint</th><th></th></tr></thead>
        <tbody>${view.waiting.map((r) => html`<tr key=${r.id} data-peer=${r.id}>
          <td><span class=${`fa-dot${r.online ? ' on' : ''}`} title=${r.online ? 'online' : 'offline'}></span> ${r.name || html`<span class="muted">unnamed</span>`} <code class="muted" title=${r.id}>${shortID(r.id)}</code></td>
          <td><code class="fa-fp-full">${r.fingerprint || '—'}</code></td>
          <td class="fa-acts"><button type="button" class="primary" data-fa="trust" onClick=${() => onTrust(r)}>Trust…</button></td>
        </tr>`)}</tbody>
      </table>`)}
    ${view.trusted.length > 0 && html`<p class="fs-ok" id="fleet-setup-trusted">✓ Trusted: ${view.trusted.map((r, i) => html`${i ? ', ' : ''}<b>${r.label}</b> <span class="muted">(${r.level}, ${r.online ? 'online' : 'offline'})</span>`)}.
      <button type="button" class="fa-link" onClick=${onPeers}>Peers…</button></p>`}
    <p class="muted">Restricted, the default, gives a peer only what you grant it in the next step. For your own machines you can trust <b>unrestricted</b> instead: every peer permission on every group, including groups created later.</p>
    <${Cli}><code>tclaude federation peers</code>, <code>tclaude federation trust inst_… --label <i>name</i> [--level unrestricted]</code><//>
  </div>`;
}

// presetHeld says whether a peer already holds every grant of a preset on
// the group, so the guide does not offer it again.
export function presetHeld(preset, rows, group) {
  return preset.slugs.every((slug) => rows.some((r) => !r.pool && r.slug === slug && r.group === group));
}

// shareGrants is the peer grant requests one share sends: the picked presets'
// grants on the group the peer does not hold yet.
export function shareGrants({ peer, group, presets, rows }) {
  const out = [];
  for (const preset of SHARE_PRESETS) {
    if (!presets.includes(preset.id)) continue;
    for (const slug of preset.slugs) {
      if (rows.some((r) => !r.pool && r.slug === slug && r.group === group)) continue;
      if (out.some((g) => g.slug === slug)) continue;
      const body = { peer, slug, scope: `group=${group}` };
      if (slug === 'groups.members.spawn') body.spawn_policy = { max_live: SPAWN_CAP };
      out.push(body);
    }
  }
  return out;
}

function ShareStep({ view, groups, actions, confirm, toast, reload, onOpenGrants }) {
  const restricted = view.trusted.filter((r) => r.level !== 'unrestricted');
  const [peer, setPeer] = useState(restricted[0]?.id || '');
  const [group, setGroup] = useState(groups[0] || '');
  const [presets, setPresets] = useState(['mail']);
  const [grants, setGrants] = useState(null);
  const [error, setError] = useState('');
  const [tick, setTick] = useState(0);
  const current = restricted.find((r) => r.id === peer) || restricted[0] || null;
  const currentGroup = groups.includes(group) ? group : groups[0] || '';
  useEffect(() => {
    if (!current) return undefined;
    let off = false;
    setGrants(null); setError('');
    actions.grants(current.id).then((g) => { if (!off) setGrants(g); }).catch((e) => { if (!off) setError(errText(e)); });
    return () => { off = true; };
  }, [current?.id, tick]);

  const intro = html`<h3>Share groups</h3>
    <p>A trusted peer can do nothing on this node until you grant it something. Pick one of your groups and what the peer may do in it. The other operator shares their groups with you on their node, the same way.</p>`;
  if (!view.trusted.length) return html`<div class="fs-body" data-setup-step="share">${intro}<p class="fs-warn">Trust a peer first (step 2).</p></div>`;
  if (!current) {
    return html`<div class="fs-body" data-setup-step="share">${intro}
      <p class="fs-ok">✓ Every peer you trust is unrestricted: it already holds every peer permission on all groups.</p></div>`;
  }
  if (!groups.length) return html`<div class="fs-body" data-setup-step="share">${intro}<p class="fs-warn">This node has no groups yet. Create one in Groups, then share it here.</p></div>`;
  const rows = grants ? grantRows(grants, { groups }) : [];
  const pending = grants ? shareGrants({ peer: current.id, group: currentGroup, presets, rows }) : [];
  const toggle = (id) => setPresets(presets.includes(id) ? presets.filter((p) => p !== id) : [...presets, id]);
  const share = () => {
    if (!pending.length) return;
    const lines = pending.map((g) => grantConsequence({ target: current.label, slug: g.slug, group: currentGroup, maxLive: g.spawn_policy?.max_live }));
    const sensitive = pending.some((g) => g.slug === 'groups.members.spawn');
    confirm({
      title: `Share ${currentGroup} with ${current.label}?`,
      body: `${lines.join(' ')}${sensitive ? ' Spawning lets it start agents on this node, not only read.' : ''}`,
      okLabel: 'Share',
      busyLabel: 'Sharing…',
      action: async () => {
        const granted = [];
        for (const body of pending) {
          try {
            await actions.grant(body);
            granted.push(body.slug);
          } catch (e) {
            throw new Error(`${granted.length ? `Granted ${granted.join(', ')}; ` : ''}${body.slug} failed: ${errText(e)}`);
          }
        }
        return granted;
      },
    }).then((granted) => {
      if (!granted) return;
      toast(`Shared ${currentGroup} with ${current.label}: ${granted.join(', ')}`, false);
      setTick((n) => n + 1);
      reload();
    }).catch((e) => { toast(`Share failed: ${errText(e)}`, true); setTick((n) => n + 1); reload(); });
  };
  return html`<div class="fs-body" data-setup-step="share">${intro}
    <div class="fs-share">
      <label><span class="fa-k">peer</span><select id="fleet-setup-peer" value=${current.id} onChange=${(e) => setPeer(e.currentTarget.value)}>
        ${restricted.map((r) => html`<option key=${r.id} value=${r.id}>${r.label}</option>`)}</select></label>
      <label><span class="fa-k">group</span><select id="fleet-setup-group" value=${currentGroup} onChange=${(e) => setGroup(e.currentTarget.value)}>
        ${groups.map((g) => html`<option key=${g} value=${g}>${g}</option>`)}</select></label>
    </div>
    <div class="fs-presets" role="group" aria-label="What the peer may do">
      ${SHARE_PRESETS.map((p) => {
        const held = grants && presetHeld(p, rows, currentGroup);
        return html`<label key=${p.id} class=${`fs-preset${presets.includes(p.id) ? ' on' : ''}${held ? ' held' : ''}${p.sensitive ? ' sensitive' : ''}`} data-preset=${p.id}>
          <input type="checkbox" checked=${presets.includes(p.id) || held} disabled=${held} onChange=${() => toggle(p.id)} />
          <span><b>${p.label}</b>${held ? html` <span class="muted">· already shared</span>` : ''}<br /><span class="muted">${p.what}</span></span>
        </label>`;
      })}
    </div>
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="fs-actions">
      <button id="fleet-setup-share" type="button" class="primary" disabled=${!grants || !pending.length} onClick=${share}>Share…</button>
      <span class="muted">${!grants ? 'Loading its grants…' : pending.length ? `Grants ${pending.map((g) => g.slug).join(', ')} in ${currentGroup}.` : 'Nothing new to grant: pick what to share.'}</span>
    </div>
    <p class="muted">Every permission, other scopes and spawn launch settings: <button type="button" class="fa-link" onClick=${() => onOpenGrants(current.id)}>Peer grants…</button>${view.trusted.some((r) => r.level === 'unrestricted') ? ' Unrestricted peers already hold everything and are not listed.' : ''}</p>
    <${Cli}>${pending.length
      ? pending.map((g, i) => html`${i ? html`<br />` : ''}<code>tclaude federation grant ${current.label} ${g.slug} --scope group=${currentGroup}${g.spawn_policy ? ` --max-live ${g.spawn_policy.max_live}` : ''}</code>`)
      : html`<code>tclaude federation grant <i>peer</i> <i>permission</i> --scope group=<i>group</i></code>`}<//>
  </div>`;
}

function AgentsStep({ view, copy, toast }) {
  const peers = view.trusted;
  return html`<div class="fs-body" data-setup-step="agents">
    <h3>Let your agents use peers</h3>
    <p>Below is what each peer shares with this node. Your agents need their own permission for it: local grants never reach another node, so a grant names the peer and its group as <code>peer=<i>peer</i>/<i>group</i></code>. Towards an unrestricted peer, your agents' ordinary grants count too. You can always reach a peer's agents yourself, from Messages or the node row.</p>
    ${!peers.length && html`<p class="fs-warn">Trust a peer first (step 2).</p>`}
    ${peers.map((r) => html`<div class="fs-peer" key=${r.id} data-peer=${r.id}>
      <div class="fs-peer-head"><span class=${`fa-dot${r.online ? ' on' : ''}`}></span> <b>${r.label}</b> <span class=${`fa-level ${r.level}`}>${r.level}</span></div>
      ${!r.catalog.length
        ? html`<div class="muted">Shares nothing with this node yet; its operator grants that on their node (their step 3).</div>`
        : html`<table class="fa-table">
          <thead><tr><th>Its group</th><th>Shared with you</th><th>Give an agent access</th></tr></thead>
          <tbody>${r.catalog.map((g) => {
            const slugs = agentSlugs(g.caps);
            return html`<tr key=${g.name} data-remote-group=${g.name}>
              <td><b>${g.name}</b></td>
              <td>${capWords(g.caps).join(', ') || html`<span class="muted">visible only</span>`}</td>
              <td>${slugs.length && r.level !== 'unrestricted'
                ? slugs.map((slug, i) => html`${i ? html`<br />` : ''}<${Copyable} value=${`tclaude agent permissions grant AGENT ${slug} --scope peer=${r.label}/${g.name}`} what="Command" copy=${copy} toast=${toast} />`)
                : html`<span class="muted">${r.level === 'unrestricted' ? 'your agents\' own grants apply' : 'nothing for agents to use'}</span>`}</td>
            </tr>`;
          })}</tbody>
        </table>`}
    </div>`)}
    <p class="muted">An agent's or group's permission editor takes the same <code>peer=</code> scope. A grant on a local group reaches every member. Agents then find remote members with <code>tclaude agent ls --remote</code>.</p>
    <${Cli}><code>tclaude federation remote</code>, <code>tclaude agent permissions grant <i>agent</i> message.direct --scope peer=<i>peer</i>/<i>group</i></code><//>
  </div>`;
}

// SetupGuide is the Fleet → Setup guide page. step is the step shown; it
// starts on the first one not done.
// onSeen runs once it is on screen, which retires the Fleet chip's "set up"
// hint.
export function SetupGuide({ view, groups = [], actions, confirm, toast, copy, reload, onTrust, onSettings, onPeers, onOpenGrants, onSeen = null }) {
  const progress = setupProgress(view);
  const [step, setStep] = useState(progress.current);
  useEffect(() => { onSeen?.(); }, []);
  const shown = progress.steps.find((s) => s.id === step) || progress.steps[0];
  const next = shown.done ? progress.steps[progress.steps.indexOf(shown) + 1] : null;
  return html`<div class="fleet-setup" id="fleet-setup">
    <ol class="fs-steps" aria-label="Setup steps">
      ${progress.steps.map((s, i) => html`<li key=${s.id}>
        <button type="button" class=${`fs-step${s.done ? ' done' : ''}${s.id === shown.id ? ' on' : ''}`} data-step=${s.id} aria-current=${s.id === shown.id ? 'step' : undefined} onClick=${() => setStep(s.id)}>
          <span class="fs-n" aria-hidden="true">${s.done ? '✓' : i + 1}</span>
          <span>${s.title}${s.optional ? html` <span class="muted">(optional)</span>` : ''}</span>
        </button>
      </li>`)}
      <li class="fs-progress muted">${progress.complete ? 'Set up. Come back here to link more nodes.' : `${progress.done} of ${progress.total} done`}</li>
    </ol>
    ${shown.id === 'hub' ? html`<${HubStep} view=${view} actions=${actions} confirm=${confirm} toast=${toast} copy=${copy} reload=${reload} onSettings=${onSettings} />`
      : shown.id === 'trust' ? html`<${TrustStep} view=${view} copy=${copy} toast=${toast} onTrust=${onTrust} onPeers=${onPeers} />`
      : shown.id === 'share' ? html`<${ShareStep} view=${view} groups=${groups} actions=${actions} confirm=${confirm} toast=${toast} reload=${reload} onOpenGrants=${onOpenGrants} />`
      : html`<${AgentsStep} view=${view} copy=${copy} toast=${toast} />`}
    ${next && html`<div class="fs-next"><button type="button" data-setup-next=${next.id} onClick=${() => setStep(next.id)}>Next: ${next.title} →</button></div>`}
  </div>`;
}
