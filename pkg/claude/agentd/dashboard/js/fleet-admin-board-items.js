import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { BundleInspectDialog } from './fleet-admin-bundle.js';

const html = htm.bind(h);

// Board items: signed, versioned config (roles, profiles, templates, …) that
// members post to a board. Opening one fetches and verifies it on this node
// and lets the operator read it; importing previews the changes to this
// node's config first and applies only on an explicit confirm. Nothing on a
// board is ever applied automatically.

// POSTABLE are the config sections a board carries (never broad config or
// default permissions).
export const POSTABLE = Object.freeze(['roles', 'profiles', 'sandbox-profiles', 'templates', 'process-templates']);

export const canPost = (board) => board?.role === 'owner' || board?.role === 'publisher';

function errText(error) { return error?.message || String(error); }

function size(n) {
  if (!(n >= 0)) return '—';
  return n < 1024 ? `${n} B` : n < 1 << 20 ? `${(n / 1024).toFixed(1)} KiB` : `${(n / (1 << 20)).toFixed(1)} MiB`;
}

const short = (v) => String(v || '').slice(0, 10);

// postedBy names who signed an item, and who re-posted it here if that is
// someone else.
export function postedBy(item, name) {
  const by = name(item.publisher) || item.publisher || 'unknown';
  return item.republisher && item.republisher !== item.publisher ? `${by}, re-posted by ${name(item.republisher) || item.republisher}` : by;
}

// selection turns the publish form into the route's only list: the ticked
// whole sections plus the named items (section/name) outside them.
export function selection(sections, named) {
  const picked = String(named || '').split(/[\s,]+/).map((s) => s.trim()).filter(Boolean);
  return [...sections, ...picked.filter((p) => !sections.includes(p.split('/')[0]))];
}

// importConsequence spells out what importing a preview does here.
export function importConsequence(item, board, preview, skip, by) {
  const live = (preview?.changes || []).filter((c) => c.action !== 'unchanged' && !skip.includes(c.item));
  const replaces = live.filter((c) => c.action === 'replace').map((c) => c.item);
  const security = live.filter((c) => c.security).map((c) => c.item);
  return `Applies ${live.length} item${live.length === 1 ? '' : 's'} from ${item.name} (board ${board.name}, posted by ${by}) to this node's configuration; they take effect immediately: `
    + `${live.length - replaces.length} new${replaces.length ? `, ${replaces.length} overwritten (${replaces.join(', ')})` : ''}.`
    + `${security.length ? ` Security-relevant: ${security.join(', ')} — these decide what this node's agents may do.` : ''}`;
}

// parentSelection reads what a version shares, as section/name selectors,
// from its verified contents (sections/<section>.json lists the items), so a
// new version starts from the same selection. Unknown → [].
export async function parentSelection(actions, board, item, version) {
  try {
    await actions.fetchBoardItem(board, item, version);
    const entries = (await actions.boardItemContents(board, item, version))?.entries || [];
    const out = [];
    for (const e of entries) {
      const m = /^sections\/([a-z-]+)\.json$/.exec(e?.path || '');
      if (!m || !POSTABLE.includes(m[1])) continue;
      const r = await actions.boardItemEntry(board, item, version, e.path, 0, 1 << 20);
      if (r?.truncated) return [];
      for (const it of JSON.parse(String(r?.text || '[]'))) if (it?.name) out.push(`${m[1]}/${it.name}`);
    }
    return out;
  } catch (_) {
    return [];
  }
}

