// fused-mail.js — the Messages tab in a fused node scope (tcl-1glyla): the
// ticked nodes' mail in the Messages layout, every row labelled with its node
// and every action sent back to that node. Single-node Messages (mail.js) is
// untouched; CSS shows this view instead of it while fused.
//
// Reads follow the fused polling budget (skynet-merged-island.js): only while
// the view is on screen and the page visible, staggered, one read in flight
// per node and at most four overall. A failed read keeps what the node last
// showed, marked with its age; a node that withdraws trust keeps nothing.
// Everything a node returns is rendered as text.
import { Fragment, h, render } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { dashboardState } from './snapshot-store.js';
import { shellConfirm, shellToast } from './shell-state.js';
import { fmtAge, nodeHref, pollDelay, remoteNodeID, staggerOffset } from './skynet-model.js';
import { tickedNodes, withFused } from './skynet-scope.js';
import { approveConsequence } from './remote-inbox.js';
import { replySubject } from './peer-mail.js';
import {
  FUSED_FOLDERS, PAGE_SIZE, MAX_PAGE_SIZE, apiBase, canActOnHuman, folderURL, mergeMessages, mergeRequests,
  nodeFolders, ownerRoutes, readFailure, replyTarget,
} from './fused-mail-model.js';

const html = htm.bind(h);

export const MAIL_POLL_MS = 10000;
const ROSTER_EVERY = 3; // mailboxes refresh every third page read
const MAX_IN_FLIGHT = 4;

function errText(e) { return e?.message || String(e); }
function when(s) { const t = Date.parse(s || ''); return Number.isFinite(t) ? new Date(t).toLocaleString() : ''; }

async function readJSON(fetchFn, url, init) {
  const res = await fetchFn(url, { credentials: 'same-origin', cache: 'no-store', ...init });
  let data = null;
  try { data = await res.json(); } catch (_) { data = null; }
  if (!res.ok) { const e = new Error(data?.error || `HTTP ${res.status}`); e.status = res.status; e.body = data; throw e; }
  return data;
}