// PublishDialog posts config to a board, as a new item or a new version of
// one (item + parent).
function PublishDialog({ board, item, actions, confirm, onClose, onDone }) {
  const [name, setName] = useState(item?.name || '');
  const [sections, setSections] = useState([]);
  const [named, setNamed] = useState('');
  const [error, setError] = useState('');
  const [prefilled, setPrefilled] = useState('');
  // A new version starts from what the previous version shared.
  useEffect(() => {
    if (!item?.latest_version) return undefined;
    let off = false;
    parentSelection(actions, board.id, item.id, item.latest_version).then((sel) => {
      if (off || !sel.length) return;
      setNamed((cur) => {
        if (cur) return cur;
        setPrefilled(item.latest_version);
        return sel.join(', ');
      });
    });
    return () => { off = true; };
  }, []);
  const only = selection(sections, named);
  const ready = name.trim() && only.length > 0;
  const post = () => {
    setError('');
    confirm({
      title: item ? `Post a new version of ${item.name}?` : `Post to ${board.name}?`,
      body: `Shares ${only.join(', ')} from this node's config with every member of ${board.name}, signed by this node. Members can read it and import it into their own config; nothing changes on their nodes until they do. Anything that looks like a credential is refused, but free text (role prompts, templates) is shared as written.`,
      okLabel: 'Post',
      busyLabel: 'Posting…',
      action: () => actions.publishBoardItem(board.id, { name: name.trim(), only, ...(item ? { item: item.id, parent: item.latest_version } : {}) }),
    }).then((r) => { if (r) onDone(`Posted ${name.trim()} to ${board.name}`); }).catch((e) => setError(errText(e)));
  };
  return html`<${Overlay} id="fleet-board-publish" labelledby="fleet-board-publish-title" onClose=${onClose}>
    <h3 id="fleet-board-publish-title">${item ? `New version of ${item.name}` : `Post config to ${board.name}`}</h3>
    <label class="fa-of-row"><span class="fa-k">name</span><input id="fleet-board-publish-name" value=${name} disabled=${!!item} placeholder="what members see, e.g. review roles" onInput=${(e) => setName(e.currentTarget.value)} /></label>
    <div class="fa-cli-note">Share these parts of this node's config:</div>
    <div class="fa-grant-form">${POSTABLE.map((s) => html`<label key=${s}><input type="checkbox" data-section=${s} checked=${sections.includes(s)}
      onChange=${(e) => setSections(e.currentTarget.checked ? [...sections, s] : sections.filter((x) => x !== s))} /> ${s}</label>`)}</div>
    <label class="fa-of-row"><span class="fa-k">or only</span><input id="fleet-board-publish-only" value=${named} placeholder="roles/reviewer, profiles/fast" autocomplete="off" spellcheck="false" onInput=${(e) => setNamed(e.currentTarget.value)} /></label>
    ${prefilled && html`<div class="muted" id="fleet-board-publish-prefilled">Prefilled with the items version ${short(prefilled)} shares; this node's current config for them is posted. To share a whole section again, including items added since, tick it.</div>`}
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation boards publish --board ${board.id} --name … --only roles/NAME${item ? ` --item ${item.id} --parent ${item.latest_version}` : ''}</code></div>
    <div class="modal-buttons"><span class="spacer"></span>
      <button type="button" onClick=${onClose}>Cancel</button>
      <button type="button" class="primary" id="fleet-board-publish-post" disabled=${!ready} onClick=${post}>Post…</button></div>
  </${Overlay}>`;
}

// repostTargets are the other boards this node may post to.
export function repostTargets(board, boards) {
  return (boards || []).filter((b) => b.id !== board.id && canPost(b) && !b.frozen);
}

// RepostDialog posts one version, exactly as its author signed it, to another
// board (tclaude federation boards publish --from-board …).
function RepostDialog({ board, boards, item, version, by, actions, confirm, onClose, onDone }) {
  const targets = repostTargets(board, boards);
  const [dest, setDest] = useState(targets.length === 1 ? targets[0].id : '');
  const [error, setError] = useState('');
  const to = targets.find((b) => b.id === dest);
  const post = () => {
    setError('');
    confirm({
      title: `Post ${item.name} to ${to.name}?`,
      body: `Posts version ${short(version)} of ${item.name} to ${to.name} exactly as it is here: unchanged, still signed by ${by}, with this node listed as re-posting it. Every member of ${to.name} can read it and import it into their own config; nothing changes on their nodes until they do.`,
      okLabel: 'Post',
      busyLabel: 'Posting…',
      action: () => actions.publishBoardItem(to.id, { source: { board: board.id, item: item.id, version } }),
    }).then((r) => { if (r) onDone(`Posted ${item.name} to ${to.name}`); }).catch((e) => setError(errText(e)));
  };
  return html`<${Overlay} id="fleet-board-repost" labelledby="fleet-board-repost-title" onClose=${onClose}>
    <h3 id="fleet-board-repost-title">Post ${item.name} to another board</h3>
    <div class="muted">Version ${short(version)}, from ${board.name}, by ${by}. Posted unchanged; to change it, post your own config instead.</div>
    <label class="fa-of-row"><span class="fa-k">board</span><select id="fleet-board-repost-dest" value=${dest} onChange=${(e) => setDest(e.currentTarget.value)}>
      <option value="">pick a board…</option>
      ${targets.map((b) => html`<option key=${b.id} value=${b.id} selected=${dest === b.id}>${b.name}</option>`)}</select></label>
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation boards publish --board ${dest || 'DEST'} --from-board ${board.id} --from-item ${item.id} --from-version ${version}</code></div>
    <div class="modal-buttons"><span class="spacer"></span>
      <button type="button" onClick=${onClose}>Cancel</button>
      <button type="button" class="primary" id="fleet-board-repost-post" disabled=${!to} onClick=${post}>Post…</button></div>
  </${Overlay}>`;
}

// ImportDialog previews an item version against this node's config and
// imports it with the preview's token and the same choices.
function ImportDialog({ board, item, version, by, actions, confirm, onClose, onDone }) {
  const [opts, setOpts] = useState({ skip: [], values: {}, replace: false });
  const [preview, setPreview] = useState(null);
  // seen keeps every change and placeholder any preview showed: a re-preview
  // leaves skipped items out, and they must stay on screen to tick again.
  const [seen, setSeen] = useState({ changes: [], unresolved: [] });
  const [stale, setStale] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const seq = useRef(0);
  const choices = (o) => {
    const values = Object.fromEntries(Object.entries(o.values).filter(([, v]) => String(v).trim() !== ''));
    return { ...(o.skip.length ? { skip: [...o.skip] } : {}), ...(Object.keys(values).length ? { values } : {}), ...(o.replace ? { replace: true } : {}) };
  };
  const run = (o = opts) => {
    const mine = ++seq.current;
    setBusy(true); setError('');
    actions.previewBoardItem(board.id, item.id, version, choices(o))
      .then((p) => {
        if (mine !== seq.current) return;
        setPreview(p); setStale(false);
        setSeen((s) => {
          const merge = (old, now, key) => { const by = new Map(old.map((x) => [x[key], x])); for (const x of now || []) by.set(x[key], x); return [...by.values()]; };
          return { changes: merge(s.changes, p?.changes, 'item'), unresolved: merge(s.unresolved, p?.unresolved, 'name') };
        });
      })
      .catch((e) => { if (mine === seq.current) setError(errText(e)); })
      .finally(() => { if (mine === seq.current) setBusy(false); });
  };
  useEffect(() => run(), []);
  // A change invalidates any preview still in flight, too.
  const change = (patch) => { seq.current += 1; setBusy(false); const next = { ...opts, ...patch }; setOpts(next); setStale(true); return next; };
  const changes = seen.changes;
  const unresolved = seen.unresolved;
  const conflicts = changes.some((c) => c.action === 'replace' && !opts.skip.includes(c.item));
  const missing = (preview?.unresolved || []).some((u) => !String(opts.values[u.name] || '').trim());
  const nothing = !changes.some((c) => c.action !== 'unchanged' && !opts.skip.includes(c.item));
  const blocked = !preview?.preview_token || stale || busy || missing || nothing || (conflicts && !opts.replace);
  const apply = () => confirm({
    title: `Import ${item.name} into this node's config?`,
    body: importConsequence(item, board, preview, opts.skip, by),
    okLabel: 'Import',
    busyLabel: 'Importing…',
    action: () => actions.importBoardItem(board.id, item.id, version, preview.preview_token, choices(opts)),
  }).then((r) => { if (r) onDone(`Imported ${(r.applied || []).length} items from ${item.name}`); })
    .catch((e) => {
      // Every import attempt spends the preview token: preview again.
      setStale(true);
      setError(e?.code === 'preview_required' || e?.code === 'preview_changed' ? 'This node\'s config or the choices changed since the preview — preview again, then import.' : `${errText(e)} — preview again to retry.`);
    });
  return html`<${Overlay} id="fleet-board-import" labelledby="fleet-board-import-title" onClose=${onClose}>
    <h3 id="fleet-board-import-title">Import ${item.name} <span class="muted">v ${short(version)}</span></h3>
    <div class="muted">From board ${board.name}, posted by ${by}. Nothing changes here until you import.</div>
    ${changes.length > 0 && html`<table class="fa-table" id="fleet-board-import-changes"><thead><tr><th></th><th>Item</th><th>Change</th><th></th></tr></thead>
      <tbody>${changes.map((c) => html`<tr key=${c.item} data-item=${c.item}>
        <td><input type="checkbox" aria-label=${`include ${c.item}`} checked=${!opts.skip.includes(c.item)} disabled=${c.action === 'unchanged'}
          onChange=${(e) => change({ skip: e.currentTarget.checked ? opts.skip.filter((x) => x !== c.item) : [...opts.skip, c.item] })} /></td>
        <td><code>${c.item}</code></td>
        <td class=${c.action === 'replace' ? 'fa-warn' : c.action === 'unchanged' ? 'muted' : ''}>${c.action === 'replace' ? 'overwrites yours' : c.action === 'create' ? 'new' : 'unchanged'}</td>
        <td>${c.security && c.action !== 'unchanged' ? html`<span class="fa-badge fa-sensitive" title="decides what this node's agents may do">security</span>` : ''}</td>
      </tr>`)}</tbody></table>`}
    ${preview && !changes.length && html`<div class="muted">This version has nothing to import.</div>`}
    ${(preview?.warnings || []).map((w) => html`<div class="fa-warn">${w}</div>`)}
    ${changes.some((c) => c.action === 'replace') && html`<label class="fa-of-check"><input id="fleet-board-import-replace" type="checkbox" checked=${opts.replace} onChange=${(e) => change({ replace: e.currentTarget.checked })} /> overwrite my items that differ (otherwise untick them)</label>`}
    ${unresolved.length > 0 && html`<div class="fa-cli-note">Values this item needs on this node:</div>
      ${unresolved.map((u) => html`<label class="fa-of-row" key=${u.name}><span class="fa-k">${u.name}</span><input data-placeholder=${u.name} value=${opts.values[u.name] || ''} placeholder=${u.original || `${u.item} ${u.field}`} autocomplete="off" spellcheck="false" onInput=${(e) => change({ values: { ...opts.values, [u.name]: e.currentTarget.value } })} /></label>`)}`}
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation boards preview|import --board ${board.id} --item ${item.id} --version ${version}</code></div>
    <div class="modal-buttons"><span class="spacer"></span>
      <button type="button" id="fleet-board-import-preview" disabled=${busy} onClick=${() => run()}>${busy ? 'Previewing…' : stale && preview ? 'Preview again' : 'Preview'}</button>
      <button type="button" class="primary" id="fleet-board-import-apply" disabled=${blocked} title=${stale && preview ? 'Preview again after changing options' : ''} onClick=${apply}>Import…</button>
      <button type="button" onClick=${onClose}>Close</button></div>
  </${Overlay}>`;
}