// createFusedMailActions posts an action to the node that owns the message.
export function createFusedMailActions({ fetchImpl = (...a) => globalThis.fetch(...a), localFetch = globalThis.__tclaudeRemoteNode?.localFetch || fetchImpl } = {}) {
  const post = (node, url, body) => readJSON(node.local ? localFetch : fetchImpl, url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  return Object.freeze({
    markAgent: (node, id, read) => post(node, ownerRoutes(node).markRead, { ids: [id], read }),
    reply: (node, to, subject, body) => post(node, ownerRoutes(node).message, { to, subject, body }),
    humanRead: (node, id) => post(node, ownerRoutes(node).humanRead, { id }),
    humanReply: (node, id, body) => post(node, ownerRoutes(node).humanReply, { id, body }),
    decide: (node, id, decision) => post(node, ownerRoutes(node).decide(id), { decision: decision === 'approve' ? 'approve' : 'deny' }),
  });
}

// openOnNode shows a node's own Messages, where everything this view cannot
// do from here lives: it leaves the fused view.
function defaultOpenOnNode(node) {
  globalThis.location.assign(nodeHref(node.local ? '' : node.id, { pathname: '/messages', search: withFused(globalThis.location.search, null) }));
}

function NodePill({ node }) {
  return html`<span class="scope-pill" style=${`--nc:${node.color}`}>${node.local ? '⌂ ' : ''}${node.name}</span>`;
}

// useFusedMail reads what the selected folder needs from each node.
function useFusedMail({ active, nodes, folder, q, size, fetchImpl, localFetch, timers, now, snapshot, remote, reload }) {
  const [entries, setEntries] = useState({});
  const ref = useRef(entries);
  ref.current = entries;
  const nodesKey = nodes.map((n) => `${n.id}:${n.online}:${n.level}`).join(',');
  // sig names what a page shows; a page read for another folder is never shown.
  const sig = `${folder.scope}:${folder.node || ''}:${folder.id}:${q}:${size}`;
  useEffect(() => {
    if (!active || !nodes.length) return undefined;
    let disposed = false;
    const pending = new Map();
    const inflight = new Set();
    const commit = (id, patch) => { if (!disposed) setEntries((cur) => ({ ...cur, [id]: { ...cur[id], ...patch } })); };
    const schedule = (node, delay) => { pending.set(node.id, timers.setTimeout(() => tick(node), delay)); };
    // Which nodes this folder reads: a node folder reads its node; the fused
    // access folder reads the nodes whose requests this operator may answer.
    const readers = nodes.filter((n) => (folder.scope === 'node' ? n.id === folder.node : folder.id !== 'access' || (!n.local && canActOnHuman(n))));
    let ticks = 0;
    async function tick(node) {
      if (disposed || inflight.has(node.id)) return;
      if (globalThis.document?.hidden) { schedule(node, pollDelay({ base: MAIL_POLL_MS })); return; }
      if (inflight.size >= MAX_IN_FLIGHT) { schedule(node, 250 + Math.round(250 * Math.random())); return; }
      if (!node.local && node.online === false) { commit(node.id, { status: { kind: 'offline', label: 'offline' } }); schedule(node, pollDelay({ base: MAIL_POLL_MS * 2 })); return; }
      inflight.add(node.id);
      const fetchFn = node.local ? localFetch : fetchImpl;
      let failed = false;
      try {
        const mode = !readers.includes(node) ? 'roster' : folder.scope === 'fused' && folder.id === 'access' ? 'requests' : 'page';
        if (mode === 'requests') {
          const data = await readJSON(fetchFn, `${apiBase(node)}human-inbox`);
          commit(node.id, { requests: Array.isArray(data?.access_requests) ? data.access_requests : [], status: null, at: now() });
        } else {
          if (mode === 'roster' || ticks % ROSTER_EVERY === 0 || !ref.current[node.id]?.mailboxes) {
            const roster = await readJSON(fetchFn, `${apiBase(node)}mailboxes`);
            commit(node.id, { mailboxes: Array.isArray(roster?.mailboxes) ? roster.mailboxes : [], ...(mode === 'roster' ? { status: null, at: now() } : {}) });
            // An unrestricted peer's access requests keep the sidebar count
            // current in every folder.
            if (!node.local && canActOnHuman(node)) {
              const inbox = await readJSON(fetchFn, `${apiBase(node)}human-inbox`);
              commit(node.id, { requests: Array.isArray(inbox?.access_requests) ? inbox.access_requests : [] });
            }
          }
          if (mode === 'page') {
            const page = await readJSON(fetchFn, folderURL(node, folder.id, { q, size }));
            commit(node.id, { page: { sig, messages: Array.isArray(page?.messages) ? page.messages : [], total: page?.total || 0 }, status: null, at: now() });
          }
        }
      } catch (e) {
        failed = true;
        const f = readFailure(e.status || 0, e.body);
        commit(node.id, f.kind === 'gone' || f.kind === 'not_shared' ? { status: f, mailboxes: [], page: null, requests: [] } : { status: f });
      } finally {
        inflight.delete(node.id);
      }
      ticks += 1;
      if (!disposed) schedule(node, pollDelay({ base: readers.includes(node) ? MAIL_POLL_MS : MAIL_POLL_MS * ROSTER_EVERY, failures: failed ? 1 : 0 }));
    }
    // Every ticked node's roster feeds the sidebar, whatever the folder.
    const all = [...readers, ...nodes.filter((n) => !readers.includes(n))];
    all.forEach((node, i) => schedule(node, staggerOffset(i, all.length, 600)));
    return () => { disposed = true; pending.forEach((t) => timers.clearTimeout(t)); };
  }, [active, nodesKey, sig, reload]);
  // This node's pending access requests come with the page's own snapshot.
  const local = nodes.find((n) => n.local);
  const localRequests = !remote && local ? (snapshot.value?.access_requests || []) : [];
  return { entries, sig, localRequests };
}

function Reader({ item, actions, confirm, toast, openOnNode, onDone }) {
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);
  useEffect(() => { setText(''); setBusy(false); }, [item?.key]);
  if (!item) return html`<section class="mail-reader fused-mail-reader" id="fused-mail-reader"><div class="empty">Select a message.</div></section>`;
  const { node } = item;
  const run = (what, p, done) => { setBusy(true); p.then(() => { setBusy(false); setText(''); toast(done, false); onDone(); }, (e) => {
    setBusy(false);
    toast(e.status === 403 ? `${what}: ${node.name} does not allow it from here (${errText(e)}). Open it on ${node.name}.` : `${what} failed: ${errText(e)}`, true);
  }); };
  const open = html`<button type="button" class="tool" data-fused-open=${node.id} title=${`Show ${node.name}'s own Messages (leaves the fused view)`} onClick=${() => openOnNode(node)}>Open on ${node.name}</button>`;
  let body;
  let buttons;
  if (item.kind === 'access') {
    const human = canActOnHuman(node);
    body = html`<div class="fused-mail-head"><${NodePill} node=${node} /> <b>🔐 ${item.perm}</b></div>
      <div class="muted">${item.title || item.agent_id || 'an agent'} is waiting${item.deadline ? ` · auto-declines ${new Date(item.deadline).toLocaleTimeString()}` : ''}</div>
      ${item.path && html`<div><code>${item.path}</code></div>`}
      ${item.body && html`<pre class="remote-inbox-body">${item.body}</pre>`}`;
    buttons = human ? html`
      <button type="button" data-fused="deny" disabled=${busy} onClick=${() => run('Deny', actions.decide(node, item.id, 'deny'), `Denied on ${node.name}`)}>Deny</button>
      <button type="button" class="primary" data-fused="approve" disabled=${busy} onClick=${() => confirm({
        title: `Approve ${item.perm} on ${node.name}?`, body: approveConsequence(item, node.name), okLabel: 'Approve once', busyLabel: 'Approving…',
        action: () => actions.decide(node, item.id, 'approve'),
      }).then((r) => { if (r) { toast(`Approved once on ${node.name}`, false); onDone(); } }, (e) => toast(`Approve failed: ${errText(e)}`, true))}>Approve once…</button>`
      : html`<span class="muted">Answer this on ${node.name}'s own dashboard.</span>`;
  } else {
    const target = replyTarget(item);
    const human = item.kind === 'human';
    const canHuman = human && canActOnHuman(node);
    body = html`<div class="fused-mail-head"><${NodePill} node=${node} /> <b>${item.subject || '(no subject)'}</b></div>
      <div class="muted">from ${item.from_title || item.from_agent || 'operator'}${item.to_title || item.to_agent ? ` → ${item.to_title || item.to_agent}` : ''}${item.group ? ` · ${item.group}` : ''} · ${when(item.created_at)}</div>
      <pre class="remote-inbox-body">${item.body}</pre>
      ${(item.attachments?.length > 0 || item.attachment) && html`<div class="muted">Attachments stay on ${node.name}.</div>`}
      ${((canHuman && target) || (!human && target)) && html`<textarea class="fused-mail-reply" rows="3" placeholder=${`Reply to ${item.from_title || target} on ${node.name}`} value=${text} onInput=${(e) => setText(e.currentTarget.value)}></textarea>`}`;
    buttons = human
      ? (canHuman ? html`
        ${!item.read && html`<button type="button" data-fused="read" disabled=${busy} onClick=${() => run('Mark read', actions.humanRead(node, item.id), `Marked read on ${node.name}`)}>Mark read</button>`}
        ${target && html`<button type="button" class="primary" data-fused="reply" disabled=${busy || !text.trim()} onClick=${() => run('Reply', actions.humanReply(node, item.id, text.trim()), `Replied on ${node.name}`)}>Send reply</button>`}`
        : html`<span class="muted">${node.name} shares its notifications for reading; answer them on its own dashboard.</span>`)
      : html`
        <button type="button" data-fused="mark" disabled=${busy} onClick=${() => run(item.read ? 'Mark unread' : 'Mark read', actions.markAgent(node, item.id, !item.read), `Marked ${item.read ? 'unread' : 'read'} on ${node.name}`)}>${item.read ? 'Mark unread' : 'Mark read'}</button>
        ${target && html`<button type="button" class="primary" data-fused="reply" disabled=${busy || !text.trim()} onClick=${() => run('Reply', actions.reply(node, target, replySubject(item.subject), text.trim()), `Sent to ${item.from_title || target} on ${node.name}`)}>Send reply</button>`}`;
  }
  return html`<section class="mail-reader fused-mail-reader" id="fused-mail-reader">
    ${body}
    <div class="modal-buttons fused-mail-actions">${buttons}<span class="spacer"></span>${open}</div>
  </section>`;
}