// VersionsDialog lists an item's versions: open one, or keep (pin) it.
function VersionsDialog({ board, item, name, actions, confirm, toast, onOpen, onRepost, onClose, onPinned }) {
  const [versions, setVersions] = useState(null);
  useEffect(() => {
    let off = false;
    actions.boardItemVersions(board.id, item.id).then((v) => { if (!off) setVersions(v); }).catch((e) => { if (!off) setVersions({ error: errText(e) }); });
    return () => { off = true; };
  }, []);
  const pin = (v) => confirm({
    title: `Keep version ${short(v.version)} of ${item.name}?`,
    body: `This node keeps version ${short(v.version)} through the board's retention, and the list shows when a newer version is posted. It is not imported.`,
    okLabel: 'Keep version',
    busyLabel: 'Saving…',
    action: () => actions.pinBoardItem(board.id, item.id, v.version),
  }).then((r) => { if (r) { toast(`Keeping ${short(v.version)}`, false); onPinned(); } }).catch((e) => toast(`Keep failed: ${errText(e)}`, true));
  const list = Array.isArray(versions) ? versions : [];
  return html`<${Overlay} id="fleet-board-versions" labelledby="fleet-board-versions-title" onClose=${onClose}>
    <h3 id="fleet-board-versions-title">Versions of ${item.name}</h3>
    ${versions?.error ? html`<div class="fa-danger">${versions.error}</div>` : !versions ? html`<div class="muted">Loading…</div>` : html`<table class="fa-table" id="fleet-board-version-list">
      <thead><tr><th>Version</th><th>Posted by</th><th>Size</th><th></th></tr></thead>
      <tbody>${list.map((v) => html`<tr key=${v.version} data-version=${v.version}>
        <td><code title=${v.version}>${short(v.version)}</code>${v.version === item.pinned_version ? html` <span class="fa-badge">kept</span>` : ''}${v.version === item.latest_version ? html` <span class="muted">latest</span>` : ''}</td>
        <td>${v.invalid ? html`<span class="fa-danger">${v.error || 'invalid'}</span>` : postedBy(v, name)}</td>
        <td>${size(v.bytes)}</td>
        <td class="fa-acts">${!v.invalid && html`<button type="button" data-version-act="open" onClick=${() => onOpen(v)}>Open…</button>
          ${v.version !== item.pinned_version && html`<button type="button" data-version-act="pin" onClick=${() => pin(v)}>Keep…</button>`}
          ${onRepost && html`<button type="button" data-version-act="repost" onClick=${() => onRepost(v)}>Post to…</button>`}`}</td>
      </tr>`)}</tbody></table>`}
    <div class="modal-buttons"><span class="spacer"></span><button type="button" onClick=${onClose}>Close</button></div>
  </${Overlay}>`;
}