// FusedMail is the fused Messages view.
export function FusedMail({
  state, snapshot = dashboardState.snapshot, fetchImpl = (...a) => globalThis.fetch(...a),
  localFetch = globalThis.__tclaudeRemoteNode?.localFetch || fetchImpl, timers = globalThis, now = () => Date.now(),
  remote = remoteNodeID(), actions = null, confirm = shellConfirm, toast = shellToast, openOnNode = defaultOpenOnNode,
}) {
  const current = state.view.value;
  const fleet = current.fleet;
  const active = current.activeTab === 'messages' && !!current.fused && !!fleet;
  const nodes = fleet && current.fused ? tickedNodes(fleet, current.fused) : [];
  const [folder, setFolder] = useState({ scope: 'fused', id: 'all' });
  const [query, setQuery] = useState('');
  const [q, setQ] = useState('');
  const [size, setSize] = useState(PAGE_SIZE);
  const [selected, setSelected] = useState('');
  const [reload, setReload] = useState(0);
  const acts = useRef(actions);
  if (!acts.current) acts.current = createFusedMailActions({ fetchImpl, localFetch });
  useEffect(() => { const t = timers.setTimeout(() => setQ(query.trim()), 300); return () => timers.clearTimeout(t); }, [query]);
  useEffect(() => { setSize(PAGE_SIZE); setSelected(''); }, [folder.scope, folder.node, folder.id, q]);
  // A node folder whose node is no longer ticked falls back to All.
  useEffect(() => { if (folder.scope === 'node' && !nodes.some((n) => n.id === folder.node)) setFolder({ scope: 'fused', id: 'all' }); }, [nodes.map((n) => n.id).join(',')]);
  const { entries, sig, localRequests } = useFusedMail({ active, nodes, folder, q, size, fetchImpl, localFetch, timers, now, snapshot, remote, reload });
  if (!current.fused) return null;
  if (!fleet) return html`<div class="empty">Loading the fused nodes…</div>`;
  const t = now();
  const kindOf = folder.id === 'human' ? 'human' : 'agent';
  const shownNodes = folder.scope === 'node' ? nodes.filter((n) => n.id === folder.node) : nodes;
  const requestRows = mergeRequests(nodes.map((node) => ({ node, requests: node.local ? localRequests : canActOnHuman(node) ? entries[node.id]?.requests : [] })));
  const rows = folder.id === 'access' && folder.scope === 'fused'
    ? requestRows
    : mergeMessages(shownNodes.map((node) => ({ node, kind: kindOf, messages: entries[node.id]?.page?.sig === sig ? entries[node.id].page.messages : [] })));
  const item = rows.find((r) => r.key === selected) || null;
  const more = folder.id !== 'access' && size < MAX_PAGE_SIZE && shownNodes.some((n) => entries[n.id]?.page?.sig === sig && (entries[n.id].page.total || 0) > size);
  const status = (n) => {
    const e = entries[n.id];
    if (e?.status) return `${e.status.label}${e.at && e.status.kind === 'offline' ? ` · data ${fmtAge(Math.max(0, t - e.at))} old` : ''}`;
    if (folder.id === 'access' && folder.scope === 'fused' && n.local && remote) return 'its requests show on its own page';
    if (folder.id === 'access' && folder.scope === 'fused' && !n.local && !canActOnHuman(n)) return 'requests answered on its own dashboard';
    return e?.at ? '' : 'loading…';
  };
  const pick = (f) => { setFolder(f); };
  const isOn = (f) => folder.scope === f.scope && folder.id === f.id && (folder.node || '') === (f.node || '');
  const box = (f, icon, title, count) => html`<div class="mailbox-row"><span class="mail-box-check-spacer"></span><button type="button"
    class=${`mailbox${isOn(f) ? ' active' : ''}${count ? ' has-unread' : ''}`} aria-current=${isOn(f) ? 'true' : undefined} onClick=${() => pick(f)}>
    <span class="mailbox-icon">${icon}</span><span class="mailbox-name">${title}</span>${count ? html`<span class="mailbox-unread">${count > 99 ? '99+' : count}</span>` : ''}</button></div>`;
  return html`<div class="mail-client fused-mail">
    <input type="text" class="mail-sidebar-filter" placeholder="Fused: the ticked nodes' mail" disabled />
    <div class="mail-list-filter">
      <input type="text" id="fused-mail-filter" placeholder="Filter messages on every ticked node" autocomplete="off" spellcheck=${false}
        value=${query} onInput=${(e) => setQuery(e.currentTarget.value)} />
      <span class="filter-count">${rows.length}</span>
    </div>
    <div class="mail-col mail-sidebar-col">
      <aside class="mail-sidebar" id="fused-mail-sidebar">
        <div class="fused-mail-nodes">${nodes.map((n) => html`<div key=${n.id} class="fused-mail-node" data-node=${n.id}><${NodePill} node=${n} />${status(n) && html` <span class="muted">${status(n)}</span>`}</div>`)}</div>
        ${FUSED_FOLDERS.map((f) => html`<${Fragment} key=${f.id}>${box({ scope: 'fused', id: f.id }, f.icon, f.title, f.id === 'access' ? requestRows.length : 0)}</${Fragment}>`)}
        ${nodes.map((n) => {
          const { groups, agents } = nodeFolders(entries[n.id]?.mailboxes);
          if (!groups.length && !agents.length) return null;
          return html`<div key=${n.id} class="fused-mail-section"><div class="mailbox-section"><${NodePill} node=${n} /></div>
            ${box({ scope: 'node', node: n.id, id: 'human' }, '📬', 'Human notifications', 0)}
            ${[...groups, ...agents].map((m) => html`<${Fragment} key=${m.id}>${box({ scope: 'node', node: n.id, id: m.id }, m.kind === 'group' ? '👥' : '●', m.title, m.unread)}</${Fragment}>`)}
          </div>`;
        })}
      </aside>
    </div>
    <div class="mail-col mail-list-col">
      <div class="mail-list" id="fused-mail-list" role="list">
        ${!rows.length && html`<div class="empty">${shownNodes.every((n) => entries[n.id]?.status || (folder.id === 'access' && folder.scope === 'fused' ? entries[n.id]?.at || n.local : entries[n.id]?.page?.sig === sig)) ? 'No messages here.' : 'Loading…'}</div>`}
        ${rows.map((m) => html`<div class="mail-row-wrap" key=${m.key} data-key=${m.key} role="listitem">
          <button type="button" class=${`mail-row${m.key === selected ? ' active' : ''}${m.kind !== 'access' && !m.read ? ' unread' : ''}${m.kind === 'access' ? ' unread' : ''}`}
            aria-current=${m.key === selected ? 'true' : undefined} onClick=${() => setSelected(m.key)}>
            <span class="mail-row-top"><${NodePill} node=${m.node} />
              <span class="mail-row-party">${m.kind === 'access' ? (m.title || m.agent_id || 'agent') : (m.from_title || m.from_agent || 'operator')}</span>
              ${m.kind === 'agent' && (m.to_title || m.to_agent) && html`<span class="mail-row-arrow">→</span><span class="mail-row-party">${m.to_title || m.to_agent}</span>`}
              ${m.group && html`<span class="mail-row-group">${m.group}</span>`}
              <span class="mail-row-time" title=${when(m.created_at)}>${when(m.created_at)}</span></span>
            <span class="mail-row-subject">${m.kind === 'access' ? `🔐 ${m.perm}` : (m.subject || String(m.body || '').slice(0, 120))}</span>
          </button></div>`)}
        ${more && html`<button type="button" class="tool fused-mail-more" onClick=${() => setSize(Math.min(MAX_PAGE_SIZE, size + PAGE_SIZE))}>Load older</button>`}
      </div>
    </div>
    <${Reader} item=${item} actions=${acts.current} confirm=${confirm} toast=${toast} openOnNode=${openOnNode} onDone=${() => setReload((r) => r + 1)} />
  </div>`;
}

export function mountFusedMailIsland({ host, state, registerCleanup }) {
  render(html`<${FusedMail} state=${state} />`, host);
  registerCleanup(() => render(null, host));
}