// BoardItems is a board's item list inside its detail view.
export function BoardItems({ board, boards = [], name, actions, confirm, toast }) {
  const [items, setItems] = useState(null);
  const [tick, setTick] = useState(0);
  const [dialog, setDialog] = useState(null);
  useEffect(() => {
    let off = false;
    actions.boardItems(board.id).then((v) => { if (!off) setItems(v); }).catch((e) => { if (!off) setItems({ error: errText(e) }); });
    return () => { off = true; };
  }, [board.id, tick]);
  const reload = () => setTick((n) => n + 1);
  const done = (msg) => { setDialog(null); toast(msg, false); reload(); };
  const list = Array.isArray(items) ? items : [];
  const poster = canPost(board) && !board.frozen;
  const reposter = repostTargets(board, boards).length > 0;
  const withVersion = (it, v) => ({ ...it, digest: v.digest, bytes: v.bytes, publisher: v.publisher, republisher: v.republisher });
  const open = (item, version) => setDialog({ kind: 'inspect', item, version });
  const d = dialog;
  const by = d?.item ? postedBy(d.item, name) : '';
  // The inspector reads a fetched version like a bundle offer.
  const inspectActions = d?.kind === 'inspect' && {
    fetchOffer: () => actions.fetchBoardItem(board.id, d.item.id, d.version),
    offerContents: () => actions.boardItemContents(board.id, d.item.id, d.version),
    offerEntry: (_, path, offset, max) => actions.boardItemEntry(board.id, d.item.id, d.version, path, offset, max),
    downloadOffer: () => actions.downloadBoardItem(board.id, d.item.id, d.version),
  };
  return html`<div class="fa-board-items" id="fleet-board-items">
    <div class="fa-row"><b>Shared config</b><span class="spacer"></span>
      ${poster && html`<button type="button" id="fleet-board-post" onClick=${() => setDialog({ kind: 'publish' })}>Post config…</button>`}</div>
    ${items?.error ? html`<div class="fa-danger">${items.error}</div>` : !items ? html`<div class="muted">Loading…</div>` : !list.length ? html`<div class="muted" id="fleet-board-items-empty">Nothing posted yet.</div>` : html`<table class="fa-table" id="fleet-board-item-list">
      <thead><tr><th>Item</th><th>Posted by</th><th>Size</th><th></th></tr></thead>
      <tbody>${list.map((it) => it.invalid ? html`<tr key=${`${it.id}/${it.version}`} data-item-id=${it.id} class="fa-invalid">
          <td>${it.name || 'Invalid item'}</td><td colspan="2" class="fa-danger">${it.error || 'could not be verified'}</td><td class="fa-acts muted">not importable</td></tr>`
        : html`<tr key=${it.id} data-item-id=${it.id}>
          <td>${it.name}${it.update_available ? html` <span class="fa-badge" title="A newer version than the one you keep was posted">update</span>` : ''}${it.pinned_version ? html` <span class="muted">kept ${short(it.pinned_version)}</span>` : ''}</td>
          <td>${postedBy(it, name)}</td>
          <td>${size(it.bytes)}</td>
          <td class="fa-acts">
            <button type="button" data-item-act="open" onClick=${() => open(it, it.latest_version || it.version)}>Open…</button>
            <button type="button" data-item-act="versions" onClick=${() => setDialog({ kind: 'versions', item: it })}>Versions…</button>
            ${poster && html`<button type="button" data-item-act="update" onClick=${() => setDialog({ kind: 'publish', item: it })}>New version…</button>`}
            ${reposter && html`<button type="button" data-item-act="repost" title="Post this item, unchanged, to another board you can post to" onClick=${() => setDialog({ kind: 'repost', item: it, version: it.latest_version || it.version })}>Post to…</button>`}
          </td></tr>`)}</tbody></table>`}
    ${d?.kind === 'publish' && html`<${PublishDialog} board=${board} item=${d.item} actions=${actions} confirm=${confirm} onClose=${() => setDialog(null)} onDone=${done} />`}
    ${d?.kind === 'versions' && html`<${VersionsDialog} board=${board} item=${d.item} name=${name} actions=${actions} confirm=${confirm} toast=${toast} onOpen=${(v) => open(withVersion(d.item, v), v.version)}
      onRepost=${reposter ? (v) => setDialog({ kind: 'repost', item: withVersion(d.item, v), version: v.version }) : null} onClose=${() => setDialog(null)} onPinned=${() => { setDialog(null); reload(); }} />`}
    ${d?.kind === 'inspect' && html`<${BundleInspectDialog} offer=${{ offer: { type: 'config', summary: `${d.item.name} · posted by ${by} · sha256 ${d.item.digest || '—'}`, bytes: d.item.bytes }, peer: '' }} label=${() => ''}
      title=${`${d.item.name} (version ${short(d.version)})`} note="verified on this node; nothing is imported until you import it."
      actions=${inspectActions} toast=${toast} onImport=${() => setDialog({ kind: 'import', item: d.item, version: d.version })} onClose=${() => setDialog(null)} />`}
    ${d?.kind === 'repost' && html`<${RepostDialog} board=${board} boards=${boards} item=${d.item} version=${d.version} by=${by} actions=${actions} confirm=${confirm} onClose=${() => setDialog(null)} onDone=${(m) => { setDialog(null); toast(m, false); }} />`}
    ${d?.kind === 'import' && html`<${ImportDialog} board=${board} item=${d.item} version=${d.version} by=${by} actions=${actions} confirm=${confirm} onClose=${() => setDialog(null)} onDone=${done} />`}
  </div>`;
}
